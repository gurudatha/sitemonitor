// Package store keeps check results and events in memory, snapshots them to
// disk, and de-duplicates results that arrive twice through replication.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"sitemonitor/internal/checks"
)

const (
	defaultHistory   = 200
	defaultMaxEvents = 500
)

// Event is something a human should see: an alert, a recovery, a failover.
type Event struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Node     string    `json:"node"`
	Kind     string    `json:"kind"`     // ALERT | RECOVERY | FAILOVER | HANDBACK | PEER | CONFIG | INFO
	Severity string    `json:"severity"` // OK | WARN | CRIT
	Site     string    `json:"site,omitempty"`
	Check    string    `json:"check,omitempty"`
	Message  string    `json:"message"`
	// Key identifies the problem this event is about (e.g. "site:shop|cert").
	// Events with the same key are grouped into one Incident: duplicates of an
	// open problem are dropped, clears and re-appearances are recorded.
	Key string `json:"key,omitempty"`
}

type Store struct {
	mu        sync.RWMutex
	history   map[string][]checks.Result // key: site|check, oldest first
	events    []Event
	seen      map[string]bool // event IDs
	incidents map[string]*Incident
	path      string
	keep      int // results kept per site/check (~0.6 KB each in memory)
	maxEv     int // events kept
}

// New creates a store bounded by limits.history_per_check and limits.max_events.
func New(dataDir string, historyPerCheck, maxEvents int) *Store {
	if historyPerCheck <= 0 {
		historyPerCheck = defaultHistory
	}
	if maxEvents <= 0 {
		maxEvents = defaultMaxEvents
	}
	s := &Store{history: map[string][]checks.Result{}, seen: map[string]bool{}, incidents: map[string]*Incident{}, path: filepath.Join(dataDir, "results.json"), keep: historyPerCheck, maxEv: maxEvents}
	s.load()
	return s
}

func key(site, check string) string { return site + "|" + check }

// AddResult stores a result; returns false if it was already present.
func (s *Store) AddResult(r checks.Result) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(r.Site, r.Check)
	h := s.history[k]
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].ID == r.ID {
			return false
		}
	}
	// insert keeping time order (replicated results can arrive late)
	i := sort.Search(len(h), func(i int) bool { return h[i].Time.After(r.Time) })
	h = append(h, checks.Result{})
	copy(h[i+1:], h[i:])
	h[i] = r
	if len(h) > s.keep {
		h = h[len(h)-s.keep:]
	}
	s.history[k] = h
	return true
}

// AddEvent stores an event. It returns false for an event already stored
// (replication) or a duplicate of an open incident; the caller then must not
// notify or replicate it.
func (s *Store) AddEvent(e Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[e.ID] {
		return false
	}
	s.seen[e.ID] = true
	if e.Key != "" && !s.trackLocked(&e) {
		return false
	}
	i := sort.Search(len(s.events), func(i int) bool { return s.events[i].Time.After(e.Time) })
	s.events = append(s.events, Event{})
	copy(s.events[i+1:], s.events[i:])
	s.events[i] = e
	if len(s.events) > s.maxEv {
		for _, old := range s.events[:len(s.events)-s.maxEv] {
			delete(s.seen, old.ID)
		}
		s.events = s.events[len(s.events)-s.maxEv:]
	}
	return true
}

// Latest returns the newest result of every site/check, from whichever node ran it.
func (s *Store) Latest() []checks.Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]checks.Result, 0, len(s.history))
	for _, h := range s.history {
		if len(h) > 0 {
			out = append(out, h[len(h)-1])
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Site != out[j].Site {
			return out[i].Site < out[j].Site
		}
		return out[i].Check < out[j].Check
	})
	return out
}

// History returns up to n most recent results for a site/check.
func (s *Store) History(site, check string, n int) []checks.Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.history[key(site, check)]
	if len(h) > n {
		h = h[len(h)-n:]
	}
	return append([]checks.Result(nil), h...)
}

// Events returns the newest n events, newest first.
func (s *Store) Events(n int) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Event, 0, n)
	for i := len(s.events) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s.events[i])
	}
	return out
}

// Uptime returns the share of OK/WARN availability results in the stored window.
func (s *Store) Uptime(site string) (float64, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.history[key(site, "availability")]
	if len(h) == 0 {
		return 0, 0
	}
	up := 0
	for _, r := range h {
		if r.Status != checks.StatusCrit {
			up++
		}
	}
	return float64(up) / float64(len(h)) * 100, len(h)
}

type snapshot struct {
	History   map[string][]checks.Result `json:"history"`
	Events    []Event                    `json:"events"`
	Incidents map[string]*Incident       `json:"incidents"`
}

// Save writes a snapshot atomically (write temp file, then rename).
func (s *Store) Save() error {
	s.mu.RLock()
	b, err := json.Marshal(snapshot{History: s.history, Events: s.events, Incidents: s.incidents})
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var snap snapshot
	if json.Unmarshal(b, &snap) != nil {
		return
	}
	for k, h := range snap.History {
		if len(h) > s.keep {
			h = h[len(h)-s.keep:]
		}
		s.history[k] = h
	}
	s.events = snap.Events
	for _, e := range s.events {
		s.seen[e.ID] = true
	}
	if snap.Incidents != nil {
		s.incidents = snap.Incidents
	}
}
