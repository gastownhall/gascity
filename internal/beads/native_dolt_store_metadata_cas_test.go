package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestNativeDoltStoreMetadataCASConformance holds NativeDoltStore to the
// complete value-CAS contract. The in-memory native fixture serializes
// transaction callbacks so its contention behavior matches real Dolt.
func TestNativeDoltStoreMetadataCASConformance(t *testing.T) {
	beadstest.RunMetadataCASConformance(t, "NativeDoltStore",
		func(_ *testing.T) beads.Store { return beads.NewNativeDoltStoreForConformance() })
}

func TestNativeDoltStoreConditionalWriterConformance(t *testing.T) {
	beadstest.RunConditionalWriterConformanceWithOptions(t, "NativeDoltStore",
		func(_ *testing.T) beads.Store { return beads.NewNativeDoltStoreForConformance() },
		beadstest.ConditionalWriterOptions{
			RowBackedMutationFlavors: true,
			RestrictedUpdateFields:   true,
			SuppliesCurrent:          true,
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
