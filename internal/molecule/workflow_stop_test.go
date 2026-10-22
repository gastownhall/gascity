package molecule

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestStoppedWorkflowAncestorWalksDrainLinks(t *testing.T) {
	store := beads.NewMemStore()
	create := func(meta map[string]string) beads.Bead {
		t.Helper()
		b, err := store.Create(beads.Bead{Title: "b", Type: "task", Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	run := create(map[string]string{"gc.kind": "workflow"})
	drain := create(map[string]string{"gc.root_bead_id": run.ID, "gc.kind": "drain"})
	item := create(map[string]string{"gc.kind": "workflow", "gc.drain_control_id": drain.ID})
	step := create(map[string]string{"gc.root_bead_id": item.ID})

	if stop, err := StoppedWorkflowAncestor(store, step); err != nil || stop.Stopped() {
		t.Fatalf("live chain: stop=%+v err=%v, want none", stop, err)
	}
	if _, err := store.CloseAll([]string{run.ID}, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatal(err)
	}
	stop, err := StoppedWorkflowAncestor(store, step)
	if err != nil {
		t.Fatal(err)
	}
	if stop.RootID != run.ID || stop.Canceled || stop.Outcome() != "skipped" {
		t.Fatalf("settled run: stop=%+v, want run %s settled (skipped)", stop, run.ID)
	}
	// The item root handed out whole is gated by its ancestors, not itself.
	if stop, err := StoppedWorkflowAncestor(store, item); err != nil || stop.RootID != run.ID {
		t.Fatalf("item root: stop=%+v err=%v, want run %s", stop, err, run.ID)
	}
}

func TestStoppedWorkflowAncestorEndsOnCycleAndMissingLinks(t *testing.T) {
	store := beads.NewMemStore()
	create := func(meta map[string]string) beads.Bead {
		t.Helper()
		b, err := store.Create(beads.Bead{Title: "b", Type: "task", Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// A root whose drain control names the root itself: a cycle.
	root := create(map[string]string{"gc.kind": "workflow"})
	control := create(map[string]string{"gc.root_bead_id": root.ID, "gc.kind": "drain"})
	if err := store.SetMetadataBatch(root.ID, map[string]string{"gc.drain_control_id": control.ID}); err != nil {
		t.Fatal(err)
	}
	step := create(map[string]string{"gc.root_bead_id": root.ID})
	if stop, err := StoppedWorkflowAncestor(store, step); err != nil || stop.Stopped() {
		t.Fatalf("cycle: stop=%+v err=%v, want the walk to end without a stop", stop, err)
	}

	orphan := create(map[string]string{"gc.root_bead_id": "gc-missing"})
	if stop, err := StoppedWorkflowAncestor(store, orphan); err != nil || stop.Stopped() {
		t.Fatalf("missing root: stop=%+v err=%v, want none (unreadable ancestry is not a stop)", stop, err)
	}
}

func TestWorkflowStopReasonCancelBeatsSettleAndSparesTeardown(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root", Type: "molecule", Metadata: map[string]string{"gc.kind": "workflow"}})
	if err != nil {
		t.Fatal(err)
	}
	step, err := store.Create(beads.Bead{Title: "s", Type: "task", Metadata: map[string]string{"gc.root_bead_id": root.ID}})
	if err != nil {
		t.Fatal(err)
	}
	teardown, err := store.Create(beads.Bead{Title: "t", Type: "task", Metadata: map[string]string{
		"gc.root_bead_id": root.ID, "gc.scope_role": "teardown", "gc.step_id": "cleanup",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CloseAll([]string{root.ID}, map[string]string{"gc.outcome": "canceled", "gc.cancel_requested": "true"}); err != nil {
		t.Fatal(err)
	}
	stop, err := WorkflowStopReason(store, step)
	if err != nil || !stop.Canceled || stop.Outcome() != "canceled" {
		t.Fatalf("canceled root: stop=%+v err=%v, want canceled", stop, err)
	}
	if stop, err := WorkflowStopReason(store, teardown); err != nil || stop.Stopped() {
		t.Fatalf("teardown: stop=%+v err=%v, want exempt", stop, err)
	}
}

// TestStoppedWorkflowAncestorStopsOnCancelMarkOfOpenRun pins the state a run
// cancel creates with its first write: the run root still open, carrying only
// gc.cancel_requested, every member and drain item root still open. That mark
// alone must stop the run's steps and the steps of a nested item workflow whose
// own root is live, so no claim lands while the cancel closes the rest.
func TestStoppedWorkflowAncestorStopsOnCancelMarkOfOpenRun(t *testing.T) {
	store := beads.NewMemStore()
	create := func(meta map[string]string) beads.Bead {
		t.Helper()
		b, err := store.Create(beads.Bead{Title: "b", Type: "task", Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	run := create(map[string]string{"gc.kind": "workflow"})
	runStep := create(map[string]string{"gc.root_bead_id": run.ID})
	drain := create(map[string]string{"gc.root_bead_id": run.ID, "gc.kind": "drain"})
	item := create(map[string]string{"gc.kind": "workflow", "gc.drain_control_id": drain.ID})
	itemStep := create(map[string]string{"gc.root_bead_id": item.ID})
	if err := store.SetMetadata(run.ID, "gc.cancel_requested", "true"); err != nil {
		t.Fatal(err)
	}

	for _, step := range []beads.Bead{runStep, itemStep} {
		stop, err := WorkflowStopReason(store, step)
		if err != nil {
			t.Fatal(err)
		}
		if stop.RootID != run.ID || !stop.Canceled || stop.Outcome() != "canceled" {
			t.Errorf("step %s under a run marked canceling: stop=%+v, want run %s canceled", step.ID, stop, run.ID)
		}
	}
}
