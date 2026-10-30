package server

import (
	"net/http"
	"regexp"
	"sort"
	"strings"

	"sitemonitor/internal/config"
)

// route is one API endpoint. The same table registers the handlers and
// generates /api/openapi.json, so the description agents read always matches
// what the server serves.
type route struct {
	Method  string
	Path    string
	Role    string // "" = public; viewer | operator | admin
	Tag     string
	Summary string
	Body    string // request body description ("" = none)
	BodyCT  string // request content type (default application/json)
	Query   []string
	Returns string // response description
	H       http.HandlerFunc
}

func (s *Server) routes() []route {
	v, op, ad := config.RoleViewer, config.RoleOperator, config.RoleAdmin
	rs := []route{
		{"GET", "/healthz", "", "system", "Health check: ok, or 503 if the server is not completing checks", "", "", nil, "text", s.healthz},
		{"GET", "/api/openapi.json", "", "system", "This API description (OpenAPI 3.1)", "", "", nil, "OpenAPI document", s.openapi},
		{"GET", "/api/auth/info", "", "auth", "Login providers and server name", "", "", nil, "{providers, node, node_name}", s.authInfo},
		{"POST", "/api/login", "", "auth", "Log in; sets the session cookie (browsers). Agents should use API tokens instead", `{"username","password"}`, "", nil, "{user, csrf_token}", s.login},
		{"POST", "/api/logout", "", "auth", "End the session", "", "", nil, "204", s.logout},
		{"GET", "/api/me", v, "auth", "The authenticated user or token", "", "", nil, "{user, csrf_token, node, node_name}", s.me},

		{"GET", "/api/status", v, "monitoring", "Everything the dashboard shows: cluster, summary, sites with stored results and stats, events, incidents", "", "", nil, "status object", s.status},
		{"GET", "/api/summary", v, "monitoring", "KPI summary aggregated by the database", "", "", nil, "summary", s.summary},
		{"GET", "/api/charts/activity", v, "monitoring", "Events per day (by severity) and requests / failed requests per day for the last N days, or per hour for one day, in the viewer's time zone", "", "", []string{"days", "day=YYYY-MM-DD", "tz=minutes east of UTC"}, "{unit, buckets}", s.activity},
		{"GET", "/api/stats", v, "monitoring", "Stored counters of every site/check and job (requests, failures, latency, uptime 24h/30d)", "", "", nil, "[]CheckStats", s.stats},
		{"GET", "/api/history", v, "monitoring", "Last raw results of one site/check (for a job: site=job:<name>, check=job)", "", "", []string{"site", "check", "limit"}, "[]Result", s.history},
		{"GET", "/api/incidents", v, "monitoring", "Incidents (one per problem: first seen, clears, re-appearances)", "", "", []string{"status=open|cleared"}, "[]Incident", s.incidents},
		{"GET", "/api/events", v, "monitoring", "Newest events first", "", "", []string{"limit"}, "[]Event", s.events},
		{"GET", "/api/limits", v, "monitoring", "This server's identity, peer, limits and auth settings", "", "", nil, "object", s.limits},
		{"GET", "/metrics", v, "monitoring", "Prometheus metrics (from the database)", "", "", nil, "text/plain", s.metrics},

		{"GET", "/api/tokens", ad, "tokens", "List API tokens (secrets are never returned)", "", "", nil, "[]APIToken", s.listTokens},
		{"POST", "/api/tokens", ad, "tokens", "Create an API token for an agent; the secret is returned once", `{"name","role":"viewer|operator|admin","ttl":"720h" (optional)}`, "", nil, "{token, secret}", s.createToken},
		{"DELETE", "/api/tokens/{id}", ad, "tokens", "Revoke an API token (on both servers)", "", "", nil, "204", s.revokeToken},
	}
	if s.Cfg.ConfigAPI.Enabled {
		rs = append(rs,
			route{"GET", "/api/config/sites", v, "config", "Raw sites.yaml (ETag = sha256)", "", "", nil, "application/yaml", s.getSites},
			route{"GET", "/api/config/sites/{name}", v, "config", "One site as written in sites.yaml", "", "", nil, "application/yaml", s.getSite},
			route{"GET", "/api/config/backups", op, "config", "Saved versions of sites.yaml or node.yaml", "", "", []string{"file=sites|node"}, "{file, backups}", s.backups},
			route{"POST", "/api/config/sites/validate", op, "config", "Validate a sites.yaml without saving", "sites.yaml", "application/yaml", nil, "{valid, version, sites, hash}", s.validateSites},
			route{"PUT", "/api/config/sites", op, "config", "Replace sites.yaml (If-Match: <ETag> recommended); applied and synced to the peer", "sites.yaml", "application/yaml", nil, "{changed, version, diff, peer_synced}", s.putSites},
			route{"PUT", "/api/config/sites/{name}", op, "config", "Add or replace one site (YAML or JSON)", "site", "application/json", nil, "{changed, version, diff, peer_synced}", s.putSite},
			route{"DELETE", "/api/config/sites/{name}", op, "config", "Remove one site", "", "", nil, "{changed, version, diff}", s.deleteSite},
			route{"GET", "/api/config/node", ad, "config", "Raw node.yaml (${VAR} unexpanded)", "", "", nil, "application/yaml", s.getNode},
			route{"POST", "/api/config/node/validate", ad, "config", "Validate a node.yaml without saving", "node.yaml", "application/yaml", nil, "{valid, ...}", s.validateNode},
			route{"PUT", "/api/config/node", ad, "config", "Replace node.yaml; ?restart=true restarts the server", "node.yaml", "application/yaml", []string{"restart=true"}, "{changed, restart_required}", s.putNode},
			route{"POST", "/api/admin/restart", ad, "config", "Graceful restart", "", "", nil, "202", s.restart},
		)
	}
	if s.Jobs != nil {
		rs = append(rs, s.jobRoutes()...)
	}
	return rs
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	if !s.Sched.Healthy() {
		http.Error(w, "not completing checks", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

func (s *Server) authInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"providers": s.Auth.Providers(), "node": s.Cfg.Node.ID, "node_name": s.Cfg.Node.Name})
}

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// openapi renders the route table as an OpenAPI 3.1 document.
func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	paths := map[string]map[string]any{}
	for _, rt := range s.routes() {
		op := map[string]any{
			"summary":     rt.Summary,
			"tags":        []string{rt.Tag},
			"operationId": strings.ToLower(rt.Method) + strings.NewReplacer("/", "_", "{", "", "}", "", ".", "_", "-", "_").Replace(rt.Path),
			"responses": map[string]any{
				"200": map[string]any{"description": rt.Returns},
				"401": map[string]any{"description": "not authenticated"},
				"403": map[string]any{"description": "role too low, or CSRF/cross-origin check failed"},
			},
		}
		if rt.Role == "" {
			op["security"] = []any{}
		} else {
			op["x-required-role"] = rt.Role
		}
		var params []any
		for _, m := range pathParam.FindAllStringSubmatch(rt.Path, -1) {
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
		}
		for _, q := range rt.Query {
			name, desc, _ := strings.Cut(q, "=")
			params = append(params, map[string]any{"name": name, "in": "query", "description": desc, "schema": map[string]string{"type": "string"}})
		}
		if params != nil {
			op["parameters"] = params
		}
		if rt.Body != "" {
			ct := rt.BodyCT
			if ct == "" {
				ct = "application/json"
			}
			op["requestBody"] = map[string]any{"description": rt.Body, "content": map[string]any{ct: map[string]any{}}}
		}
		if paths[rt.Path] == nil {
			paths[rt.Path] = map[string]any{}
		}
		paths[rt.Path][strings.ToLower(rt.Method)] = op
	}
	var tags []string
	seen := map[string]bool{}
	for _, rt := range s.routes() {
		if !seen[rt.Tag] {
			seen[rt.Tag] = true
			tags = append(tags, rt.Tag)
		}
	}
	sort.Strings(tags)
	tagObjs := []any{}
	for _, t := range tags {
		tagObjs = append(tagObjs, map[string]string{"name": t})
	}
	writeJSON(w, map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{"title": "SiteMonitor API", "version": "1.0",
			"description": "Every feature of SiteMonitor. Agents: create a token (POST /api/tokens, admin) and send 'Authorization: Bearer <token>'. " +
				"x-required-role gives the minimum role (viewer < operator < admin). Writes from browsers need the X-CSRF-Token header; bearer and Basic clients do not."},
		"servers": []any{map[string]string{"url": "https://" + r.Host}},
		"tags":    tagObjs,
		"paths":   paths,
		"components": map[string]any{"securitySchemes": map[string]any{
			"bearer":  map[string]string{"type": "http", "scheme": "bearer", "description": "API token from POST /api/tokens"},
			"basic":   map[string]string{"type": "http", "scheme": "basic", "description": "local user or Active Directory account"},
			"session": map[string]string{"type": "apiKey", "in": "cookie", "name": "sm_session"},
		}},
		"security": []any{map[string]any{"bearer": []string{}}, map[string]any{"basic": []string{}}, map[string]any{"session": []string{}}},
	})
}
