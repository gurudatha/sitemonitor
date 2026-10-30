//go:build unix

package jobs

import (
	"os/exec"
	"syscall"
)

// isolate starts the program in its own process group and, when the context
// ends, kills the whole group (so child processes do not outlive a timeout).
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
