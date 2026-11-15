//go:build !windows

package tmux

import (
	"errors"
	"fmt"
	"syscall"
)

func signalPID(pid int, signal processSignal) error {
	var sig syscall.Signal
	switch signal {
	case processSignalTerm:
		sig = syscall.SIGTERM
	case processSignalKill:
		sig = syscall.SIGKILL
	default:
		return fmt.Errorf("unsupported process signal %q", signal)
	}
	return syscall.Kill(pid, sig)
}

func isProcessSignalGoneError(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

// isProcessSignalDeniedError reports a signal the kernel refused for want of
// permission: the target runs as another user, such as a root-owned sudo
// wrapper.
func isProcessSignalDeniedError(err error) bool {
	return errors.Is(err, syscall.EPERM)
}
