package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Native input admission on the controller's demand side (#8832): a withheld
// step — one whose workflow input is held, blocked, deferred or waiting on a
// prerequisite — is not work for its assignee. The hook's claim path will not
// serve it, so counting it as keep-awake work, wake demand, drain-ack
// claimability or ladder work wakes or holds a session for work it cannot
// claim: the loop a dispatch hold caused (ga-5736js), read through the same
// live verdict the ready reader applies.

// inputAdmissionSeatFixture is one assigned native workflow step over an
// input: the input bead, its tracking convoy, the root molecule recording
// gc.input_convoy_id, and an OPEN step assigned to a seat that names the root.
type inputAdmissionSeatFixture struct {
	input beads.Bead
	conv  beads.Bead
	root  beads.Bead
	step  beads.Bead
	store beads.Store
}

// materializeInputAdmissionSeat stamps the fixture into store and holds the
// input on hold:external.
func materializeInputAdmissionSeat(t *testing.T, store beads.Store) inputAdmissionSeatFixture {
	t.Helper()
	fx := inputAdmissionSeatFixture{store: store}
	mustCreateBead(t, store, &fx.input, beads.Bead{Title: "input", Type: "task", Labels: []string{"hold:external"}})
	mustCreateBead(t, store, &fx.conv, beads.Bead{Title: "input convoy", Type: "convoy"})
	if err := store.DepAdd(fx.conv.ID, fx.input.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd tracks: %v", err)
	}
	mustCreateBead(t, store, &fx.root, beads.Bead{
		Title:    "workflow root",
		Type:     "molecule",
		Metadata: map[string]string{"gc.input_convoy_id": fx.conv.ID},
	})
	mustCreateBead(t, store, &fx.step, beads.Bead{
		Title:    "graph-step",
		Type:     "task",
		Assignee: "seat-1",
		Metadata: map[string]string{"gc.root_bead_id": fx.root.ID},
	})
	return fx
}

// mustCreateBead creates b in store and stores the created row back through
// out.
func mustCreateBead(t *testing.T, store beads.Store, out *beads.Bead, b beads.Bead) {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("creating %q: %v", b.Title, err)
	}
	*out = created
}

// TestNativeInputAdmissionKeepAwakeIgnoresWithheldAssignedStep covers AC-5's
// keep-awake arm: a seat whose only assigned ready work is a step input
// admission withholds does not keep the session awake, and does again once
// the input's hold is released.
func TestNativeInputAdmissionKeepAwakeIgnoresWithheldAssignedStep(t *testing.T) {
	store := beads.NewMemStore()
	fx := materializeInputAdmissionSeat(t, store)
	has, err := sessionHasReadyAssignedWorkForTier(store, "seat-1", beads.TierIssues)
	if err != nil {
		t.Fatalf("sessionHasReadyAssignedWorkForTier: %v", err)
	}
	if has {
		t.Fatalf("keep-awake counts assigned step %s whose input %s carries hold:external; want it ignored", fx.step.ID, fx.input.ID)
	}
	if err := store.Update(fx.input.ID, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
		t.Fatalf("releasing hold: %v", err)
	}
	has, err = sessionHasReadyAssignedWorkForTier(store, "seat-1", beads.TierIssues)
	if err != nil {
		t.Fatalf("sessionHasReadyAssignedWorkForTier after release: %v", err)
	}
	if !has {
		t.Fatalf("keep-awake omits assigned step %s whose input's hold is released; want it counted", fx.step.ID)
	}
}

// TestNativeInputAdmissionKeepAwakeKeepsInProgressStep covers AC-7's ownership
// arm on the same probe: an in_progress step is existing ownership and is
// never judged, whatever its input carries.
func TestNativeInputAdmissionKeepAwakeKeepsInProgressStep(t *testing.T) {
	store := beads.NewMemStore()
	fx := materializeInputAdmissionSeat(t, store)
	status := "in_progress"
	if err := store.Update(fx.step.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("starting step: %v", err)
	}
	// sessionHasReadyAssignedWorkForTier answers the ready tier; the in_progress
	// tier is its sibling probe and shares the admission verdict's subject rule.
	has, err := sessionHasOpenAssignedWorkForTier(store, "seat-1", "in_progress", beads.TierIssues, true)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForTier: %v", err)
	}
	if !has {
		t.Fatalf("in_progress arm omits step %s; input admission must never judge existing ownership", fx.step.ID)
	}
}

// TestNativeInputAdmissionDrainAckClassifiesWithheldAsNonClaimable covers
// AC-5's drain-ack arm: an open assigned row input admission withholds is
// provably non-claimable — the seat that drained past it pulled correctly —
// and is again once the input's hold is released.
func TestNativeInputAdmissionDrainAckClassifiesWithheldAsNonClaimable(t *testing.T) {
	store := beads.NewMemStore()
	fx := materializeInputAdmissionSeat(t, store)
	nonClaimable, err := openAssignedRowProvablyNonClaimable(store, fx.step, time.Now())
	if err != nil {
		t.Fatalf("openAssignedRowProvablyNonClaimable: %v", err)
	}
	if !nonClaimable {
		t.Fatalf("open assigned step %s whose input %s carries hold:external classifies claimable; want input_withheld proof", fx.step.ID, fx.input.ID)
	}
	if err := store.Update(fx.input.ID, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
		t.Fatalf("releasing hold: %v", err)
	}
	nonClaimable, err = openAssignedRowProvablyNonClaimable(store, fx.step, time.Now())
	if err != nil {
		t.Fatalf("openAssignedRowProvablyNonClaimable after release: %v", err)
	}
	if nonClaimable {
		t.Fatalf("step %s whose input's hold is released still classifies non-claimable; want it settled on its own facts", fx.step.ID)
	}
}
