// Package server exposes two listeners:
//   - peer port (mTLS, only the configured peer's certificate is accepted)
//   - dashboard port (HTTPS): UI, login, JSON + config APIs and /metrics,
//     protected by the auth module (local users or Active Directory)
package server

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"sitemonitor/internal/auth"
	"sitemonitor/internal/certs"
	"sitemonitor/internal/checks"
	"sitemonitor/internal/cluster"
	"sitemonitor/internal/config"
	"sitemonitor/internal/configstore"
	"sitemonitor/internal/scheduler"
	"sitemonitor/internal/store"
)

//go:embed web
var webFS embed.FS

type Server struct {
	Cfg     *config.Node
	Store   *store.Store
	Sched   *scheduler.Scheduler
	Cluster *cluster.Manager
	Auth    *auth.Manager
	Config  *configstore.Store
	Emit    func(store.Event)
	Restart func(actor string) // graceful exit; the supervisor (docker/systemd) starts it again

	verifySem chan struct{}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

// ---------- peer listener ----------

func (s *Server) PeerHandler() http.Handler {
	s.verifySem = make(chan struct{}, s.Cfg.Limits.VerifyConcurrency)
	lim := s.Cfg.Limits
	mux := http.NewServeMux()
	mux.HandleFunc("POST /peer/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var hb cluster.Heartbeat
		if !decode(w, r, 64<<10, &hb) {
			return
		}
		s.Cluster.ObserveInbound(hb)
		writeJSON(w, s.Cluster.LocalHeartbeat())
	})
	mux.HandleFunc("POST /peer/replicate", func(w http.ResponseWriter, r *http.Request) {
		var b cluster.ReplicationBatch
		if !decode(w, r, lim.MaxReplicationBodyMB<<20, &b) {
			return
		}
		for _, res := range b.Results {
			s.Store.AddResult(res)
		}
		for _, e := range b.Events {
			s.Store.AddEvent(e)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /peer/verify", func(w http.ResponseWriter, r *http.Request) {
		select { // bounded: the peer cannot make us open unlimited connections
		case s.verifySem <- struct{}{}:
			defer func() { <-s.verifySem }()
		default:
			http.Error(w, "verify capacity exhausted", http.StatusServiceUnavailable)
			return
		}
		var req struct{ Site, Check string }
		if !decode(w, r, 4096, &req) {
			return
		}
		res, err := s.Sched.RunNow(req.Site, req.Check)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		// a verify check is a real request: count it here and on the peer
		if s.Store.AddResult(*res) {
			s.Cluster.EnqueueResult(*res)
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("GET /peer/config/sites", s.peerGetSites)
	mux.HandleFunc("PUT /peer/config/sites", s.peerPutSites)

	// Authorization: the TLS layer already verified the cert chain; additionally
	// require it to be *our* peer, not just any cert from the CA (e.g. the witness).
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if certs.ClientCN(r) != s.Cfg.Peer.ID {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// ---------- dashboard listener ----------

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) DashboardHandler() http.Handler {
	static, _ := fs.Sub(webFS, "web")
	a := s.Auth
	mux := http.NewServeMux()

	// public
	mux.Handle("GET /", http.FileServerFS(static)) // static UI only; all data comes from authenticated APIs
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.Sched.Healthy() {
			http.Error(w, "not completing checks", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/auth/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"providers": a.Providers(), "node": s.Cfg.Node.ID, "node_name": s.Cfg.Node.Name})
	})

	// viewer
	mux.Handle("GET /api/me", a.RequireFunc(config.RoleViewer, s.me))
	mux.Handle("GET /api/status", a.RequireFunc(config.RoleViewer, s.status))
	mux.Handle("GET /api/history", a.RequireFunc(config.RoleViewer, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.Store.History(r.URL.Query().Get("site"), r.URL.Query().Get("check"), 100))
	}))
	mux.Handle("GET /api/limits", a.RequireFunc(config.RoleViewer, s.limits))
	mux.Handle("GET /api/incidents", a.RequireFunc(config.RoleViewer, s.incidents))
	mux.Handle("GET /metrics", a.RequireFunc(config.RoleViewer, s.metrics))

	// configuration API (see configapi.go)
	if s.Cfg.ConfigAPI.Enabled {
		mux.Handle("GET /api/config/sites", a.RequireFunc(config.RoleViewer, s.getSites))
		mux.Handle("GET /api/config/sites/{name}", a.RequireFunc(config.RoleViewer, s.getSite))
		mux.Handle("GET /api/config/backups", a.RequireFunc(config.RoleOperator, s.backups))
		mux.Handle("POST /api/config/sites/validate", a.RequireFunc(config.RoleOperator, s.validateSites))
		mux.Handle("PUT /api/config/sites", a.RequireFunc(config.RoleOperator, s.putSites))
		mux.Handle("PUT /api/config/sites/{name}", a.RequireFunc(config.RoleOperator, s.putSite))
		mux.Handle("DELETE /api/config/sites/{name}", a.RequireFunc(config.RoleOperator, s.deleteSite))
		mux.Handle("GET /api/config/node", a.RequireFunc(config.RoleAdmin, s.getNode))
		mux.Handle("POST /api/config/node/validate", a.RequireFunc(config.RoleAdmin, s.validateNode))
		mux.Handle("PUT /api/config/node", a.RequireFunc(config.RoleAdmin, s.putNode))
		mux.Handle("POST /api/admin/restart", a.RequireFunc(config.RoleAdmin, s.restart))
	}
	return securityHeaders(mux)
}

// ---------- login / session ----------

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if o := r.Header.Get("Origin"); o != "" && o != "https://"+r.Host {
		http.Error(w, "cross-origin login rejected", http.StatusForbidden)
		return
	}
	var req struct{ Username, Password string }
	if !decode(w, r, 4096, &req) {
		return
	}
	token, csrf, id, err := s.Auth.Login(r.Context(), req.Username, req.Password, auth.ClientIP(r))
	switch {
	case err == auth.ErrLocked:
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	case err == auth.ErrNoRole:
		http.Error(w, "you are not a member of any group that grants access to this application", http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, auth.ErrInvalidCredentials.Error(), http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: token, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(s.Cfg.Auth.SessionTTL.Seconds())})
	writeJSON(w, map[string]any{"user": id, "csrf_token": csrf})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		s.Auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	writeJSON(w, map[string]any{"user": id, "csrf_token": s.Auth.CSRFFor(r),
		"node": s.Cfg.Node.ID, "node_name": s.Cfg.Node.Name, "config_api": s.Cfg.ConfigAPI.Enabled})
}

