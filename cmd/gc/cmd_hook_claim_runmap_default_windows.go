//go:build windows

package main

// runMapHasZeroConfigDir reports whether writeRunMap falls back to
// defaultRunMapDir when GC_RUNMAP_DIR is unset. Not on Windows. The manifold
// proxy that provisions and reads that directory is a Unix daemon, and
// "/run/gc-manifold-runmap" is not an absolute path here: it names a directory
// at the root of the current drive, which any user may create. Falling back to
// it would create that directory on every claim, where a Unix worker with no
// proxy finds the default uncreatable and publishes nothing.
func runMapHasZeroConfigDir() bool {
	return false
}
