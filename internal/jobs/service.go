package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sitemonitor/internal/checks"
	"sitemonitor/internal/config"
	"sitemonitor/internal/store"
)

var (
	ErrForbidden = errors.New("forbidden")
	ErrNotFound  = errNotFound
	ErrConflict  = errors.New("conflict")
)

// Actor is the authenticated caller.
type Actor struct {
	User string
	Role string // viewer | operator | admin
}

func (a Actor) isAdmin() bool { return config.RoleRank(a.Role) >= config.RoleRank(config.RoleAdmin) }
func (a Actor) isOperator() bool {
	return config.RoleRank(a.Role) >= config.RoleRank(config.RoleOperator)
}

// Service owns job definitions, binaries, scheduling and execution.
type Service struct {
	cfg      config.JobsConf
	node     string
	region   string
	st       *store.Store
	repo     *repo
	owned    func() map[string]bool
	onResult func(checks.Result)
	emit     func(kind, severity, msg string)

	// Changed is called after every local change (the server tells the peer to sync).
	Changed func()

	sem     chan struct{}
	mu      sync.Mutex
	running map[string]bool
	next    map[string]sched
	seq     atomic.Int64
	started time.Time
}

type sched struct {
	version int64
	next    time.Time
}

func New(cfg config.JobsConf, node, region string, st *store.Store, owned func() map[string]bool,
	onResult func(checks.Result), emit func(kind, severity, msg string)) (*Service, error) {
	for _, d := range []string{cfg.BinariesDir, cfg.WorkDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("jobs dir: %w", err)
		}
	}
	r, err := newRepo(st)
	if err != nil {
		return nil, fmt.Errorf("jobs schema: %w", err)
	}
	return &Service{cfg: cfg, node: node, region: region, st: st, repo: r, owned: owned, onResult: onResult,
		emit: emit, Changed: func() {}, sem: make(chan struct{}, cfg.MaxConcurrent),
		running: map[string]bool{}, next: map[string]sched{}, started: time.Now()}, nil
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Service) limits() Limits {
	creds := make([]string, 0, len(s.cfg.Remote.Credentials))
	for name := range s.cfg.Remote.Credentials {
		creds = append(creds, name)
	}
	sort.Strings(creds)
	return Limits{UserMaxRunFor: s.cfg.UserMaxRunFor, DefaultTimeout: s.cfg.DefaultTimeout, MaxTimeout: s.cfg.MaxTimeout, Credentials: creds}
}

// Options describes the choices offered to clients (UI and agents).
func (s *Service) Options() map[string]any {
	return map[string]any{
		"intervals": Intervals, "run_for": RunForChoices, "user_max_run_for": s.cfg.UserMaxRunFor.String(),
		"schedule_types": []string{TypeInterval, TypeStartup}, "target_os": []string{OSLinux, OSWindowsCygwin},
		"credentials": s.limits().Credentials, "regions": []string{"IDC", "FC"},
		"default_timeout": s.cfg.DefaultTimeout.String(), "max_timeout": s.cfg.MaxTimeout.String(),
		"exit_codes": s.cfg.ExitCodes, "require_approval": s.cfg.RequireApproval,
		"max_binary_size_mb": s.cfg.MaxBinarySizeMB, "interpreters": s.cfg.Interpreters,
	}
}

func (s *Service) touch(j *Job, who string) {
	now := time.Now().UTC()
	j.UpdatedAt, j.UpdatedBy = now, who
	if v := now.UnixNano(); v > j.Version {
		j.Version = v
	} else {
		j.Version++
	}
}

func (s *Service) save(j *Job, who string) error {
	s.touch(j, who)
	if err := s.repo.put(j); err != nil {
		return err
	}
	s.Changed()
	return nil
}

