//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"strconv"
)

// DetachedUserServiceUnit returns the name of the systemd user service unit
// that owns pid and is detached from the caller — a service a session's
// shell started (directly or through the user manager) that runs in a unit
// of its own, not in the session — or "" when pid is invalid, its cgroup
// records no recognized foreign user service, or the unit is the caller's
// own (same user manager and unit) or one of gc's supervisor units, whose
// stale orphans stay reapable. Unavailable or unrecognized target data
// returns "", preserving the caller's behavior, and a missing own cgroup
// record cannot establish an own-unit match, though the supervisor
// exclusions still apply.
//
// It is a kill-path fence, NOT part of the scanner: the scanner still
// reports such a process as a root (so its children are not promoted to
// roots in its place), and the callers that would terminate a scanned root
// — the orphan sweep and a session's pre-start orphan kill — consult this
// and leave it alone. Under `go test` without an injected procfs root it
// returns "" rather than read the host's live process table.
func DetachedUserServiceUnit(pid int) string {
	if pid <= 0 {
		return ""
	}
	if err := liveScanGuard(); err != nil {
		return ""
	}
	manager, unit := readUserManagerServiceUnit(scanRoot, pid)
	if unit == "" || isSupervisorSystemdUnit(unit) {
		return ""
	}
	if selfManager, selfUnit := readUserManagerServiceUnit(scanRoot, os.Getpid()); selfManager == manager && selfUnit == unit {
		return "" // the caller's own unit: its roots stay reapable
	}
	return unit
}

// readUserManagerServiceUnit reads pid's cgroup file from the procfs at root
// and returns its user manager and service unit per userManagerServiceUnit,
// or two empty strings when it cannot be read or recognized.
func readUserManagerServiceUnit(root string, pid int) (manager, unit string) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", ""
	}
	return userManagerServiceUnit(string(data))
}
