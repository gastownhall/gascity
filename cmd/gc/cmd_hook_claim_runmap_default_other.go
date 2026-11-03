//go:build !windows

package main

// runMapHasZeroConfigDir reports whether writeRunMap falls back to
// defaultRunMapDir when GC_RUNMAP_DIR is unset.
func runMapHasZeroConfigDir() bool {
	return true
}