// pin records the binary's current checksum and opens the run window.
func (s *Service) pin(j *Job, now time.Time) error {
	path, err := s.binPath(j.Binary)
	if err != nil {
		return err
	}
	sha, err := fileSHA(path)
	if err != nil {
		return invalid("binary %q is not available: %v", j.Binary, err)
	}
	j.BinarySHA = sha
	start := now
	if j.Schedule.StartAt.After(now) {
		start = j.Schedule.StartAt
	}
	j.Schedule.StartAt = start
	j.Schedule.EndsAt = time.Time{}
	if j.Schedule.RunFor != "" {
		d, _ := time.ParseDuration(j.Schedule.RunFor)
		j.Schedule.EndsAt = start.Add(d)
	}
	return nil
}

func (s *Service) checkBinary(name string) error {
	b, err := s.repo.binary(name)
	if err != nil || b.Deleted {
		return invalid("binary %q has not been uploaded", name)
	}
	return nil
}

func (s *Service) nameTaken(name, exceptID string) bool {
	all, _ := s.repo.list(false)
	return slices.ContainsFunc(all, func(j Job) bool { return j.Name == name && j.ID != exceptID })
}

// ---------- queries ----------

func (s *Service) List() ([]Job, error)            { return s.repo.list(false) }
func (s *Service) Get(id string) (*Job, error)     { return s.repo.get(id) }
func (s *Service) Binaries() ([]BinaryMeta, error) { return s.repo.binaries(false) }

func (s *Service) Binary(name string) (*BinaryMeta, error) {
	b, err := s.repo.binary(name)
	if err != nil || b.Deleted {
		return nil, errNotFound
	}
	return b, nil
}

// Runs returns the stored runs of a job (newest last).
func (s *Service) Runs(j *Job, n int) []checks.Result { return s.st.History(j.Site(), "job", n) }

// ---------- job lifecycle ----------

// Create stores a new job. Operators create user jobs and admins create admin
// jobs; both are active immediately. (With jobs.require_approval, user jobs
// wait for an admin first.)
func (s *Service) Create(a Actor, in Input) (*Job, error) {
	if !a.isOperator() {
		return nil, ErrForbidden
	}
	scope := ScopeUser
	if a.isAdmin() && in.Scope != ScopeUser {
		scope = ScopeAdmin
	}
	if in.Region == "" {
		in.Region = s.region
	}
	if err := validate(&in, scope, s.limits()); err != nil {
		return nil, err
	}
	if err := s.checkBinary(in.Binary); err != nil {
		return nil, err
	}
	if s.nameTaken(in.Name, "") {
		return nil, fmt.Errorf("%w: a job named %q already exists", ErrConflict, in.Name)
	}
	now := time.Now().UTC()
	j := &Job{ID: newID(), Name: in.Name, Description: in.Description, Scope: scope, Status: StatusPending,
		Owner: a.User, Region: in.Region, Binary: in.Binary, Args: in.Args, Schedule: in.Schedule,
		Timeout: in.Timeout, Target: in.Target, CreatedAt: now}
	if j.Args == nil {
		j.Args = []string{}
	}
	if scope == ScopeAdmin || !s.cfg.RequireApproval {
		if err := s.pin(j, now); err != nil {
			return nil, err
		}
		j.Status, j.ApprovedBy, j.ApprovedAt = StatusApproved, a.User, now
	}
	if err := s.save(j, a.User); err != nil {
		return nil, err
	}
	if j.Status == StatusPending {
		s.emit("JOB", "WARN", fmt.Sprintf("job %q requested by %s (%s %s, %s): waiting for admin approval", j.Name, a.User, j.Binary, describe(j), where(j)))
	} else {
		s.emit("JOB", "OK", fmt.Sprintf("job %q created by %s (%s %s, %s)", j.Name, a.User, j.Binary, describe(j), where(j)))
	}
	return j, nil
}

func describe(j *Job) string {
	if j.Schedule.Type == TypeStartup {
		return "at server start"
	}
	s := "every " + j.Schedule.Every
	if j.Schedule.RunFor != "" {
		s += " for " + j.Schedule.RunFor
	}
	return s
}

func where(j *Job) string {
	if j.Target == nil {
		return "on the " + j.Region + " monitor"
	}
	return fmt.Sprintf("on %s:%d (%s)", j.Target.Host, j.Target.Port, j.Target.OS)
}

