//go:build windows

package main

import "syscall"

// createNoWindow is CREATE_NO_WINDOW: the child gets a console of its own,
// without a window, instead of sharing this process's.
const createNoWindow = 0x08000000

// backgroundSysProcAttr returns SysProcAttr for a background child that
// outlives the terminal it was started from, as Setpgid makes it do on Unix.
// The child leads a new process group, so a Ctrl+C typed at the terminal does
// not reach it, and it runs in a console of its own without a window, so
// closing the terminal does not end it either. Console programs it starts share
// that console, so none of them opens a window.
func backgroundSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow}
}
