package splittest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// graphWispID is a production-shaped wisp id: the graph class's reserved
// prefix, the wisp segment, then an opaque suffix. Molecule steps run as beads
// with exactly this shape.
const graphWispID = "gcg-wisp-y785sz"

func mustCreate(t *testing.T, s beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := s.Create(b)
	if err != nil {
		t.Fatalf("create %+v: %v", b, err)
	}
	return created
}

// lenientSplitPair builds the double this kit exists to replace: two plain
// MemStores standing in for a work store and a graph store. They are
// prefix-labeled, but nothing enforces the labels.
func lenientSplitPair() (work, graph beads.Store) {
	w := beads.NewMemStore()
	g := beads.NewMemStore()
	g.IDPrefix = "gcg"
	return w, g
}

// TestLenientMemStoreDoublesHideProductionFailures is the red-before half of
// this kit's case, run against the doubles it replaces. Both sub-cases are
// operations the bd/Dolt and SQLite backends REJECT; both succeed here. A test
// written on these doubles passes while the path it models hard-fails in
// production, which is how a split-store landmine ships green.
//
// TestStrictStoreCatchesWhatLenientDoublesLetThrough runs the identical
// operations against the strict pair and requires each to fail.
func TestLenientMemStoreDoublesHideProductionFailures(t *testing.T) {
	t.Parallel()

	t.Run("cross-store dep is silently accepted", func(t *testing.T) {
		t.Parallel()
		work, graph := lenientSplitPair()
		workBead := mustCreate(t, work, beads.Bead{Title: "work"})
		graphBead := mustCreate(t, graph, beads.Bead{Title: "graph"})

		if err := graph.DepAdd(graphBead.ID, workBead.ID, "blocks"); err != nil {
			t.Fatalf("lenient double rejected the cross-store dep: %v", err)
		}
		deps, err := graph.DepList(graphBead.ID, "down")
		if err != nil {
			t.Fatalf("dep list: %v", err)
		}
		if len(deps) != 1 {
			t.Fatalf("lenient double recorded %d deps, want the cross-store edge recorded", len(deps))
		}
		if _, err := graph.Get(workBead.ID); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("get %q from the graph store = %v, want ErrNotFound (the edge points at a bead that is not there)", workBead.ID, err)
		}
	})

	t.Run("foreign-prefix create is silently accepted and renamed", func(t *testing.T) {
		t.Parallel()
		work, _ := lenientSplitPair()

		created, err := work.Create(beads.Bead{ID: graphWispID, Title: "graph wisp in the work store"})
		if err != nil {
			t.Fatalf("lenient double rejected the foreign-prefix create: %v", err)
		}
		if created.ID == graphWispID {
			t.Fatalf("create returned id %q; this double is expected to clobber the pinned id", created.ID)
		}
		if _, err := work.Get(graphWispID); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("get %q = %v, want ErrNotFound (the id the caller asked for was never stored)", graphWispID, err)
		}
	})
}

