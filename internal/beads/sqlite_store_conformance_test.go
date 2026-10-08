package beads_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// newSQLiteForConformance returns a fresh, empty SQLite store with cleanup
// registered.
func newSQLiteForConformance(t *testing.T) *beads.SQLiteStore {
	t.Helper()
	s, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := s.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

// TestSQLiteStoreConformance runs the tree's shared beads.Store conformance
// suite — beadstest, the canonical kit — against the SQLite store, the same
// suite MemStore/FileStore/NativeDoltStore pass.
func TestSQLiteStoreConformance(t *testing.T) {
	factory := func() beads.Store { return newSQLiteForConformance(t) }
	beadstest.RunStoreTests(t, factory)
	beadstest.RunSequentialIDTests(t, factory)
	beadstest.RunCreationOrderTests(t, factory)
	beadstest.RunDepTests(t, factory)
	beadstest.RunMetadataTests(t, factory)
	beadstest.RunCloseReasonTests(t, factory)
	beadstest.RunCloseReasonAfterReopenTests(t, factory)
}

// TestSQLiteStoreReadyParityConformance proves a primed CachingStore over the
// store serves the same ready rows, in the same order, as the store's own
// Ready. A class binding's cache is the census the controller's demand reads
// are answered from, so a divergence here offers blocked work or cuts a
// different Limit prefix on every split city.
func TestSQLiteStoreReadyParityConformance(t *testing.T) {
	beadstest.RunReadyParityConformance(t, "SQLiteStore", beadstest.ReadyParityHarness{
		Open:   func(t *testing.T) beads.Store { return newSQLiteForConformance(t) },
		Rescan: (*beads.CachingStore).ReconcileForTest,
	})
}

// TestSQLiteStoreConditionalWriterConformance runs the shared fenced-write
// suite against the embedded store, raw and under the CachingStore the
// controller puts over a binding engine. Without the capability the graph
// plane's control epochs, drain reservations, and attach fences silently
// degrade to unconditional writes on a routed city. The row-backed leg proves
// every whole-row write flavor mints a fresh revision, which is what makes the
// fence honest once the engine is stamped. The cache elides a write that
// changes nothing, so it leaves that leg off, as every cache row does.
func TestSQLiteStoreConditionalWriterConformance(t *testing.T) {
	beadstest.RunConditionalWriterConformanceWithOptions(t, "SQLiteStore", func(t *testing.T) beads.Store {
		return newSQLiteForConformance(t)
	}, beadstest.ConditionalWriterOptions{RowBackedMutationFlavors: true, SuppliesCurrent: true})
	beadstest.RunConditionalWriterConformanceWithOptions(t, "CachingStore/SQLiteStore", func(t *testing.T) beads.Store {
		cache := beads.NewCachingStoreForTest(newSQLiteForConformance(t), nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		return cache
	}, beadstest.ConditionalWriterOptions{SuppliesCurrent: true})
}

// TestSQLiteStoreAtomicCloserConformance runs the shared atomic terminal-close
// suite against the embedded store. Without the capability a session close on
// a sqlite-routed city stamps its terminal metadata and closes the row as two
// writes, and a writer landing between them strands a closed row that still
// looks live.
func TestSQLiteStoreAtomicCloserConformance(t *testing.T) {
	beadstest.RunAtomicConditionalCloserConformance(t, "SQLiteStore", func(t *testing.T) beads.Store {
		return newSQLiteForConformance(t)
	})
}

// TestSQLiteStoreFenceConformance proves the SQLite constructor persists
// ownership generations instead of exposing a vacuous zero fence.
func TestSQLiteStoreFenceConformance(t *testing.T) {
	beadstest.RunFenceConformance(t, func() beads.Store {
		return newSQLiteForConformance(t)
	})
}

// TestSQLiteStorePinnedIDFenceConformance runs the shared fenced-Create suite.
// It is the same contract every store serving a class binding owes, and this
// store is the one the shipped bindings open, so a divergence here is a
// divergence in production.
func TestSQLiteStorePinnedIDFenceConformance(t *testing.T) {
	beadstest.RunPinnedIDFenceConformance(t, func(t *testing.T, mintPrefix string, namespaces ...string) beads.Store {
		t.Helper()
		opened, err := beads.OpenSQLiteStore(t.TempDir(),
			beads.WithSQLiteStoreIDPrefix(mintPrefix),
			beads.WithSQLiteStoreReservedIDPrefixes(namespaces...),
		)
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		store := opened.(*beads.SQLiteStore)
		t.Cleanup(func() { _ = store.CloseStore() })
		return store
	})
}

// TestSQLiteStoreFenceRefusalNamesTheIDAndTheNamespaces covers what the shared
// suite deliberately will not: the prose.
//
// A provider contract can only demand a sentinel, so the conformance rows match
// on beads.ErrPinnedIDOutsideNamespace and say nothing about wording. But the
// operator reading this refusal in a log has to be able to act on it, and that
// takes both halves — which id was refused, and which namespaces this binding
// would have accepted. Neither is inferable from the sentinel.
func TestSQLiteStoreFenceRefusalNamesTheIDAndTheNamespaces(t *testing.T) {
	opened, err := beads.OpenSQLiteStore(t.TempDir(),
		beads.WithSQLiteStoreIDPrefix("gcn"),
		beads.WithSQLiteStoreReservedIDPrefixes("gcn", "gcnq"),
	)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })

	_, err = store.Create(beads.Bead{ID: "ga-42", Title: "a work id pinned into the nudges binding"})
	if err == nil {
		t.Fatal("Create accepted a foreign pinned id")
	}
	for _, want := range []string{"ga-42", "gcn", "gcnq"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q; an operator cannot tell from it what to route where", err, want)
		}
	}
}

