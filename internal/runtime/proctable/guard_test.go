//go:build linux

package proctable

import "testing"

// TestScanBySessionID_RefusesLiveProcUnderTest is the regression guard for
// gastownhall/gascity#2839. Under `go test` the scanner must NOT enumerate the
// live /proc: the orphan sweep SIGTERMs any runtime missing from its (empty,
// under-test) bead store, so on a host running a live fleet every real agent —
// the mayor and every rig worker — would be reaped. Dev laptops and CI runners
// have no live agents, so the orphan-cleanup tests pass there and the footgun
// only ever fired on a machine actually running gascity. This test fails if the
// guard regresses.
func TestScanBySessionID_RefusesLiveProcUnderTest(t *testing.T) {
	if _, err := ScanBySessionID(""); err == nil {
		t.Fatal("ScanBySessionID(\"\") enumerated the live /proc under `go test`; the safety guard did not fire (regression of #2839)")
	}
}

func TestIsScanRoot_RefusesLiveProcUnderTest(t *testing.T) {
	if IsScanRoot(2) {
		t.Fatal("IsScanRoot consulted the live /proc under `go test`; expected the safety guard to short-circuit to false")
	}
}

// TestSetScanRootForTesting_InjectsFakeRoot proves the escape hatch: a test that
// genuinely needs the scanner injects a fake procfs root, which disables the
// live-/proc refusal and confines the scan to the fake tree.
func TestSetScanRootForTesting_InjectsFakeRoot(t *testing.T) {
	restore := SetScanRootForTesting(t.TempDir())
	defer restore()

	got, err := ScanBySessionID("")
	if err != nil {
		t.Fatalf("with an injected fake procfs root the scan must not be refused: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty fake procfs must yield zero runtimes, got %d", len(got))
	}
}
