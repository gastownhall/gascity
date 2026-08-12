//go:build !windows

package tmux

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// processIsAlive reports whether pid still names a live process. Permission
// errors count as alive: cleanup must retain its SIGKILL fallback rather than
// mistake an unobservable process for an exited one.
func processIsAlive(pid string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(pid))
	if err != nil || n <= 0 {
		return false
	}
	process, err := os.FindProcess(n)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// getParentPID returns the parent process ID (PPID) for a given PID.
// Returns empty string if the process doesn't exist or PPID can't be determined.
func getParentPID(pid string) string {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", pid).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// getProcessGroupID returns the process group ID (PGID) for a given PID.
// Returns empty string if the process doesn't exist or PGID can't be determined.
func getProcessGroupID(pid string) string {
	out, err := exec.Command("ps", "-o", "pgid=", "-p", pid).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// getProcessGroupMembers returns all PIDs in a process group.
// This finds processes that share the same PGID, including those that reparented to init.
func getProcessGroupMembers(pgid string) []string {
	// Use ps to find all processes with this PGID
	// On macOS: ps -axo pid,pgid
	// On Linux: ps -eo pid,pgid
	out, err := exec.Command("ps", "-axo", "pid=,pgid=").Output()
	if err != nil {
		return nil
	}

	var members []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimSpace(fields[1]) == pgid {
			members = append(members, strings.TrimSpace(fields[0]))
		}
	}
	return members
}
