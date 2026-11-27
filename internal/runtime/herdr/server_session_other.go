//go:build !windows

package herdr

import "syscall"

// serverSysProcAttr makes the herdr session server the leader of a new Unix
// session. herdr reports a server as a detached daemon only when
// getsid(0) == getpid() (herdr src/platform/mod.rs
// current_process_is_detached_server_daemon). A server left in gc's session
// reports detached_server_daemon=false, and herdr's remote attach
// (src/remote/restart_policy.rs) then asks to restart it on every
// `herdr --remote <host> --session <city>`, which stops the city's panes.
func serverSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
