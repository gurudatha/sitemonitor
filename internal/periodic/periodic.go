// Package periodic is the one scheduler for everything that runs on a period
// in SiteMonitor: website checks and user/admin jobs.
//
// Design (kept deliberately light):
//
//   - Tasks live in a min-heap ordered by their next run time; a single timer
//     sleeps until the earliest one is due. Nothing is scanned and nothing
//     wakes up while no task is due. Add/remove/reschedule are O(log n).
//   - Providers (checks, jobs) declare their full task set per class with
//     Sync whenever something changes (config edit, job change, ownership
//     change). Tasks that are still present keep their next run time, so a
//     Sync never causes a burst or a skipped run.
//   - Each class has its own bounded worker pool and bounded wait queue, so
//     one class (a slow job) cannot starve another (website checks).
//     When the queue is full the run is dropped and retried shortly.
//   - No catch-up: after a pause or overload a task runs once, then keeps its
//     rhythm. A task never overlaps itself: a run that is still busy when the
//     next one is due makes the engine skip that occurrence.
//   - Optional shedding: when its pool is saturated, a "sheddable" task
//     (e.g. a low-criticality site) is postponed by 3 intervals.
//   - Do runs ad-hoc work (run-now, startup runs) through the same pools.
//
// The engine keeps no schedule on disk: providers compute a task's first run
// from the last stored result, so restarts and failovers keep the rhythm.
package periodic

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"time"
)

// Task is one periodic activity.
type Task struct {
	Key   string        // unique across all classes
	Class string        // worker pool, e.g. "check" or "job"
	Every time.Duration // period (> 0)
	// First is when a newly added task runs first. Ignored when the task
	// already exists with the same Every (it keeps its next run time).
	First time.Time
	Shed  bool                      // may be postponed while its pool is saturated
	Run   func(ctx context.Context) // must honour ctx
}

// PoolConfig bounds one class.
type PoolConfig struct {
	Workers int // runs at the same time
	Queue   int // runs waiting for a worker; beyond this a run is dropped
}

// ClassStats are counters of one class.
type ClassStats struct {
	Tasks     int       `json:"tasks"`
	Running   int       `json:"running"`
	Waiting   int       `json:"waiting"`
	Workers   int       `json:"workers"`
	QueueCap  int       `json:"queue_cap"`
	Completed int64     `json:"completed"`
	Dropped   int64     `json:"dropped"`        // queue full
	Skipped   int64     `json:"skipped"`        // previous run still busy
	Shed      int64     `json:"shed"`           // postponed under saturation
	LastDone  time.Time `json:"last_completed"` // zero if nothing completed yet
}

type entry struct {
	task    Task
	next    time.Time
	index   int // heap position; -1 when not in the heap
	running bool
}

type taskHeap []*entry

func (h taskHeap) Len() int           { return len(h) }
func (h taskHeap) Less(i, j int) bool { return h[i].next.Before(h[j].next) }
func (h taskHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *taskHeap) Push(x any)        { e := x.(*entry); e.index = len(*h); *h = append(*h, e) }
func (h *taskHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	e.index = -1
	return e
}

type pool struct {
	cfg   PoolConfig
	sem   chan struct{}
	stats ClassStats
}

// Engine schedules and runs tasks.
type Engine struct {
	mu    sync.Mutex
	h     taskHeap
	byKey map[string]*entry
	pools map[string]*pool
	wake  chan struct{}
	ctx   context.Context
	wg    sync.WaitGroup
	now   func() time.Time
}

var ErrUnknownClass = errors.New("periodic: unknown class")

// New creates an engine with one pool per class.
func New(pools map[string]PoolConfig) *Engine {
	e := &Engine{byKey: map[string]*entry{}, pools: map[string]*pool{}, wake: make(chan struct{}, 1),
		ctx: context.Background(), now: time.Now}
	for name, c := range pools {
		if c.Workers < 1 {
			c.Workers = 1
		}
		if c.Queue < 0 {
			c.Queue = 0
		}
		e.pools[name] = &pool{cfg: c, sem: make(chan struct{}, c.Workers),
			stats: ClassStats{Workers: c.Workers, QueueCap: c.Queue}}
	}
	return e
}

func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Upsert adds a task or replaces its definition. An existing task with the
// same period keeps its next run time.
func (e *Engine) Upsert(t Task) {
	e.mu.Lock()
	e.upsertLocked(t)
	e.mu.Unlock()
	e.poke()
}

func (e *Engine) upsertLocked(t Task) {
	if t.Every <= 0 || t.Run == nil || e.pools[t.Class] == nil {
		return
	}
	if old, ok := e.byKey[t.Key]; ok {
		keep := old.task.Every == t.Every && old.task.Class == t.Class
		old.task = t
		if !keep {
			old.next = t.First
			if old.index >= 0 {
				heap.Fix(&e.h, old.index)
			}
		}
		return
	}
	next := t.First
	if next.IsZero() {
		next = e.now()
	}
	en := &entry{task: t, next: next, index: -1}
	e.byKey[t.Key] = en
	heap.Push(&e.h, en)
}

