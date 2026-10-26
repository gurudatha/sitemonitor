package config

import (
	"os"
	"testing"
)

// The example configs shipped in config/ must always be valid.
func TestShippedConfigs(t *testing.T) {
	for _, dir := range []string{"idc", "fc"} {
		n, err := LoadNode("../../config/" + dir + "/node.yaml")
		if err != nil {
			t.Fatalf("%s/node.yaml: %v", dir, err)
		}
		if n.Node.Name == "" || n.Peer.Name == "" || n.Limits.Workers != 20 {
			t.Fatalf("%s: names/limits not loaded: %+v", dir, n.Node)
		}
		if n.Auth.Local.Users[0].PasswordHash[:4] != "$2a$" {
			t.Fatalf("%s: bcrypt hash was mangled by env expansion", dir)
		}
		s, err := LoadSites("../../config/" + dir + "/sites.yaml")
		if err != nil {
			t.Fatalf("%s/sites.yaml: %v", dir, err)
		}
		if err := s.CheckLimits(n.Limits); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnvExpansionOnlyBraces(t *testing.T) {
	os.Setenv("SM_TEST_SECRET", "s3cret")
	if got := ExpandEnv("a ${SM_TEST_SECRET} $2a$12$x $HOME"); got != "a s3cret $2a$12$x $HOME" {
		t.Fatalf("got %q", got)
	}
}

func TestLDAPRequiresTLS(t *testing.T) {
	n := Node{Auth: AuthConfig{Providers: []string{"ldap"}, LDAP: LDAPAuth{URL: "ldap://dc:389", BaseDN: "dc=x",
		UserFilter: "(cn={username})", DefaultRole: "viewer"}}}
	if err := n.validateAuth(); err == nil {
		t.Fatal("ldap:// without start_tls must be rejected")
	}
}
