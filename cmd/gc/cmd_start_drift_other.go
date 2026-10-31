//go:build !windows

package main

import (
	"fmt"
	"io"
	"syscall"
)

func printUnreadableSupervisorRestartError(stderr io.Writer, pid int, exeErr error) {
	if supervisorRuntimeGOOS == "darwin" {
		fmt.Fprintf(stderr, "error: cannot auto-restart supervisor: macOS cannot resolve the executable path for a direct supervisor (%v). If the supervisor is launchd-managed, rerun 'gc supervisor install' and then 'gc start'; otherwise stop the supervisor manually and rerun 'gc start', or pass --no-auto-restart to skip the restart and surface the drift as an error.\n", exeErr) //nolint:errcheck // best-effort stderr
		return
	}
	fmt.Fprintf(stderr, "error: cannot auto-restart supervisor: /proc/%d/exe is owned by a different user (permission denied: %v). Either rerun gc start as the supervisor's uid, or pass --no-auto-restart to skip the restart and surface the drift as an error.\n", pid, exeErr) //nolint:errcheck // best-effort stderr
}

// readSupervisorExePath returns the resolved path of the supervisor's
// executable via /proc/<pid>/exe. The kernel readlink resolves
// symlinks for us — no extra realpath layer needed.
//
// When the binary on disk has been replaced (the typical drift case:
// `go install` writes a new file at the same path, unlinking the
// original inode the supervisor still has open), the kernel decorates
// the link target with a literal " (deleted)" suffix. We strip that
// suffix because the on-disk path is what the auto-restart needs to
// spawn — the new bytes already live at the un-suffixed path.
func readSupervisorExePath(pid int) (string, error) {
	path, _, err := readSupervisorExeLink(pid)
	return path, err
}

func platformDriftRestartKill(pid int) error {
	return platformKill(pid, syscall.SIGTERM)
}
