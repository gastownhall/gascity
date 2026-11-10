//go:build unix

package tmuxtest

import "syscall"

// signalProcess delivers sig to pid.
func signalProcess(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}
