//go:build !windows

package packman

import (
	"os/exec"

	"github.com/gastownhall/gascity/internal/processgroup"
)

// configureNetworkGitDeadline makes cmd a process-group leader and points its
// cancel at the whole group, so the deadline reaches git's helpers — the
// processes that hold the output pipes and write into the repo cache.
//
// SIGTERM goes first so git's own handler can remove the partial clone, then
// SIGKILL once the grace expires. Debris left by a SIGKILL landing mid-removal
// is not a leak: the next holder of the cache write lock RemoveAll's a checkout
// carrying no completion marker.
func configureNetworkGitDeadline(cmd *exec.Cmd) {
	processgroup.StartCommandInNewGroup(cmd)
	cmd.Cancel = func() error {
		// Pass the leader's pid as the known pgid rather than letting
		// TerminateCommand look it up at cancel time. Setpgid(0,0) makes the
		// group id equal the child pid, so this is the same number in the
		// ordinary case — but a live Getpgid returns ESRCH once the leader has
		// been reaped, and TerminateCommand would then return having signaled
		// nothing, while surviving helpers keep the group alive. That is
		// exactly the orphan-writer bug this file exists to fix, re-entering
		// through a side door. A pid recorded up front still names the group.
		knownPGID := 0
		if cmd.Process != nil {
			knownPGID = cmd.Process.Pid
		}
		return processgroup.TerminateCommand(cmd, knownPGID, networkGitTerminateGrace, processgroup.Options{})
	}
}
