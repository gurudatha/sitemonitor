package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sitemonitor/internal/checks"
	"sitemonitor/internal/config"
	"sitemonitor/internal/periodic"
	"sitemonitor/internal/store"
)

var (
	admin = Actor{User: "alice", Role: config.RoleAdmin}
	opA   = Actor{User: "bob", Role: config.RoleOperator}
	opB   = Actor{User: "carol", Role: config.RoleOperator}
	view  = Actor{User: "dave", Role: config.RoleViewer}
)

func testCfg(dir string) config.JobsConf {
	n := config.Node{}
	n.Node.DataDir = dir
	n.Jobs.Enabled = true
	n.Jobs.MaxConcurrent = 2
	// reuse the real defaults
	raw := []byte("node: {id: a, region: IDC, data_dir: " + dir + "}\npeer: {id: b, region: FC, url: https://b}\ntls: {cert: c, key: k, ca: ca}\nmonitor: {sites_file: s}\nauth: {providers: [ldap], ldap: {url: 'ldaps://x', base_dn: dc=x, default_role: viewer}}\njobs: {enabled: true}\n")
	parsed, err := config.ParseNode(raw)
	if err != nil {
		panic(err)
	}
	return parsed.Jobs
}

type harness struct {
	svc     *Service
	engine  *periodic.Engine
	st      *store.Store
	results []checks.Result
	events  []string
	owned   map[string]bool
}

func newHarness(t *testing.T, mutate ...func(*config.JobsConf)) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := testCfg(dir)
	for _, m := range mutate {
		m(&cfg)
	}
	h := &harness{st: st, owned: map[string]bool{"IDC": true},
		engine: periodic.New(map[string]periodic.PoolConfig{Class: {Workers: cfg.MaxConcurrent, Queue: 16}})}
	h.svc, err = New(cfg, "monitor-idc-01", "IDC", st, h.engine, func() map[string]bool { return h.owned },
		func(r checks.Result) { h.results = append(h.results, r); st.AddResult(r) },
		func(kind, sev, msg string) { h.events = append(h.events, sev+" "+msg) })
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) upload(t *testing.T, a Actor, name, content string) {
	t.Helper()
	if _, err := h.svc.UploadBinary(a, name, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
}

func interval(name, bin string, args ...string) Input {
	return Input{Name: name, Binary: bin, Args: args, Schedule: Schedule{Type: TypeInterval, Every: "5m", RunFor: "8h"}}
}

func TestValidation(t *testing.T) {
	h := newHarness(t)
	h.upload(t, opA, "ok.sh", "#!/bin/sh\necho hi\n")
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"bad interval", Input{Name: "a", Binary: "ok.sh", Schedule: Schedule{Type: TypeInterval, Every: "3m"}}, "schedule.every"},
		{"user window > 24h", Input{Name: "a", Binary: "ok.sh", Schedule: Schedule{Type: TypeInterval, Every: "1h", RunFor: "25h"}}, "at most 24h"},
		{"unknown binary", interval("a", "missing.py"), "not been uploaded"},
		{"path traversal", interval("a", "../etc/passwd"), "binary must be"},
		{"bad name", interval("Bad Name", "ok.sh"), "name must be"},
		{"bad target os", Input{Name: "a", Binary: "ok.sh", Schedule: Schedule{Type: TypeStartup}, Target: &Target{Host: "10.0.0.5", OS: "solaris"}}, "target.os"},
		{"unknown credential", Input{Name: "a", Binary: "ok.sh", Schedule: Schedule{Type: TypeStartup}, Target: &Target{Host: "10.0.0.5", OS: OSLinux, Credential: "nope"}}, "credential"},
	}
	for _, c := range cases {
		_, err := h.svc.Create(opA, c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}
	// every allowed interval is accepted
	for i, every := range Intervals {
		in := Input{Name: "iv-" + strings.ReplaceAll(every, "m", "min") + "-" + string(rune('a'+i)), Binary: "ok.sh", Schedule: Schedule{Type: TypeInterval, Every: every}}
		if _, err := h.svc.Create(opA, in); err != nil {
			t.Errorf("interval %s rejected: %v", every, err)
		}
	}
}

