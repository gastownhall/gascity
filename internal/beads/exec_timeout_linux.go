//go:build linux

package beads

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// prepareCommandForTimeout isolates the bd subprocess in its own process
// group so killCommandTree can reap the whole tree on a timeout, and asks the
// kernel to SIGKILL the child if this process dies first.
//
// Without Pdeathsig the group isolation that makes our own timeout kill
// reliable is exactly what shields the child from the parent's death: an
// external SIGKILL of the caller's group (a Bazel or `go test` timeout, a
// ctrl-c on `bazel test //cmd/gc:gc_test`) only reaches the caller, and the
// wrapper tree — gc-beads-bd.sh plus the gc re-execs under it — runs on
// forever. On cherry 2026-09-23 that left 510 such processes (oldest 21 h)
// and load ~250 on 192 cores. Pdeathsig is kernel-enforced and fires no
// matter how the parent exits; only the direct child receives it, so a
// managed dolt sql-server the wrapper daemonizes keeps its own lifecycle.
// Mirrors proxyProcessSysProcAttr in internal/workspacesvc (ga-9br097).
func prepareCommandForTimeout(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func killCommandTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		if killErr := syscall.Kill(-pgid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) && !errors.Is(killErr, syscall.ESRCH) {
			return killErr
		}
		return nil
	}
	if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return killErr
	}
	return nil
}