// ---------- read APIs ----------

type siteView struct {
	Name        string                      `json:"name"`
	Region      string                      `json:"region"`
	URL         string                      `json:"url"`
	Owner       string                      `json:"owner"`
	Criticality string                      `json:"criticality"`
	Checks      []string                    `json:"checks"`
	Status      string                      `json:"status"`  // worst stored check status; "" = pending
	Results     map[string]checks.Result    `json:"results"` // stored current result per check
	Stats       map[string]store.CheckStats `json:"stats"`   // stored counters per check
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	sites := s.Sched.Sites()
	latest := map[string]map[string]checks.Result{}
	for _, r := range s.Store.Latest() {
		if latest[r.Site] == nil {
			latest[r.Site] = map[string]checks.Result{}
		}
		latest[r.Site][r.Check] = r
	}
	now := time.Now()
	stats := map[string]map[string]store.CheckStats{}
	for _, c := range s.Store.Stats(now) {
		if stats[c.Site] == nil {
			stats[c.Site] = map[string]store.CheckStats{}
		}
		stats[c.Site][c.Check] = c
	}
	worst := s.Store.WorstBySite()
	names := make([]string, 0, len(sites.Sites))
	views := make([]siteView, 0, len(sites.Sites))
	for _, st := range sites.Sites {
		names = append(names, st.Name)
		res, sts := latest[st.Name], stats[st.Name]
		if res == nil {
			res = map[string]checks.Result{}
		}
		if sts == nil {
			sts = map[string]store.CheckStats{}
		}
		views = append(views, siteView{Name: st.Name, Region: st.Region, URL: st.URL, Owner: st.Owner,
			Criticality: st.Criticality, Checks: st.Checks, Status: worst[st.Name], Results: res, Stats: sts})
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Region != views[j].Region {
			return views[i].Region < views[j].Region
		}
		return views[i].Name < views[j].Name
	})
	writeJSON(w, map[string]any{
		"time":      time.Now().UTC(),
		"cluster":   s.Cluster.Snapshot(),
		"scheduler": s.Sched.Stats(),
		"config":    map[string]string{"version": sites.Version, "hash": sites.Hash},
		"summary":   s.Store.Summary(names, sites.Defaults.CertWarnDays, now),
		"sites":     views,
		"events":    s.Store.Events(100),
		"incidents": s.Store.Incidents(100),
	})
}

// asYAMLMap renders a config struct with its yaml field names and "2s"-style durations.
func asYAMLMap(v any) map[string]any {
	b, _ := yaml.Marshal(v)
	out := map[string]any{}
	_ = yaml.Unmarshal(b, &out)
	return out
}

// incidents lists grouped events: ?status=open|cleared filters, default all (max 500).
func (s *Server) incidents(w http.ResponseWriter, r *http.Request) {
	want := strings.ToUpper(r.URL.Query().Get("status"))
	out := []store.Incident{}
	for _, inc := range s.Store.Incidents(500) {
		if want == "" || inc.Status == want {
			out = append(out, inc)
		}
	}
	writeJSON(w, out)
}

// limits shows the effective (defaulted) limits and identities of this server.
func (s *Server) limits(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"node":   map[string]string{"id": s.Cfg.Node.ID, "name": s.Cfg.Node.Name, "region": s.Cfg.Node.Region},
		"peer":   map[string]string{"id": s.Cfg.Peer.ID, "name": s.Cfg.Peer.Name, "region": s.Cfg.Peer.Region, "url": s.Cfg.Peer.URL},
		"limits": asYAMLMap(s.Cfg.Limits),
		"auth":   map[string]any{"providers": s.Cfg.Auth.Providers, "session_ttl": s.Cfg.Auth.SessionTTL.String(), "max_failed_logins": s.Cfg.Auth.MaxFailedLogins, "lockout": s.Cfg.Auth.Lockout.String()},
	})
}