func TestUserJobRunsImmediatelyMergeNeedsAdmin(t *testing.T) {
	h := newHarness(t)
	h.upload(t, opA, "check.py", "print('ok')\n")
	if _, err := h.svc.Create(view, interval("x", "check.py")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer must not create jobs: %v", err)
	}
	in := interval("disk-check", "check.py")
	in.Schedule.RunFor = ""
	j, err := h.svc.Create(opA, in)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if j.Scope != ScopeUser || j.Status != StatusApproved || !j.Active(now) {
		t.Fatalf("user job must be active immediately: scope=%s status=%s state=%s", j.Scope, j.Status, j.State(now))
	}
	if d := j.Schedule.EndsAt.Sub(j.Schedule.StartAt); d != 24*time.Hour {
		t.Fatalf("default user window = %s, want 24h (the maximum)", d)
	}
	if j.State(now.Add(25*time.Hour)) != "expired" {
		t.Fatal("user job must expire after its window")
	}
	if j.BinarySHA == "" {
		t.Fatal("binary checksum must be pinned")
	}

	// another operator cannot touch it; the owner can
	if _, err := h.svc.Update(opB, j.ID, interval("disk-check", "check.py")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other user edit: %v", err)
	}
	// merge: owner requests, operators cannot approve, admin approves
	if _, err := h.svc.RequestMerge(opB, j.ID, "please"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("only the owner may request a merge: %v", err)
	}
	if j, err = h.svc.RequestMerge(opA, j.ID, "useful for everyone"); err != nil || j.MergeRequest.Status != "pending" {
		t.Fatalf("merge request: %v %+v", err, j)
	}
	if !j.Active(time.Now()) {
		t.Fatal("job keeps running while the merge request is pending")
	}
	if _, err := h.svc.ApproveMerge(opA, j.ID, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator must not approve a merge: %v", err)
	}
	j, err = h.svc.ApproveMerge(admin, j.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if j.Scope != ScopeAdmin || !j.Schedule.EndsAt.IsZero() || j.MergeRequest.Status != "approved" || j.MergedBy != "alice" {
		t.Fatalf("merged job: scope=%s ends=%v mr=%+v", j.Scope, j.Schedule.EndsAt, j.MergeRequest)
	}
	if j.State(time.Now().Add(30*24*time.Hour)) != "active" {
		t.Fatal("admin activity must not expire")
	}
	// now it is an admin activity: the former owner can no longer edit it
	if _, err := h.svc.Update(opA, j.ID, interval("disk-check", "check.py")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("user edit of merged job: %v", err)
	}

	// rejection keeps the user job
	k, _ := h.svc.Create(opB, interval("cpu-check", "check.py"))
	h.svc.RequestMerge(opB, k.ID, "")
	k, err = h.svc.RejectMerge(admin, k.ID, "too frequent")
	if err != nil || k.Scope != ScopeUser || k.MergeRequest.Status != "rejected" || !k.Active(time.Now()) {
		t.Fatalf("reject merge: %v %+v", err, k)
	}
}

func TestLocalRunExitCodesArgsTimeoutOutput(t *testing.T) {
	h := newHarness(t)
	script := `import sys, os
args = sys.argv[1:]
print("args=" + "|".join(args))
print("cwd-is-scratch=" + str(os.getcwd().endswith(os.environ["SITEMONITOR_RUN_ID"])))
if args and args[0] == "big":
    print("x" * 200000)
sys.exit(int(os.environ.get("RC", args[0] if args and args[0].isdigit() else "0")))
`
	h.upload(t, opA, "probe.py", script)
	run := func(name string, args ...string) checks.Result {
		t.Helper()
		j, err := h.svc.Create(opA, interval(name, "probe.py", args...))
		if err != nil {
			t.Fatal(err)
		}
		r, err := h.svc.RunNow(opA, j.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		return *r
	}
	if r := run("rc0", "0", "a b", `$(touch /tmp/pwned)`, "'q'"); r.Status != checks.StatusOK ||
		!strings.Contains(r.Details["stdout"], "args=0|a b|$(touch /tmp/pwned)|'q'") || !strings.Contains(r.Details["stdout"], "cwd-is-scratch=True") {
		t.Fatalf("rc0: %s %q", r.Status, r.Details["stdout"])
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		t.Fatal("arguments must never be interpreted by a shell")
	}
	if r := run("rc1", "1"); r.Status != checks.StatusWarn || r.Details["exit_code"] != "1" {
		t.Fatalf("exit 1 must be WARN: %s %s", r.Status, r.Details["exit_code"])
	}
	if r := run("rc2", "2"); r.Status != checks.StatusCrit || r.Details["exit_code"] != "2" {
		t.Fatalf("exit 2 must be CRIT: %s", r.Status)
	}
	if r := run("big", "big"); len(r.Details["stdout"]) > 66*1024 || !strings.Contains(r.Details["stdout"], "truncated") {
		t.Fatalf("output must be capped at 64 KB, got %d bytes", len(r.Details["stdout"]))
	}
	if len(h.svc.Runs(&Job{Name: "rc2"}, 10)) != 1 {
		t.Fatal("runs must be stored as results of site job:<name>")
	}

	h.upload(t, opA, "sleep.sh", "sleep 30\n")
	in := interval("slow", "sleep.sh")
	in.Timeout = "1s"
	j, _ := h.svc.Create(opA, in)
	start := time.Now()
	r, _ := h.svc.RunNow(opA, j.ID, true)
	if r.Status != checks.StatusCrit || !strings.Contains(r.Message, "timed out") || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %s %q after %s", r.Status, r.Message, time.Since(start))
	}
}

func TestReplacedBinary(t *testing.T) {
	h := newHarness(t)
	h.upload(t, admin, "shared.sh", "exit 0\n")
	adminJob, err := h.svc.Create(admin, interval("admin-job", "shared.sh"))
	if err != nil || adminJob.Scope != ScopeAdmin {
		t.Fatalf("admin job: %v", err)
	}
	h.upload(t, opA, "mine.sh", "exit 0\n")
	userJob, _ := h.svc.Create(opA, interval("user-job", "mine.sh"))

	// another operator cannot overwrite someone else's binary
	if _, err := h.svc.UploadBinary(opB, "shared.sh", strings.NewReader("evil")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("overwrite by other user: %v", err)
	}
	// admin replaces the binary of an admin activity: blocked until re-approved
	h.upload(t, admin, "shared.sh", "exit 0 # v2\n")
	r, _ := h.svc.RunNow(admin, adminJob.ID, true)
	if r.Status != checks.StatusCrit || !strings.Contains(r.Message, "changed since approval") {
		t.Fatalf("changed binary must not run: %s %q", r.Status, r.Message)
	}
	if _, err := h.svc.Approve(admin, adminJob.ID); err != nil {
		t.Fatal(err)
	}
	if r, _ = h.svc.RunNow(admin, adminJob.ID, true); r.Status != checks.StatusOK {
		t.Fatalf("after re-approval: %s %q", r.Status, r.Message)
	}
	// the owner's own user job follows their new upload automatically
	h.upload(t, opA, "mine.sh", "exit 0 # v2\n")
	if r, _ = h.svc.RunNow(opA, userJob.ID, true); r.Status != checks.StatusOK {
		t.Fatalf("user job after own re-upload: %s %q", r.Status, r.Message)
	}
	// a binary in use cannot be deleted by its uploader
	if err := h.svc.DeleteBinary(opA, "mine.sh"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete binary in use: %v", err)
	}
}

func TestSchedulingAndOwnership(t *testing.T) {
	h := newHarness(t)
	h.upload(t, opA, "ok.sh", "exit 0\n")
	j, _ := h.svc.Create(opA, interval("tick", "ok.sh")) // Create -> Reconcile -> engine task
	fc := interval("fc-job", "ok.sh")
	fc.Region = "FC"
	h.svc.Create(opA, fc)

	key := "job:" + j.ID
	first, ok := h.engine.Next(key)
	if !ok {
		t.Fatal("an active job of an owned region must be an engine task")
	}
	if d := time.Until(first); d > 31*time.Second {
		t.Fatalf("first run must be within 30s, is in %s", d)
	}
	if h.engine.Stats(Class).Tasks != 1 {
		t.Fatal("the FC job must not be scheduled on a server that doesn't own FC")
	}

	h.engine.Tick(first)
	h.engine.Wait()
	if len(h.results) != 1 || h.results[0].Site != "job:tick" {
		t.Fatalf("want one run of the IDC job, got %+v", h.results)
	}
	if next, _ := h.engine.Next(key); !next.Equal(first.Add(5 * time.Minute)) {
		t.Fatalf("next run one interval later, got %s", next.Sub(first))
	}

	// restart: a fresh engine must not run the job before last run + interval
	h.engine.Sync(Class, nil)
	h.svc.Reconcile()
	if next, _ := h.engine.Next(key); next.Before(h.results[0].Time.Truncate(time.Millisecond).Add(5 * time.Minute)) {
		t.Fatalf("after restart the next run (%s) is earlier than last run + interval", next)
	}

	// failover: this server takes over FC -> the FC job becomes a task here
	h.owned["FC"] = true
	h.svc.Reconcile()
	if h.engine.Stats(Class).Tasks != 2 {
		t.Fatal("after takeover the FC job must be scheduled here")
	}
	// disabling / expiring removes the task
	h.svc.SetEnabled(opA, j.ID, false, "pause")
	if _, ok := h.engine.Next(key); ok {
		t.Fatal("a disabled job must not stay scheduled")
	}
}

func TestPeerSync(t *testing.T) {
	a, b := newHarness(t), newHarness(t)
	a.upload(t, opA, "sync.py", "print(1)\n")
	j, _ := a.svc.Create(opA, interval("synced", "sync.py"))
	a.st.CreateToken("agent", config.RoleViewer, "alice", 0)

	stA, _ := a.svc.State()
	n, err := b.svc.Merge(stA, func(name string) ([]byte, error) { return a.svc.BinaryContent(name) })
	if err != nil || n != 3 {
		t.Fatalf("merge: %d %v", n, err)
	}
	if a.svc.Hash() != b.svc.Hash() {
		t.Fatal("after merge both servers must have the same state hash")
	}
	if got, _ := b.svc.Get(j.ID); got == nil || got.BinarySHA != j.BinarySHA {
		t.Fatal("job not replicated")
	}
	if r, _ := b.svc.RunNow(opA, j.ID, true); r.Status != checks.StatusOK {
		t.Fatalf("replicated job must run on the peer (binary copied): %s %q", r.Status, r.Message)
	}
	// tampered content is refused
	a.upload(t, opA, "sync.py", "print(2)\n")
	stA, _ = a.svc.State()
	if _, err := b.svc.Merge(stA, func(string) ([]byte, error) { return []byte("tampered"), nil }); err == nil {
		t.Fatal("binary content that doesn't match its sha256 must be rejected")
	}
	// deletion propagates (tombstone), and older versions don't win
	a.svc.Delete(opA, j.ID)
	stA, _ = a.svc.State()
	b.svc.Merge(stA, func(name string) ([]byte, error) { return a.svc.BinaryContent(name) })
	if list, _ := b.svc.List(); len(list) != 0 {
		t.Fatal("deleted job must be removed on the peer")
	}
}
