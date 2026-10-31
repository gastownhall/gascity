//go:build windows

package workspacesvc

import (
	"errors"
	"fmt"
	"log"
	"syscall"
)

// errOrphanSweepUnsupported reports that this host cannot identify orphaned
// service processes, so the sweep must decline instead of reporting a clean
// process table.
var errOrphanSweepUnsupported = errors.New("workspacesvc: orphaned-process identification is unsupported on Windows")

// listProcessTablePIDs declines on Windows.
//
// The sweep's identity rules are procfs-shaped — reparent-to-subreaper, exact
// argv, and an environment block read from another process — and Windows
// provides none of them: it has no subreaper reparenting, and a process's
// environment is not readable from outside without injection.
//
// Reading the "/proc" path anyway would be worse than useless here. An absolute
// "/proc/<pid>/environ" resolves to C:\proc\<pid>\environ; the root of the
// system drive grants Authenticated Users add-subdirectory rights by default
// and C:\proc does not exist, so any local user could create that tree and
// hand-write the records that decide which PIDs this sweep sends SIGKILL to.
// Nothing here touches that path.
func listProcessTablePIDs() ([]int, error) {
	return nil, errOrphanSweepUnsupported
}

// readProcessEnviron declines on Windows, for the same reason.
func readProcessEnviron(pid int) ([]byte, error) {
	return nil, fmt.Errorf("workspacesvc: reading the environment of pid %d: %w", pid, errOrphanSweepUnsupported)
}

func signalProcessOrGroup(pid int, sig syscall.Signal) {
	log.Printf("workspacesvc: orphan process termination is unsupported on Windows for pid %d signal %v", pid, sig)
}
