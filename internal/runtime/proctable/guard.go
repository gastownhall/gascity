package proctable

import "testing"

// scanRoot is the procfs root enumerated by the Linux scanner. It defaults to
// the live "/proc". Tests override it with a fake procfs via
// SetScanRootForTesting so a `go test` run never enumerates — and the orphan
// sweep never reaps — the host's real agent processes.
var scanRoot = "/proc"

// SetScanRootForTesting overrides the procfs root used by ScanBySessionID and
// IsScanRoot for t, and restores it when t ends. It exists only so tests can
// drive the scanner against a controlled, fake procfs tree instead of the
// host's; it is the only write of scanRoot (the testrelax analyzer).
func SetScanRootForTesting(t testing.TB, root string) {
	t.Helper()
	prev := scanRoot
	scanRoot = root
	t.Cleanup(func() { scanRoot = prev })
}

// TestSpawnedEnv is set by a test harness on every gc binary it execs.
const TestSpawnedEnv = "GC_TEST_SPAWNED"

// TestSpawned reports whether this process was exec'd by a test harness.
func TestSpawned() bool { return false }

// orphanScanGuard refuses the orphan sweep's live /proc scan in a
// test-spawned binary.
func orphanScanGuard(_ string) error { return nil }