func (s *Service) canEdit(a Actor, j *Job) bool {
	return a.isAdmin() || (a.isOperator() && j.Scope == ScopeUser && j.Owner == a.User)
}

// Update changes a job. The run window restarts from now (a user job's window
// is still limited to 24h). A user's change to an admin activity is not allowed.
func (s *Service) Update(a Actor, id string, in Input) (*Job, error) {
	j, err := s.repo.get(id)
	if err != nil || j.Deleted {
		return nil, errNotFound
	}
	if !s.canEdit(a, j) {
		return nil, ErrForbidden
	}
	if in.Name == "" {
		in.Name = j.Name
	}
	if in.Name != j.Name {
		return nil, invalid("name cannot be changed (create a new job instead)")
	}
	if in.Region == "" {
		in.Region = j.Region
	}
	if err := validate(&in, j.Scope, s.limits()); err != nil {
		return nil, err
	}
	if err := s.checkBinary(in.Binary); err != nil {
		return nil, err
	}
	j.Description, j.Region, j.Binary, j.Args, j.Schedule, j.Timeout, j.Target =
		in.Description, in.Region, in.Binary, in.Args, in.Schedule, in.Timeout, in.Target
	if j.Args == nil {
		j.Args = []string{}
	}
	now := time.Now().UTC()
	switch {
	case j.Scope == ScopeUser && s.cfg.RequireApproval && !a.isAdmin():
		j.Status, j.ApprovedBy, j.ApprovedAt, j.BinarySHA, j.Reason = StatusPending, "", time.Time{}, "", ""
	case j.Status == StatusApproved || j.Status == StatusDisabled:
		if err := s.pin(j, now); err != nil {
			return nil, err
		}
		j.ApprovedBy, j.ApprovedAt = a.User, now
	}
	if err := s.save(j, a.User); err != nil {
		return nil, err
	}
	s.emit("JOB", "OK", fmt.Sprintf("job %q changed by %s (status %s)", j.Name, a.User, j.Status))
	return j, nil
}

// Delete removes a job (a tombstone, so the peer removes it too).
func (s *Service) Delete(a Actor, id string) error {
	j, err := s.repo.get(id)
	if err != nil || j.Deleted {
		return errNotFound
	}
	if !s.canEdit(a, j) {
		return ErrForbidden
	}
	j.Deleted = true
	if err := s.save(j, a.User); err != nil {
		return err
	}
	s.emit("JOB", "OK", fmt.Sprintf("job %q deleted by %s", j.Name, a.User))
	return nil
}

// Approve approves a pending job (jobs.require_approval mode) or re-approves an
// admin activity after its binary was replaced.
func (s *Service) Approve(a Actor, id string) (*Job, error) {
	return s.approve(a, id, false, nil)
}

// RequestMerge: the owner asks for a user job to become an admin periodic
// activity. The job keeps running as a user job until an admin decides.
func (s *Service) RequestMerge(a Actor, id, note string) (*Job, error) {
	j, err := s.repo.get(id)
	if err != nil || j.Deleted {
		return nil, errNotFound
	}
	if !a.isOperator() || (j.Owner != a.User && !a.isAdmin()) {
		return nil, ErrForbidden
	}
	if j.Scope != ScopeUser {
		return nil, fmt.Errorf("%w: job is already an admin activity", ErrConflict)
	}
	if j.MergeRequest != nil && j.MergeRequest.Status == "pending" {
		return nil, fmt.Errorf("%w: a merge request is already pending", ErrConflict)
	}
	if len(note) > 500 {
		return nil, invalid("note is longer than 500 characters")
	}
	j.MergeRequest = &MergeRequest{Status: "pending", RequestedBy: a.User, RequestedAt: time.Now().UTC(), Note: strings.TrimSpace(note)}
	if err := s.save(j, a.User); err != nil {
		return nil, err
	}
	s.emit("JOB", "WARN", fmt.Sprintf("%s asks to merge job %q (%s %s, %s) into admin periodic activities: waiting for admin approval",
		a.User, j.Name, j.Binary, describe(j), where(j)))
	return j, nil
}

