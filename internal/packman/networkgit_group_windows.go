//go:build windows

package packman

import "os/exec"

// startNetworkGitInOwnGroup is a no-op on Windows, which has no process groups
// in the POSIX sense. internal/processgroup is Unix-only for the same reason.
func startNetworkGitInOwnGroup(*exec.Cmd) {}

// terminateNetworkGitGroup kills git alone. Descendants are not reachable
// without a job object, so on Windows the bound rests on WaitDelay closing the
// pipes rather than on the writers being stopped.
func terminateNetworkGitGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
