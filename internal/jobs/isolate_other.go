//go:build !unix

package jobs

import "os/exec"

// isolate: on non-Unix systems the default (kill the process) is used.
func isolate(cmd *exec.Cmd) {}