// ApproveMerge (admin) moves a user job into the admin's periodic activities:
// scope admin, and runFor (nil/empty = no end) replaces the 24h user window.
// Admins can also merge a user job that has no request.
func (s *Service) ApproveMerge(a Actor, id string, runFor *string) (*Job, error) {
	return s.approve(a, id, true, runFor)
}

// RejectMerge (admin) declines a merge request; the job stays a user job.
func (s *Service) RejectMerge(a Actor, id, reason string) (*Job, error) {
	if !a.isAdmin() {
		return nil, ErrForbidden
	}
	j, err := s.repo.get(id)
	if err != nil || j.Deleted {
		return nil, errNotFound
	}
	if j.MergeRequest == nil || j.MergeRequest.Status != "pending" {
		return nil, fmt.Errorf("%w: no pending merge request", ErrConflict)
	}
	mr := j.MergeRequest
	mr.Status, mr.DecidedBy, mr.DecidedAt, mr.Reason = "rejected", a.User, time.Now().UTC(), strings.TrimSpace(reason)
	if err := s.save(j, a.User); err != nil {
		return nil, err
	}
	s.emit("JOB", "OK", fmt.Sprintf("merge of job %q (requested by %s) rejected by %s: %s; it stays a user job", j.Name, mr.RequestedBy, a.User, mr.Reason))
	return j, nil
}

func (s *Service) approve(a Actor, id string, merge bool, runFor *string) (*Job, error) {
	if !a.isAdmin() {
		return nil, ErrForbidden
	}
	j, err := s.repo.get(id)
	if err != nil || j.Deleted {
		return nil, errNotFound
	}
	if j.Status != StatusPending && j.Status != StatusApproved && j.Status != StatusDisabled {
		return nil, fmt.Errorf("%w: job is %s", ErrConflict, j.Status)
	}
	now := time.Now().UTC()
	if merge {
		if j.Scope == ScopeAdmin {
			return nil, fmt.Errorf("%w: job is already an admin activity", ErrConflict)
		}
		j.Scope, j.MergedBy, j.MergedAt = ScopeAdmin, a.User, now
		j.Schedule.RunFor = ""
		j.Schedule.StartAt = time.Time{}
		if j.MergeRequest != nil && j.MergeRequest.Status == "pending" {
			j.MergeRequest.Status, j.MergeRequest.DecidedBy, j.MergeRequest.DecidedAt = "approved", a.User, now
		} else {
			j.MergeRequest = &MergeRequest{Status: "approved", RequestedBy: a.User, RequestedAt: now, Note: "merged by admin", DecidedBy: a.User, DecidedAt: now}
		}
	}
	if runFor != nil {
		j.Schedule.RunFor = *runFor
	}
	in := Input{Name: j.Name, Description: j.Description, Region: j.Region, Binary: j.Binary, Args: j.Args,
		Schedule: j.Schedule, Timeout: j.Timeout, Target: j.Target}
	if err := validate(&in, j.Scope, s.limits()); err != nil {
		return nil, err
	}
	j.Schedule, j.Timeout = in.Schedule, in.Timeout
	if j.Status != StatusApproved || merge || runFor != nil {
		j.Schedule.StartAt = time.Time{} // a new window starts now
	}
	if err := s.pin(j, now); err != nil {
		return nil, err
	}
	j.Status, j.ApprovedBy, j.ApprovedAt, j.Reason = StatusApproved, a.User, now, ""
	if err := s.save(j, a.User); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("job %q approved by %s (%s, %s)", j.Name, a.User, describe(j), where(j))
	if merge {
		msg = fmt.Sprintf("job %q of %s merged into admin periodic activities by %s (%s, %s)", j.Name, j.Owner, a.User, describe(j), where(j))
	}
	s.emit("JOB", "OK", msg)
	return j, nil
}

