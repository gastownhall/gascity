//go:build darwin

package proctable

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// OpenFiles returns the absolute paths pid holds open, or nothing when the
// process is gone. It refuses the live process table under go test for the
// same reason [ScanBySessionID] does.
//
// Darwin has no /proc: the paths come from lsof's name field ("n<path>"),
// which prints a path with bytes it cannot show verbatim escaped, so such a
// path matches nothing a caller compares it with.
func OpenFiles(pid int) ([]string, error) {
	if err := liveScanGuard(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), processSnapshotTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-w", "-a", "-p", strconv.Itoa(pid), "-Fn").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(out) == 0 {
			// lsof exits non-zero, printing nothing, when the pid does not exist.
			return nil, nil
		}
		return nil, fmt.Errorf("running lsof for pid %d: %w", pid, err)
	}
	return parseLsofNamePaths(string(out)), nil
}

// parseLsofNamePaths returns the absolute paths in lsof field output's name
// lines ("n<name>"); other fields, and names that are not absolute paths
// (sockets, pipes), are skipped.
func parseLsofNamePaths(out string) []string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		name, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "n")
		if ok && filepath.IsAbs(name) {
			paths = append(paths, name)
		}
	}
	return paths
}

// ProcessCwd returns pid's working directory: lsof's name for its cwd
// descriptor, read as [OpenFiles] reads the others. A process that is gone
// has none, and a name that is not one absolute path names none: an error
// either way. It refuses the live process table under go test, as
// [ScanBySessionID] does.
func ProcessCwd(pid int) (string, error) {
	if err := liveScanGuard(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), processSnapshotTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-w", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", fmt.Errorf("running lsof for the working directory of pid %d: %w", pid, err)
	}
	return parseLsofCwd(pid, string(out))
}

// parseLsofCwd returns the working directory lsof field output for pid's cwd
// descriptor names: its one absolute path.
func parseLsofCwd(pid int, out string) (string, error) {
	if paths := parseLsofNamePaths(out); len(paths) == 1 {
		return paths[0], nil
	}
	return "", fmt.Errorf("lsof names no single working directory for pid %d: %q", pid, out)
}
