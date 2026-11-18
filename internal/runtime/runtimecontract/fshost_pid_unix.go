//go:build !windows

package runtimecontract

import (
	"errors"
	"fmt"
	"syscall"
)

// hostPIDLiveness reports whether pid is a live host process the caller may
// signal, by sending signal 0.
func hostPIDLiveness(pid int) (Status, string) {
	switch err := syscall.Kill(pid, 0); {
	case errors.Is(err, syscall.ESRCH):
		return StatusFail, fmt.Sprintf("agent_pid %d is no process on this host (a pid from another pid namespace?)", pid)
	case errors.Is(err, syscall.EPERM):
		return StatusFail, fmt.Sprintf("agent_pid %d belongs to another user", pid)
	case err != nil:
		return StatusFail, fmt.Sprintf("agent_pid %d: %v", pid, err)
	}
	return StatusPass, fmt.Sprintf("agent_pid %d is a live host process of the caller's user", pid)
}