// TestStrictStoreCatchesWhatLenientDoublesLetThrough runs the exact operations
// TestLenientMemStoreDoublesHideProductionFailures shows sailing through a
// plain MemStore pair, and requires each to fail the way production fails.
func TestStrictStoreCatchesWhatLenientDoublesLetThrough(t *testing.T) {
	t.Parallel()

	t.Run("cross-store dep is rejected with a bd-shaped error", func(t *testing.T) {
		t.Parallel()
		work, graph := NewSplitStores(t)
		workBead := mustCreate(t, work, beads.Bead{Title: "work"})
		graphBead := mustCreate(t, graph, beads.Bead{Title: "graph"})

		err := graph.DepAdd(graphBead.ID, workBead.ID, "blocks")
		if err == nil {
			t.Fatal("cross-store DepAdd succeeded; the strict store must reject an endpoint it cannot resolve")
		}
		// bd's own wording, so a test cannot pass on a classification
		// production could never satisfy.
		for _, want := range []string{"adding dep", "resolving issue ID", "no issue found", workBead.ID} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q; it must be shaped like bd's failure", err, want)
			}
		}
		if errors.Is(err, beads.ErrNotFound) {
			t.Error("cross-store DepAdd error wraps beads.ErrNotFound; bd reports a subprocess stderr string that callers can only classify textually, so a typed error would let in-process tests pass on an errors.Is check production cannot satisfy")
		}
		deps, err := graph.DepList(graphBead.ID, "down")
		if err != nil {
			t.Fatalf("dep list: %v", err)
		}
		if len(deps) != 0 {
			t.Fatalf("rejected DepAdd still recorded %d deps; the reject must happen before the leaf write", len(deps))
		}
	})

	t.Run("foreign-prefix create is rejected", func(t *testing.T) {
		t.Parallel()
		work, _ := NewSplitStores(t)

		_, err := work.Create(beads.Bead{ID: graphWispID, Title: "graph wisp in the work store"})
		if err == nil {
			t.Fatal("foreign-prefix create succeeded; the strict work store must reject an id outside its namespace")
		}
		for _, want := range []string{graphWispID, "does not match store id prefix", `"gc"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})
}

// TestStrictStoreDepAddAcceptsSameStoreEdges pins the other half of the DepAdd
// contract: a guard that rejects everything is not a guard, it is a broken
// store. Every edge whose endpoints both live here must still land.
func TestStrictStoreDepAddAcceptsSameStoreEdges(t *testing.T) {
	t.Parallel()
	_, graph := NewSplitStores(t)
	parent := mustCreate(t, graph, beads.Bead{Title: "parent"})
	child := mustCreate(t, graph, beads.Bead{Title: "child"})

	if err := graph.DepAdd(child.ID, parent.ID, "blocks"); err != nil {
		t.Fatalf("same-store DepAdd rejected: %v", err)
	}
	deps, err := graph.DepList(child.ID, "down")
	if err != nil {
		t.Fatalf("dep list: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != parent.ID {
		t.Fatalf("deps = %+v, want one edge to %q", deps, parent.ID)
	}
}

// TestStrictStoreDepAddPreservesParentChildShortCircuit pins the one case
// beads.BdStore.DepAdd answers without touching the backend: a parent-child dep
// that merely restates the bead's own ParentID. On a split store the parent may
// legitimately live in another store, and bd never sees the call — so neither
// may the endpoint guard.
func TestStrictStoreDepAddPreservesParentChildShortCircuit(t *testing.T) {
	t.Parallel()
	work, graph := NewSplitStores(t)
	workParent := mustCreate(t, work, beads.Bead{Title: "work parent"})
	child := mustCreate(t, graph, beads.Bead{Title: "graph child", ParentID: workParent.ID})

	if err := graph.DepAdd(child.ID, workParent.ID, "parent-child"); err != nil {
		t.Fatalf("parent-child restatement rejected: %v", err)
	}
	// A parent-child dep naming a DIFFERENT bead is a real edge and still
	// resolves both endpoints.
	if err := graph.DepAdd(child.ID, "gcg-absent", "parent-child"); err == nil {
		t.Fatal("parent-child dep to an unrelated missing id succeeded; only a restatement of the bead's own ParentID short-circuits")
	}
}

// TestStrictStoreHandlesWispTierIDs pins the tier the live incidents happened
// in. Production molecules materialize as ephemeral wisps carrying pinned
// <prefix>-wisp-<suffix> ids, so the kit must round-trip such an id, read it
// back through a wisp-tier query, and let it carry dependency edges.
func TestStrictStoreHandlesWispTierIDs(t *testing.T) {
	t.Parallel()
	work, graph := NewSplitStores(t)

	wisp, err := graph.Create(beads.Bead{ID: graphWispID, Title: "molecule step", Ephemeral: true})
	if err != nil {
		t.Fatalf("create wisp %q: %v", graphWispID, err)
	}
	if wisp.ID != graphWispID {
		t.Fatalf("wisp id = %q, want the pinned %q round-tripped", wisp.ID, graphWispID)
	}
	got, err := graph.Get(graphWispID)
	if err != nil {
		t.Fatalf("get wisp: %v", err)
	}
	if !got.Ephemeral {
		t.Error("wisp lost its Ephemeral flag through the strict wrapper")
	}

	// Tier-transparent, not tier-expanding: an issues-tier read must NOT see
	// the wisp, and a wisp-tier read must.
	issues, err := graph.List(beads.ListQuery{Type: "task", TierMode: beads.TierIssues})
	if err != nil {
		t.Fatalf("issues-tier list: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues-tier list returned %d beads, want 0; the wrapper must not expand the caller's tier", len(issues))
	}
	for _, mode := range []beads.TierMode{beads.TierWisps, beads.TierBoth} {
		found, err := graph.List(beads.ListQuery{Type: "task", TierMode: mode})
		if err != nil {
			t.Fatalf("list tier %v: %v", mode, err)
		}
		if len(found) != 1 || found[0].ID != graphWispID {
			t.Fatalf("list tier %v = %+v, want just %q", mode, found, graphWispID)
		}
	}

	// A wisp is a first-class dep endpoint within its own store...
	root := mustCreate(t, graph, beads.Bead{Title: "molecule root"})
	if err := graph.DepAdd(graphWispID, root.ID, "blocks"); err != nil {
		t.Fatalf("same-store wisp dep rejected: %v", err)
	}
	// ...and no more resolvable from the work store than any other graph bead.
	workBead := mustCreate(t, work, beads.Bead{Title: "work"})
	if err := work.DepAdd(workBead.ID, graphWispID, "blocks"); err == nil {
		t.Fatal("work store accepted a dep on a graph-resident wisp; the wisp tier is not an exception to the residence invariant")
	}
	// The work store must refuse to MINT the wisp id too — the other half of
	// the same invariant.
	if _, err := work.Create(beads.Bead{ID: graphWispID, Title: "wisp in the wrong store", Ephemeral: true}); err == nil {
		t.Fatal("work store created a graph-prefixed wisp")
	}
}

// TestStrictStoreCreateAcceptsInPrefixExplicitIDs pins that the create guard is
// about the NAMESPACE, not about explicit ids: bd accepts an in-prefix --id, so
// the kit must too, or fixtures cannot pin the stable ids production pins.
func TestStrictStoreCreateAcceptsInPrefixExplicitIDs(t *testing.T) {
	t.Parallel()
	work, graph := NewSplitStores(t)

	for _, tc := range []struct {
		name  string
		store beads.Store
		id    string
	}{
		{"work in-prefix", work, "gc-pinned"},
		{"graph in-prefix", graph, "gcg-pinned"},
		{"graph uppercase in-prefix", graph, "GCG-shouty"},
	} {
		created, err := tc.store.Create(beads.Bead{ID: tc.id, Title: tc.name})
		if err != nil {
			t.Errorf("%s: create %q: %v", tc.name, tc.id, err)
			continue
		}
		if created.ID != tc.id {
			t.Errorf("%s: created id = %q, want %q round-tripped", tc.name, created.ID, tc.id)
		}
	}
}

// TestStrictStoreCreateRejectsAClobberingLeaf pins the post-check. A leaf that
// silently renames a pinned id cannot model a real store, and a kit that let it
// through would reintroduce the exact leniency it exists to remove — so wrapping
// one is an error at the first pinned create, not a mystery later.
func TestStrictStoreCreateRejectsAClobberingLeaf(t *testing.T) {
	t.Parallel()
	leaf := beads.NewMemStore()
	leaf.IDPrefix = "gcg" // mints gcg-<n>, but clobbers pinned ids
	strict := StrictWithPrefix(leaf, "gcg")

	_, err := strict.Create(beads.Bead{ID: graphWispID, Title: "wisp"})
	if err == nil {
		t.Fatal("create with a clobbering leaf succeeded; the pinned id was silently replaced")
	}
	for _, want := range []string{graphWispID, "clobbers pinned ids"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// TestStrictStoreCreateRejectsAWrongPrefixLeaf pins the other post-check: a leaf
// minting outside the namespace the wrapper declares puts a foreign-prefix row
// inside a split store, which is the residence-invariant violation itself.
func TestStrictStoreCreateRejectsAWrongPrefixLeaf(t *testing.T) {
	t.Parallel()
	strict := StrictWithPrefix(beads.NewMemStore(), "gcg") // leaf mints gc-<n>

	_, err := strict.Create(beads.Bead{Title: "store-minted"})
	if err == nil {
		t.Fatal("store-minted create succeeded on a leaf minting outside the declared namespace")
	}
	if !strings.Contains(err.Error(), "outside its declared id namespace") {
		t.Errorf("error %q does not name the namespace violation", err)
	}
}

// TestStrictStoreTxCreateIsGuarded pins that a transaction is not a side door:
// the same foreign-prefix create the direct path rejects must be rejected
// inside Tx, or every guard is one refactor away from being bypassed.
func TestStrictStoreTxCreateIsGuarded(t *testing.T) {
	t.Parallel()
	work, _ := NewSplitStores(t)

	var inner error
	if err := work.Tx("guarded", func(tx beads.Tx) error {
		_, inner = tx.Create(beads.Bead{ID: graphWispID, Title: "graph wisp via tx"})
		return nil
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}
	if inner == nil {
		t.Fatal("Tx.Create accepted a foreign-prefix id that Create rejects")
	}
	if !strings.Contains(inner.Error(), "does not match store id prefix") {
		t.Errorf("error %q is not the create-guard rejection", inner)
	}
}

// TestStrictStoreWriterHandleKeepsTheGuards pins that a caller who discovers
// the write surface through beads.HandlesFor — as production write paths do —
// gets the strict store, not the leaf underneath it.
func TestStrictStoreWriterHandleKeepsTheGuards(t *testing.T) {
	t.Parallel()
	work, graph := NewSplitStores(t)
	writer := beads.HandlesFor(work).Writer

	if _, err := writer.Create(beads.Bead{ID: graphWispID, Title: "via writer"}); err == nil {
		t.Error("Writer.Create accepted a foreign-prefix id")
	}
	workBead := mustCreate(t, work, beads.Bead{Title: "work"})
	graphBead := mustCreate(t, graph, beads.Bead{Title: "graph"})
	if err := writer.DepAdd(workBead.ID, graphBead.ID, "blocks"); err == nil {
		t.Error("Writer.DepAdd accepted a cross-store edge")
	}
}

// TestStrictStoreRoutesByPrefix pins the routing contract the split depends on:
// storeref must be able to name the owning store for any id, in either
// direction, with no read.
func TestStrictStoreRoutesByPrefix(t *testing.T) {
	t.Parallel()
	work, graph := NewSplitStores(t)
	stores := []beads.Store{work, graph}

	graphPrefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("config.BeadClassGraph has no reserved prefix")
	}
	if got := graph.(storeref.HasIDPrefix).IDPrefix(); got != graphPrefix {
		t.Errorf("graph IDPrefix() = %q, want %q", got, graphPrefix)
	}
	if got := storeref.PrefixOwner(graphWispID, stores); got != graph {
		t.Errorf("PrefixOwner(%q) did not route to the graph store", graphWispID)
	}
	if got := storeref.PrefixOwner("gc-7", stores); got != work {
		t.Error(`PrefixOwner("gc-7") did not route to the work store`)
	}
}

// TestStrictStoreForwardsOptionalCapabilities pins the capability set an
// interface-embedding wrapper would otherwise strip. Production discovers each
// of these by type assertion, so a silently-stripped one changes behavior
// without failing anything.
func TestStrictStoreForwardsOptionalCapabilities(t *testing.T) {
	t.Parallel()
	_, graph := NewSplitStores(t)
	bead := mustCreate(t, graph, beads.Bead{Title: "capability probe"})

	t.Run("conditional writer resolves through the wrapper", func(t *testing.T) {
		writer, ok := beads.ConditionalWriterFor(graph)
		if !ok {
			t.Fatal("beads.ConditionalWriterFor lost the leaf's conditional-write capability")
		}
		closable := mustCreate(t, graph, beads.Bead{Title: "closable"})
		if err := writer.CloseIfMatch(closable.ID, closable.Revision); err != nil {
			t.Fatalf("CloseIfMatch: %v", err)
		}
	})

	t.Run("conditional-writes resolve target is the leaf", func(t *testing.T) {
		targeter, ok := graph.(beads.ConditionalWritesResolveTargeter)
		if !ok {
			t.Fatal("strict store does not declare a conditional-writes resolve target; a resolve through it would collapse to legacy silently")
		}
		if _, isStrict := targeter.ConditionalWritesResolveTarget().(*StrictStore); isStrict {
			t.Error("resolve target is the wrapper, not the leaf")
		}
	})

	t.Run("conditional assignment release reaches the leaf", func(t *testing.T) {
		releaser, ok := graph.(beads.ConditionalAssignmentReleaser)
		if !ok {
			t.Fatal("strict store does not expose ConditionalAssignmentReleaser")
		}
		if _, err := releaser.ReleaseIfCurrent(bead.ID, "nobody"); err != nil {
			t.Fatalf("ReleaseIfCurrent: %v", err)
		}
	})

	t.Run("unsupported capabilities report the documented sentinel", func(t *testing.T) {
		counter, ok := graph.(beads.Counter)
		if !ok {
			t.Fatal("strict store does not expose Counter")
		}
		if _, err := counter.Count(context.Background(), beads.ListQuery{AllowScan: true}); !errors.Is(err, beads.ErrCountUnsupported) {
			t.Errorf("Count on a leaf without Counter = %v, want ErrCountUnsupported so callers fall back to List", err)
		}
		if _, ok := beads.GraphApplyFor(graph); ok {
			t.Error("beads.GraphApplyFor claimed graph-apply for a leaf that has none")
		}
		if _, ok := graph.(beads.StorageCreateStore); ok {
			t.Error("strict store claimed StorageCreateStore for a MemStore leaf; the flag-based storage fallback would stop firing")
		}
	})

	t.Run("atomic-tx reports the leaf's guarantee", func(t *testing.T) {
		leaf := beads.NewMemStore()
		leaf.IDPrefix = "gcg"
		leaf.HonorExplicitIDs = true
		if got, want := beads.StoreSupportsAtomicTx(StrictWithPrefix(leaf, "gcg")), beads.StoreSupportsAtomicTx(leaf); got != want {
			t.Errorf("AtomicTx through the wrapper = %v, leaf = %v; wrapping must neither add nor remove atomicity", got, want)
		}
	})

	t.Run("dep list batch reaches the leaf", func(t *testing.T) {
		batcher, ok := graph.(interface {
			DepListBatch(ids []string) (map[string][]beads.Dep, error)
		})
		if !ok {
			t.Fatal("strict store does not expose DepListBatch")
		}
		dependent := mustCreate(t, graph, beads.Bead{Title: "dependent"})
		if err := graph.DepAdd(dependent.ID, bead.ID, "blocks"); err != nil {
			t.Fatalf("dep add: %v", err)
		}
		got, err := batcher.DepListBatch([]string{dependent.ID})
		if err != nil {
			t.Fatalf("DepListBatch: %v", err)
		}
		if len(got[dependent.ID]) != 1 {
			t.Errorf("DepListBatch(%q) = %+v, want the one edge", dependent.ID, got)
		}
	})
}

// TestStrictStoreCreateWithForeignIDBypassesTheGuard pins the documented escape
// hatch the create-guard error points callers at: the forced foreign-prefix
// create the class-store migration uses to keep a legacy id.
func TestStrictStoreCreateWithForeignIDBypassesTheGuard(t *testing.T) {
	t.Parallel()
	_, graph := NewSplitStores(t)
	creator, ok := graph.(beads.ForeignIDCreator)
	if !ok {
		t.Fatal("strict store does not expose ForeignIDCreator")
	}

	const legacyID = "gc-legacy-1"
	created, err := creator.CreateWithForeignID(beads.Bead{ID: legacyID, Title: "migrated"})
	if err != nil {
		t.Fatalf("CreateWithForeignID: %v", err)
	}
	if created.ID != legacyID {
		t.Fatalf("created id = %q, want the legacy id %q kept verbatim", created.ID, legacyID)
	}
	if _, err := creator.CreateWithForeignID(beads.Bead{Title: "no id"}); err == nil {
		t.Error("CreateWithForeignID accepted an empty id")
	}
}

func TestNormalizePrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"gcg", "gcg"},
		{"GCG-", "gcg"},
		{"  gcg-  ", "gcg"},
		{"-", ""},
		{"", ""},
	} {
		if got := normalizePrefix(tc.in); got != tc.want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
