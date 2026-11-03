//go:build linux

package proctable

// UserServiceUnitOf reads pid's cgroup from the scanner's procfs root and
// reports the user-manager service unit that owns it. See
// UserManagerServiceUnit. A pid the root does not name, or whose cgroup
// cannot be read, reports "", false. Under go test without an injected
// procfs root it reports "", false rather than read the host.
func UserServiceUnitOf(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	if err := liveScanGuard(); err != nil {
		return "", false
	}
	cgroup, ok, err := readProcessCgroup(scanRoot, pid)
	if err != nil || !ok {
		return "", false
	}
	return UserManagerServiceUnit(cgroup)
}
