//go:build !windows

package fsyncprobe

import "syscall"

// workerSysProcAttr detaches the worker into its own session.
func workerSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
