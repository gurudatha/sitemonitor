package jobs

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"sitemonitor/internal/checks"
	"sitemonitor/internal/config"
)

// sshServer is a tiny SSH server that runs "exec" requests with sh -c,
// standing in for a Linux (or Cygwin) host.
func sshServer(t *testing.T, password string) (addr string, hostKey ssh.PublicKey, commands *[]string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if c.User() == "svc" && string(p) == password {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var cmds []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, creqs, _ := nc.Accept()
					go func() {
						defer ch.Close()
						for req := range creqs {
							if req.Type != "exec" {
								req.Reply(false, nil)
								continue
							}
							n := binary.BigEndian.Uint32(req.Payload)
							command := string(req.Payload[4 : 4+n])
							cmds = append(cmds, command)
							req.Reply(true, nil)
							c := exec.Command("sh", "-c", command)
							c.Stdin, c.Stdout, c.Stderr = ch, ch, ch.Stderr()
							code := 0
							if err := c.Run(); err != nil {
								if ee, ok := err.(*exec.ExitError); ok {
									code = ee.ExitCode()
								} else {
									code = 255
								}
							}
							status := make([]byte, 4)
							binary.BigEndian.PutUint32(status, uint32(code))
							ch.SendRequest("exit-status", false, status)
							return
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String(), signer.PublicKey(), &cmds
}

func TestRemoteRunOverSSH(t *testing.T) {
	addr, hostKey, cmds := sshServer(t, "s3cret")
	host, portS, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscan(portS, &port)
	remoteDir := t.TempDir()
	var knownHosts string
	h := newHarness(t, func(c *config.JobsConf) {
		knownHosts = filepath.Join(c.WorkDir, "..", "known_hosts")
		c.Remote.KnownHosts = knownHosts
		c.Remote.RemoteDir = remoteDir
		c.Remote.Credentials = map[string]config.Credential{"linux-svc": {User: "svc", Password: "s3cret"}}
	})
	line := fmt.Sprintf("[%s]:%d %s", host, port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey))))
	os.WriteFile(knownHosts, []byte(line+"\n"), 0o600)

	h.upload(t, opA, "remote.sh", "echo \"host=$(hostname) args=$*\"; echo oops >&2; exit 3\n")
	in := interval("remote-check", "remote.sh", "one", "two words", "it's")
	in.Target = &Target{Host: host, Port: port, OS: OSLinux, Credential: "linux-svc"}
	j, err := h.svc.Create(opA, in)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.svc.RunNow(opA, j.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != checks.StatusCrit || r.Details["exit_code"] != "3" {
		t.Fatalf("remote exit 3 must be CRIT: %s %v %q", r.Status, r.Details["exit_code"], r.Message)
	}
	if !strings.Contains(r.Details["stdout"], "args=one two words it's") || !strings.Contains(r.Details["stderr"], "oops") {
		t.Fatalf("output: %q / %q", r.Details["stdout"], r.Details["stderr"])
	}
	if len(*cmds) != 3 || !strings.HasPrefix((*cmds)[0], "umask 077 && cat > ") || !strings.HasPrefix((*cmds)[2], "rm -f ") {
		t.Fatalf("want push, run, cleanup; got %q", *cmds)
	}
	if left, _ := os.ReadDir(remoteDir); len(left) != 0 {
		t.Fatalf("pushed program must be removed from the server, found %v", left)
	}

	// an unknown host key is refused (no trust-on-first-use)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPriv)
	os.WriteFile(knownHosts, []byte(fmt.Sprintf("[%s]:%d %s", host, port, ssh.MarshalAuthorizedKey(other.PublicKey()))), 0o600)
	r, _ = h.svc.RunNow(opA, j.ID, true)
	if r.Status != checks.StatusCrit || !strings.Contains(r.Message, "key mismatch") {
		t.Fatalf("host key mismatch must fail: %q", r.Message)
	}
	// missing known_hosts is a clear error
	os.Remove(knownHosts)
	r, _ = h.svc.RunNow(opA, j.ID, true)
	if !strings.Contains(r.Message, "known_hosts file") {
		t.Fatalf("missing known_hosts: %q", r.Message)
	}
}

func TestRemoteCommandForCygwin(t *testing.T) {
	cases := map[string]string{
		"/tmp/x.ps1": `powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$(cygpath -w '/tmp/x.ps1')" 'a b'`,
		"/tmp/x.bat": `cmd.exe /c "$(cygpath -w '/tmp/x.bat')" 'a b'`,
		"/tmp/x.exe": `'/tmp/x.exe' 'a b'`,
		"/tmp/x.py":  `python3 '/tmp/x.py' 'a b'`,
	}
	for path, want := range cases {
		if got := remoteCommand(OSWindowsCygwin, path, []string{"a b"}); got != want {
			t.Errorf("%s:\n got %s\nwant %s", path, got, want)
		}
	}
	if got := remoteCommand(OSLinux, "/tmp/x.ps1", nil); got != `pwsh -NoLogo -NoProfile -NonInteractive -File '/tmp/x.ps1'` {
		t.Errorf("linux ps1: %s", got)
	}
	if shq(`a'b`) != `'a'\''b'` {
		t.Error("shell quoting of single quotes")
	}
}
