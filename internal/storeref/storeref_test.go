package storeref

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// prefixedStore wraps a MemStore and reports an IDPrefix(), standing in for the
// production city/rig stores (BdStore/CachingStore) whose IDPrefix() PrefixOwner
// routes on. The single-store upstream has no SQLiteStore, so the test fixture
// supplies the prefix the store would be configured with rather than minting it
// from a backend.
type prefixedStore struct {
	beads.Store
	prefix string
}

// IDPrefix returns the configured prefix this fixture store owns.
func (p prefixedStore) IDPrefix() string { return p.prefix }

func newPrefixed(prefix string) prefixedStore {
	return prefixedStore{Store: beads.NewMemStore(), prefix: prefix}
}

// hardErrStore reports an IDPrefix but fails every Get with a non-ErrNotFound
// error, standing in for an unavailable backend (I/O or connection failure) as
// opposed to a clean miss.
type hardErrStore struct {
	beads.Store
	prefix string
	err    error
}

func (h hardErrStore) IDPrefix() string               { return h.prefix }
func (h hardErrStore) Get(string) (beads.Bead, error) { return beads.Bead{}, h.err }

func TestPrefixOwner(t *testing.T) {
	graph := newPrefixed("gcg")
	orders := newPrefixed("gco")
	work := beads.NewMemStore() // no IDPrefix() — stands in for the bd work store
	stores := []beads.Store{work, graph, orders}

	cases := []struct {
		id   string
		want beads.Store
	}{
		{"gcg-1", graph},
		{"gco-42", orders},
		{"gc-7", nil}, // no store claims the work prefix here
		{"zz-9", nil}, // unknown prefix
		{"gcg", nil},  // missing the namespace separator
		{"orphan", nil},
	}
	for _, tc := range cases {
		if got := PrefixOwner(tc.id, stores); got != tc.want {
			t.Errorf("PrefixOwner(%q) = %#v, want %#v", tc.id, got, tc.want)
		}
	}

	// nil stores in the slice are skipped, not panicked on.
	if got := PrefixOwner("gcg-1", []beads.Store{nil, graph}); got != graph {
		t.Errorf("PrefixOwner with a leading nil store = %#v, want graph", got)
	}
}

func TestResolve_FederationFallback(t *testing.T) {
	graph := newPrefixed("gcg")
	work := beads.NewMemStore()

	gb, err := graph.Create(beads.Bead{Title: "graph node"})
	if err != nil {
		t.Fatalf("seed graph: %v", err)
	}
	wb, err := work.Create(beads.Bead{Title: "work item"})
	if err != nil {
		t.Fatalf("seed work: %v", err)
	}
	stores := []beads.Store{work, graph}

	// Prefix-routed read.
	if got, err := Resolve(gb.ID, stores); err != nil || got.ID != gb.ID {
		t.Fatalf("Resolve(%q) = (%+v, %v), want the graph bead", gb.ID, got, err)
	}
	// Probe fallback: the work store has no IDPrefix, so its bead is found by probe.
	if got, err := Resolve(wb.ID, stores); err != nil || got.ID != wb.ID {
		t.Fatalf("Resolve(%q) = (%+v, %v), want the work bead via probe fallback", wb.ID, got, err)
	}
	// Absent everywhere.
	if _, err := Resolve("gcg-does-not-exist", stores); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Resolve(absent) err = %v, want ErrNotFound", err)
	}
}

// TestResolvePreservesHardError pins the federation error semantics: a hard
// read failure from any probed store must surface to the caller rather than
// being masked as a clean ErrNotFound by a later store's miss.
func TestResolvePreservesHardError(t *testing.T) {
	boom := errors.New("owner store unavailable")

	t.Run("owner hard error wins over a fallback not-found", func(t *testing.T) {
		owner := hardErrStore{Store: beads.NewMemStore(), prefix: "gcg", err: boom}
		fallback := beads.NewMemStore() // clean miss on every id
		_, err := Resolve("gcg-1", []beads.Store{owner, fallback})
		if !errors.Is(err, boom) {
			t.Fatalf("Resolve err = %v, want owner hard error %v", err, boom)
		}
		if errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("Resolve err = %v, must not be masked as ErrNotFound", err)
		}
	})

	t.Run("first hard error survives a preceding not-found in the probe scan", func(t *testing.T) {
		// No prefix owner for this id, so resolution probes the full list in
		// order: a clean-miss store first, then an unavailable store.
		missing := beads.NewMemStore()
		broken := hardErrStore{Store: beads.NewMemStore(), prefix: "", err: boom}
		_, err := Resolve("orphan-1", []beads.Store{missing, broken})
		if !errors.Is(err, boom) {
			t.Fatalf("Resolve err = %v, want hard error %v", err, boom)
		}
	})
}

func TestResolveSkipsNilStores(t *testing.T) {
	owner := beads.NewMemStore()
	seeded, err := owner.Create(beads.Bead{Title: "seed", Status: "open"})
	if err != nil {
		t.Fatalf("seeding bead: %v", err)
	}
	got, err := Resolve(seeded.ID, []beads.Store{nil, owner, nil})
	if err != nil {
		t.Fatalf("Resolve error = %v, want nil", err)
	}
	if got.ID != seeded.ID {
		t.Fatalf("Resolve bead = %q, want %q", got.ID, seeded.ID)
	}
}

func TestResolveEmptyStoreListIsNotFound(t *testing.T) {
	if _, err := Resolve("gcg-1", nil); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Resolve(nil) error = %v, want ErrNotFound", err)
	}
}
