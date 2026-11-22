//go:build windows

package main

import (
	"os/exec"

	"github.com/gastownhall/gascity/internal/processgroup"
)

// startOrderCommand starts an order's command inside a Job Object that owns
// its whole tree, and returns the console group it leads.
//
// Canceling an order terminates the command's tree, and on Windows only an
// owned tree can be terminated: a console process group routes Ctrl+Break and
// contains nothing. The tree survives release, as a Unix process group
// survives its leader's exit, so a process the command left behind keeps
// running once the command itself has finished.
func startOrderCommand(cmd *exec.Cmd) (int, error) {
	return processgroup.StartCommand(cmd, processgroup.SurviveRelease)
}

// releaseOrderCommand ends the launcher's hold on a finished order command's
// tree. It runs after any cleanup the command needed, since a released tree
// can no longer be terminated.
func releaseOrderCommand(cmd *exec.Cmd) {
	_ = processgroup.ReleaseCommand(cmd)
}
