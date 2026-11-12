package proctable

import (
	"path/filepath"
	"strings"
)

// SupervisorSystemdUnitStem is the shared stem of gc's supervisor's systemd
// user-service units: the default "<stem>.service" and the isolated-home
// suffixed "<stem>-<suffix>.service" (cmd/gc's defaultSupervisorSystemdUnit
// and supervisorSystemdUnitPrefix; duplicated here so this package stays
// free of the cmd/gc dependency tree, and pinned equal by a test). It names
// city infrastructure, not a configured agent role.
const SupervisorSystemdUnitStem = "gascity-supervisor"

// SupervisorSystemdUnitEnv and SupervisorSystemdScopeEnv name the variables
// through which an operator delegates the supervisor's lifecycle to a systemd
// unit of their own (cmd/gc's supervisorSystemdUnitEnv and
// supervisorSystemdScopeEnv; duplicated here and pinned equal by a test). A
// unit delegated in the user manager hosts gc's supervisor as the stem units
// do, so the detached user-service fence treats it as gc's own.
const (
	SupervisorSystemdUnitEnv  = "GC_SUPERVISOR_SYSTEMD_UNIT"
	SupervisorSystemdScopeEnv = "GC_SUPERVISOR_SYSTEMD_SCOPE"
)

// userManagerServiceUnit returns the systemd user manager
// ("user@<UID>.service") and the user service unit beneath it that owns the
// process whose /proc/<pid>/cgroup content cgroupFile holds, or two empty
// strings when the records do not prove one.
//
// The unified record ("0::<path>") is preferred; without it a cgroup v1
// record whose controllers field includes "name=systemd" is used, and
// unrelated controllers are ignored. A malformed line, or conflicting
// records of the kind selected (two different paths), prove nothing.
//
// The path must name a user manager, "user@<UID>.service" with a decimal UID,
// and the unit is the first ".service" component below it: intervening
// ".slice" components are skipped, delegated descendants below a service
// belong to it, and a ".scope" is never looked through — a process in a scope
// is in no service unit. A path without the manager (a system service, or a
// cgroup namespace that hides the manager), the manager alone or with only
// slices below it, an unrecognized component, or a non-absolute path proves
// nothing, so the caller keeps its prior behavior.
func userManagerServiceUnit(cgroupFile string) (manager, unit string) {
	path, ok := selectSystemdCgroupPath(cgroupFile)
	if !ok {
		return "", ""
	}
	return userServiceUnitFromCgroupPath(path)
}

// selectSystemdCgroupPath picks the cgroup path that names a systemd-owned
// hierarchy from the records of a /proc/<pid>/cgroup file: the unified v2
// record when one is present, else a v1 "name=systemd" record. Conflicting
// records of the kind selected prove nothing.
func selectSystemdCgroupPath(cgroupFile string) (string, bool) {
	var unified, v1 string
	var haveUnified, haveV1 bool
	for _, line := range strings.Split(cgroupFile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 || fields[2] == "" {
			return "", false
		}
		hierarchy, controllers, path := fields[0], fields[1], fields[2]
		switch {
		case hierarchy == "0" && controllers == "":
			if haveUnified && unified != path {
				return "", false
			}
			unified, haveUnified = path, true
		case !haveUnified && hasController(controllers, "name=systemd"):
			if haveV1 && v1 != path {
				return "", false
			}
			v1, haveV1 = path, true
		}
	}
	if haveUnified {
		return unified, true
	}
	return v1, haveV1
}

// hasController reports whether a cgroup record's controllers field names
// controller among its comma-separated entries.
func hasController(controllers, controller string) bool {
	for _, c := range strings.Split(controllers, ",") {
		if c == controller {
			return true
		}
	}
	return false
}

// userServiceUnitFromCgroupPath walks a systemd-owned cgroup path and
// returns its user manager and service unit, per userManagerServiceUnit's
// rules.
func userServiceUnitFromCgroupPath(cgroupPath string) (manager, unit string) {
	if !strings.HasPrefix(cgroupPath, "/") {
		return "", ""
	}
	cleaned := filepath.Clean(cgroupPath)
	if cleaned == "/" {
		return "", ""
	}
	components := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	managerIdx := -1
	for i, component := range components {
		if isUserManagerUnit(component) {
			managerIdx = i
			break
		}
	}
	if managerIdx < 0 || managerIdx == len(components)-1 {
		return "", "" // no user manager, or the manager's own cgroup
	}
	for _, component := range components[managerIdx+1:] {
		switch {
		case strings.HasSuffix(component, ".slice"):
			continue
		case strings.HasSuffix(component, ".scope"):
			return "", "" // a scope is never looked through
		case isServiceUnit(component):
			return components[managerIdx], component // delegated descendants below belong to this unit
		default:
			return "", "" // an unrecognized component proves no service unit
		}
	}
	return "", "" // only slices below the manager
}

// isUserManagerUnit reports whether component is a systemd user manager's
// unit, "user@<UID>.service" with a decimal UID; the unnumbered template
// form "user@.service" is not a manager.
func isUserManagerUnit(component string) bool {
	rest, ok := strings.CutPrefix(component, "user@")
	if !ok {
		return false
	}
	uid, ok := strings.CutSuffix(rest, ".service")
	return ok && isDecimal(uid)
}

// isServiceUnit reports whether component names a service unit: a nonempty
// name with the ".service" suffix.
func isServiceUnit(component string) bool {
	name, ok := strings.CutSuffix(component, ".service")
	return ok && name != ""
}

// isDecimal reports whether s is a nonempty run of ASCII digits.
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
