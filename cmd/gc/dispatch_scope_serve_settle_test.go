package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
)

// TestServeLoopSettlesScopeAfterMemberClose covers gastownhall/gascity#9507 at
// the narrow level: a worker's pass close of a scope member, observed as a
// bead.closed event, is recorded by the serve loop's watcher, and the reconcile
// the loop runs for that id settles the scope as failed because a sibling
// already failed. The store is in memory; reconcileClosedScopeMembers' own
// store open is not exercised here.
func TestServeLoopSettlesScopeAfterMemberClose(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	mustCreate := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("create %q: %v", b.Title, err)
		}
		if b.Status == "closed" {
			if err := store.Close(created.ID); err != nil {
				t.Fatalf("close %q: %v", b.Title, err)
			}
		}
		return created
	}
	workflow := mustCreate(beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.formula_contract": "graph.v2",
	}})
	body := mustCreate(beads.Bead{Title: "body", Type: "task", Metadata: map[string]string{
		"gc.kind": "scope", "gc.scope_role": "body", "gc.root_bead_id": workflow.ID, "gc.step_ref": "demo.body",
	}})
	failed := mustCreate(beads.Bead{Title: "member B (failed)", Type: "task", Status: "closed", Metadata: map[string]string{
		"gc.root_bead_id": workflow.ID, "gc.scope_ref": "body", "gc.scope_role": "member",
		"gc.step_ref": "demo.b", "gc.outcome": "fail",
	}})
	inFlight := mustCreate(beads.Bead{Title: "member A (in flight)", Type: "task", Metadata: map[string]string{
		"gc.root_bead_id": workflow.ID, "gc.scope_ref": "body", "gc.scope_role": "member", "gc.step_ref": "demo.a",
	}})
	if err := store.DepAdd(body.ID, failed.ID, "blocks"); err != nil {
		t.Fatalf("dep body->B: %v", err)
	}
	if err := store.DepAdd(body.ID, inFlight.ID, "blocks"); err != nil {
		t.Fatalf("dep body->A: %v", err)
	}

	// Worker's pass close of A: outcome set, then the bead closes; the close
	// event carries the closed bead snapshot.
	if err := store.SetMetadata(inFlight.ID, "gc.outcome", "pass"); err != nil {
		t.Fatalf("set outcome: %v", err)
	}
	if err := store.Close(inFlight.ID); err != nil {
		t.Fatalf("close A: %v", err)
	}
	closed, err := store.Get(inFlight.ID)
	if err != nil {
		t.Fatalf("get A: %v", err)
	}
	payload, err := beads.EncodeBeadEventPayload(closed)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}

	watcher := &scopeCloseWatcher{Watcher: &fakeEventWatcher{evts: []events.Event{
		{Type: events.BeadClosed, Payload: payload},
	}}}
	if _, err := watcher.Next(); err != nil {
		t.Fatalf("watcher Next: %v", err)
	}
	ids := watcher.takeClosedScopeMembers()
	if len(ids) != 1 || ids[0] != inFlight.ID {
		t.Fatalf("recorded ids = %v, want [%s]", ids, inFlight.ID)
	}

	// The loop's per-id reconcile, against the same store.
	for _, id := range ids {
		if _, err := dispatch.ReconcileClosedScopeMember(beads.GraphStore{Store: store}, id); err != nil {
			t.Fatalf("reconcile %s: %v", id, err)
		}
	}

	got, err := store.Get(body.ID)
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("body status = %q, want closed", got.Status)
	}
	if outcome := got.Metadata["gc.outcome"]; outcome != "fail" {
		t.Fatalf("body gc.outcome = %q, want fail (sibling B failed)", outcome)
	}
}
