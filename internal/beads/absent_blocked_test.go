package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// noListingStore stands in for a bd/Dolt engine: a store with no
// absent-blocker listing, whose Ready never strands a dependent.
type noListingStore struct{ beads.Store }

// TestReadyBlockedByAbsentForAnswersForTheWrappedStore pins the capability as
// an answer, not a method set (#8360 review, Finding 1). A cache forwards the
// listing whatever it wraps, so a reader that asked only whether the methods
// exist took every cached bd leg for a stranding store, and trusted each row
// that leg served as already judged.
func TestReadyBlockedByAbsentForAnswersForTheWrappedStore(t *testing.T) {
	sqlite, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := sqlite.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore()
		}
	})
	for _, tc := range []struct {
		name  string
		store beads.Store
		want  bool
	}{
		{"the in-memory engine", beads.NewMemStore(), true},
		{"the SQLite engine", sqlite, true},
		{"a cache over the in-memory engine", beads.NewCachingStoreForTest(beads.NewMemStore(), nil), true},
		{"a cache over the SQLite engine", beads.NewCachingStoreForTest(sqlite, nil), true},
		{"a store with no listing", noListingStore{beads.NewMemStore()}, false},
		{"a cache over a store with no listing", beads.NewCachingStoreForTest(noListingStore{beads.NewMemStore()}, nil), false},
		{"a cache over a cache over a store with no listing", beads.NewCachingStoreForTest(beads.NewCachingStoreForTest(noListingStore{beads.NewMemStore()}, nil), nil), false},
	} {
		reader, got := beads.ReadyBlockedByAbsentFor(tc.store)
		if got != tc.want {
			t.Errorf("%s: ReadyBlockedByAbsentFor = %v, want %v", tc.name, got, tc.want)
		}
		if got != (reader != nil) {
			t.Errorf("%s: ReadyBlockedByAbsentFor reported %v beside reader %v", tc.name, got, reader)
		}
	}
}
