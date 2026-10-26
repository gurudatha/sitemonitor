package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jimlambrt/gldap"
	"github.com/jimlambrt/gldap/testdirectory"

	"sitemonitor/internal/config"
)

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func baseCfg() config.AuthConfig {
	return config.AuthConfig{SessionTTL: time.Hour, MaxSessions: 10, BasicCacheTTL: time.Minute, MaxFailedLogins: 3, Lockout: time.Minute}
}

func TestLocalLoginRolesAndLockout(t *testing.T) {
	cfg := baseCfg()
	cfg.Providers = []string{"local"}
	cfg.Local.Users = []config.LocalUser{
		{Username: "admin", PasswordHash: mustHash(t, "admin-password-123"), Role: "admin"},
		{Username: "viewer", PasswordHash: mustHash(t, "viewer-password-123"), Role: "viewer"},
	}
	var events []string
	m, err := New(cfg, func(_, _, msg string) { events = append(events, msg) })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	id, err := m.Authenticate(ctx, "admin", "admin-password-123", "1.2.3.4")
	if err != nil || id.Role != "admin" || id.Provider != "local" {
		t.Fatalf("admin login: %v %+v", err, id)
	}
	if _, err := m.Authenticate(ctx, "nobody", "whatever-password", "1.2.3.4"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: want invalid credentials, got %v", err)
	}
	if _, err := m.Authenticate(ctx, "viewer", "", "1.2.3.4"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("empty password must be rejected, got %v", err)
	}

	for i := 0; i < 3; i++ {
		_, _ = m.Authenticate(ctx, "viewer", "wrong", "5.6.7.8")
	}
	if _, err := m.Authenticate(ctx, "viewer", "viewer-password-123", "5.6.7.8"); !errors.Is(err, ErrLocked) {
		t.Fatalf("after 3 failures the correct password must still be locked out, got %v", err)
	}
	if _, err := m.Authenticate(ctx, "viewer", "viewer-password-123", "9.9.9.9"); err != nil {
		t.Fatalf("lockout is per user+IP; other IP should work: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 lockout event, got %v", events)
	}

	tok, csrf, _, err := m.Login(ctx, "admin", "admin-password-123", "1.2.3.4")
	if err != nil || tok == "" || csrf == "" {
		t.Fatalf("login: %v", err)
	}
	m.Logout(tok)
	if len(m.sessions) != 0 {
		t.Fatal("logout must remove the session")
	}
}

// startDirectory runs an in-process LDAP server (TLS) that behaves like a small AD.
func startDirectory(t *testing.T) (*testdirectory.Directory, string) {
	t.Helper()
	td := testdirectory.Start(t,
		testdirectory.WithDefaults(t, &testdirectory.Defaults{AllowAnonymousBind: false}),
	)
	// memberOf holds full group DNs, as in Active Directory.
	users := testdirectory.NewUsers(t, []string{"alice"}, testdirectory.WithMembersOf(t, "CN=Admins,OU=Groups,DC=example,DC=org"))
	users = append(users, testdirectory.NewUsers(t, []string{"bob"}, testdirectory.WithMembersOf(t, "cn=viewers,ou=groups,dc=example,dc=org"))...)
	users = append(users, testdirectory.NewUsers(t, []string{"carol"}, testdirectory.WithMembersOf(t, "cn=others,ou=groups,dc=example,dc=org"))...)
	// service account used for searches
	svc := gldap.NewEntry("cn=svc-monitor,ou=people,dc=example,dc=org", map[string][]string{
		"cn": {"svc-monitor"}, "password": {"svc-password"},
	})
	td.SetUsers(append(users, svc)...)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, []byte(td.Cert()), 0o600); err != nil {
		t.Fatal(err)
	}
	return td, caFile
}

func TestLDAPLoginAndGroupMapping(t *testing.T) {
	td, caFile := startDirectory(t)
	cfg := baseCfg()
	cfg.Providers = []string{"ldap"}
	cfg.LDAP = config.LDAPAuth{
		URL:          fmt.Sprintf("ldaps://127.0.0.1:%d", td.Port()),
		CAFile:       caFile,
		BindDN:       "cn=svc-monitor,ou=people,dc=example,dc=org",
		BindPassword: "svc-password",
		BaseDN:       "ou=people,dc=example,dc=org",
		UserFilter:   "(cn={username})",
		DisplayAttr:  "cn",
		Timeout:      5 * time.Second,
		RoleGroups: map[string][]string{
			"admin":  {"cn=admins,ou=groups,dc=example,dc=org"}, // DN compare is case-insensitive
			"viewer": {"cn=viewers,ou=groups,dc=example,dc=org"},
		},
	}
	m, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	id, err := m.Authenticate(ctx, "alice", "password", "1.1.1.1")
	if err != nil || id.Role != "admin" || id.Provider != "ldap" {
		t.Fatalf("alice (admins group) should be admin: %v %+v", err, id)
	}
	id, err = m.Authenticate(ctx, "bob", "password", "1.1.1.1")
	if err != nil || id.Role != "viewer" {
		t.Fatalf("bob (viewers group) should be viewer: %v %+v", err, id)
	}
	if _, err := m.Authenticate(ctx, "alice", "wrong-password", "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: want invalid credentials, got %v", err)
	}
	if _, err := m.Authenticate(ctx, "carol", "password", "1.1.1.1"); !errors.Is(err, ErrNoRole) {
		t.Fatalf("carol is in no mapped group: want ErrNoRole, got %v", err)
	}
	if _, err := m.Authenticate(ctx, "nobody", "password", "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: want invalid credentials, got %v", err)
	}
	if _, err := m.Authenticate(ctx, "alice)(cn=*", "password", "1.1.1.1"); err == nil {
		t.Fatal("LDAP filter injection must not authenticate")
	}
}

func TestLDAPFallsBackToLocalWhenDirectoryDown(t *testing.T) {
	cfg := baseCfg()
	cfg.Providers = []string{"ldap", "local"} // AD first, local break-glass second
	cfg.LDAP = config.LDAPAuth{URL: "ldaps://127.0.0.1:1", BaseDN: "dc=x", UserFilter: "(cn={username})",
		Timeout: time.Second, DefaultRole: "viewer"}
	cfg.Local.Users = []config.LocalUser{{Username: "breakglass", PasswordHash: mustHash(t, "break-glass-pass-1"), Role: "admin"}}
	m, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.Authenticate(context.Background(), "breakglass", "break-glass-pass-1", "1.1.1.1")
	if err != nil || id.Provider != "local" {
		t.Fatalf("break-glass login should work while AD is down: %v %+v", err, id)
	}
}
