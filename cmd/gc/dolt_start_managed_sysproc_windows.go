//go:build windows

package main

import "syscall"

func platformManagedDoltSQLServerSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func platformCloseOnExec(fd int) {
}