// Reject declines a pending job.
func (s *Service) Reject(a Actor, id, reason string) (*Job, error) {
	if !a.isAdmin() {
		return nil, ErrForbidden
	}
	j, err := s.repo.get(id)
	if err != nil || j.Deleted {
		return nil, errNotFound
	}
	if j.Status != StatusPending {
		return nil, fmt.Errorf("%w: only pending jobs can be rejected (job is %s)", ErrConflict, j.Status)
	}
	j.Status, j.Reason = StatusRejected, strings.TrimSpace(reason)
	if err := s.save(j, a.User); err != nil {
		return nil, err
	}
	s.emit("JOB", "OK", fmt.Sprintf("job %q (requested by %s) rejected by %s: %s", j.Name, j.Owner, a.User, j.Reason))
	return j, nil
}

// SetEnabled pauses or resumes an approved job.
func (s *Service) SetEnabled(a Actor, id string, enabled bool, reason string) (*Job, error) {
	j, err := s.repo.get(id)
	if err != nil || j.Deleted {
		return nil, errNotFound
	}
	if !s.canEdit(a, j) {
		return nil, ErrForbidden
	}
	switch {
	case enabled && j.Status == StatusDisabled && j.ApprovedBy != "":
		j.Status, j.Reason = StatusApproved, ""
	case !enabled && j.Status == StatusApproved:
		j.Status, j.Reason = StatusDisabled, reason
	default:
		return nil, fmt.Errorf("%w: job is %s", ErrConflict, j.Status)
	}
	if err := s.save(j, a.User); err != nil {
		return nil, err
	}
	s.emit("JOB", "OK", fmt.Sprintf("job %q %s by %s", j.Name, map[bool]string{true: "enabled", false: "disabled"}[enabled], a.User))
	return j, nil
}

// ---------- binaries ----------

// UploadBinary adds or replaces a program in the binaries directory.
// Jobs pinned to the previous content stop running until re-approved.
func (s *Service) UploadBinary(a Actor, name string, r io.Reader) (*BinaryMeta, error) {
	if !a.isOperator() {
		return nil, ErrForbidden
	}
	if old, err := s.repo.binary(name); err == nil && !old.Deleted && old.UploadedBy != a.User && !a.isAdmin() {
		return nil, fmt.Errorf("%w: %q was uploaded by %s; only that user or an admin can replace it", ErrForbidden, name, old.UploadedBy)
	}
	sha, size, err := s.writeBinary(name, r, s.cfg.MaxBinarySizeMB<<20)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	b := &BinaryMeta{Name: name, SHA256: sha, Size: size, UploadedBy: a.User, UploadedMs: now.UnixMilli(), Version: now.UnixNano(), Kind: kindOf(name)}
	if err := s.st.Write(func(tx *sql.Tx) error { return putBinary(tx, b) }); err != nil {
		return nil, err
	}
	s.Changed()
	jobs, _ := s.repo.list(false)
	var blocked []string
	for i := range jobs {
		j := &jobs[i]
		if j.Binary != name || j.BinarySHA == sha || (j.Status != StatusApproved && j.Status != StatusDisabled) {
			continue
		}
		// A user's own user jobs follow their new upload (they run without approval);
		// admin activities keep the approved version pinned until an admin re-approves.
		if j.Scope == ScopeUser && j.Owner == a.User && !s.cfg.RequireApproval {
			j.BinarySHA = sha
			if err := s.save(j, a.User); err != nil {
				return nil, err
			}
			continue
		}
		blocked = append(blocked, j.Name)
	}
	msg := fmt.Sprintf("binary %q uploaded by %s (%d bytes, sha256 %s…)", name, a.User, size, sha[:12])
	sev := "OK"
	if len(blocked) > 0 {
		sev = "WARN"
		msg += fmt.Sprintf("; jobs %s still expect the previous version and won't run until re-approved by an admin (POST /api/jobs/{id}/approve)", strings.Join(blocked, ", "))
	}
	s.emit("JOB", sev, msg)
	return b, nil
}

