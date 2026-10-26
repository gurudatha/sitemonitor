package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"sitemonitor/internal/alert"
	"sitemonitor/internal/auth"
	"sitemonitor/internal/cluster"
	"sitemonitor/internal/configstore"
)

// Configuration API. Bodies are raw YAML (JSON is accepted where a single
// site is sent). Every write is validated, size-limited, backed up, applied
// immediately (sites.yaml) and recorded as a CONFIG event with the user name.
// Send "If-Match: <hash>" (from the ETag of a GET) to avoid overwriting
// someone else's change.

func actor(r *http.Request) string {
	if id, ok := auth.FromContext(r.Context()); ok {
		return fmt.Sprintf("%s (%s/%s)", id.Username, id.Provider, id.Role)
	}
	return "unknown"
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	max := s.Cfg.Limits.MaxConfigSizeKB << 10
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		http.Error(w, fmt.Sprintf("body exceeds limits.max_config_size_kb (%d KB)", s.Cfg.Limits.MaxConfigSizeKB), http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return b, true
}

func configError(w http.ResponseWriter, err error) {
	code := http.StatusUnprocessableEntity // validation errors
	switch {
	case errors.Is(err, configstore.ErrConflict):
		code = http.StatusPreconditionFailed
	case errors.Is(err, configstore.ErrTooLarge):
		code = http.StatusRequestEntityTooLarge
	case errors.Is(err, configstore.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, configstore.ErrImmutable):
		code = http.StatusConflict
	}
	writeJSONStatus(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) event(severity, msg string) {
	s.Emit(alert.NewEvent(s.Cfg.Node.ID, "CONFIG", severity, "", "", msg))
}

// ---------- sites.yaml ----------

func (s *Server) getSites(w http.ResponseWriter, _ *http.Request) {
	b, hash, mod, err := s.Config.ReadSites()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", `"`+hash+`"`)
	w.Header().Set("Last-Modified", mod.Format(http.TimeFormat))
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Write(b)
}

func (s *Server) getSite(w http.ResponseWriter, r *http.Request) {
	b, hash, err := s.Config.GetSite(r.PathValue("name"))
	if err != nil {
		configError(w, err)
		return
	}
	w.Header().Set("ETag", `"`+hash+`"`)
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Write(b)
}

func (s *Server) validateSites(w http.ResponseWriter, r *http.Request) {
	b, ok := s.readBody(w, r)
	if !ok {
		return
	}
	sites, err := s.Config.ValidateSites(b)
	if err != nil {
		configError(w, err)
		return
	}
	writeJSON(w, map[string]any{"valid": true, "version": sites.Version, "sites": len(sites.Sites), "hash": configstore.Hash(b)})
}

func (s *Server) putSites(w http.ResponseWriter, r *http.Request) {
	b, ok := s.readBody(w, r)
	if !ok {
		return
	}
	sites, diff, changed, err := s.Config.WriteSites(b, r.Header.Get("If-Match"), time.Time{})
	if err != nil {
		configError(w, err)
		return
	}
	s.afterSitesChange(w, r, sites.Version, len(sites.Sites), diff, changed)
}

func (s *Server) putSite(w http.ResponseWriter, r *http.Request) {
	b, ok := s.readBody(w, r)
	if !ok {
		return
	}
	st, err := configstore.ParseSite(b)
	if err != nil {
		configError(w, err)
		return
	}
	sites, diff, err := s.Config.UpsertSite(r.PathValue("name"), st, r.Header.Get("If-Match"))
	if err != nil {
		configError(w, err)
		return
	}
	s.afterSitesChange(w, r, sites.Version, len(sites.Sites), diff, !diff.Empty())
}

func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request) {
	sites, diff, err := s.Config.DeleteSite(r.PathValue("name"), r.Header.Get("If-Match"))
	if err != nil {
		configError(w, err)
		return
	}
	s.afterSitesChange(w, r, sites.Version, len(sites.Sites), diff, true)
}

// afterSitesChange records the change and pushes the new file to the peer.
func (s *Server) afterSitesChange(w http.ResponseWriter, r *http.Request, version string, n int, diff configstore.Diff, changed bool) {
	content, hash, mod, err := s.Config.ReadSites()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]any{"changed": changed, "version": version, "sites": n, "hash": hash, "diff": diff}
	if changed {
		who := actor(r)
		s.event("OK", fmt.Sprintf("sites.yaml updated by %s via API: %s (version %s)", who, diff, version))
		if s.Cfg.ConfigAPI.SyncToPeer {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			err := s.Cluster.PushSites(ctx, cluster.SitesPayload{Content: string(content), Hash: hash, ModTime: mod, Actor: who})
			cancel()
			resp["peer_synced"] = err == nil
			if err != nil {
				resp["peer_error"] = err.Error()
				s.event("WARN", fmt.Sprintf("sites.yaml change not yet on %s (%v); it will auto-sync when the peer is reachable", s.Cfg.Peer.Name, err))
			}
		}
	}
	w.Header().Set("ETag", `"`+hash+`"`)
	writeJSON(w, resp)
}

