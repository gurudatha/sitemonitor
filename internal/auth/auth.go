// Package auth authenticates dashboard and API users against pluggable
// providers (local bcrypt users for a POC, Active Directory/LDAP for
// production), issues sessions and enforces role-based access.
//
// Roles: viewer (read-only) < operator (edit sites.yaml) < admin (edit node.yaml, restart).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"sitemonitor/internal/config"
)

var (
	// ErrInvalidCredentials: unknown user or wrong password (never say which).
	ErrInvalidCredentials = errors.New("invalid username or password")
	// ErrNoRole: authenticated, but not a member of any group mapped to a role.
	ErrNoRole = errors.New("user has no sitemonitor role")
	// ErrLocked: too many failed attempts.
	ErrLocked = errors.New("too many failed logins; try again later")
)

// Identity is an authenticated user.
type Identity struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Provider    string `json:"provider"` // local | ldap
}

// Provider checks a username/password pair.
type Provider interface {
	Name() string
	Authenticate(ctx context.Context, username, password string) (*Identity, error)
}

type session struct {
	id      Identity
	csrf    string
	expires time.Time
}

type failure struct {
	count       int
	lockedUntil time.Time
}

type cached struct {
	id      Identity
	expires time.Time
}

// Manager owns providers, sessions, lockouts and the Basic-auth cache.
type Manager struct {
	cfg       config.AuthConfig
	providers []Provider
	onEvent   func(kind, severity, msg string)

	mu       sync.Mutex
	sessions map[string]*session // key: sha256(token)
	failures map[string]*failure // key: username|ip
	basic    map[string]cached   // key: sha256(user\x00password)
}

// New builds the providers listed in auth.providers, in order.
func New(cfg config.AuthConfig, onEvent func(kind, severity, msg string)) (*Manager, error) {
	m := &Manager{cfg: cfg, onEvent: onEvent, sessions: map[string]*session{},
		failures: map[string]*failure{}, basic: map[string]cached{}}
	for _, name := range cfg.Providers {
		switch name {
		case "local":
			m.providers = append(m.providers, NewLocal(cfg.Local))
		case "ldap":
			p, err := NewLDAP(cfg.LDAP)
			if err != nil {
				return nil, err
			}
			m.providers = append(m.providers, p)
		default:
			return nil, fmt.Errorf("unknown auth provider %q", name)
		}
	}
	if len(m.providers) == 0 {
		return nil, errors.New("no auth providers configured")
	}
	return m, nil
}

func (m *Manager) Providers() []string {
	out := make([]string, len(m.providers))
	for i, p := range m.providers {
		out[i] = p.Name()
	}
	return out
}

func hashKey(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return string(h[:])
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Authenticate tries each provider in order, with lockout protection.
func (m *Manager) Authenticate(ctx context.Context, username, password, ip string) (*Identity, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" || len(username) > 256 || len(password) > 1024 {
		return nil, ErrInvalidCredentials // also blocks LDAP "unauthenticated bind" with empty password
	}
	fkey := strings.ToLower(username) + "|" + ip
	m.mu.Lock()
	if f := m.failures[fkey]; f != nil && time.Now().Before(f.lockedUntil) {
		m.mu.Unlock()
		return nil, ErrLocked
	}
	m.mu.Unlock()

	var lastErr error = ErrInvalidCredentials
	for _, p := range m.providers {
		id, err := p.Authenticate(ctx, username, password)
		if err == nil {
			m.mu.Lock()
			delete(m.failures, fkey)
			m.mu.Unlock()
			slog.Info("login ok", "user", id.Username, "role", id.Role, "provider", id.Provider, "ip", ip)
			return id, nil
		}
		if errors.Is(err, ErrNoRole) {
			lastErr = err
			break // authenticated by this directory but not authorized: do not fall through
		}
		if !errors.Is(err, ErrInvalidCredentials) {
			slog.Warn("auth provider error", "provider", p.Name(), "err", err) // e.g. AD unreachable: try next provider
		}
	}

	m.mu.Lock()
	f := m.failures[fkey]
	if f == nil {
		f = &failure{}
		m.failures[fkey] = f
	}
	f.count++
	locked := f.count >= m.cfg.MaxFailedLogins
	if locked {
		f.lockedUntil = time.Now().Add(m.cfg.Lockout)
		f.count = 0
	}
	m.mu.Unlock()
	slog.Warn("login failed", "user", username, "ip", ip, "reason", lastErr)
	if locked && m.onEvent != nil {
		m.onEvent("AUTH", "WARN", fmt.Sprintf("user %q locked out for %s after %d failed logins from %s", username, m.cfg.Lockout, m.cfg.MaxFailedLogins, ip))
	}
	return nil, lastErr
}

