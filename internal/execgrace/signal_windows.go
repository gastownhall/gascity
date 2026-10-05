//go:build windows

package execgrace

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on Windows, which has no POSIX process groups;
// cancellation degrades to signaling the leader (and then Kill) via
// terminateProcessGroup.
func setProcessGroup(_ *exec.Cmd) {}

// terminateProcessGroup signals the command's process directly on Windows.
// Windows cannot deliver a catchable termination signal to an arbitrary
// process, so this returns an error and the caller falls back to Kill,
// matching the pre-existing Windows behavior. The returned CancelOutcome is
// only meaningful when the error is nil; there is no process-group concept on
// Windows, so a successful direct signal reports CancelLeaderSignaledOnly.
func terminateProcessGroup(cmd *exec.Cmd) (CancelOutcome, error) {
	if cmd.Process == nil {
		return CancelNotDelivered, os.ErrProcessDone
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		return CancelNotDelivered, err
	}
	return CancelLeaderSignaledOnly, nil
}
