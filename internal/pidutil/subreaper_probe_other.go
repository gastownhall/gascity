//go:build !linux

package pidutil

// probeOrphanAdopter reports init as the adopter. PR_SET_CHILD_SUBREAPER is
// Linux-only: on other Unix platforms an orphan is reparented to init, and
// Windows does not reparent at all, so neither has a subreaper to detect.
func probeOrphanAdopter() (int, error) {
	return 1, nil
}
