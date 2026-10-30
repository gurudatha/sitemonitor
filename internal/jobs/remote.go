package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"sitemonitor/internal/config"
)

// Remote execution over SSH, for Linux and for Windows with Cygwin (whose
// sshd gives a POSIX shell). Steps:
//
//	1. connect, verifying the host key against jobs.remote.known_hosts
//	2. push the program to <remote_dir>/sitemonitor-<run id>-<name> (mode 0700)
//	3. run it with the job's arguments (each one shell-quoted) and collect
//	   stdout, stderr and the exit code
//	4. delete the pushed file
//
// The credential (user + password or private key) comes from node.yaml.

// shq quotes s for a POSIX shell (bash on Linux and Cygwin).
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// remoteCommand builds the command line that runs the pushed file.
func remoteCommand(osName, path string, args []string) string {
	var cmd string
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case osName == OSWindowsCygwin && ext == ".ps1":
		cmd = `powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$(cygpath -w ` + shq(path) + `)"`
	case osName == OSWindowsCygwin && (ext == ".bat" || ext == ".cmd"):
		cmd = `cmd.exe /c "$(cygpath -w ` + shq(path) + `)"`
	case ext == ".ps1":
		cmd = "pwsh -NoLogo -NoProfile -NonInteractive -File " + shq(path)
	case ext == ".py":
		cmd = "python3 " + shq(path)
	case ext == ".pl":
		cmd = "perl " + shq(path)
	case ext == ".sh":
		cmd = "sh " + shq(path)
	case ext == ".bash":
		cmd = "bash " + shq(path)
	default:
		cmd = shq(path)
	}
	for _, a := range args {
		cmd += " " + shq(a)
	}
	return cmd
}

func sshConfig(cred config.Credential, knownHostsFile string, timeout time.Duration) (*ssh.ClientConfig, error) {
	if _, err := os.Stat(knownHostsFile); err != nil {
		return nil, fmt.Errorf("known_hosts file %s is missing: add the server's host key (ssh-keyscan -p PORT HOST >> %s) so it can be verified", knownHostsFile, knownHostsFile)
	}
	hostKeys, err := knownhosts.New(knownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	var auth []ssh.AuthMethod
	if cred.PrivateKey != "" {
		pem, err := os.ReadFile(cred.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("private key: %w", err)
		}
		var signer ssh.Signer
		if cred.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(cred.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(pem)
		}
		if err != nil {
			return nil, fmt.Errorf("private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if cred.Password != "" {
		auth = append(auth, ssh.Password(cred.Password))
	}
	return &ssh.ClientConfig{User: cred.User, Auth: auth, HostKeyCallback: hostKeys, Timeout: timeout}, nil
}

func (s *Service) runRemote(ctx context.Context, j *Job, binPath, runID string) Outcome {
	start := time.Now()
	fail := func(err error) Outcome { return Outcome{ExitCode: -1, Err: err, Duration: time.Since(start)} }
	t := j.Target
	cred, ok := s.cfg.Remote.Credentials[t.Credential]
	if !ok {
		return fail(fmt.Errorf("credential %q is not configured on this server", t.Credential))
	}
	if cred.Password == "" && cred.PrivateKey == "" {
		return fail(fmt.Errorf("credential %q has no password or private key on this server (is its environment variable set?)", t.Credential))
	}
	cfg, err := sshConfig(cred, s.cfg.Remote.KnownHosts, s.cfg.Remote.ConnectTimeout)
	if err != nil {
		return fail(err)
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	d := net.Dialer{Timeout: s.cfg.Remote.ConnectTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fail(fmt.Errorf("connect %s: %w", addr, err))
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return fail(fmt.Errorf("ssh %s: %w", addr, err))
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() }) // timeout: drop the connection (ends the remote session)
	defer stop()

	program, err := os.ReadFile(binPath)
	if err != nil {
		return fail(err)
	}
	remotePath := strings.TrimRight(s.cfg.Remote.RemoteDir, "/") + "/sitemonitor-" + runID + "-" + j.Binary

	// 2. push
	push, err := client.NewSession()
	if err != nil {
		return fail(fmt.Errorf("ssh session: %w", err))
	}
	push.Stdin = bytes.NewReader(program)
	var pushErr bytes.Buffer
	push.Stderr = &pushErr
	err = push.Run("umask 077 && cat > " + shq(remotePath) + " && chmod 700 " + shq(remotePath))
	push.Close()
	if err != nil {
		return fail(fmt.Errorf("push program to %s: %v %s", addr, err, strings.TrimSpace(pushErr.String())))
	}
	defer func() { // 4. clean up, even after a failure
		if rm, err := client.NewSession(); err == nil {
			rm.Run("rm -f " + shq(remotePath))
			rm.Close()
		}
	}()

	// 3. run
	run, err := client.NewSession()
	if err != nil {
		return fail(fmt.Errorf("ssh session: %w", err))
	}
	defer run.Close()
	max := int(s.cfg.MaxOutputKB << 10)
	stdout, stderr := &capped{max: max}, &capped{max: max}
	run.Stdout, run.Stderr = stdout, stderr
	err = run.Run(remoteCommand(t.OS, remotePath, j.Args))
	out := Outcome{ExitCode: 0, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Duration: time.Since(start)}
	var exitErr *ssh.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		out.ExitCode, out.Err = -1, fmt.Errorf("timed out after %s", j.Timeout)
	case errors.As(err, &exitErr):
		out.ExitCode = exitErr.ExitStatus()
	case err != nil:
		out.ExitCode, out.Err = -1, fmt.Errorf("remote run: %w", err)
	}
	return out
}
