package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// An order whose pour is abandoned — root open, members open, nobody ever
// touched them — holds hasOpenWork true and the open-work gate refuses that
// order on every dispatch tick, forever. The scheduled order-tracking-sweep
// could never reach it, because wisp recovery refused to run without an
// explicit order name and the sweep order ships without one (sr-kjwpz). An
// unscoped sweep is the recovery path that closes that hole, so it must find
// the abandoned subtree with no name supplied.
func TestSweepStaleOrderWispSubtreesUnscopedClosesAbandonedRoot(t *testing.T) {
	store := beads.NewMemStore()

	root, err := store.Create(beads.Bead{
		Title:  "ticket-intake",
		Type:   "task",
		Labels: []string{"order-run:ticket-intake:rig:st"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	step, err := store.Create(beads.Bead{
		Title: "Poll recent_tickets",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "ticket-intake.poll",
		},
	})
	if err != nil {
		t.Fatalf("Create(step): %v", err)
	}

	closed, err := sweepStaleOrderWispSubtrees(store, step.CreatedAt.Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("sweepStaleOrderWispSubtrees(unscoped): %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2 (root and step)", closed)
	}
	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
	}
}

// The regression this feature can most easily cause. openSubtreeOlderThan
// filters on CreatedAt alone, which is safe for an operator who named the
// order and knows it is wedged. Run unscoped on a schedule, it would
// force-close a pour that is being worked right now: on srvcity a HEALTHY
// ticket-intake pour lived 11m31s from create to close, so a 10m bound would
// have killed it mid-flight while the poller held its poll step.
//
// Unscoped recovery therefore requires the subtree to be UNTOUCHED as well as
// old: no open member updated since the cutoff. Progress on any member spares
// the whole subtree at any age.
func TestSweepStaleOrderWispSubtreesUnscopedSparesSubtreeWithProgress(t *testing.T) {
	store := beads.NewMemStore()

	root, err := store.Create(beads.Bead{
		Title:  "ticket-intake",
		Type:   "task",
		Labels: []string{"order-run:ticket-intake:rig:st"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	step, err := store.Create(beads.Bead{
		Title: "Poll recent_tickets",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "ticket-intake.poll",
		},
	})
	if err != nil {
		t.Fatalf("Create(step): %v", err)
	}

	assignee := "st--support__intake-poller-pool"
	if err := store.Update(step.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("Update(step claim): %v", err)
	}
	touched, err := store.Get(step.ID)
	if err != nil {
		t.Fatalf("Get(step): %v", err)
	}

	// The claim's own timestamp is the cutoff. Every bead here was CREATED
	// strictly before it, so the age bound alone would sweep the subtree; the
	// claimed step's UpdatedAt sits exactly ON it, which openSubtreeUntouchedSince
	// reads as touched (it requires strictly-before).
	cutoff := touched.UpdatedAt

	closed, err := sweepStaleOrderWispSubtrees(store, cutoff, nil)
	if err != nil {
		t.Fatalf("sweepStaleOrderWispSubtrees(unscoped): %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0: a claimed step is work in flight", closed)
	}
	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("%s status = %q, want open", id, got.Status)
		}
	}
}

// The named path is the operator's deliberate "I know this order is wedged,
// force it" and must keep its existing CreatedAt-only semantics. Same fixture
// as the test above: unscoped spares it, named closes it.
func TestSweepStaleOrderWispSubtreesNamedStillClosesSubtreeWithProgress(t *testing.T) {
	store := beads.NewMemStore()

	root, err := store.Create(beads.Bead{
		Title:  "ticket-intake",
		Type:   "task",
		Labels: []string{"order-run:ticket-intake:rig:st"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	step, err := store.Create(beads.Bead{
		Title: "Poll recent_tickets",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "ticket-intake.poll",
		},
	})
	if err != nil {
		t.Fatalf("Create(step): %v", err)
	}

	assignee := "st--support__intake-poller-pool"
	if err := store.Update(step.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("Update(step claim): %v", err)
	}
	touched, err := store.Get(step.ID)
	if err != nil {
		t.Fatalf("Get(step): %v", err)
	}
	cutoff := touched.UpdatedAt

	closed, err := sweepStaleOrderWispSubtrees(store, cutoff, orderFilterForTest("ticket-intake:rig:st"))
	if err != nil {
		t.Fatalf("sweepStaleOrderWispSubtrees(named): %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2: the named path stays CreatedAt-only", closed)
	}
}

// The predicate's boundaries, stated exactly. The store-level tests above can
// only place timestamps relative to a real clock; these pin the comparisons
// themselves, including the two cases a store test cannot construct on demand:
// a bead written exactly ON the cutoff, and a legacy row with no UpdatedAt.
func TestOrderWispSubtreeSweepableBoundaries(t *testing.T) {
	cutoff := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour)
	newer := cutoff.Add(time.Hour)
	named := orderFilterForTest("ticket-intake:rig:st")

	for _, tc := range []struct {
		name            string
		subtree         []beads.Bead
		wantUnscoped    bool
		wantNamedSweeps bool
	}{{
		name:            "created and written before the cutoff",
		subtree:         []beads.Bead{{Status: "open", CreatedAt: old, UpdatedAt: old}},
		wantUnscoped:    true,
		wantNamedSweeps: true,
	}, {
		name:            "written after the cutoff",
		subtree:         []beads.Bead{{Status: "open", CreatedAt: old, UpdatedAt: newer}},
		wantUnscoped:    false,
		wantNamedSweeps: true,
	}, {
		name:            "written exactly ON the cutoff counts as touched",
		subtree:         []beads.Bead{{Status: "open", CreatedAt: old, UpdatedAt: cutoff}},
		wantUnscoped:    false,
		wantNamedSweeps: true,
	}, {
		name:            "legacy row with no UpdatedAt counts as untouched",
		subtree:         []beads.Bead{{Status: "open", CreatedAt: old}},
		wantUnscoped:    true,
		wantNamedSweeps: true,
	}, {
		name:            "a CLOSED member written recently does not veto",
		subtree:         []beads.Bead{{Status: "open", CreatedAt: old, UpdatedAt: old}, {Status: "closed", CreatedAt: old, UpdatedAt: newer}},
		wantUnscoped:    true,
		wantNamedSweeps: true,
	}, {
		name:            "created after the cutoff fails the age bound for both",
		subtree:         []beads.Bead{{Status: "open", CreatedAt: newer, UpdatedAt: newer}},
		wantUnscoped:    false,
		wantNamedSweeps: false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := orderWispSubtreeSweepable(tc.subtree, cutoff, nil); got != tc.wantUnscoped {
				t.Errorf("unscoped = %v, want %v", got, tc.wantUnscoped)
			}
			if got := orderWispSubtreeSweepable(tc.subtree, cutoff, named); got != tc.wantNamedSweeps {
				t.Errorf("named = %v, want %v", got, tc.wantNamedSweeps)
			}
		})
	}
}

// The age bound still applies unscoped: a pour poured moments ago is not an
// orphan, however untouched it is.
func TestSweepStaleOrderWispSubtreesUnscopedSparesYoungSubtree(t *testing.T) {
	store := beads.NewMemStore()

	root, err := store.Create(beads.Bead{
		Title:  "ticket-intake",
		Type:   "task",
		Labels: []string{"order-run:ticket-intake:rig:st"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title: "Poll recent_tickets",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	}); err != nil {
		t.Fatalf("Create(step): %v", err)
	}

	closed, err := sweepStaleOrderWispSubtrees(store, root.CreatedAt.Add(-time.Hour), nil)
	if err != nil {
		t.Fatalf("sweepStaleOrderWispSubtrees(unscoped): %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
}

// Unscoped means EVERY order, not the first one found. The whole point is to
// recover an order nobody has been bitten by yet, so a city with two
// independently wedged orders must come back with both drained in one pass.
func TestSweepStaleOrderWispSubtreesUnscopedCoversEveryOrder(t *testing.T) {
	store := beads.NewMemStore()

	var rootIDs []string
	for _, name := range []string{"ticket-intake:rig:st", "mol-dog-stale-db"} {
		root, err := store.Create(beads.Bead{
			Title:  name,
			Type:   "task",
			Labels: []string{"order-run:" + name},
			Metadata: map[string]string{
				"gc.kind": "workflow",
			},
		})
		if err != nil {
			t.Fatalf("Create(root %s): %v", name, err)
		}
		if _, err := store.Create(beads.Bead{
			Title: name + " step",
			Metadata: map[string]string{
				"gc.root_bead_id": root.ID,
			},
		}); err != nil {
			t.Fatalf("Create(step %s): %v", name, err)
		}
		rootIDs = append(rootIDs, root.ID)
	}

	closed, err := sweepStaleOrderWispSubtrees(store, time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("sweepStaleOrderWispSubtrees(unscoped): %v", err)
	}
	if closed != 4 {
		t.Fatalf("closed = %d, want 4 (two roots and two steps)", closed)
	}
	for _, id := range rootIDs {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
	}
}

// The invariant that keeps an unscoped sweep from mutating beads it has no
// business touching: THE SWEEP MUST CLOSE EXACTLY WHAT THE GATE COUNTS.
//
// A root left open after every member completed looks wedged and is not: the
// gate reaches it through storeHasOpenDescendants, finds nothing open, and
// does not count it as work in flight — so that order is dispatching
// perfectly well and the sweep must leave the root alone. Widening the sweep
// past the gate would turn a scheduled recovery into a scheduled mutation of
// beads nobody is blocked on.
//
// This shape also exercises the walk fallback, because with no open stamped
// member staleOrderWispSubtreeBatchCloseIDs declines the whole store
// ("legacy graph-v2 wisps may only expose descendants through deps"), and
// unscoped sweeps meet it far more often than named ones did.
//
// Added while reviewing the unscoped selection rather than driven red-first.
func TestSweepStaleOrderWispSubtreesUnscopedLeavesRootTheGateIgnores(t *testing.T) {
	store := beads.NewMemStore()

	root, err := store.Create(beads.Bead{
		Title:  "ticket-intake",
		Type:   "task",
		Labels: []string{"order-run:ticket-intake:rig:st"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	step, err := store.Create(beads.Bead{
		Title: "Poll recent_tickets",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(step): %v", err)
	}
	if err := store.Close(step.ID); err != nil {
		t.Fatalf("Close(step): %v", err)
	}

	// What the open-work gate asks of this root (openWorkGateShut ->
	// hasOpenWork -> wispRootHasOpenWork, for a root that is not wisp-only).
	gated, err := storeHasOpenDescendants(store, root.ID, isTransientNotificationBead)
	if err != nil {
		t.Fatalf("storeHasOpenDescendants: %v", err)
	}
	if gated {
		t.Fatal("precondition: the gate must NOT count this root as open work")
	}

	closed, err := sweepStaleOrderWispSubtrees(store, time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("sweepStaleOrderWispSubtrees(unscoped): %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0: the sweep must not close what the gate does not count", closed)
	}
	got, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("root status = %q, want open", got.Status)
	}
}

// A bead that merely carries an order-run label is not a pour. Only wisp and
// workflow roots are force-closable; anything else (a tracking bead, a plain
// task someone labeled) must survive an unscoped pass.
func TestSweepStaleOrderWispSubtreesUnscopedSkipsNonWispRoots(t *testing.T) {
	store := beads.NewMemStore()

	tracking, err := store.Create(beads.Bead{
		Title:  "order:ticket-intake:rig:st",
		Labels: []string{"order-run:ticket-intake:rig:st", labelOrderTracking},
	})
	if err != nil {
		t.Fatalf("Create(tracking): %v", err)
	}
	plain, err := store.Create(beads.Bead{
		Title:  "not a pour",
		Labels: []string{"order-run:ticket-intake:rig:st"},
	})
	if err != nil {
		t.Fatalf("Create(plain): %v", err)
	}

	closed, err := sweepStaleOrderWispSubtrees(store, time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("sweepStaleOrderWispSubtrees(unscoped): %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	for _, id := range []string{tracking.ID, plain.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "open" {
			t.Fatalf("%s status = %q, want open", id, got.Status)
		}
	}
}

// The wisp half of an UNSCOPED sweep gets its own, much longer bound: the
// tracking half runs at --stale-after 10m, which is shorter than a healthy
// pour's lifetime and would be lethal applied to wisp subtrees unscoped. A
// subtree older than the tracking bound but younger than
// unscopedOrderWispStaleAfter survives.
func TestSweepStaleOrderTrackingUnscopedWispsHonorSeparateStaleBound(t *testing.T) {
	store := beads.NewMemStore()

	root, err := store.Create(beads.Bead{
		Title:  "ticket-intake",
		Type:   "task",
		Labels: []string{"order-run:ticket-intake:rig:st"},
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title: "Poll recent_tickets",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	}); err != nil {
		t.Fatalf("Create(step): %v", err)
	}

	// Half the unscoped wisp bound past the pour: well past the 10m tracking
	// bound the scheduled sweep runs at, well inside the wisp bound.
	inside := root.CreatedAt.Add(unscopedOrderWispStaleAfter / 2)

	result, err := sweepStaleOrderTrackingWithOptionsLimitMode(
		store, inside, 10*time.Minute, nil, orderTrackingSweepMetadataInitiator, true, 0, false,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptionsLimitMode: %v", err)
	}
	if result.wispClosed != 0 {
		t.Fatalf("wispClosed = %d, want 0: the pour is younger than the unscoped wisp bound", result.wispClosed)
	}

	// Past the wisp bound it is drained.
	result, err = sweepStaleOrderTrackingWithOptionsLimitMode(
		store, root.CreatedAt.Add(unscopedOrderWispStaleAfter+time.Hour), 10*time.Minute, nil, orderTrackingSweepMetadataInitiator, true, 0, false,
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderTrackingWithOptionsLimitMode(past bound): %v", err)
	}
	if result.wispClosed != 2 {
		t.Fatalf("wispClosed = %d, want 2", result.wispClosed)
	}
}
