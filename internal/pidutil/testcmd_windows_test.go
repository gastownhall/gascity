//go:build windows

package pidutil

import "os/exec"

// longLivedTestCommand returns an unstarted command that stays alive well past
// any assertion here, together with the argv sequence Cmdline would report for
// it once running.
//
// ping is used rather than a sleep because Windows has no sleep binary and
// timeout.exe refuses to run without a console. Pinging loopback once a second
// keeps the process alive without touching the network stack beyond the local
// interface.
func longLivedTestCommand() (*exec.Cmd, []string) {
	return exec.Command("ping", "-n", "61", "127.0.0.1"), []string{"ping", "-n", "61", "127.0.0.1"}
}

// shortLivedTestCommand returns an unstarted command that exits immediately and
// successfully, for staging an already-dead PID.
func shortLivedTestCommand() *exec.Cmd {
	return exec.Command("cmd.exe", "/c", "exit 0")
}
