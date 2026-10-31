//go:build windows

package main

import (
	"os/exec"

	"github.com/gastownhall/gascity/internal/processgroup"
)

// runWorkQueryCommand runs a work query to completion and returns its standard
// output as cmd.Output does.
//
// The query starts inside a Job Object that owns its whole tree
// (runPreparedCommand), and canceling it terminates that tree at once,
// replacing any Cancel cmd carries. The interpreter is only the root of a
// query: the bd and dolt processes under it hold the store connections, and
// they outlive a killed interpreter unless something owns them. That is what
// killing the query's process group does on Unix. A query that finishes
// leaves what it started running, as a Unix process group outlives its
// leader.
func runWorkQueryCommand(cmd *exec.Cmd) ([]byte, error) {
	return runPreparedCommand(cmd, killWorkQueryTreeOnCancel)
}

// killWorkQueryTreeOnCancel makes canceling cmd terminate its owned tree with
// no cooperative phase.
func killWorkQueryTreeOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return processgroup.KillCommandGroup(cmd, 0, processgroup.Options{})
	}
}
