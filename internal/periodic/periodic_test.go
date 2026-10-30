package periodic

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

type recorder struct {
	mu   sync.Mutex
	runs []string
}

func (r *recorder) task(key string, every time.Duration, first time.Time) Task {
	return Task{Key: key, Class: "check", Every: every, First: first, Run: func(context.Context) {
		r.mu.Lock()
		r.runs = append(r.runs, key)
		r.mu.Unlock()
	}}
}

func (r *recorder) count(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, k := range r.runs {
		if k == key {
			n++
		}
	}
	return n
}

func settle(e *Engine) { e.wg.Wait() }

func TestOrderAndRhythm(t *testing.T) {
	e := New(map[string]PoolConfig{"check": {Workers: 4, Queue: 10}})
	r := &recorder{}
	e.Upsert(r.task("a", time.Minute, t0))
	e.Upsert(r.task("b", 5*time.Minute, t0.Add(30*time.Second)))

	if wait := e.dispatchDue(t0); wait != 30*time.Second {
		t.Fatalf("sleep until the earliest task: got %s", wait)
	}
	settle(e)
	if r.count("a") != 1 || r.count("b") != 0 {
		t.Fatalf("only a is due at t0: %v", r.runs)
	}
	e.dispatchDue(t0.Add(30 * time.Second))
	settle(e)
	if n, _ := e.Next("a"); !n.Equal(t0.Add(time.Minute)) {
		t.Fatalf("a keeps its rhythm, next %s", n)
	}
	if n, _ := e.Next("b"); !n.Equal(t0.Add(30*time.Second + 5*time.Minute)) {
		t.Fatalf("b next %s", n)
	}
}

func TestNoCatchUpAfterPause(t *testing.T) {
	e := New(map[string]PoolConfig{"check": {Workers: 1, Queue: 1}})
	r := &recorder{}
	e.Upsert(r.task("a", time.Minute, t0))
	e.dispatchDue(t0.Add(10 * time.Minute)) // 10 intervals late
	settle(e)
	e.dispatchDue(t0.Add(10*time.Minute + time.Second))
	settle(e)
	if r.count("a") != 1 {
		t.Fatalf("missed runs must not be replayed: %d runs", r.count("a"))
	}
	if n, _ := e.Next("a"); !n.Equal(t0.Add(11 * time.Minute)) {
		t.Fatalf("next run one interval after the late run, got %s", n)
	}
}

func TestNeverOverlapsAndPoolsAreBounded(t *testing.T) {
	e := New(map[string]PoolConfig{"job": {Workers: 1, Queue: 0}, "check": {Workers: 2, Queue: 2}})
	release := make(chan struct{})
	var started atomic.Int32
	slow := Task{Key: "slow", Class: "job", Every: time.Minute, First: t0, Run: func(context.Context) { started.Add(1); <-release }}
	e.Upsert(slow)
	e.dispatchDue(t0)
	for started.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	e.dispatchDue(t0.Add(time.Minute)) // still running: skipped, not overlapped
	if s := e.Stats("job"); s.Skipped != 1 || s.Running != 1 {
		t.Fatalf("overlap must be skipped: %+v", s)
	}
	// a busy job pool does not block checks
	r := &recorder{}
	e.Upsert(r.task("check-1", time.Minute, t0))
	e.dispatchDue(t0.Add(time.Minute))
	time.Sleep(20 * time.Millisecond)
	if r.count("check-1") != 1 {
		t.Fatal("checks must run while the job pool is busy")
	}
	// job pool full (1 worker, no queue): another job is dropped and retried soon
	other := Task{Key: "other", Class: "job", Every: time.Hour, First: t0, Run: func(context.Context) {}}
	e.Upsert(other)
	e.dispatchDue(t0.Add(2 * time.Minute))
	if s := e.Stats("job"); s.Dropped != 1 {
		t.Fatalf("full pool must drop: %+v", s)
	}
	if n, _ := e.Next("other"); !n.Equal(t0.Add(2*time.Minute + 10*time.Second)) {
		t.Fatalf("dropped run retried after 10s, got %s", n)
	}
	close(release)
	settle(e)
}

