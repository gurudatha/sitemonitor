package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"sitemonitor/internal/alert"
	"sitemonitor/internal/auth"
	"sitemonitor/internal/checks"
	"sitemonitor/internal/config"
	"sitemonitor/internal/jobs"
	"sitemonitor/internal/store"
)

func (s *Server) jobRoutes() []route {
	v, op, ad := config.RoleViewer, config.RoleOperator, config.RoleAdmin
	return []route{
		{"GET", "/api/jobs/options", v, "jobs", "Choices for new jobs: intervals, run_for windows (max 24h for user jobs), target OS, credential names, regions", "", "", nil, "object", s.jobOptions},
		{"GET", "/api/jobs", v, "jobs", "All jobs with their state, last run and stored run counters", "", "", []string{"scope=user|admin", "owner", "merge=pending"}, "[]JobView", s.listJobs},
		{"POST", "/api/jobs", op, "jobs", "Add a job. User jobs run immediately for their window (<= 24h); admins create admin periodic activities (scope admin, no limit)",
			`{"name","description","region":"IDC|FC","binary","args":[],"schedule":{"type":"interval|startup","every":"1m|2m|5m|10m|15m|20m|30m|60m|1h|2h|4h|6h","run_for":"8h"},"timeout":"2m","target":{"host","port":22,"os":"linux|windows-cygwin","credential"} (omit for local),"scope":"user|admin" (admins)}`, "", nil, "Job", s.createJob},
		{"GET", "/api/jobs/{id}", v, "jobs", "One job", "", "", nil, "JobView", s.getJob},
		{"PUT", "/api/jobs/{id}", op, "jobs", "Change a job (owner or admin); the run window restarts now", "same as POST /api/jobs (name cannot change)", "", nil, "Job", s.updateJob},
		{"DELETE", "/api/jobs/{id}", op, "jobs", "Delete a job (owner or admin)", "", "", nil, "204", s.deleteJob},
		{"POST", "/api/jobs/{id}/run", op, "jobs", "Run now (owner or admin). ?wait=true returns the result", "", "", []string{"wait=true"}, "Result or 202", s.runJob},
		{"GET", "/api/jobs/{id}/runs", v, "jobs", "Stored runs (exit code, stdout, stderr, duration), oldest first", "", "", []string{"limit"}, "[]Result", s.jobRuns},
		{"POST", "/api/jobs/{id}/disable", op, "jobs", "Pause a job (owner or admin)", `{"reason"}`, "", nil, "Job", s.disableJob},
		{"POST", "/api/jobs/{id}/enable", op, "jobs", "Resume a paused job (owner or admin)", "", "", nil, "Job", s.enableJob},
		{"POST", "/api/jobs/{id}/merge-request", op, "jobs", "Ask an admin to merge this user job into the admin's periodic activities (the job keeps running meanwhile)", `{"note"}`, "", nil, "Job", s.requestMerge},
		{"POST", "/api/jobs/{id}/merge", ad, "jobs", "Approve the merge: the job becomes an admin periodic activity (no 24h limit unless run_for is given)", `{"run_for":"" (optional)}`, "", nil, "Job", s.approveMerge},
		{"POST", "/api/jobs/{id}/merge/reject", ad, "jobs", "Decline the merge request; the job stays a user job", `{"reason"}`, "", nil, "Job", s.rejectMerge},
		{"POST", "/api/jobs/{id}/approve", ad, "jobs", "Re-approve after its binary was replaced (pins the new checksum); approves pending jobs when jobs.require_approval is on", "", "", nil, "Job", s.approveJob},
		{"POST", "/api/jobs/{id}/reject", ad, "jobs", "Reject a pending job (jobs.require_approval mode)", `{"reason"}`, "", nil, "Job", s.rejectJob},

		{"GET", "/api/binaries", v, "binaries", "Programs in the binaries directory (Python, Perl, PowerShell, shell, executables)", "", "", nil, "[]BinaryMeta", s.listBinaries},
		{"PUT", "/api/binaries/{name}", op, "binaries", "Upload or replace a program (raw bytes, max jobs.max_binary_size_mb). Replacing a binary used by an admin activity requires re-approval", "program bytes", "application/octet-stream", nil, "BinaryMeta", s.uploadBinary},
		{"GET", "/api/binaries/{name}", v, "binaries", "Metadata of one program (size, sha256, uploader, kind)", "", "", nil, "BinaryMeta", s.getBinary},
		{"GET", "/api/binaries/{name}/content", op, "binaries", "Download a program", "", "", nil, "application/octet-stream", s.downloadBinary},
		{"DELETE", "/api/binaries/{name}", op, "binaries", "Delete a program (admin, or the uploader if no job uses it)", "", "", nil, "204", s.deleteBinary},
	}
}

