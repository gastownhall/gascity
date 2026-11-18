//go:build darwin

package proctable

import "github.com/gastownhall/gascity/internal/runtime"

// ScanBySessionIDOutsideTmux is ScanBySessionID for a runtime whose agents
// never run under a tmux server: it never reports a process that is, or
// descends from, a tmux server or client carrying a GC_SESSION_ID in the
// same ps snapshot (scanRecordsOutsideTmux). See the Linux variant for why.
func ScanBySessionIDOutsideTmux(id string) ([]runtime.LiveRuntime, error) {
	if err := liveScanGuard(); err != nil {
		return []runtime.LiveRuntime{}, err
	}
	records, err := psRecords()
	if err != nil {
		return []runtime.LiveRuntime{}, err
	}
	roots, fenceErr := scanRecordsOutsideTmux(records, id)
	return withStartIdentities(roots), fenceErr
}
