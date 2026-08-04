package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestNativeDoltStoreMetadataCASConformance holds NativeDoltStore to the
// narrow value-CAS contract, including both traps the in-tree implementations
// historically diverged on.
//
// The contention leg is declared unevaluatable HERE and only here: the
// in-memory fixture behind this factory snapshots for rollback and then runs
// the transaction callback unlocked, so concurrent CAS calls interleave freely
// no matter how the store behaves. The property is not waived — it is proven
// against real Dolt by
// TestNativeDoltStoreMetadataCASContentionAgainstRealDolt (build tag
// `integration`), where 8 racers yield exactly one winner.
func TestNativeDoltStoreMetadataCASConformance(t *testing.T) {
	beadstest.RunMetadataCASConformanceWithOptions(t, "NativeDoltStore",
		func(_ *testing.T) beads.Store { return beads.NewNativeDoltStoreForConformance() },
		beadstest.MetadataCASOptions{
			FixtureLacksIsolationReason: "nativeDoltMemStorage.RunInTransaction models rollback but not " +
				"isolation (it unlocks before running the callback); contention is covered against real " +
				"Dolt by TestNativeDoltStoreMetadataCASContentionAgainstRealDolt (-tags=integration)",
		},
	)
}

// TestMemStoreMetadataCASConformance and TestFileStoreMetadataCASConformance
// run the SAME narrow suite against the two stores whose fixtures do provide
// isolation (both guard the whole CAS under their own lock), so the contention
// leg is genuinely exercised at unit level and the suite cannot rot into a
// table where every store has opted out of it.
func TestMemStoreMetadataCASConformance(t *testing.T) {
	beadstest.RunMetadataCASConformance(t, "MemStore",
		func(_ *testing.T) beads.Store { return beads.NewMemStore() })
}

func TestFileStoreMetadataCASConformance(t *testing.T) {
	beadstest.RunMetadataCASConformance(t, "FileStore",
		func(t *testing.T) beads.Store {
			store, err := beads.OpenFileStore(fsys.OSFS{}, t.TempDir()+"/beads.json")
			if err != nil {
				t.Fatalf("OpenFileStore: %v", err)
			}
			return store
		})
}