func actorOf(r *http.Request) jobs.Actor {
	id, _ := auth.FromContext(r.Context())
	if id == nil {
		return jobs.Actor{}
	}
	return jobs.Actor{User: id.Username, Role: id.Role}
}

func jobError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, jobs.ErrForbidden):
		code = http.StatusForbidden
	case errors.Is(err, jobs.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, jobs.ErrConflict):
		code = http.StatusConflict
	case jobs.IsValidation(err):
		code = http.StatusUnprocessableEntity
	}
	writeJSONStatus(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 {
		return true // empty body allowed for optional bodies
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.Cfg.Limits.MaxAPIBodyKB<<10)).Decode(v); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

// JobView is a job plus its effective state and stored run statistics.
type JobView struct {
	jobs.Job
	State   string            `json:"state"` // active | scheduled | expired | disabled | pending | rejected
	LastRun *checks.Result    `json:"last_run,omitempty"`
	Stats   *store.CheckStats `json:"stats,omitempty"`
}

func (s *Server) jobViews(list []jobs.Job) []JobView {
	now := time.Now()
	latest := map[string]checks.Result{}
	for _, r := range s.Store.Latest() {
		if r.Check == "job" {
			latest[r.Site] = r
		}
	}
	stats := map[string]store.CheckStats{}
	for _, c := range s.Store.Stats(now) {
		if c.Check == "job" {
			stats[c.Site] = c
		}
	}
	out := make([]JobView, 0, len(list))
	for _, j := range list {
		v := JobView{Job: j, State: j.State(now)}
		if r, ok := latest[j.Site()]; ok {
			v.LastRun = &r
		}
		if c, ok := stats[j.Site()]; ok {
			v.Stats = &c
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) jobOptions(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Jobs.Options()) }

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	list, err := s.Jobs.List()
	if err != nil {
		jobError(w, err)
		return
	}
	q := r.URL.Query()
	filtered := list[:0]
	for _, j := range list {
		if sc := q.Get("scope"); sc != "" && j.Scope != sc {
			continue
		}
		if o := q.Get("owner"); o != "" && j.Owner != o {
			continue
		}
		if q.Get("merge") == "pending" && (j.MergeRequest == nil || j.MergeRequest.Status != "pending") {
			continue
		}
		filtered = append(filtered, j)
	}
	writeJSON(w, s.jobViews(filtered))
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.Jobs.Get(r.PathValue("id"))
	if err != nil || j.Deleted {
		jobError(w, jobs.ErrNotFound)
		return
	}
	writeJSON(w, s.jobViews([]jobs.Job{*j})[0])
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var in jobs.Input
	if !s.decodeBody(w, r, &in) {
		return
	}
	j, err := s.Jobs.Create(actorOf(r), in)
	if err != nil {
		jobError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, j)
}

func (s *Server) updateJob(w http.ResponseWriter, r *http.Request) {
	var in jobs.Input
	if !s.decodeBody(w, r, &in) {
		return
	}
	j, err := s.Jobs.Update(actorOf(r), r.PathValue("id"), in)
	if err != nil {
		jobError(w, err)
		return
	}
	writeJSON(w, j)
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	if err := s.Jobs.Delete(actorOf(r), r.PathValue("id")); err != nil {
		jobError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runJob(w http.ResponseWriter, r *http.Request) {
	wait := r.URL.Query().Get("wait") == "true"
	res, err := s.Jobs.RunNow(actorOf(r), r.PathValue("id"), wait)
	if err != nil {
		jobError(w, err)
		return
	}
	if res == nil {
		writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "started"})
		return
	}
	writeJSON(w, res)
}

