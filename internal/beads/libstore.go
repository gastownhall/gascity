package beads

import "path/filepath"

// BeadsLibStore is a bd-backed Store scoped to a specific directory and bead
// ID prefix. It wraps BdStore with an env-isolated CommandRunner that sets
// BEADS_DIR and BEADS_DOLT_AUTO_START for all bd CLI invocations.
//
// Use NewBeadsLibStore to construct one. Call Shutdown when done to release
// any in-process state (the dolt server itself is managed externally by bd).
type BeadsLibStore struct { //nolint:revive // name intentional: disambiguates from BdStore and CachingStore in this package
	*BdStore
	dir string
}

// NewBeadsLibStore opens a BeadsLibStore rooted at dir. bd CLI calls will use
// BEADS_DIR=dir/.beads and BEADS_DOLT_AUTO_START=1 so they reach the dolt
// server started by "bd init --server" in that directory.
func NewBeadsLibStore(dir, idPrefix string) (*BeadsLibStore, error) {
	beadsDir := filepath.Join(dir, ".beads")
	runner := ExecCommandRunnerWithEnv(map[string]string{
		"BEADS_DIR":             beadsDir,
		"BEADS_DOLT_AUTO_START": "1",
	})
	bd := NewBdStoreWithPrefix(dir, runner, idPrefix)
	return &BeadsLibStore{BdStore: bd, dir: dir}, nil
}

// Shutdown releases any in-process state held by the store. The dolt server
// that backs bd is managed externally and is not stopped here.
func (s *BeadsLibStore) Shutdown() error { return nil }
