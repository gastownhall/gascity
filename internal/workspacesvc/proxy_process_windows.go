//go:build windows

package workspacesvc

import "syscall"

// proxyProcessSysProcAttr returns the process attributes used to spawn a
// proxy_process child. The new process group only keeps the child from
// inheriting this console's Ctrl+C; it is not a containment boundary and does
// not stand in for Pdeathsig. The spawn path assigns the child to a
// kill-on-release Job Object for that.
func proxyProcessSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
