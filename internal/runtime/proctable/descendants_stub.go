//go:build !linux && !darwin && !windows

package proctable

import "fmt"

// snapshotProcesses is unavailable on platforms without process table access.
//
// It returns an error rather than an empty snapshot because every caller reads
// absence as a fact about the host: DescendantAlive over no records reports the
// agent dead, and an identity-fenced teardown over no records finds nothing to
// fence. An empty slice here is indistinguishable from "the machine is idle".
func snapshotProcesses() ([]ProcessRecord, error) {
	return nil, fmt.Errorf("proctable: process table snapshots are unavailable: %w", ErrUnsupported)
}
