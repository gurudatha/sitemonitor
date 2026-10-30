package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Outcome of one run.
type Outcome struct {
	ExitCode int           // -1 if the program did not finish (timeout, start error, connection error)
	Stdout   []byte        // capped at jobs.max_output_kb
	Stderr   []byte        // capped at jobs.max_output_kb
	Duration time.Duration //
	Err      error         // set when the program could not be run or did not finish
}

// capped is an io.Writer that keeps the first max bytes and counts the rest.
type capped struct {
	buf     bytes.Buffer
	max     int
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.dropped += len(p) - room
		} else {
			c.buf.Write(p)
		}
	} else {
		c.dropped += len(p)
	}
	return len(p), nil
}

func (c *capped) Bytes() []byte {
	if c.dropped > 0 {
		return append(c.buf.Bytes(), []byte(fmt.Sprintf("\n... [%d bytes truncated]", c.dropped))...)
	}
	return c.buf.Bytes()
}

// localArgv builds the command for a program in the binaries directory.
// There is no shell: arguments are passed to the program unchanged.
func (s *Service) localArgv(binPath string, args []string) []string {
	if pre, ok := s.cfg.Interpreters[strings.ToLower(filepath.Ext(binPath))]; ok && len(pre) > 0 {
		return append(append(append([]string{}, pre...), binPath), args...)
	}
	return append([]string{binPath}, args...)
}

// runLocal executes the program on this server in a fresh scratch directory.
func (s *Service) runLocal(ctx context.Context, j *Job, binPath, runID string) Outcome {
	start := time.Now()
	work := filepath.Join(s.cfg.WorkDir, runID)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return Outcome{ExitCode: -1, Err: fmt.Errorf("work dir: %w", err)}
	}
	defer os.RemoveAll(work)

	argv := s.localArgv(binPath, j.Args)
	if _, err := exec.LookPath(argv[0]); err != nil {
		return Outcome{ExitCode: -1, Err: fmt.Errorf("cannot run %s: %v (is the interpreter installed on this server?)", argv[0], err)}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = work
	// Minimal environment: no secrets of the monitor leak into user programs.
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + work, "TMPDIR=" + work, "LANG=C.UTF-8",
		"SITEMONITOR_JOB=" + j.Name, "SITEMONITOR_RUN_ID=" + runID, "SITEMONITOR_NODE=" + s.node,
	}
	max := int(s.cfg.MaxOutputKB << 10)
	stdout, stderr := &capped{max: max}, &capped{max: max}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	isolate(cmd) // own process group; the whole group is killed on timeout
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	out := Outcome{ExitCode: 0, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Duration: time.Since(start)}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		out.ExitCode, out.Err = -1, fmt.Errorf("timed out after %s", j.Timeout)
	case errors.As(err, &exitErr):
		out.ExitCode = exitErr.ExitCode()
		if out.ExitCode < 0 {
			out.Err = fmt.Errorf("terminated: %v", err)
		}
	case err != nil:
		out.ExitCode, out.Err = -1, err
	}
	return out
}
