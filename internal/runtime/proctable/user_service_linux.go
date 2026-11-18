//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DetachedUserServiceUnit returns the name of the systemd user service unit
// that owns pid and is detached from the caller, or "" when pid is invalid,
// its cgroup records no recognized foreign user service, or the unit is gc's
// own: the caller's own unit (same user manager and unit) or one of gc's
// supervisor units (isSupervisorSystemdUnit), whose stale orphans stay
// reapable. Unavailable or unrecognized target data returns "", preserving
// the caller's behavior, and a missing own cgroup record cannot establish an
// own-unit match, though the supervisor exclusions still apply.
//
// Such a service carries a session's identity through its environment, not
// because the session runs in it: it inherited the user manager's
// environment (after `systemctl --user import-environment` or
// `dbus-update-activation-environment --systemd` from an agent shell, which
// every unit started or restarted afterwards inherits), or it was given the
// identity with `systemd-run --user --setenv`. The cgroup cannot show which
// session, if any, started the unit, so any foreign user service qualifies,
// including one that hosts a session's runtime in its own cgroup (a terminal
// or tmux server run as a user service).
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

// isSupervisorSystemdUnit reports whether unit is one of gc's supervisor
// units: "<SupervisorSystemdUnitStem>.service",
// "<SupervisorSystemdUnitStem>-<suffix>.service" with a nonempty suffix, or
// the unit an operator delegates the supervisor to in the user manager
// (delegatedSupervisorUserUnit).
func isSupervisorSystemdUnit(unit string) bool {
	if delegated := delegatedSupervisorUserUnit(); delegated != "" && unit == delegated {
		return true
	}
	name, ok := strings.CutSuffix(unit, ".service")
	if !ok {
		return false
	}
	if name == SupervisorSystemdUnitStem {
		return true
	}
	suffix, ok := strings.CutPrefix(name, SupervisorSystemdUnitStem+"-")
	return ok && suffix != ""
}

// delegatedSupervisorUserUnit returns the service unit the operator delegates
// the supervisor's lifecycle to in the user manager: the unit
// SupervisorSystemdUnitEnv names, with the ".service" suffix systemctl adds
// to a bare name, while SupervisorSystemdScopeEnv is "user". It returns ""
// when no unit is delegated there. The default system scope places the unit
// beneath no user manager, so no root this fence reads can be in it.
func delegatedSupervisorUserUnit() string {
	if strings.TrimSpace(os.Getenv(SupervisorSystemdScopeEnv)) != "user" {
		return ""
	}
	unit := strings.TrimSpace(os.Getenv(SupervisorSystemdUnitEnv))
	if unit == "" {
		return ""
	}
	if !strings.HasSuffix(unit, ".service") {
		unit += ".service"
	}
	return unit
}
