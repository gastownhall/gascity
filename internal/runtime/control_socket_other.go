//go:build !windows

package runtime

// platformControlSocketAbsent reports no extra absent-socket errors: the
// portable checks in ClassifyControlSocketDial cover unix platforms.
func platformControlSocketAbsent(error) bool { return false }
