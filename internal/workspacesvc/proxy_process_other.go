//go:build !linux

package workspacesvc

import "syscall"

// proxyProcessSysProcAttr returns the process attributes used to spawn a
// proxy_process child. Pdeathsig is Linux-only; non-Linux platforms keep the
// prior Setpgid-only behavior.
func proxyProcessSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
