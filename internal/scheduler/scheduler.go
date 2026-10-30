// Package scheduler turns the website checks of sites.yaml into tasks of the
// periodic engine and runs each check with a per-host concurrency limit and a
// hard deadline.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"sitemonitor/internal/checks"
	"sitemonitor/internal/config"
	"sitemonitor/internal/periodic"
)

// Class is the periodic-engine pool used by website checks.
const Class = "check"

type Stats struct {
	Jobs       int       `json:"jobs"`
	QueueDepth int       `json:"queue_depth"`
	QueueCap   int       `json:"queue_cap"`
	Busy       int64     `json:"busy_workers"`
	Workers    int       `json:"workers"`
	Saturation float64   `json:"saturation"`
	Completed  int64     `json:"completed"`
	Dropped    int64     `json:"dropped"`
	Skipped    int64     `json:"skipped"` // previous run of the same check still busy
	Shed       int64     `json:"shed"`    // low-criticality runs postponed under saturation
	LastCheck  time.Time `json:"last_check_completed"`
}

// Scheduler turns sites.yaml into periodic tasks for the regions this server
// owns. Timing, pools and back-pressure are handled by the periodic engine.
type Scheduler struct {
	cfg      *config.Node
	sites    atomic.Pointer[config.Sites]
	owned    func() map[string]bool
	engine   *periodic.Engine
	lastRun  func(site, check string) time.Time // newest stored result (either server)
	onResult func(checks.Result)
	onConfig func(old, new *config.Sites)

	hostMu  sync.Mutex
	hostSem map[string]chan struct{}

	lastCompleted atomic.Int64
	started       time.Time
	seq           atomic.Int64
}

func New(cfg *config.Node, sites *config.Sites, owned func() map[string]bool, engine *periodic.Engine,
	lastRun func(site, check string) time.Time, onResult func(checks.Result), onConfig func(old, new *config.Sites)) *Scheduler {
	s := &Scheduler{
		cfg: cfg, owned: owned, engine: engine, lastRun: lastRun, onResult: onResult, onConfig: onConfig,
		hostSem: map[string]chan struct{}{},
		started: time.Now(),
	}
	s.sites.Store(sites)
	return s
}

func (s *Scheduler) Sites() *config.Sites { return s.sites.Load() }

// Run keeps the task set current until ctx ends. The cluster calls Reconcile
// on ownership changes and SetSites calls it on config changes; the slow
// ticker here is only a safety net.
func (s *Scheduler) Run(ctx context.Context) {
	go s.watchConfig(ctx)
	s.Reconcile()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Reconcile()
		}
	}
}

// Reconcile declares one task per site/check of the owned regions. Tasks that
// already exist keep their schedule; new ones (startup, config change,
// takeover) start spread over min(interval, 2m), but not before one interval
// after the last stored result, so a restart or failover keeps the rhythm.
func (s *Scheduler) Reconcile() {
	sites := s.sites.Load()
	owned := s.owned()
	now := time.Now()
	var tasks []periodic.Task
	for i := range sites.Sites {
		site := sites.Sites[i]
		if !owned[site.Region] {
			continue
		}
		for _, c := range site.Checks {
			iv := sites.Interval(&site, c)
			first := now.Add(time.Duration(rand.Int64N(int64(min(iv, 2*time.Minute)))))
			if s.lastRun != nil {
				if next := s.lastRun(site.Name, c).Add(iv); next.After(first) {
					first = next
				}
			}
			check := c
			tasks = append(tasks, periodic.Task{
				Key: "check:" + site.Name + "|" + c, Every: iv, First: first,
				Shed: site.Criticality == "low", // postponed first when the pool is saturated
				Run: func(ctx context.Context) {
					if r := s.execute(ctx, sites, &site, check); r != nil {
						s.onResult(*r)
					}
				},
			})
		}
	}
	s.engine.Sync(Class, tasks)
}

