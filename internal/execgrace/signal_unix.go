//go:build !windows

package execgrace

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
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
// foreground child receives it alongside the shell leader. It preserves the
// os.ErrProcessDone signal the caller special-cases: an already-exited target
// reports ErrProcessDone rather than a spurious failure. If the group id cannot
// be resolved it falls back to signaling the leader directly.
func interruptProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return cmd.Process.Signal(os.Interrupt)
	}
	if killErr := syscall.Kill(-pgid, syscall.SIGINT); killErr != nil {
		if errors.Is(killErr, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return killErr
	}
	return nil
}
