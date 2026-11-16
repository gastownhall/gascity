//go:build !windows

package workspacesvc

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// listProcessTablePIDs enumerates every PID in the process table by reading
// /proc. A host without procfs (darwin) reports the read error, and the sweep
// declines rather than reporting an empty process table.
func listProcessTablePIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("workspacesvc: reading the process table: %w", err)
	}
	pids := make([]int, 0, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// readProcessEnviron returns a process's raw NUL-separated environment block.
func readProcessEnviron(pid int) ([]byte, error) {
	return os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
}

// signalProcessOrGroup signals the process group led by pid, falling back
// to the process itself when pid is not a group leader. Unsafe targets are
// refused outright.
func signalProcessOrGroup(pid int, sig syscall.Signal) {
	if unsafeSignalTarget(pid) {
		log.Printf("workspacesvc: refusing to signal unsafe orphan-reap target pid %d", pid)
		return
	}
	if err := syscall.Kill(-pid, sig); err == nil {
		return
	}
	_ = syscall.Kill(pid, sig)
}

// unsafeSignalTarget reports whether pid must never be signaled: init,
// nonpositive pids (kill(2) broadcast and current-group semantics), and the
// sweeper's own process group. The scan cannot produce such pids today;
// this mirrors processgroup.Terminate's refusal so kill-adjacent code stays
// safe under future refactors.
func unsafeSignalTarget(pid int) bool {
	return pid <= 1 || pid == syscall.Getpgrp()
}
