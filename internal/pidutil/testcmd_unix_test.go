//go:build unix

package pidutil

import "os/exec"

// longLivedTestCommand returns an unstarted command that stays alive well past
// any assertion here, together with the argv sequence Cmdline must report for
// it once running.
func longLivedTestCommand() (*exec.Cmd, []string) {
	return exec.Command("sleep", "60"), []string{"sleep", "60"}
}

// shortLivedTestCommand returns an unstarted command that exits immediately and
// successfully, for staging an already-dead PID.
func shortLivedTestCommand() *exec.Cmd {
	return exec.Command("true")
}
