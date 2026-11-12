//go:build darwin

package proctable

import "github.com/gastownhall/gascity/internal/runtime"

// ScanBySessionIDOutsideTmux is ScanBySessionID for a runtime whose sessions
// never run under a tmux server: it never reports a process that has a tmux
// server or client among its ancestors in the same ps snapshot
// (scanRecordsOutsideTmux). See the Linux variant for why.
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

// FenceTmuxSubtrees keeps the roots of found that have no tmux server or
// client among their ancestors in a fresh ps snapshot. See the Linux
// variant.
func FenceTmuxSubtrees(found []runtime.LiveRuntime) ([]runtime.LiveRuntime, error) {
	if len(found) == 0 {
		return []runtime.LiveRuntime{}, nil
	}
	if err := liveScanGuard(); err != nil {
		return []runtime.LiveRuntime{}, err
	}
	records, err := psRecords()
	if err != nil {
		return []runtime.LiveRuntime{}, err
	}
	return fenceTmuxSubtreeRecords(records, found)
}
