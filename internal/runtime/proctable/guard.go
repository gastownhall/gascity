package proctable

import (
	"fmt"
	"os"
	"testing"
)

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

// TestSpawnedEnv is set by a test harness on every gc binary it execs. Such a
// binary is neither a `go test` binary (testing.Testing() is false) nor named
// *.test, so liveScanGuard cannot see that it runs under test.
const TestSpawnedEnv = "GC_TEST_SPAWNED"

// TestSpawned reports whether this process was exec'd by a test harness. Any
// non-empty value counts, so an ambiguous value errs toward protection.
func TestSpawned() bool { return os.Getenv(TestSpawnedEnv) != "" }

// orphanScanGuard refuses the orphan sweep's live /proc scan (empty id) in a
// test-spawned binary, for the reason liveScanGuard exists (#2839): the sweep
// would see the host's real agents as orphans. A scan scoped to one session id
// stays allowed, since it only matches that session's own processes and a
// spawned supervisor needs it.
func orphanScanGuard(id string) error {
	if id == "" && scanRoot == "/proc" && TestSpawned() {
		return fmt.Errorf("proctable: refusing the orphan sweep's live /proc scan in a test-spawned binary (%s is set); "+
			"it would treat the host's real agent runtimes as orphans (see gastownhall/gascity#2839)", TestSpawnedEnv)
	}
	return nil
}
