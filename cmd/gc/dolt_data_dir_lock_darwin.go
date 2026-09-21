//go:build darwin

package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// managedDoltLsofLockHolders lists every process with lockPath open. Combined
// with the caller's successful conflicting-flock probe, a sole open target is
// the sole possible holder: an flock holder must retain an open descriptor.
// Any lsof failure remains indeterminate and therefore fails closed.
var managedDoltLsofLockHolders = func(lockPath string) ([]byte, error) {
	return lsofOutput("-w", "-F0p", lockPath)
}

// managedDoltLockHolderPIDs resolves flock ownership on Darwin through lsof.
// Darwin has no /proc/locks, but lsof can enumerate every PID with the lock
// file open. The generic gate has already proved the file is flock-held; it
// permits SIGKILL only when this complete listing contains the target alone.
func managedDoltLockHolderPIDs(lockPath, _ string) ([]int, error) {
	out, err := managedDoltLsofLockHolders(lockPath)
	if err != nil {
		return nil, fmt.Errorf("inspect lock file %s with lsof: %w", lockPath, err)
	}
	pids, err := parseManagedDoltLsofHolderPIDs(out)
	if err != nil {
		return nil, fmt.Errorf("parse lsof holders for %s: %w", lockPath, err)
	}
	return pids, nil
}

func parseManagedDoltLsofHolderPIDs(out []byte) ([]int, error) {
	seen := make(map[int]struct{})
	for _, raw := range strings.Split(string(out), "\x00") {
		field := strings.Trim(raw, "\r\n")
		if !strings.HasPrefix(field, "p") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(field, "p"))
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("invalid pid field %q", field)
		}
		seen[pid] = struct{}{}
	}
	pids := make([]int, 0, len(seen))
	for pid := range seen {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}
