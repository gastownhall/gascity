//go:build !windows

package main

import "os/exec"

// startOrderCommand starts an order's command and returns the process group
// it leads. Its caller has already made it a group leader.
func startOrderCommand(cmd *exec.Cmd) (int, error) {
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pgid := cmd.Process.Pid
	if got, err := platformGetpgid(cmd.Process.Pid); err == nil {
		pgid = got
	}
	return pgid, nil
}

// releaseOrderCommand ends the launcher's hold on a finished order command's
// tree. A Unix process group is addressed by its ID, so there is nothing to
// release.
func releaseOrderCommand(*exec.Cmd) {}
