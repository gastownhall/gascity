//go:build linux

package proctable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// ProcessIdentity returns the Linux /proc start-time token used by
// SnapshotProcesses for pid.
func ProcessIdentity(pid int) (string, error) {
	return processIdentityWithReader(pid, os.ReadFile)
}

func processIdentityWithReader(pid int, readFile func(string) ([]byte, error)) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid PID %d", pid)
	}
	if readFile == nil {
		return "", fmt.Errorf("process identity reader is nil")
	}
	path := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return "", fmt.Errorf("%w: PID %d", ErrProcessGone, pid)
		}
		return "", fmt.Errorf("reading process identity for PID %d: %w", pid, err)
	}
	_, _, startTime, ok, err := parseProcStatIdentity(string(data))
	if err != nil {
		return "", fmt.Errorf("parsing process identity for PID %d: %w", pid, err)
	}
	if !ok || startTime == "" {
		return "", fmt.Errorf("process identity for PID %d is empty", pid)
	}
	return startTime, nil
}
