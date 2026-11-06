//go:build integration

package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestRealNativeDoltStoreGuardedBatchConformance runs the guarded-batch
// contract against real upstream native storage, so the unit tier's double is
// not the only witness for the real batch applier: all or nothing, typed item
// refusals, row versions, and a closed no-history pinned create.
func TestRealNativeDoltStoreGuardedBatchConformance(t *testing.T) {
	beadstest.RunGuardedBatchConformance(t, "RealNativeDoltStore", beads.OpenRealNativeDoltStoreForGuardedBatch)
}
