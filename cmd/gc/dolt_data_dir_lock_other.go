//go:build !linux

package main

import (
	"fmt"
	"runtime"
)

// managedDoltLockHolderPIDs reports that flock ownership cannot be resolved
// on this platform. Only Linux exposes per-lock holder PIDs (via
// /proc/locks); elsewhere the SIGKILL gate has no way to tell its own target
// from a foreign holder, so it fails closed exactly as it did before the
// sole-holder exception existed.
func managedDoltLockHolderPIDs(lockPath, procLocksPath string) ([]int, error) {
	return nil, fmt.Errorf("dolt lock ownership inspection is unavailable on %s", runtime.GOOS)
}
