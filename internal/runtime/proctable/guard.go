package proctable

// scanRoot is the procfs root enumerated by the Linux scanner. It defaults to
// the live "/proc". Tests override it with a fake procfs via
// SetScanRootForTesting so a `go test` run never enumerates — and the orphan
// sweep never reaps — the host's real agent processes.
var scanRoot = "/proc"

// SetScanRootForTesting overrides the procfs root used by ScanBySessionID and
// IsScanRoot, returning a restore function. It exists only so tests can drive
// the scanner against a controlled, fake procfs tree instead of the host's.
func SetScanRootForTesting(root string) (restore func()) {
	prev := scanRoot
	scanRoot = root
	return func() { scanRoot = prev }
}