// DeleteBinary removes a program. Admins can delete any; users only their own
// uploads that no job uses.
func (s *Service) DeleteBinary(a Actor, name string) error {
	b, err := s.repo.binary(name)
	if err != nil || b.Deleted {
		return errNotFound
	}
	jobs, _ := s.repo.list(false)
	var users []string
	for _, j := range jobs {
		if j.Binary == name {
			users = append(users, j.Name)
		}
	}
	if !a.isAdmin() {
		if !a.isOperator() || b.UploadedBy != a.User {
			return ErrForbidden
		}
		if len(users) > 0 {
			return fmt.Errorf("%w: used by jobs %s", ErrConflict, strings.Join(users, ", "))
		}
	}
	if path, err := s.binPath(name); err == nil {
		os.Remove(path)
	}
	b.Deleted, b.Version = true, time.Now().UnixNano()
	if err := s.st.Write(func(tx *sql.Tx) error { return putBinary(tx, b) }); err != nil {
		return err
	}
	s.Changed()
	s.emit("JOB", "OK", fmt.Sprintf("binary %q deleted by %s", name, a.User))
	return nil
}

// ---------- running ----------

// RunNow runs an approved job immediately (owner or admin). With wait, the
// result is returned; otherwise the run happens in the background.
func (s *Service) RunNow(a Actor, id string, wait bool) (*checks.Result, error) {
	j, err := s.repo.get(id)
	if err != nil || j.Deleted {
		return nil, errNotFound
	}
	if !s.canEdit(a, j) {
		return nil, ErrForbidden
	}
	if j.Status != StatusApproved {
		return nil, fmt.Errorf("%w: job is %s; only approved jobs can run", ErrConflict, j.Status)
	}
	if !s.tryStart(j.ID) {
		return nil, fmt.Errorf("%w: job is already running", ErrConflict)
	}
	trigger := "manual by " + a.User
	if !wait {
		go func() { defer s.finish(j.ID); s.execute(j, trigger) }()
		return nil, nil
	}
	defer s.finish(j.ID)
	r := s.execute(j, trigger)
	return &r, nil
}

func (s *Service) tryStart(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[id] {
		return false
	}
	s.running[id] = true
	return true
}

func (s *Service) finish(id string) {
	s.mu.Lock()
	delete(s.running, id)
	s.mu.Unlock()
}

func (s *Service) status(code int) string {
	switch {
	case slices.Contains(s.cfg.ExitCodes.OK, code):
		return checks.StatusOK
	case slices.Contains(s.cfg.ExitCodes.Warn, code):
		return checks.StatusWarn
	}
	return checks.StatusCrit
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	return line
}

