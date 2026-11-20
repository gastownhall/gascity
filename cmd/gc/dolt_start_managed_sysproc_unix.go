//go:build !windows

package main

import "syscall"

func platformManagedDoltSQLServerSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func platformCloseOnExec(fd int) {
	syscall.CloseOnExec(fd)
}