func TestSyncKeepsScheduleAndRemoves(t *testing.T) {
	e := New(map[string]PoolConfig{"check": {Workers: 2, Queue: 2}, "job": {Workers: 1}})
	r := &recorder{}
	e.Sync("check", []Task{r.task("a", time.Minute, t0.Add(10*time.Second)), r.task("b", time.Minute, t0)})
	e.Upsert(Task{Key: "j", Class: "job", Every: time.Minute, First: t0, Run: func(context.Context) {}})
	// same period, new First: keeps its next run (no burst on config reload)
	e.Sync("check", []Task{r.task("a", time.Minute, t0.Add(59*time.Second))})
	if n, ok := e.Next("a"); !ok || !n.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("a must keep its schedule, got %s", n)
	}
	if _, ok := e.Next("b"); ok {
		t.Fatal("b is no longer declared and must be removed")
	}
	if _, ok := e.Next("j"); !ok {
		t.Fatal("Sync of one class must not touch another")
	}
	// period changed: rescheduled from First
	e.Sync("check", []Task{r.task("a", 2*time.Minute, t0.Add(40*time.Second))})
	if n, _ := e.Next("a"); !n.Equal(t0.Add(40 * time.Second)) {
		t.Fatalf("changed period must reschedule, got %s", n)
	}
	if s := e.Stats("check"); s.Tasks != 1 {
		t.Fatalf("task count %d", s.Tasks)
	}
}

func TestShedUnderSaturation(t *testing.T) {
	e := New(map[string]PoolConfig{"check": {Workers: 1, Queue: 0}})
	release := make(chan struct{})
	var started atomic.Int32
	e.Upsert(Task{Key: "busy", Class: "check", Every: time.Hour, First: t0, Run: func(context.Context) { started.Add(1); <-release }})
	e.dispatchDue(t0)
	for started.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	r := &recorder{}
	low := r.task("low", time.Minute, t0)
	low.Shed = true
	e.Upsert(low)
	e.dispatchDue(t0)
	if n, _ := e.Next("low"); !n.Equal(t0.Add(3*time.Minute)) || e.Stats("check").Shed != 1 {
		t.Fatalf("low-criticality task must be postponed 3 intervals, next %s", n)
	}
	close(release)
	settle(e)
}

func TestRunLoopAndDo(t *testing.T) {
	e := New(map[string]PoolConfig{"check": {Workers: 2, Queue: 2}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	var n atomic.Int32
	e.Upsert(Task{Key: "fast", Class: "check", Every: 20 * time.Millisecond, First: time.Now(), Run: func(context.Context) { n.Add(1) }})
	time.Sleep(110 * time.Millisecond)
	if got := n.Load(); got < 3 || got > 7 {
		t.Fatalf("real timer: want ~5 runs in 110ms, got %d", got)
	}
	ran := false
	if err := e.Do(ctx, "check", func(context.Context) { ran = true }); err != nil || !ran {
		t.Fatalf("Do: %v", err)
	}
	if err := e.Do(ctx, "nope", func(context.Context) {}); err != ErrUnknownClass {
		t.Fatalf("unknown class: %v", err)
	}
	e.Remove("fast")
	before := n.Load()
	time.Sleep(60 * time.Millisecond)
	if n.Load() > before+1 {
		t.Fatal("removed task must stop running")
	}
	cancel()
	<-done
}

// BenchmarkIdle shows the cost of 100k scheduled tasks when none is due:
// one heap peek per wake-up, independent of the number of tasks.
func BenchmarkDispatchNothingDue(b *testing.B) {
	e := New(map[string]PoolConfig{"check": {Workers: 10, Queue: 10}})
	for i := 0; i < 100000; i++ {
		e.Upsert(Task{Key: string(rune(i)) + "k" + time.Duration(i).String(), Class: "check", Every: time.Hour,
			First: t0.Add(time.Hour + time.Duration(i)*time.Millisecond), Run: func(context.Context) {}})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.dispatchDue(t0)
	}
}