func (s *Server) jobRuns(w http.ResponseWriter, r *http.Request) {
	j, err := s.Jobs.Get(r.PathValue("id"))
	if err != nil || j.Deleted {
		jobError(w, jobs.ErrNotFound)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 500 {
		n = 50
	}
	writeJSON(w, s.Jobs.Runs(j, n))
}

type reasonBody struct {
	Reason string  `json:"reason"`
	Note   string  `json:"note"`
	RunFor *string `json:"run_for"`
}

func (s *Server) jobAction(w http.ResponseWriter, r *http.Request, fn func(jobs.Actor, string, reasonBody) (*jobs.Job, error)) {
	var b reasonBody
	if !s.decodeBody(w, r, &b) {
		return
	}
	j, err := fn(actorOf(r), r.PathValue("id"), b)
	if err != nil {
		jobError(w, err)
		return
	}
	writeJSON(w, j)
}

func (s *Server) disableJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(a jobs.Actor, id string, b reasonBody) (*jobs.Job, error) {
		return s.Jobs.SetEnabled(a, id, false, b.Reason)
	})
}
func (s *Server) enableJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(a jobs.Actor, id string, b reasonBody) (*jobs.Job, error) {
		return s.Jobs.SetEnabled(a, id, true, "")
	})
}
func (s *Server) requestMerge(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(a jobs.Actor, id string, b reasonBody) (*jobs.Job, error) {
		return s.Jobs.RequestMerge(a, id, b.Note)
	})
}
func (s *Server) approveMerge(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(a jobs.Actor, id string, b reasonBody) (*jobs.Job, error) {
		return s.Jobs.ApproveMerge(a, id, b.RunFor)
	})
}
func (s *Server) rejectMerge(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(a jobs.Actor, id string, b reasonBody) (*jobs.Job, error) {
		return s.Jobs.RejectMerge(a, id, b.Reason)
	})
}
func (s *Server) approveJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(a jobs.Actor, id string, b reasonBody) (*jobs.Job, error) { return s.Jobs.Approve(a, id) })
}
func (s *Server) rejectJob(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(a jobs.Actor, id string, b reasonBody) (*jobs.Job, error) { return s.Jobs.Reject(a, id, b.Reason) })
}

// ---------- binaries ----------

func (s *Server) listBinaries(w http.ResponseWriter, _ *http.Request) {
	list, err := s.Jobs.Binaries()
	if err != nil {
		jobError(w, err)
		return
	}
	writeJSON(w, list)
}

func (s *Server) uploadBinary(w http.ResponseWriter, r *http.Request) {
	max := s.Cfg.Jobs.MaxBinarySizeMB << 20
	b, err := s.Jobs.UploadBinary(actorOf(r), r.PathValue("name"), http.MaxBytesReader(w, r.Body, max+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("binary exceeds jobs.max_binary_size_mb (%d MB)", s.Cfg.Jobs.MaxBinarySizeMB)})
			return
		}
		jobError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, b)
}

func (s *Server) getBinary(w http.ResponseWriter, r *http.Request) {
	b, err := s.Jobs.Binary(r.PathValue("name"))
	if err != nil {
		jobError(w, err)
		return
	}
	writeJSON(w, b)
}