// Login authenticates and creates a session. Returns (token, csrf, identity).
func (m *Manager) Login(ctx context.Context, username, password, ip string) (string, string, *Identity, error) {
	id, err := m.Authenticate(ctx, username, password, ip)
	if err != nil {
		return "", "", nil, err
	}
	token, csrf := randomToken(), randomToken()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	if len(m.sessions) >= m.cfg.MaxSessions {
		m.evictOldestLocked()
	}
	m.sessions[hashKey(token)] = &session{id: *id, csrf: csrf, expires: time.Now().Add(m.cfg.SessionTTL)}
	return token, csrf, id, nil
}

func (m *Manager) Logout(token string) {
	m.mu.Lock()
	delete(m.sessions, hashKey(token))
	m.mu.Unlock()
}

func (m *Manager) gcLocked() {
	now := time.Now()
	for k, s := range m.sessions {
		if now.After(s.expires) {
			delete(m.sessions, k)
		}
	}
	for k, c := range m.basic {
		if now.After(c.expires) {
			delete(m.basic, k)
		}
	}
	for k, f := range m.failures {
		if f.count == 0 && now.After(f.lockedUntil) {
			delete(m.failures, k)
		}
	}
}

func (m *Manager) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, s := range m.sessions {
		if oldestKey == "" || s.expires.Before(oldest) {
			oldestKey, oldest = k, s.expires
		}
	}
	delete(m.sessions, oldestKey)
}

// ---------- HTTP integration ----------

const (
	CookieName = "sm_session"
	CSRFHeader = "X-CSRF-Token"
	UIHeader   = "X-Requested-With" // the dashboard sends this; suppresses the browser's Basic-auth popup
)

type ctxKey struct{}

// FromContext returns the identity attached by Middleware.
func FromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(*Identity)
	return id, ok
}

func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isMutation(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// identify resolves the caller from a session cookie or HTTP Basic credentials.
// csrfOK reports whether a mutating request carries proof it is not cross-site.
func (m *Manager) identify(r *http.Request) (id *Identity, csrfOK bool, err error) {
	if c, cerr := r.Cookie(CookieName); cerr == nil && c.Value != "" {
		m.mu.Lock()
		s := m.sessions[hashKey(c.Value)]
		if s != nil && time.Now().After(s.expires) {
			delete(m.sessions, hashKey(c.Value))
			s = nil
		}
		m.mu.Unlock()
		if s != nil {
			ok := subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(s.csrf)) == 1
			idCopy := s.id
			return &idCopy, ok, nil
		}
	}
	if u, p, ok := r.BasicAuth(); ok {
		key := hashKey(u, p)
		m.mu.Lock()
		c, hit := m.basic[key]
		m.mu.Unlock()
		if hit && time.Now().Before(c.expires) {
			idCopy := c.id
			return &idCopy, true, nil
		}
		ident, err := m.Authenticate(r.Context(), u, p, ClientIP(r))
		if err != nil {
			return nil, false, err
		}
		m.mu.Lock()
		if len(m.basic) < 10000 {
			m.basic[key] = cached{id: *ident, expires: time.Now().Add(m.cfg.BasicCacheTTL)}
		}
		m.mu.Unlock()
		// Basic auth is not sent automatically cross-site for non-simple
		// requests; the Origin check in Middleware covers the rest.
		return ident, true, nil
	}
	return nil, false, ErrInvalidCredentials
}

// sameOrigin rejects browser requests coming from another site.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client
	}
	return origin == "https://"+r.Host || origin == "http://"+r.Host
}

func unauthorized(w http.ResponseWriter, r *http.Request, msg string) {
	if r.Header.Get(UIHeader) == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="sitemonitor", charset="UTF-8"`)
	}
	http.Error(w, msg, http.StatusUnauthorized)
}

// Require wraps a handler so it needs at least the given role.
func (m *Manager) Require(minRole string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, csrfOK, err := m.identify(r)
		if err != nil {
			if errors.Is(err, ErrLocked) {
				http.Error(w, err.Error(), http.StatusTooManyRequests)
				return
			}
			if errors.Is(err, ErrNoRole) {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			unauthorized(w, r, "authentication required")
			return
		}
		if isMutation(r.Method) && (!csrfOK || !sameOrigin(r)) {
			http.Error(w, "missing or invalid CSRF token / cross-origin request", http.StatusForbidden)
			return
		}
		if config.RoleRank(id.Role) < config.RoleRank(minRole) {
			http.Error(w, fmt.Sprintf("forbidden: role %s required (you are %s)", minRole, id.Role), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// RequireFunc is Require for a HandlerFunc.
func (m *Manager) RequireFunc(minRole string, f http.HandlerFunc) http.Handler {
	return m.Require(minRole, f)
}

// CSRFFor returns the CSRF token of the session in the request (for /api/me).
func (m *Manager) CSRFFor(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[hashKey(c.Value)]; s != nil {
		return s.csrf
	}
	return ""
}

// Run periodically removes expired sessions and cache entries.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.mu.Lock()
			m.gcLocked()
			m.mu.Unlock()
		}
	}
}