// Remove deletes a task. A run in progress finishes but is not rescheduled.
func (e *Engine) Remove(key string) {
	e.mu.Lock()
	e.removeLocked(key)
	e.mu.Unlock()
	e.poke()
}

func (e *Engine) removeLocked(key string) {
	en, ok := e.byKey[key]
	if !ok {
		return
	}
	delete(e.byKey, key)
	if en.index >= 0 {
		heap.Remove(&e.h, en.index)
	}
}

// Sync makes the tasks of one class exactly the given set.
func (e *Engine) Sync(class string, tasks []Task) {
	e.mu.Lock()
	want := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		t.Class = class
		want[t.Key] = true
		e.upsertLocked(t)
	}
	for key, en := range e.byKey {
		if en.task.Class == class && !want[key] {
			e.removeLocked(key)
		}
	}
	e.mu.Unlock()
	e.poke()
}

// Next returns when a task runs next (for tests and diagnostics).
func (e *Engine) Next(key string) (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	en, ok := e.byKey[key]
	if !ok {
		return time.Time{}, false
	}
	return en.next, true
}

// Running reports whether a task's run is in progress.
func (e *Engine) Running(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	en, ok := e.byKey[key]
	return ok && en.running
}

// Stats returns the counters of a class.
func (e *Engine) Stats(class string) ClassStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.pools[class]
	if p == nil {
		return ClassStats{}
	}
	s := p.stats
	for _, en := range e.byKey {
		if en.task.Class == class {
			s.Tasks++
		}
	}
	return s
}

func (p *pool) saturated() bool {
	return p.stats.Running >= p.cfg.Workers && p.stats.Waiting >= p.cfg.Queue*8/10
}

// dispatchDue starts every task that is due at now and returns how long to
// sleep until the next one. Run calls it from its loop; tests call it with a
// chosen now.
func (e *Engine) dispatchDue(now time.Time) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	for e.h.Len() > 0 && !e.h[0].next.After(now) {
		en := e.h[0]
		t := en.task
		p := e.pools[t.Class]
		// next occurrence, without catch-up
		en.next = en.next.Add(t.Every)
		if !en.next.After(now) {
			en.next = now.Add(t.Every)
		}
		switch {
		case en.running:
			p.stats.Skipped++ // still busy: never overlap
		case t.Shed && p.saturated():
			p.stats.Shed++
			en.next = now.Add(3 * t.Every)
		case p.stats.Waiting >= p.cfg.Queue && len(p.sem) == cap(p.sem):
			p.stats.Dropped++
			en.next = now.Add(min(t.Every, 10*time.Second)) // retry soon
		default:
			en.running = true
			e.start(p, en)
		}
		heap.Fix(&e.h, 0)
	}
	if e.h.Len() == 0 {
		return time.Hour
	}
	return e.h[0].next.Sub(now)
}

// Tick dispatches what is due at now and returns the time until the next task.
// Run calls it from its timer loop; it is exported for tests of providers.
func (e *Engine) Tick(now time.Time) time.Duration { return e.dispatchDue(now) }

// Wait blocks until all task runs started so far have finished (tests).
func (e *Engine) Wait() { e.wg.Wait() }

// start runs one occurrence through the class pool (called with e.mu held).
func (e *Engine) start(p *pool, en *entry) {
	p.stats.Waiting++
	run, ctx := en.task.Run, e.ctx
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		select {
		case p.sem <- struct{}{}:
		case <-ctx.Done():
			e.mu.Lock()
			p.stats.Waiting--
			en.running = false
			e.mu.Unlock()
			return
		}
		e.mu.Lock()
		p.stats.Waiting--
		p.stats.Running++
		e.mu.Unlock()

		run(ctx)

		<-p.sem
		e.mu.Lock()
		p.stats.Running--
		p.stats.Completed++
		p.stats.LastDone = e.now()
		en.running = false
		e.mu.Unlock()
	}()
}

// Do runs fn now through a class pool (blocking until a worker is free or ctx
// ends). Used for run-now requests and startup runs.
func (e *Engine) Do(ctx context.Context, class string, fn func(ctx context.Context)) error {
	e.mu.Lock()
	p := e.pools[class]
	if p == nil {
		e.mu.Unlock()
		return ErrUnknownClass
	}
	p.stats.Waiting++
	e.mu.Unlock()
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		e.mu.Lock()
		p.stats.Waiting--
		e.mu.Unlock()
		return ctx.Err()
	}
	e.mu.Lock()
	p.stats.Waiting--
	p.stats.Running++
	e.mu.Unlock()
	defer func() {
		<-p.sem
		e.mu.Lock()
		p.stats.Running--
		p.stats.Completed++
		p.stats.LastDone = e.now()
		e.mu.Unlock()
	}()
	fn(ctx)
	return nil
}

// Run drives the engine until ctx ends, then waits for running tasks.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	e.ctx = ctx
	e.mu.Unlock()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			e.wg.Wait()
			return
		case <-timer.C:
		case <-e.wake:
		}
		wait := e.dispatchDue(e.now())
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
	}
}