func (s *Server) downloadBinary(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.Jobs.Binary(name); err != nil {
		jobError(w, err)
		return
	}
	data, err := s.Jobs.BinaryContent(name)
	if err != nil {
		jobError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(data)
}

func (s *Server) deleteBinary(w http.ResponseWriter, r *http.Request) {
	if err := s.Jobs.DeleteBinary(actorOf(r), r.PathValue("name")); err != nil {
		jobError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- API tokens ----------

func (s *Server) listTokens(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Store.Tokens(false, false))
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Role string `json:"role"`
		TTL  string `json:"ttl"`
	}
	if !s.decodeBody(w, r, &in) {
		return
	}
	if in.Name == "" || len(in.Name) > 100 || config.RoleRank(in.Role) == 0 {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]string{"error": "name (1-100 chars) and role (viewer|operator|admin) are required"})
		return
	}
	var ttl time.Duration
	if in.TTL != "" {
		d, err := time.ParseDuration(in.TTL)
		if err != nil || d <= 0 {
			writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]string{"error": "ttl must be a duration such as 720h"})
			return
		}
		ttl = d
	}
	a := actorOf(r)
	t, secret, err := s.Store.CreateToken(in.Name, in.Role, a.User, ttl)
	if err != nil {
		jobError(w, err)
		return
	}
	s.syncChanged()
	s.Emit(alert.NewEvent(s.Cfg.Node.ID, "AUTH", "OK", "", "", fmt.Sprintf("API token %q (%s) created by %s", t.Name, t.Role, a.User)))
	t.Hash = ""
	writeJSONStatus(w, http.StatusCreated, map[string]any{"token": t, "secret": secret,
		"usage": "send 'Authorization: Bearer <secret>'; the secret is shown only now"})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.RevokeToken(r.PathValue("id")); err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	s.syncChanged()
	s.Emit(alert.NewEvent(s.Cfg.Node.ID, "AUTH", "OK", "", "", fmt.Sprintf("API token %s revoked by %s", r.PathValue("id"), actorOf(r).User)))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) syncChanged() {
	if s.Jobs != nil {
		s.Jobs.Changed()
	}
}

// ---------- other read endpoints for agents ----------

func (s *Server) summary(w http.ResponseWriter, _ *http.Request) {
	sites := s.Sched.Sites()
	names := make([]string, 0, len(sites.Sites))
	for _, st := range sites.Sites {
		names = append(names, st.Name)
	}
	writeJSON(w, s.Store.Summary(names, sites.Defaults.CertWarnDays, time.Now()))
}

func (s *Server) stats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Store.Stats(time.Now()))
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 500 {
		n = 100
	}
	writeJSON(w, s.Store.History(r.URL.Query().Get("site"), r.URL.Query().Get("check"), n))
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 500 {
		n = 100
	}
	writeJSON(w, s.Store.Events(n))
}

// ---------- peer side of the jobs sync ----------

func (s *Server) peerJobsState(w http.ResponseWriter, _ *http.Request) {
	st, err := s.Jobs.State()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, st)
}

func (s *Server) peerJobsBinary(w http.ResponseWriter, r *http.Request) {
	data, err := s.Jobs.BinaryContent(r.PathValue("name"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

func (s *Server) peerJobsChanged(w http.ResponseWriter, _ *http.Request) {
	go s.PullJobs()
	w.WriteHeader(http.StatusAccepted)
}

// PullJobs fetches the peer's jobs/binaries/tokens and merges them (newest wins).
func (s *Server) PullJobs() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var remote jobs.SyncState
	if err := s.Cluster.PeerJSON(ctx, http.MethodGet, "/peer/jobs/state", 30*time.Second, nil, &remote); err != nil {
		slog.Warn("jobs sync: cannot fetch peer state", "err", err)
		return
	}
	max := s.Cfg.Jobs.MaxBinarySizeMB << 20
	n, err := s.Jobs.Merge(remote, func(name string) ([]byte, error) {
		return s.Cluster.PeerBytes(ctx, "/peer/jobs/binaries/"+name, max)
	})
	if err != nil {
		slog.Warn("jobs sync: merge failed", "err", err)
	}
	if n > 0 {
		slog.Info("jobs sync: merged changes from peer", "records", n)
	}
}

// notifyPeerJobs tells the peer to pull now (it also re-syncs on heartbeat mismatch).
func (s *Server) NotifyPeerJobs() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Cluster.PeerJSON(ctx, http.MethodPost, "/peer/jobs/changed", 10*time.Second, nil, nil)
	}()
}
