package demotarget

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSHOptions configure the demo SSH server (docker-compose only): a stand-in
// for a remote Linux server so remote jobs can be tried locally.
type SSHOptions struct {
	Addr          string
	Name          string // host name the monitors use (written to known_hosts)
	User          string
	Password      string
	HostKeyFile   string // created on first start, so the key survives restarts
	KnownHostsOut string // the public host key is written here for the monitors
}

func hostSigner(path string) (ssh.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(b)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// RunSSH serves SSH "exec" requests with /bin/sh -c (password auth only).
func RunSSH(o SSHOptions) error {
	if o.Password == "" {
		return errors.New("demo-ssh: set a password")
	}
	signer, err := hostSigner(o.HostKeyFile)
	if err != nil {
		return err
	}
	_, port, _ := net.SplitHostPort(o.Addr)
	line := fmt.Sprintf("[%s]:%s %s", o.Name, port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))))
	if err := os.WriteFile(o.KnownHostsOut, []byte(line+"\n"), 0o644); err != nil {
		return err
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if c.User() == o.User && string(p) == o.Password {
			return nil, nil
		}
		return nil, errors.New("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return err
	}
	slog.Info("demo ssh server listening", "addr", o.Addr, "user", o.User, "known_hosts", o.KnownHostsOut)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go serveSSH(conn, cfg)
	}
}

func serveSSH(conn net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range creqs {
				if req.Type != "exec" || len(req.Payload) < 4 {
					req.Reply(false, nil)
					continue
				}
				n := binary.BigEndian.Uint32(req.Payload)
				if int(n) > len(req.Payload)-4 {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				c := exec.Command("/bin/sh", "-c", string(req.Payload[4:4+n]))
				c.Stdin, c.Stdout, c.Stderr = ch, ch, ch.Stderr()
				code := 0
				if err := c.Run(); err != nil {
					var ee *exec.ExitError
					if errors.As(err, &ee) {
						code = ee.ExitCode()
					} else {
						code = 127
					}
				}
				status := make([]byte, 4)
				binary.BigEndian.PutUint32(status, uint32(code))
				ch.SendRequest("exit-status", false, status)
				return
			}
		}()
	}
}
