//go:build !windows

package main

import "syscall"

func platformKill(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}
