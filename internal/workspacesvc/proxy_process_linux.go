//go:build linux

package workspacesvc

import "syscall"

// proxyProcessSysProcAttr returns the process attributes used to spawn a
// proxy_process child. Pdeathsig is kernel-enforced: it fires no matter how
// the supervisor process ends, including the Go test -timeout watchdog's
// direct os.Exit (which runs no defer or t.Cleanup anywhere in the
// process), so it is the only way to guarantee the child does not survive a
// hard parent exit (ga-9br097).
func proxyProcessSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
