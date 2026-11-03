//go:build !linux

package proctable

// UserServiceUnitOf is unavailable where the scanner does not read a
// procfs cgroup. No user service is classified, so the reap fence does
// not skip on this platform.
func UserServiceUnitOf(int) (string, bool) { return "", false }