// execute runs one job (bounded by the worker pool) and records the result.
func (s *Service) execute(j *Job, trigger string) checks.Result {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	start := time.Now()
	runID := fmt.Sprintf("%s-%d-%d", s.node, s.started.Unix(), s.seq.Add(1))
	r := checks.Result{ID: s.node + "-job-" + runID, Site: j.Site(), Region: j.Region, Check: "job", Node: s.node,
		NodeRegion: s.region, Time: start.UTC(), Details: map[string]string{
			"run_id": runID, "trigger": trigger, "binary": j.Binary, "scope": j.Scope, "owner": j.Owner, "target": "local"}}
	if j.Target != nil {
		r.Details["target"] = fmt.Sprintf("%s:%d (%s)", j.Target.Host, j.Target.Port, j.Target.OS)
	}
	defer func() { s.onResult(r) }()

	path, err := s.binPath(j.Binary)
	if err == nil {
		var sha string
		if sha, err = fileSHA(path); err == nil && sha != j.BinarySHA {
			err = fmt.Errorf("binary %s changed since approval (sha256 %s…, approved %s…): re-approval required", j.Binary, sha[:12], short(j.BinarySHA))
		}
	}
	if err != nil {
		r.Status, r.Message = checks.StatusCrit, "not run: "+err.Error()
		r.Details["exit_code"] = "-1"
		return r
	}
	r.Details["sha256"] = j.BinarySHA

	timeout, _ := time.ParseDuration(j.Timeout)
	if timeout <= 0 {
		timeout = s.cfg.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var out Outcome
	if j.Target == nil {
		out = s.runLocal(ctx, j, path, runID)
	} else {
		out = s.runRemote(ctx, j, path, runID)
	}
	r.LatencyMs = out.Duration.Milliseconds()
	r.Details["exit_code"] = fmt.Sprint(out.ExitCode)
	r.Details["duration_ms"] = fmt.Sprint(out.Duration.Milliseconds())
	r.Details["stdout"] = string(out.Stdout)
	r.Details["stderr"] = string(out.Stderr)
	switch {
	case out.Err != nil:
		r.Status, r.Message = checks.StatusCrit, out.Err.Error()
	default:
		r.Status = s.status(out.ExitCode)
		r.Message = fmt.Sprintf("exit %d in %s", out.ExitCode, out.Duration.Round(time.Millisecond))
		if l := firstLine(out.Stdout); l != "" && r.Status == checks.StatusOK {
			r.Message += ": " + l
		} else if l := firstLine(out.Stderr); l != "" {
			r.Message += ": " + l
		} else if l := firstLine(out.Stdout); l != "" {
			r.Message += ": " + l
		}
	}
	return r
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// ---------- scheduling ----------

// Run schedules jobs until ctx ends. ready is closed once ownership is known;
// startup jobs then run once for the regions this server owns.
func (s *Service) Run(ctx context.Context, ready <-chan struct{}) {
	select {
	case <-ctx.Done():
		return
	case <-ready:
	}
	s.runStartupJobs()

	t := time.NewTicker(time.Second)
	defer t.Stop()
	var cached []Job
	var loaded time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if time.Since(loaded) > 3*time.Second { // jobs change rarely; re-read every few seconds
				if all, err := s.repo.list(false); err == nil {
					cached, loaded = all, now
				}
			}
			s.tick(now, cached)
		}
	}
}

func (s *Service) runStartupJobs() {
	all, err := s.repo.list(false)
	if err != nil {
		slog.Error("startup jobs", "err", err)
		return
	}
	owned := s.owned()
	now := time.Now()
	for i := range all {
		j := all[i]
		if j.Schedule.Type != TypeStartup || !j.Active(now) || !owned[j.Region] {
			continue
		}
		if s.tryStart(j.ID) {
			go func() { defer s.finish(j.ID); s.execute(&j, "server startup") }()
		}
	}
}

func (s *Service) tick(now time.Time, all []Job) {
	owned := s.owned()
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for i := range all {
		j := all[i]
		if j.Schedule.Type != TypeInterval || !j.Active(now) || !owned[j.Region] {
			continue
		}
		seen[j.ID] = true
		every := j.Interval()
		sc, ok := s.next[j.ID]
		if !ok || sc.version != j.Version {
			// New, changed, or first seen after a restart / takeover: run soon (spread
			// over up to 30s so jobs don't start together), but never before one
			// interval after the last stored run, whichever server made it.
			spread := min(every, 30*time.Second)
			var b [2]byte
			rand.Read(b[:])
			next := now.Add(time.Duration(int(b[0])<<8|int(b[1])) * spread / 65536)
			if last := s.st.LastResultTime(j.Site(), "job"); !ok && last.Add(every).After(next) {
				next = last.Add(every)
			}
			sc = sched{version: j.Version, next: next}
		}
		if !now.Before(sc.next) && !s.running[j.ID] {
			select {
			case s.sem <- struct{}{}: // only start if a worker is free; otherwise try next second
				<-s.sem
				s.running[j.ID] = true
				sc.next = now.Add(every)
				go func() { defer s.finish(j.ID); s.execute(&j, "schedule every "+j.Schedule.Every) }()
			default:
			}
		}
		s.next[j.ID] = sc
	}
	for id := range s.next {
		if !seen[id] {
			delete(s.next, id)
		}
	}
}
