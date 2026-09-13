//go:build windows

package execgrace

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on Windows, which has no POSIX process groups;
// cancellation degrades to interrupting the leader (and then Kill) via
// interruptProcessGroup.
func setProcessGroup(_ *exec.Cmd) {}

// interruptProcessGroup signals the command's process directly on Windows.
// os.Interrupt is unsupported there, so this returns an error and the caller
// falls back to Kill, matching the pre-existing Windows behavior.
func interruptProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Signal(os.Interrupt)
}
