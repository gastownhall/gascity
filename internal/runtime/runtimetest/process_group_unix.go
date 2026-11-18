//go:build !windows

package runtimetest

import "syscall"

// killProcessGroup SIGKILLs the process group led by pid.
func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
