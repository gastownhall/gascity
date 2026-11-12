//go:build darwin

package proctable

import "github.com/gastownhall/gascity/internal/runtime"

// ScanBySessionIDOutsideTmux is ScanBySessionID for a runtime whose sessions
// never run under a tmux server: it never reports a process that has a tmux
// server or client among its ancestors in the same ps snapshot
// (fenceTmuxSubtreeRecords). See the Linux variant for why.
func ScanBySessionIDOutsideTmux(id string) ([]runtime.LiveRuntime, error) {
	if err := liveScanGuard(); err != nil {
		return []runtime.LiveRuntime{}, err
	}
	records, err := psRecords()
	if err != nil {
		return []runtime.LiveRuntime{}, err
	}
	roots := fenceTmuxSubtreeRecords(records, scanRecordsBySessionID(records, id))
	if id == "" {
		roots = orphanCandidates(roots)
	}
	return withStartIdentities(roots), nil
}
