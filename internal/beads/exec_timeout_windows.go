//go:build windows

package beads

import "os/exec"

func prepareCommandForTimeout(_ *exec.Cmd) {}

func killCommandTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
