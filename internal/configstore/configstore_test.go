package configstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sitemonitor/internal/config"
)

var zero time.Time

const sitesYAML = `version: "1"
# comment that must survive per-site edits
sites:
  # the portal
  - name: portal
    region: IDC
    url: https://portal.example.com
`

func setup(t *testing.T) (*Store, string, *[]*config.Sites) {
	t.Helper()
	dir := t.TempDir()
	sp := filepath.Join(dir, "sites.yaml")
	np := filepath.Join(dir, "node.yaml")
	os.WriteFile(sp, []byte(sitesYAML), 0o640)
	os.WriteFile(np, []byte("node: {id: a}\n"), 0o640)
	node := &config.Node{}
	node.Node.ID, node.Node.Region = "monitor-idc-01", "IDC"
	node.Peer.ID, node.Peer.Region = "monitor-fc-01", "FC"
	node.Monitor.SitesFile = sp
	node.Limits.MaxSites = 3
	node.Limits.MaxConfigSizeKB = 64
	node.ConfigAPI.Backups = 2
	var applied []*config.Sites
	return New(np, node, func(s *config.Sites) { applied = append(applied, s) }), dir, &applied
}

func TestWriteSitesValidationConflictAndBackups(t *testing.T) {
	s, dir, applied := setup(t)
	_, hash, _, _ := s.ReadSites()

	if _, _, _, err := s.WriteSites([]byte("sites: [{name: x, region: XX, url: https://x}]"), "", zero); err == nil {
		t.Fatal("invalid region must be rejected")
	}
	if _, _, _, err := s.WriteSites([]byte(sitesYAML+"    bogus_field: 1\n"), "", zero); err == nil {
		t.Fatal("unknown fields must be rejected")
	}
	if _, _, _, err := s.WriteSites([]byte(sitesYAML), "wrong-hash", zero); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	big := "version: x\nsites:\n" + strings.Repeat("# padding\n", 8000)
	if _, _, _, err := s.WriteSites([]byte(big), "", zero); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}

	next := strings.Replace(sitesYAML, `version: "1"`, `version: "2"`, 1) + "  - {name: shop, region: FC, url: https://shop.example.com}\n"
	_, d, changed, err := s.WriteSites([]byte(next), hash, zero)
	if err != nil || !changed || len(d.Added) != 1 || d.Added[0] != "shop" {
		t.Fatalf("write: %v %v %+v", err, changed, d)
	}
	if len(*applied) != 1 {
		t.Fatal("new config must be applied to the scheduler")
	}
	for i := 0; i < 3; i++ { // more writes than backups kept
		v := strings.Replace(next, `version: "2"`, `version: "v`+string(rune('a'+i))+`"`, 1)
		if _, _, _, err := s.WriteSites([]byte(v), "", zero); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "backups", "sites.yaml.*"))
	if len(files) != 2 {
		t.Fatalf("want 2 backups kept, got %d", len(files))
	}
}

func TestPerSiteEditsKeepComments(t *testing.T) {
	s, _, _ := setup(t)
	st, err := ParseSite([]byte(`{"region":"FC","url":"https://api.example.com","checks":["availability","api"],"timeout":"5s"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, d, err := s.UpsertSite("api", st, ""); err != nil || len(d.Added) != 1 {
		t.Fatalf("upsert: %v %+v", err, d)
	}
	b, _, _, _ := s.ReadSites()
	out := string(b)
	for _, want := range []string{"# comment that must survive", "# the portal", "name: api", "timeout: 5s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	got, _, err := s.GetSite("api")
	if err != nil || !strings.Contains(string(got), "https://api.example.com") {
		t.Fatalf("get: %v %s", err, got)
	}
	// max_sites = 3
	s.UpsertSite("third", config.Site{Region: "IDC", URL: "https://3.example.com"}, "")
	if _, _, err := s.UpsertSite("fourth", config.Site{Region: "IDC", URL: "https://4.example.com"}, ""); err == nil {
		t.Fatal("limits.max_sites must be enforced")
	}
	if _, _, err := s.DeleteSite("missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, d, err := s.DeleteSite("portal", ""); err != nil || len(d.Removed) != 1 {
		t.Fatalf("delete: %v %+v", err, d)
	}
}
