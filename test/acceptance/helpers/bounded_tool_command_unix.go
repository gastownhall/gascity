//go:build unix

package acceptancehelpers

import (
	"os"
	"os/exec"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup"
)

const boundedToolTerminationGrace = 250 * time.Millisecond

func prepareBoundedToolCommand(cmd *exec.Cmd) {
	processgroup.StartCommandInNewGroup(cmd)
}

func terminateBoundedToolCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	return processgroup.TerminateCommand(cmd, cmd.Process.Pid, boundedToolTerminationGrace, processgroup.Options{})
}

func cleanupBoundedToolCommand(cmd *exec.Cmd) error {
	return terminateBoundedToolCommand(cmd)
}
