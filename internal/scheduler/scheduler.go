// Package scheduler decides which checks are due and runs them on a bounded
// worker pool with per-host concurrency limits and hard deadlines.
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
)

type job struct {
	key   string
	sites *config.Sites
	site  config.Site
	check string
}

type entry struct {
	next time.Time
}

type Stats struct {
	Jobs       int       `json:"jobs"`
	QueueDepth int       `json:"queue_depth"`
	QueueCap   int       `json:"queue_cap"`
	Busy       int64     `json:"busy_workers"`
	Workers    int       `json:"workers"`
	Saturation float64   `json:"saturation"`
	Completed  int64     `json:"completed"`
	Dropped    int64     `json:"dropped"`
	LastCheck  time.Time `json:"last_check_completed"`
}

type Scheduler struct {
	cfg      *config.Node
	sites    atomic.Pointer[config.Sites]
	owned    func() map[string]bool
	onResult func(checks.Result)
	onConfig func(old, new *config.Sites)

	queue    chan job
	mu       sync.Mutex
	jobs     map[string]*entry
	inflight map[string]bool

	hostMu  sync.Mutex
	hostSem map[string]chan struct{}

	busy, completed, dropped atomic.Int64
	lastCompleted            atomic.Int64
	started                  time.Time
	seq                      atomic.Int64
}

func New(cfg *config.Node, sites *config.Sites, owned func() map[string]bool,
	onResult func(checks.Result), onConfig func(old, new *config.Sites)) *Scheduler {
	s := &Scheduler{
		cfg: cfg, owned: owned, onResult: onResult, onConfig: onConfig,
		queue:    make(chan job, cfg.Limits.QueueSize),
		jobs:     map[string]*entry{},
		inflight: map[string]bool{},
		hostSem:  map[string]chan struct{}{},
		started:  time.Now(),
	}
	s.sites.Store(sites)
	return s
}

func (s *Scheduler) Sites() *config.Sites { return s.sites.Load() }

func (s *Scheduler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Limits.Workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.worker(ctx) }()
	}
	go s.watchConfig(ctx)

	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait() // graceful: let in-flight checks finish (each is time-bound)
			return
		case <-t.C:
			s.tick()
		}
	}
}

// jitter returns d ± 10%.
func jitter(d time.Duration) time.Duration {
	return d + time.Duration((rand.Float64()*0.2-0.1)*float64(d))
}

func (s *Scheduler) saturated() bool {
	return len(s.queue) > cap(s.queue)*8/10 || s.busy.Load() >= int64(s.cfg.Limits.Workers)
}

func (s *Scheduler) tick() {
	sites := s.sites.Load()
	owned := s.owned()
	now := time.Now()

	desired := map[string]job{}
	for _, site := range sites.Sites {
		if !owned[site.Region] {
			continue
		}
		for _, c := range site.Checks {
			k := site.Name + "|" + c
			desired[k] = job{key: k, sites: sites, site: site, check: c}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.jobs {
		if _, ok := desired[k]; !ok {
			delete(s.jobs, k) // region handed back, or site removed from config
		}
	}
	slow := s.saturated()
	for k, j := range desired {
		iv := sites.Interval(&j.site, j.check)
		e, ok := s.jobs[k]
		if !ok {
			// New job (startup, config change or takeover): spread the first run
			// over one interval so a takeover does not create a burst.
			first := iv
			if first > 2*time.Minute {
				first = 2 * time.Minute
			}
			s.jobs[k] = &entry{next: now.Add(time.Duration(rand.Int64N(int64(first))))}
			continue
		}
		if now.Before(e.next) || s.inflight[k] {
			continue
		}
		if slow && j.site.Criticality == "low" {
			iv *= 3 // shed load from low-criticality sites when saturated
		}
		select {
		case s.queue <- j:
			s.inflight[k] = true
			e.next = now.Add(jitter(iv))
		default:
			s.dropped.Add(1)
			e.next = now.Add(10 * time.Second)
		}
	}
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

func (s *Scheduler) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-s.queue:
			s.busy.Add(1)
			r := s.execute(ctx, j.sites, &j.site, j.check)
			s.busy.Add(-1)
			s.mu.Lock()
			delete(s.inflight, j.key)
			s.mu.Unlock()
			if r != nil {
				s.onResult(*r)
			}
		}
	}
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
	s.completed.Add(1)
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
	s.mu.Lock()
	n := len(s.jobs)
	s.mu.Unlock()
	if n == 0 {
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
	s.mu.Lock()
	n := len(s.jobs)
	s.mu.Unlock()
	busy := s.busy.Load()
	st := Stats{
		Jobs: n, QueueDepth: len(s.queue), QueueCap: cap(s.queue), Busy: busy,
		Workers: s.cfg.Limits.Workers, Completed: s.completed.Load(), Dropped: s.dropped.Load(),
		Saturation: float64(busy) / float64(s.cfg.Limits.Workers),
	}
	if last := s.lastCompleted.Load(); last > 0 {
		st.LastCheck = time.Unix(0, last).UTC()
	}
	return st
}

// SetSites swaps in a new sites configuration (config API or file change).
// Jobs for removed sites disappear and new ones are scheduled on the next tick.
func (s *Scheduler) SetSites(ns *config.Sites) {
	old := s.sites.Swap(ns)
	if old.Hash != ns.Hash && s.onConfig != nil {
		s.onConfig(old, ns)
	}
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
