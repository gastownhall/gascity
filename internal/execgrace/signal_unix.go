//go:build !windows

package execgrace

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// getpgid and killProcessGroup are indirection seams over the corresponding
// syscalls so tests can force the fallback branches below deterministically —
// a real process group cannot be made to fail Getpgid or a group kill on
// demand.
var (
	getpgid          = syscall.Getpgid
	killProcessGroup = syscall.Kill
)

// setProcessGroup puts the command in its own process group so a cooperative
// cancellation can be delivered to the whole group — reaching any foreground
// child (for example a long-running git checkout under a setup shell) that
// would otherwise keep the shell from running its rollback trap before the
// forced kill.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// interruptProcessGroup sends os.Interrupt to the command's process group so a
// foreground child receives it alongside the shell leader, reporting which
// path delivered the signal. It preserves the os.ErrProcessDone signal the
// caller special-cases: an already-exited target reports ErrProcessDone
// rather than a spurious failure. If the group id cannot be resolved it falls
// back to signaling the leader directly. The returned CancelOutcome is only
// meaningful when the error is nil.
func interruptProcessGroup(cmd *exec.Cmd) (CancelOutcome, error) {
	if cmd.Process == nil {
		return CancelNotDelivered, os.ErrProcessDone
	}
	pgid, err := getpgid(cmd.Process.Pid)
	if err != nil {
		if sigErr := cmd.Process.Signal(os.Interrupt); sigErr != nil {
			return CancelNotDelivered, sigErr
		}
		return CancelLeaderSignaledOnly, nil
	}
	if killErr := killProcessGroup(-pgid, syscall.SIGINT); killErr != nil {
		if errors.Is(killErr, syscall.ESRCH) {
			return CancelNotDelivered, os.ErrProcessDone
		}
		return CancelNotDelivered, killErr
	}
	return CancelGroupSignaled, nil
}