// TestSQLiteStoreDeleteBatch pins the BatchDeleter contract the wisp-GC
// closure purge relies on: batched removal, edges to external dependents
// dropped while the dependents themselves survive (orphaned, never
// rewritten), tolerance of already-gone ids, and chunking past the
// bound-parameter limit.
func TestSQLiteStoreDeleteBatch(t *testing.T) {
	st := newSQLiteForConformance(t)
	var _ beads.BatchDeleter = st

	a, err := st.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.Create(beads.Bead{Title: "step", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	ext, err := st.Create(beads.Bead{Title: "external dependent", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DepAdd(b.ID, a.ID, "parent-child"); err != nil {
		t.Fatal(err)
	}
	if err := st.DepAdd(ext.ID, a.ID, "blocks"); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteBatch([]string{a.ID, b.ID}); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if _, err := st.Get(id); err == nil {
			t.Fatalf("%s survived the batch delete", id)
		}
	}
	if _, err := st.Get(ext.ID); err != nil {
		t.Fatalf("external dependent was deleted, want orphaned: %v", err)
	}
	deps, err := st.DepList(ext.ID, "down")
	if err != nil || len(deps) != 0 {
		t.Fatalf("external dependent's edge not scrubbed: (%+v, %v)", deps, err)
	}

	// Idempotent over missing ids, and nil is a no-op.
	if err := st.DeleteBatch([]string{"gcg-nope"}); err != nil {
		t.Fatalf("DeleteBatch(missing): %v", err)
	}
	if err := st.DeleteBatch(nil); err != nil {
		t.Fatalf("DeleteBatch(nil): %v", err)
	}

	// Chunking: more ids than one statement carries.
	ids := make([]string, 0, 600)
	for i := 0; i < 600; i++ {
		created, err := st.Create(beads.Bead{Title: "bulk", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, created.ID)
	}
	if err := st.DeleteBatch(ids); err != nil {
		t.Fatalf("DeleteBatch(600): %v", err)
	}
	if _, err := st.Get(ids[len(ids)-1]); err == nil {
		t.Fatal("chunked batch left rows behind")
	}
}
