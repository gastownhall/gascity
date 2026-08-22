//go:build !windows

package packman

import (
	"os/exec"

	"github.com/gastownhall/gascity/internal/processgroup"
)

// startNetworkGitInOwnGroup makes cmd a process-group leader so the deadline can
// reach git's helpers, which are the processes that actually hold the output
// pipes and write into the repo cache.
func startNetworkGitInOwnGroup(cmd *exec.Cmd) {
	processgroup.StartCommandInNewGroup(cmd)
}

// terminateNetworkGitGroup signals git's whole process group, SIGTERM first so
// git can remove its partial clone, then SIGKILL.
func terminateNetworkGitGroup(cmd *exec.Cmd) error {
	return processgroup.TerminateCommand(cmd, 0, networkGitTerminateGrace, processgroup.Options{})
}
