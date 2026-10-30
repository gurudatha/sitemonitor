package jobs

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"sitemonitor/internal/checks"
	"sitemonitor/internal/config"
)

// specialArgs are the examples documented in the dashboard's Info tab. Each
// must reach the program byte-for-byte, locally and over SSH.
var specialArgs = []string{
	"hello world",                      // spaces
	`say "hi"`,                         // double quotes
	"it's",                             // single quote
	"$HOME",                            // shell variable syntax (must stay literal)
	"${PATH}",                          //
	"`id`",                             // backticks
	"$(whoami)",                        // command substitution
	`C:\Program Files\App`,             // Windows path with backslashes
	`\\server\share`,                   // UNC path
	"*.log",                            // glob
	"a?b[c]",                           //
	"one;two",                          // command separator
	"a&b|c",                            // & and |
	"<in >out",                         // redirections
	"100%",                             // percent
	"!important",                       // history expansion char
	"#not-a-comment",                   // hash
	"--name=value",                     // option style
	"-v",                               // leading dash
	`{"k": "v", "n": 1}`,               // JSON
	`^\d+(\.\d+)?$`,                    // regex
	"https://x.example/a?b=1&c=2#frag", // URL
	"ünïcödé ✓ 日本",                     // non-ASCII
	"  padded  ",                       // leading/trailing spaces are kept
	"tab\there",                        // tab
	"",                                 // empty argument (API only)
}

const echoPy = "import sys\nfor a in sys.argv[1:]:\n    sys.stdout.write('[' + a + ']\\n')\n"
const echoPl = "binmode(STDOUT, ':raw');\nprint \"[$_]\\n\" for @ARGV;\n"
const echoSh = "for a in \"$@\"; do printf '[%s]\\n' \"$a\"; done\n"

func expectArgs(t *testing.T, label string, r *checks.Result) {
	t.Helper()
	var want strings.Builder
	for _, a := range specialArgs {
		want.WriteString("[" + a + "]\n")
	}
	if r.Status != checks.StatusOK || r.Details["stdout"] != want.String() {
		t.Errorf("%s: arguments changed on the way\nstatus %s %s\n got:\n%s\nwant:\n%s", label, r.Status, r.Message, r.Details["stdout"], want.String())
	}
}

func TestSpecialCharacterArgumentsLocal(t *testing.T) {
	h := newHarness(t)
	for _, p := range []struct{ name, code string }{{"echo.py", echoPy}, {"echo.pl", echoPl}, {"echo.sh", echoSh}} {
		h.upload(t, opA, p.name, p.code)
		j, err := h.svc.Create(opA, interval("args-"+strings.ReplaceAll(p.name, ".", "-"), p.name, specialArgs...))
		if err != nil {
			t.Fatal(err)
		}
		r, _ := h.svc.RunNow(opA, j.ID, true)
		expectArgs(t, "local "+p.name, r)
	}
}

func TestSpecialCharacterArgumentsRemote(t *testing.T) {
	addr, hostKey, _ := sshServer(t, "pw")
	host, portS, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscan(portS, &port)
	var kh string
	h := newHarness(t, func(c *config.JobsConf) {
		kh = filepath.Join(c.WorkDir, "..", "known_hosts")
		c.Remote.KnownHosts = kh
		c.Remote.RemoteDir = t.TempDir()
		c.Remote.Credentials = map[string]config.Credential{"svc": {User: "svc", Password: "pw"}}
	})
	os.WriteFile(kh, []byte(fmt.Sprintf("[%s]:%d %s", host, port, ssh.MarshalAuthorizedKey(hostKey))), 0o600)
	for _, p := range []struct{ name, code string }{{"recho.py", echoPy}, {"recho.pl", echoPl}, {"recho.sh", echoSh}} {
		h.upload(t, opA, p.name, p.code)
		in := interval("rargs-"+strings.ReplaceAll(p.name, ".", "-"), p.name, specialArgs...)
		in.Target = &Target{Host: host, Port: port, OS: OSLinux, Credential: "svc"}
		j, err := h.svc.Create(opA, in)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := h.svc.RunNow(opA, j.ID, true)
		expectArgs(t, "remote "+p.name, r)
	}
}
