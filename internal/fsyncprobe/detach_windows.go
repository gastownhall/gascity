//go:build windows

package fsyncprobe

import "syscall"

// workerSysProcAttr detaches the worker into its own process group. Windows
// cannot hand the worker its result pipe (exec.Cmd.ExtraFiles), so Start
// fails and Probe reports an error result rather than a measurement.
func workerSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
