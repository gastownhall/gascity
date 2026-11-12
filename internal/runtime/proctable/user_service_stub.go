//go:build !linux

package proctable

// DetachedUserServiceUnit returns "" on platforms without a procfs cgroup
// record to read: no detached systemd user service can be recognized there,
// so the kill paths keep their prior behavior.
func DetachedUserServiceUnit(int) string { return "" }
