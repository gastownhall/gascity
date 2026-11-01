//go:build !linux && !darwin

package proctable

// snapshotProcesses is unavailable on platforms without process table access.
func snapshotProcesses() ([]ProcessRecord, error) {
	return nil, nil
}

// snapshotProcessTree is unavailable on platforms without process table access.
func snapshotProcessTree() ([]ProcessRecord, error) {
	return nil, nil
}
