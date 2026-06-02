package proctable

import (
	"fmt"
	"testing"
)

// scanRoot is the procfs root enumerated by the Linux scanner. It defaults to
// the live "/proc". Tests override it with a fake procfs via
// SetScanRootForTesting so a `go test` run never enumerates — and the orphan
// sweep never reaps — the host's real agent processes.
var scanRoot = "/proc"

// liveScanRoot returns the procfs root to scan, or an error when a `go test`
// run would scan the live "/proc" without injecting a fake root first.
//
// Why this guard exists (gastownhall/gascity#2839): the process-table scanner
// reads the real /proc, and the orphan sweep SIGTERMs any runtime that is not
// present in its bead store. Under `go test` that store is empty/sandboxed, so
// on a host actually running gascity EVERY live agent (the mayor and every rig
// worker) looks like an orphan and gets killed. Dev laptops and CI runners have
// no live agents, so the orphan-cleanup tests pass there and the footgun only
// fires on a machine running a real fleet. Refusing the live scan under test
// closes that hole; a test that genuinely needs the scanner must inject a fake
// procfs via SetScanRootForTesting.
func liveScanRoot() (string, error) {
	if scanRoot == "/proc" && testing.Testing() {
		return "", fmt.Errorf("proctable: refusing to scan the live /proc under go test; " +
			"inject a fake procfs root with SetScanRootForTesting (guards against reaping real agent runtimes — see gastownhall/gascity#2839)")
	}
	return scanRoot, nil
}

// SetScanRootForTesting overrides the procfs root used by ScanBySessionID and
// IsScanRoot, returning a restore function. It exists only so tests can drive
// the scanner against a controlled, fake procfs tree instead of the host's.
func SetScanRootForTesting(root string) (restore func()) {
	prev := scanRoot
	scanRoot = root
	return func() { scanRoot = prev }
}
