// Package witness is a tiny lease service at a third location. A monitor may
// only take over a region if the witness grants it that region's lease, which
// prevents both servers from acting on the same region during a network split.
package witness

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"sitemonitor/internal/certs"
)

type lease struct {
	Region  string    `json:"region"`
	Holder  string    `json:"holder"`
	Expires time.Time `json:"expires"`
}

type Server struct {
	mu     sync.Mutex
	leases map[string]*lease
}

func New() *Server { return &Server{leases: map[string]*lease{}} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/lease", s.acquire)
	mux.HandleFunc("DELETE /v1/lease", s.release)
	mux.HandleFunc("GET /v1/leases", s.list)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// acquire grants or renews a lease. 200 = granted, 409 = held by someone else.
func (s *Server) acquire(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region     string `json:"region"`
		Node       string `json:"node"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Region == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if cn := certs.ClientCN(r); cn != req.Node {
		http.Error(w, "node does not match client certificate", http.StatusForbidden)
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 || ttl > 5*time.Minute {
		ttl = 20 * time.Second
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[req.Region]
	if l != nil && l.Holder != req.Node && now.Before(l.Expires) {
		writeJSON(w, http.StatusConflict, map[string]any{"granted": false, "holder": l.Holder, "expires": l.Expires})
		return
	}
	if l == nil || l.Holder != req.Node {
		slog.Info("lease granted", "region", req.Region, "holder", req.Node)
	}
	s.leases[req.Region] = &lease{Region: req.Region, Holder: req.Node, Expires: now.Add(ttl)}
	writeJSON(w, http.StatusOK, map[string]any{"granted": true, "holder": req.Node, "expires": now.Add(ttl)})
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	region, node := r.URL.Query().Get("region"), r.URL.Query().Get("node")
	if certs.ClientCN(r) != node {
		http.Error(w, "node does not match client certificate", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.leases[region]; l != nil && l.Holder == node {
		delete(s.leases, region)
		slog.Info("lease released", "region", region, "holder", node)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) list(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []lease{}
	for _, l := range s.leases {
		out = append(out, *l)
	}
	writeJSON(w, http.StatusOK, out)
}