func (s *Server) backups(w http.ResponseWriter, r *http.Request) {
	which := r.URL.Query().Get("file")
	if which != "node" {
		which = "sites"
	}
	writeJSON(w, map[string]any{"file": which, "backups": s.Config.Backups(which)})
}

// ---------- peer side of the sites.yaml sync ----------

func (s *Server) peerGetSites(w http.ResponseWriter, _ *http.Request) {
	b, hash, mod, err := s.Config.ReadSites()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, cluster.SitesPayload{Content: string(b), Hash: hash, ModTime: mod})
}

func (s *Server) peerPutSites(w http.ResponseWriter, r *http.Request) {
	var p cluster.SitesPayload
	if !decode(w, r, s.Cfg.Limits.MaxConfigSizeKB<<10+(64<<10), &p) {
		return
	}
	_, diff, changed, err := s.Config.WriteSites([]byte(p.Content), "", p.ModTime)
	if err != nil {
		configError(w, err)
		return
	}
	if changed {
		s.event("OK", fmt.Sprintf("sites.yaml synced from %s (changed by %s): %s", s.Cfg.Peer.Name, p.Actor, diff))
	}
	w.WriteHeader(http.StatusOK)
}

// PullFromPeer fetches a newer sites.yaml from the peer (auto-sync after an outage).
func (s *Server) PullFromPeer() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := s.Cluster.FetchPeerSites(ctx)
	if err != nil {
		slog.Warn("auto-sync: cannot fetch sites.yaml from peer", "err", err)
		return
	}
	_, diff, changed, err := s.Config.WriteSites([]byte(p.Content), "", p.ModTime)
	if err != nil {
		s.event("WARN", fmt.Sprintf("auto-sync: newer sites.yaml from %s rejected: %v", s.Cfg.Peer.Name, err))
		return
	}
	if changed {
		s.event("OK", fmt.Sprintf("auto-sync: pulled newer sites.yaml from %s: %s", s.Cfg.Peer.Name, diff))
	}
}

// ---------- node.yaml (admin) ----------

func (s *Server) getNode(w http.ResponseWriter, _ *http.Request) {
	b, hash, err := s.Config.ReadNode()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", `"`+hash+`"`)
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Write(b) // ${VAR} references are returned unexpanded: secrets are never exposed
}

func (s *Server) validateNode(w http.ResponseWriter, r *http.Request) {
	b, ok := s.readBody(w, r)
	if !ok {
		return
	}
	n, err := s.Config.ValidateNode(b)
	if err != nil {
		configError(w, err)
		return
	}
	writeJSON(w, map[string]any{"valid": true, "node": n.Node.ID, "name": n.Node.Name, "limits": n.Limits, "auth_providers": n.Auth.Providers})
}

func (s *Server) putNode(w http.ResponseWriter, r *http.Request) {
	b, ok := s.readBody(w, r)
	if !ok {
		return
	}
	changed, err := s.Config.WriteNode(b, r.Header.Get("If-Match"))
	if err != nil {
		configError(w, err)
		return
	}
	resp := map[string]any{"changed": changed, "restart_required": changed}
	if changed {
		who := actor(r)
		s.event("WARN", fmt.Sprintf("node.yaml updated by %s via API; takes effect after restart", who))
		if r.URL.Query().Get("restart") == "true" {
			resp["restarting"] = true
			go s.Restart(who)
		}
	}
	_, hash, _ := s.Config.ReadNode()
	w.Header().Set("ETag", `"`+hash+`"`)
	writeJSON(w, resp)
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	who := actor(r)
	s.event("WARN", fmt.Sprintf("restart of %s requested by %s", s.Cfg.Node.Name, who))
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"restarting": true})
	go s.Restart(who)
}
