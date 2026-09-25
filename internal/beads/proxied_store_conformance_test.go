package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// proxiedConformancePrefix is the id namespace the conformance fixture mints
// under. It is a work-shaped prefix on purpose: a reserved class prefix would
// make splittest's constructors demand a namespace fence, and the proxied store
// is not a class binding.
const proxiedConformancePrefix = "prx"

func newProxiedConformanceStore() beads.Store {
	return beads.NewProxiedStoreForConformance(proxiedConformancePrefix)
}

// TestProxiedStoreConformance runs the shared Store suite against the SPLIT
// store.
//
// The suite is the only instrument that answers the question this wrapper's whole
// design turns on: "does routing reads to one leaf and writes to another still
// produce a Store?" Every row writes through the bd leaf and reads back through
// the native one, so a routing rule that sent a read to the wrong place — or a
// write to the leaf the fixture latched READ-ONLY, exactly as the proxied opener
// latches it — fails here rather than in a city.
func TestProxiedStoreConformance(t *testing.T) {
	beadstest.RunStoreTests(t, newProxiedConformanceStore)
}

// TestProxiedStoreMetadataConformance covers the metadata surface, whose writes
// and reads land on different leaves for every single row.
func TestProxiedStoreMetadataConformance(t *testing.T) {
	beadstest.RunMetadataTests(t, newProxiedConformanceStore)
}

// TestProxiedStoreSurvivesTheStrictLeafChecks puts the split store under
// splittest's strict leaf, which is where strictness earns its keep here:
// StrictStore.DepAdd resolves BOTH endpoints in this store before delegating, so
// an edge whose endpoints the two leaves disagree about is caught at the call
// rather than becoming a dangling row that silently drops its dependent out of
// Ready. It also exercises the capability forwarding, because the constructor
// reads the leaf's IDPrefix and the wrapper's Count/DepMetadata/AtomicTx/
// GraphApplyHandle all have to answer through it.
//
// StrictWithPrefix rather than Strict, because the residence checks have to be
// ABOUT a namespace the fixture chose rather than one the store happens to
// report; BdSemantics, because the leaf these writes reach in production is bd.
//
// This is written out rather than run as beadstest.RunStoreTests/RunDepTests
// through the strict wrapper, because BOTH of those combinations fail inside the
// kit for reasons that have nothing to do with this store and reproduce
// identically against a plain beads.NewNativeDoltStoreForConformance leaf
// (verified in this session):
//
//   - splittest.StrictStore.Tx replaces the caller's callback with a closure of
//     its own, so a leaf's nil-callback guard cannot fire and RunStoreTests'
//     TxRejectsNilCallback row panics;
//   - RunDepTests pins bare ids ("a", "b") outside any store's mint prefix, which
//     the strict leaf's endpoint resolution refuses by design.
func TestProxiedStoreSurvivesTheStrictLeafChecks(t *testing.T) {
	store := splittest.StrictWithPrefix(t, newProxiedConformanceStore(),
		proxiedConformancePrefix, splittest.BdSemantics)

	from, err := store.Create(beads.Bead{Title: "blocked step", Type: "task"})
	if err != nil {
		t.Fatalf("Create through the strict leaf: %v", err)
	}
	to, err := store.Create(beads.Bead{Title: "blocking step", Type: "task"})
	if err != nil {
		t.Fatalf("Create through the strict leaf: %v", err)
	}
	// Both endpoints resolve through the NATIVE read leaf while the rows were
	// written through the bd one: an edge only lands if the split agrees about
	// what exists.
	if err := store.DepAdd(from.ID, to.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd across the split: %v", err)
	}
	deps, err := store.DepList(from.ID, "down")
	if err != nil {
		t.Fatalf("DepList across the split: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != to.ID {
		t.Fatalf("DepList = %#v, want one edge to %s", deps, to.ID)
	}
	if err := store.DepRemove(from.ID, to.ID); err != nil {
		t.Fatalf("DepRemove across the split: %v", err)
	}
	if deps, err = store.DepList(from.ID, "down"); err != nil || len(deps) != 0 {
		t.Fatalf("DepList after remove = %#v (err %v), want none", deps, err)
	}
}
