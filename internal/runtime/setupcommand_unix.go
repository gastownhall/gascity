//go:build !windows

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// applySetupCommandCancellation gives a host-side lifecycle command the same
// cooperative cancellation shape as the provider runners: interrupt the
// command's process group first. The common WaitDelay bounds how long the
// command can remain in that cooperative phase; passing true to the returned
// finisher then force-kills the group synchronously before the caller returns.
func applySetupCommandCancellation(cmd *exec.Cmd, _ time.Duration) func(bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		pid := cmd.Process.Pid
		if err := syscall.Kill(-pid, syscall.SIGINT); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	return func(force bool) {
		if force && cmd.Process != nil {
			// The leader may have exited after running its trap while a
			// redirected descendant is still alive in this process group.
			// Kill the group synchronously so the caller never observes a
			// canceled setup command before its descendants are stopped.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
}
