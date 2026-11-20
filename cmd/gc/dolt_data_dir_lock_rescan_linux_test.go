//go:build linux

package main

import (
	"errors"
	"testing"
)

// TestManagedDoltLockHoldersRescanAnEmptyScan: a scan of /proc/locks that
// lock churn made miss a held lock (an empty holder set) is scanned again,
// up to managedDoltProcLocksScans times, before it is believed; a scan that
// finds a holder, or fails, is believed at once.
func TestManagedDoltLockHoldersRescanAnEmptyScan(t *testing.T) {
	prev := managedDoltScanProcLocksOnce
	t.Cleanup(func() { managedDoltScanProcLocksOnce = prev })
	run := func(results ...[]int) ([]int, int, error) {
		scans := 0
		managedDoltScanProcLocksOnce = func(string, string, string) ([]int, error) {
			scans++
			if scans > len(results) {
				return nil, nil
			}
			return results[scans-1], nil
		}
		pids, err := managedDoltLockHolderPIDsWithProcRoot("lock", "locks", "/proc")
		return pids, scans, err
	}
	if pids, scans, err := run(nil, nil, []int{42}); err != nil || len(pids) != 1 || pids[0] != 42 || scans != 3 {
		t.Fatalf("two missed scans, then the holder = %v after %d scan(s), %v; want [42] after 3", pids, scans, err)
	}
	if pids, scans, err := run([]int{7}); err != nil || len(pids) != 1 || scans != 1 {
		t.Fatalf("a first scan that finds the holder = %v after %d scan(s), %v; want one scan", pids, scans, err)
	}
	if pids, scans, err := run(); err != nil || len(pids) != 0 || scans != managedDoltProcLocksScans {
		t.Fatalf("no holder at all = %v after %d scan(s), %v; want none after %d", pids, scans, err, managedDoltProcLocksScans)
	}
	managedDoltScanProcLocksOnce = func(string, string, string) ([]int, error) { return nil, errors.New("unreadable") }
	if _, err := managedDoltLockHolderPIDsWithProcRoot("lock", "locks", "/proc"); err == nil {
		t.Fatal("a scan that fails was not reported")
	}
}
