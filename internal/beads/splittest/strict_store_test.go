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

// graphNamespaces is the fence a leaf minting "gcg" has to carry: the
// namespaces the graph class claims, which is what NewClassStore passes and
// what newStrict now requires of any SQLiteSemantics leaf minting a reserved
// class prefix. Reading it from config rather than writing {"gcg"} here keeps
// these fixtures honest if the class ever gains an auxiliary namespace the way
// nudges has one.
func graphNamespaces() []string {
	return config.ReservedClassPrefixesFor(config.BeadClassGraph)
}

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
// residence-invariant violations: bd hard-fails them, and SQLite accepts them
// and carries the damage forward. A plain MemStore does neither — it accepts
// them and carries NO damage, so the test sees a healthy store either way.
//
// TestStrictStoreCatchesWhatLenientDoublesLetThrough runs the identical
// operations against the strict pair, where the work leaf fails the way bd
// fails and the class leaf corrupts the way SQLite corrupts.
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
// plain MemStore pair, against the work leaf, which models bd and must reject
// each of them at the call site.
func TestStrictStoreCatchesWhatLenientDoublesLetThrough(t *testing.T) {
	t.Parallel()

	t.Run("cross-store dep is rejected", func(t *testing.T) {
		t.Parallel()
		work, graph := NewSplitStores(t)
		workBead := mustCreate(t, work, beads.Bead{Title: "work"})
		graphBead := mustCreate(t, graph, beads.Bead{Title: "graph"})

		err := work.DepAdd(workBead.ID, graphBead.ID, "blocks")
		if err == nil {
			t.Fatal("cross-store DepAdd succeeded; a bd-semantics store must reject an endpoint it cannot resolve")
		}
		// A cross-PREFIX target is the one endpoint bd itself passes through as
		// an external ref, so this rejection is the domain co-residence
		// invariant, and it must say so rather than borrow bd's wording for a
		// failure bd does not produce.
		for _, want := range []string{"adding dep", graphBead.ID, "another store's id namespace", "ErrMemberNotCoResident"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
		if strings.Contains(err.Error(), "no issue found") {
			t.Errorf("error %q borrows bd's resolution failure; bd resolves a cross-prefix target as an external ref and never emits that message here", err)
		}
		if errors.Is(err, beads.ErrNotFound) {
			t.Error("cross-store DepAdd error wraps beads.ErrNotFound; bd reports a subprocess stderr string that callers can only classify textually, so a typed error would let in-process tests pass on an errors.Is check production cannot satisfy")
		}
		deps, err := work.DepList(workBead.ID, "down")
		if err != nil {
			t.Fatalf("dep list: %v", err)
		}
		if len(deps) != 0 {
			t.Fatalf("rejected DepAdd still recorded %d deps; the reject must happen before the leaf write", len(deps))
		}
	})

	t.Run("same-prefix missing endpoint is rejected in bd's wording", func(t *testing.T) {
		t.Parallel()
		work, _ := NewSplitStores(t)
		workBead := mustCreate(t, work, beads.Bead{Title: "work"})

		err := work.DepAdd(workBead.ID, "gc-absent", "blocks")
		if err == nil {
			t.Fatal("dep on a missing same-prefix id succeeded; bd hard-fails when the target does not resolve")
		}
		// bd's own wording (cmd/bd/dep.go resolveIDWithRouting), so a test
		// cannot pass on a classification production could never satisfy.
		for _, want := range []string{"adding dep", "resolving issue ID gc-absent", `no issue found matching "gc-absent"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q; it must be shaped like bd's failure", err, want)
			}
		}
	})

	t.Run("foreign-prefix create is rejected", func(t *testing.T) {
		t.Parallel()
		work, _ := NewSplitStores(t)

		_, err := work.Create(beads.Bead{ID: graphWispID, Title: "graph wisp in the work store"})
		if err == nil {
			t.Fatal("foreign-prefix create succeeded; a bd-semantics work store must reject an id outside its namespace")
		}
		// bd's own wording (validation.ValidateIDPrefixAllowed).
		for _, want := range []string{graphWispID, "prefix mismatch", `database uses "gc-"`, "use --force to override"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})
}

// TestClassStoreModelsSQLitesSilentAcceptance is the other half of the split:
// the coordination-class leaf runs on SQLite, which rejects NEITHER of those
// writes. It accepts them, and the damage shows up later as a dangling edge and
// an unroutable row. A kit that hard-failed here would put a test on an error
// branch production never takes, so it accepts too — and records, so the fixture
// still cannot walk past it.
//
// The dep half is the whole story for a class store. The pinned-id half is not:
// OpenEngine also FENCES a class binding's store to the namespaces it claims, so
// the unroutable row is refused before SQLite ever gets the chance to accept it.
// Both outcomes are real, and which one a leaf gives depends on whether it
// serves a binding — see pinned_id_fence.go and the two rows below.
func TestClassStoreModelsSQLitesSilentAcceptance(t *testing.T) {
	t.Parallel()

	t.Run("cross-store dep is accepted and drops its dependent out of Ready", func(t *testing.T) {
		t.Parallel()
		work, graph := NewSplitStores(t)
		workBead := mustCreate(t, work, beads.Bead{Title: "work"})
		graphBead := mustCreate(t, graph, beads.Bead{Title: "graph"})

		if err := graph.DepAdd(graphBead.ID, workBead.ID, "blocks"); err != nil {
			t.Fatalf("class store rejected a cross-store dep SQLite accepts: %v", err)
		}
		deps, err := graph.DepList(graphBead.ID, "down")
		if err != nil {
			t.Fatalf("dep list: %v", err)
		}
		if len(deps) != 1 {
			t.Fatalf("recorded %d deps, want the dangling edge SQLite's foreign-key-free deps table keeps", len(deps))
		}
		ready, err := graph.Ready(beads.ReadyQuery{})
		if err != nil {
			t.Fatalf("ready: %v", err)
		}
		for _, b := range ready {
			if b.ID == graphBead.ID {
				t.Errorf("%q is still Ready; production drops a bead blocked by an unresolvable edge, which is how the dangling edge wedges a convoy silently", graphBead.ID)
			}
		}
		assertClaimedViolation(t, graph, "dep-add", workBead.ID)
	})

	// The pinned-id half is the one the fence took back. An UNFENCED SQLite
	// store still accepts a foreign-prefix row verbatim — normalizeCreate has no
	// prefix check — so the kit models that outcome for a leaf with no namespace
	// claim, and this row is what keeps that arm honest. A leaf serving a class
	// binding is fenced, and the refusal is pinned beside it.
	//
	// The leaf mints a WORK-shaped prefix, because an unfenced SQLite store is
	// exactly the work-serving engine binding: EngineReservedPrefixes returns
	// nothing for any class set containing work, so that binding is opened with
	// no namespace claim at all. A SQLite leaf minting a reserved class prefix
	// and claiming nothing is a store production never opens, and newStrict
	// refuses to build one.
	t.Run("foreign-prefix create is accepted verbatim without a fence", func(t *testing.T) {
		t.Parallel()
		unfenced := newStrictMemLeaf(t, "hq", SQLiteSemantics)

		created, err := unfenced.Create(beads.Bead{ID: "gc-123", Title: "another ledger's row in this database"})
		if err != nil {
			t.Fatalf("unfenced SQLite leaf rejected a foreign-prefix create SQLite accepts: %v", err)
		}
		if created.ID != "gc-123" {
			t.Fatalf("created id = %q, want %q kept verbatim as SQLiteStore.normalizeCreate keeps it", created.ID, "gc-123")
		}
		if _, err := unfenced.Get("gc-123"); err != nil {
			t.Fatalf("get the foreign-prefix row: %v", err)
		}
		if owner := storeref.PrefixOwner("gc-123", []beads.Store{unfenced}); owner != nil {
			t.Error("prefix routing found the foreign row in this store; production's routing looks in the store that mints gc- and never sees it, which is what makes the row unreachable")
		}
		assertClaimedViolation(t, unfenced, "create", "gc-123")
	})

	t.Run("a class binding's store is fenced, so the same create is refused", func(t *testing.T) {
		t.Parallel()
		_, graph := NewSplitStores(t)

		if _, err := graph.Create(beads.Bead{ID: "gc-123", Title: "work-prefixed row in the class database"}); !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
			t.Fatalf("the class store answered %v, want ErrPinnedIDOutsideNamespace: OpenEngine fences a class binding to the namespaces it claims, so the unreachable row above is never written in the first place", err)
		}
		if _, err := graph.Get("gc-123"); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("after the refusal Get(%q) = %v, want ErrNotFound", "gc-123", err)
		}
	})
}

// assertClaimedViolation drains the store's recorded violations and requires
// exactly one of the given kind, mentioning subject. Draining is also what stops
// the constructor's cleanup check from failing a test whose whole point is the
// accepted violation.
func assertClaimedViolation(t *testing.T, s beads.Store, op, subject string) {
	t.Helper()
	violations := TakeResidenceViolations(s)
	if len(violations) != 1 {
		t.Fatalf("recorded %v, want exactly one %s violation", violations, op)
	}
	if violations[0].Op != op {
		t.Errorf("violation op = %q, want %q", violations[0].Op, op)
	}
	if !strings.Contains(violations[0].Detail, subject) {
		t.Errorf("violation detail %q does not name %q", violations[0].Detail, subject)
	}
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
	graphParent := mustCreate(t, graph, beads.Bead{Title: "graph parent"})
	workChild := mustCreate(t, work, beads.Bead{Title: "work child", ParentID: graphParent.ID})

	if err := work.DepAdd(workChild.ID, graphParent.ID, "parent-child"); err != nil {
		t.Fatalf("parent-child restatement rejected: %v", err)
	}
	if deps, err := work.DepList(workChild.ID, "down"); err != nil {
		t.Fatalf("dep list: %v", err)
	} else if len(deps) != 0 {
		t.Fatalf("short-circuited parent-child recorded %d deps; bd never sees the call", len(deps))
	}
	// A parent-child dep naming a DIFFERENT bead is a real edge and still
	// resolves both endpoints.
	if err := work.DepAdd(workChild.ID, "gc-absent", "parent-child"); err == nil {
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
//
// It is a check on the DOUBLE, not on a backend — bd and SQLite both honor a
// pinned id — so it holds under both semantics.
func TestStrictStoreCreateRejectsAClobberingLeaf(t *testing.T) {
	t.Parallel()
	for _, semantics := range []Semantics{BdSemantics, SQLiteSemantics} {
		t.Run(semantics.String(), func(t *testing.T) {
			t.Parallel()
			leaf := beads.NewMemStore()
			leaf.IDPrefix = "gcg" // mints gcg-<n>, but clobbers pinned ids
			strict := StrictWithPrefix(t, leaf, "gcg", semantics, graphNamespaces()...)

			_, err := strict.Create(beads.Bead{ID: graphWispID, Title: "wisp"})
			if err == nil {
				t.Fatal("create with a clobbering leaf succeeded; the pinned id was silently replaced")
			}
			for _, want := range []string{graphWispID, "clobbers pinned ids"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestStrictStoreCreateRejectsAWrongPrefixLeaf pins the other post-check: a leaf
// MINTING outside the namespace the wrapper declares is a broken double, not a
// production behavior, so it too is rejected under both semantics.
func TestStrictStoreCreateRejectsAWrongPrefixLeaf(t *testing.T) {
	t.Parallel()
	for _, semantics := range []Semantics{BdSemantics, SQLiteSemantics} {
		t.Run(semantics.String(), func(t *testing.T) {
			t.Parallel()
			strict := StrictWithPrefix(t, beads.NewMemStore(), "gcg", semantics, graphNamespaces()...) // leaf mints gc-<n>

			_, err := strict.Create(beads.Bead{Title: "store-minted"})
			if err == nil {
				t.Fatal("store-minted create succeeded on a leaf minting outside the declared namespace")
			}
			if !strings.Contains(err.Error(), "outside its declared id namespace") {
				t.Errorf("error %q does not name the namespace violation", err)
			}
		})
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
	if !strings.Contains(inner.Error(), "prefix mismatch") {
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
		// Assert the target IS the leaf, not merely that it is not the
		// wrapper: a nil return also is not the wrapper, and would collapse
		// the resolve to legacy just as silently.
		target := targeter.ConditionalWritesResolveTarget()
		if target == nil {
			t.Fatal("resolve target is nil; a resolve through the wrapper would collapse to legacy silently")
		}
		if _, isStrict := target.(*StrictStore); isStrict {
			t.Error("resolve target is the wrapper, not the leaf")
		}
		if _, isLeaf := target.(*beads.MemStore); !isLeaf {
			t.Errorf("resolve target is %T, want the *beads.MemStore leaf", target)
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
		if got, want := beads.StoreSupportsAtomicTx(StrictWithPrefix(t, leaf, "gcg", SQLiteSemantics, graphNamespaces()...)), beads.StoreSupportsAtomicTx(leaf); got != want {
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

// prefixLeaf is a leaf that declares its own id namespace through
// storeref.HasIDPrefix, which is what Strict infers from. beads.MemStore cannot
// be one: its IDPrefix is a FIELD, and Go forbids a method of the same name on
// the same type.
type prefixLeaf struct {
	beads.Store
	prefix string
}

func (l prefixLeaf) IDPrefix() string { return l.prefix }

// TestStrictInfersTheLeafsDeclaredPrefix pins the half of Strict that works: a
// leaf that declares a namespace gets a store that guards it and routes by it,
// normalized the same way StrictWithPrefix normalizes.
func TestStrictInfersTheLeafsDeclaredPrefix(t *testing.T) {
	t.Parallel()
	leaf := beads.NewMemStore()
	leaf.IDPrefix = "gcg"
	leaf.HonorExplicitIDs = true
	strict := Strict(t, prefixLeaf{Store: leaf, prefix: "GCG-"}, BdSemantics)

	if got := strict.(storeref.HasIDPrefix).IDPrefix(); got != "gcg" {
		t.Errorf("IDPrefix() = %q, want the normalized %q", got, "gcg")
	}
	if storeref.PrefixOwner("gcg-1", []beads.Store{strict}) != strict {
		t.Error("PrefixOwner could not route an in-prefix id to the store; the inferred prefix did not reach IDPrefix")
	}
	if _, err := strict.Create(beads.Bead{ID: "gc-work-bead"}); err == nil {
		t.Error("the inferred-prefix store accepted a work-prefixed id; the create guard is inert")
	}
}

// TestInferredPrefixRejectsALeafWithNoNamespace pins the case that used to make
// Strict return a store that reads as strict and silently is not: an undeclared
// namespace made the create guard pass everything and IDPrefix report "", so the
// store type-asserted as routable and then routed nothing. It is now a hard
// failure pointing at StrictWithPrefix. (Strict itself t.Fatalf's, so the rule
// is exercised through the helper it is split out into.)
func TestInferredPrefixRejectsALeafWithNoNamespace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		leaf beads.Store
	}{
		{"no accessor at all (beads.MemStore)", beads.NewMemStore()},
		{"accessor reporting empty", prefixLeaf{Store: beads.NewMemStore(), prefix: ""}},
		{"accessor reporting only a dash", prefixLeaf{Store: beads.NewMemStore(), prefix: "-"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prefix, err := inferredPrefix(tc.leaf)
			if err == nil {
				t.Fatalf("inferredPrefix accepted a leaf with no declared namespace, returning %q", prefix)
			}
			if !strings.Contains(err.Error(), "StrictWithPrefix") {
				t.Errorf("error %q does not point the caller at StrictWithPrefix", err)
			}
		})
	}
}

// TestNewStrictRejectsAnUndeclaredContract pins the same rule on the shared
// constructor: neither an empty prefix nor an undeclared backend may produce a
// store, because both would be checks that quietly do nothing.
func TestNewStrictRejectsAnUndeclaredContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		leaf      beads.Store
		prefix    string
		semantics Semantics
		want      string
	}{
		{"nil leaf", nil, "gcg", BdSemantics, "nil"},
		{"empty prefix", beads.NewMemStore(), "", BdSemantics, "empty id prefix"},
		{"prefix that normalizes to empty", beads.NewMemStore(), " - ", BdSemantics, "empty id prefix"},
		{"unset semantics", beads.NewMemStore(), "gcg", 0, "no production backend declared"},
		{"unknown semantics", beads.NewMemStore(), "gcg", Semantics(99), "no production backend declared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := newStrict(tc.leaf, tc.prefix, tc.semantics, nil)
			if err == nil {
				t.Fatalf("newStrict returned %T for %s, want an error", store, tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestNewStrictRejectsAnUnfencedClassLeaf pins the guard that catches a
// forgotten fence. The fence is variadic, so omitting it is silent and yields
// the store this kit shipped before there was one — right for every leaf except
// this shape, where the mint prefix says the leaf serves a class binding and
// storebinding.EngineReservedPrefixes fences every one of those.
//
// The two accept rows are the over-restriction controls, and they are what keep
// the guard from being "reserved prefixes are banned": a BdSemantics leaf is
// modeling bd, which has no fence to forget, and a SQLiteSemantics leaf that
// was given its namespaces is the very thing the guard is asking for.
func TestNewStrictRejectsAnUnfencedClassLeaf(t *testing.T) {
	t.Parallel()
	t.Run("SQLiteSemantics, reserved mint prefix, no namespaces", func(t *testing.T) {
		t.Parallel()
		store, err := newStrict(beads.NewMemStore(), "gcg", SQLiteSemantics, nil)
		if err == nil {
			t.Fatalf("newStrict built %T for an unfenced class leaf; production opens no such store, so every write it accepts is one the binding would have refused", store)
		}
		if !strings.Contains(err.Error(), "reserved class prefix") {
			t.Errorf("error %q does not name the rule it is enforcing", err)
		}
	})
	t.Run("the auxiliary namespace is reserved just as strongly", func(t *testing.T) {
		t.Parallel()
		if _, err := newStrict(beads.NewMemStore(), config.NudgeQueueIDPrefix, SQLiteSemantics, nil); err == nil {
			t.Errorf("newStrict accepted %q unfenced; a prefix a class HOLDS without minting it is as reserved as the mint prefix, and a leaf standing in for one is as fenced", config.NudgeQueueIDPrefix)
		}
	})
	t.Run("fenced is what the guard is asking for", func(t *testing.T) {
		t.Parallel()
		if _, err := newStrict(beads.NewMemStore(), "gcg", SQLiteSemantics, graphNamespaces()); err != nil {
			t.Errorf("newStrict refused a properly fenced class leaf: %v", err)
		}
	})
	t.Run("BdSemantics has no fence to forget", func(t *testing.T) {
		t.Parallel()
		if _, err := newStrict(beads.NewMemStore(), "gcg", BdSemantics, nil); err != nil {
			t.Errorf("newStrict refused an unfenced bd-shaped leaf: %v; bd is not the backend the fence is a property of, and refusing here would make the guard a ban on reserved prefixes", err)
		}
	})
	t.Run("a work-shaped prefix stays open", func(t *testing.T) {
		t.Parallel()
		if _, err := newStrict(beads.NewMemStore(), "hq", SQLiteSemantics, nil); err != nil {
			t.Errorf("newStrict refused an unfenced work-shaped leaf: %v; that leaf IS the work-serving engine binding, which EngineReservedPrefixes opens with no namespace claim at all", err)
		}
	})
}

// TestUnclaimedResidenceViolationsFailTheTest pins the loudness half of
// SQLiteSemantics: accepting a violation the way SQLite accepts it must not mean
// a fixture can walk past it. The cleanup hook the constructors install is one
// line over unclaimedViolationsMessage, which is what carries the judgement.
func TestUnclaimedResidenceViolationsFailTheTest(t *testing.T) {
	t.Parallel()
	if got := unclaimedViolationsMessage(nil); got != "" {
		t.Errorf("a store with nothing to report produced %q", got)
	}
	message := unclaimedViolationsMessage([]ResidenceViolation{
		{Op: "create", Detail: `bead "gc-123" was created inside the "gcg" store`},
		{Op: "dep-add", Detail: "edge gcg-1→gc-9 was recorded"},
	})
	for _, want := range []string{"2 residence-invariant", "sqlite-semantics", "gc-123", "gcg-1→gc-9", "TakeResidenceViolations"} {
		if !strings.Contains(message, want) {
			t.Errorf("cleanup message %q does not contain %q", message, want)
		}
	}
}

// TestClaimingResidenceViolationsClearsThem pins the other half: a fixture that
// asserts the production corruption claims the violations, and the store must
// then have nothing left to fail its test with.
// The leaf is unfenced because a fenced one refuses the create outright and
// records nothing; the violation this is about is the one an unfenced SQLite
// store accepts. Unfenced means work-shaped here — the work-serving engine
// binding is the SQLite store production opens with no namespace claim.
func TestClaimingResidenceViolationsClearsThem(t *testing.T) {
	t.Parallel()
	graph := newStrictMemLeaf(t, "hq", SQLiteSemantics)

	if _, err := graph.Create(beads.Bead{ID: "gc-123", Title: "foreign row"}); err != nil {
		t.Fatalf("unfenced SQLite leaf rejected a create SQLite accepts: %v", err)
	}
	if claimed := TakeResidenceViolations(graph); len(claimed) != 1 {
		t.Fatalf("claimed %v, want the one create violation", claimed)
	}
	if leftover := TakeResidenceViolations(graph); len(leftover) != 0 {
		t.Errorf("claiming left %v behind; a fixture asserting the corruption would still fail at cleanup", leftover)
	}
}

// TestBdSemanticsRecordsNothing pins that a work store never accumulates
// violations to claim: it rejects at the call site, so TakeResidenceViolations
// on it is always empty and no fixture can be lulled into claiming a rejection.
func TestBdSemanticsRecordsNothing(t *testing.T) {
	t.Parallel()
	work, _ := NewSplitStores(t)
	workBead := mustCreate(t, work, beads.Bead{Title: "work"})

	if _, err := work.Create(beads.Bead{ID: graphWispID}); err == nil {
		t.Fatal("bd-semantics store accepted a foreign-prefix create")
	}
	if err := work.DepAdd(workBead.ID, "gcg-absent", "blocks"); err == nil {
		t.Fatal("bd-semantics store accepted a cross-store dep")
	}
	if got := TakeResidenceViolations(work); len(got) != 0 {
		t.Errorf("bd-semantics store recorded %v; it rejected instead, so there is nothing to record", got)
	}
	// A store the kit never wrapped records nothing either, and must not be
	// mistaken for a clean strict one.
	if got := TakeResidenceViolations(beads.NewMemStore()); got != nil {
		t.Errorf("an unwrapped leaf reported %v", got)
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