func (s *Scheduler) hostSemaphore(rawURL string) chan struct{} {
	host := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Host
	}
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	sem, ok := s.hostSem[host]
	if !ok {
		sem = make(chan struct{}, s.cfg.Limits.PerHostLimit)
		s.hostSem[host] = sem
	}
	return sem
}

// execute runs one check with a per-host limit and a hard deadline.
func (s *Scheduler) execute(parent context.Context, sites *config.Sites, site *config.Site, check string) *checks.Result {
	timeout := site.Timeout
	if check == config.CheckTLS {
		timeout *= 2 // several sequential handshakes
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	sem := s.hostSemaphore(site.URL)
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return nil // host busy for the whole window; try next interval
	case <-parent.Done():
		return nil
	}

	r := checks.Run(ctx, sites, site, check)
	r.ID = fmt.Sprintf("%s-%d-%d", s.cfg.Node.ID, s.started.Unix(), s.seq.Add(1))
	r.Node = s.cfg.Node.ID
	r.NodeRegion = s.cfg.Node.Region
	s.lastCompleted.Store(time.Now().UnixNano())
	return &r
}

// RunNow runs a check immediately (used for cross-region verification requests).
func (s *Scheduler) RunNow(siteName, check string) (*checks.Result, error) {
	sites := s.sites.Load()
	for i := range sites.Sites {
		if sites.Sites[i].Name == siteName {
			r := s.execute(context.Background(), sites, &sites.Sites[i], check)
			if r == nil {
				return nil, fmt.Errorf("host busy")
			}
			return r, nil
		}
	}
	return nil, fmt.Errorf("unknown site %q", siteName)
}

// Healthy: this node is actually performing checks (not just running).
func (s *Scheduler) Healthy() bool {
	if s.engine.Stats(Class).Tasks == 0 {
		return true // standby: nothing to do is fine
	}
	last := s.lastCompleted.Load()
	ref := s.started
	if last > 0 {
		ref = time.Unix(0, last)
	}
	// Some check must have completed within the shortest interval (+ grace).
	min := 24 * time.Hour
	sites := s.sites.Load()
	for _, iv := range sites.Defaults.Intervals {
		if iv < min {
			min = iv
		}
	}
	return time.Since(ref) < 3*min+30*time.Second
}

func (s *Scheduler) Stats() Stats {
	e := s.engine.Stats(Class)
	st := Stats{
		Jobs: e.Tasks, QueueDepth: e.Waiting, QueueCap: e.QueueCap, Busy: int64(e.Running),
		Workers: e.Workers, Completed: e.Completed, Dropped: e.Dropped, Skipped: e.Skipped, Shed: e.Shed,
		Saturation: float64(e.Running) / float64(max(e.Workers, 1)),
	}
	if last := s.lastCompleted.Load(); last > 0 {
		st.LastCheck = time.Unix(0, last).UTC()
	}
	return st
}

// SetSites swaps in a new sites configuration (config API or file change)
// and updates the tasks right away.
func (s *Scheduler) SetSites(ns *config.Sites) {
	old := s.sites.Swap(ns)
	if old.Hash != ns.Hash && s.onConfig != nil {
		s.onConfig(old, ns)
	}
	s.Reconcile()
}

// watchConfig hot-reloads sites.yaml when it changes on disk.
func (s *Scheduler) watchConfig(ctx context.Context) {
	path := s.cfg.Monitor.SitesFile
	var lastMod time.Time
	if fi, err := os.Stat(path); err == nil {
		lastMod = fi.ModTime()
	}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fi, err := os.Stat(path)
			if err != nil || !fi.ModTime().After(lastMod) {
				continue
			}
			lastMod = fi.ModTime()
			ns, err := config.LoadSites(path)
			if err == nil {
				err = ns.CheckLimits(s.cfg.Limits)
			}
			if err != nil {
				slog.Error("sites.yaml reload failed; keeping previous config", "err", err)
				continue
			}
			s.SetSites(ns)
		}
	}
}
