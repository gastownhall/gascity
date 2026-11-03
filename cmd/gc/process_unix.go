//go:build !windows

package main

import "syscall"

func platformGetpgid(pid int) (int, error) {
	return syscall.Getpgid(pid)
}

func platformGetpgrp() int {
	return syscall.Getpgrp()
}

func platformKill(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}
