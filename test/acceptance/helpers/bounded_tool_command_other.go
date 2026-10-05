//go:build !unix

package acceptancehelpers

import (
	"os"
	"os/exec"
)

func prepareBoundedToolCommand(_ *exec.Cmd) {}

func terminateBoundedToolCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}

func cleanupBoundedToolCommand(cmd *exec.Cmd) error {
	return terminateBoundedToolCommand(cmd)
}
