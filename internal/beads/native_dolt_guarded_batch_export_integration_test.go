//go:build integration

package beads

import "testing"

// OpenRealNativeDoltStoreForGuardedBatch opens a NativeDoltStore over real
// upstream native storage, for running beadstest.RunGuardedBatchConformance
// against the real batch applier.
func OpenRealNativeDoltStoreForGuardedBatch(t *testing.T) Store {
	t.Helper()
	return openRealNativeDoltStoreForFacade(t, "guarded-batch-test")
}
