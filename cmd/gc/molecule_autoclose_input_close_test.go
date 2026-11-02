package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
)

// seedConvoyWorkflow builds a single store holding a work bead, an input
// convoy tracking it, and a graph.v2 workflow root (gc.input_convoy_id)
// with one open and one closed step. Extra tracks members can be added by
// the caller to model a convoy with more than one tracked bead.
func seedConvoyWorkflow(t *testing.T) (*beads.MemStore, beads.Bead, beads.Bead, beads.Bead, beads.Bead, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "the work", Type: "task"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	if err := convoy.TrackItem(store, conv.ID, work.ID); err != nil {
		t.Fatalf("track: %v", err)
	}
	root, err := store.Create(beads.Bead{Title: "mol-polecat-work", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   conv.ID,
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step, err := store.Create(beads.Bead{Title: "implement", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
	}})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	doneStep, err := store.Create(beads.Bead{Title: "workspace-setup", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
	}})
	if err != nil {
		t.Fatalf("create closed step: %v", err)
	}
	if err := store.Close(doneStep.ID); err != nil {
		t.Fatalf("close step: %v", err)
	}
	return store, work, conv, root, step, doneStep
}

// Closing the input convoy's tracked work retires the still-in-flight
// instance right there: open steps close skipped with the input reason, the
// root closes through the shared announcer, and the already-closed step
// keeps its recorded outcome. This is the close-time half of city-4uo — the
// claim path's done-check would only retire the instance at some seat's next
// poll, and never on a pool no seat polls.
func TestMoleculeAutocloseCancelsInFlightWorkflowOnInputBeadClose(t *testing.T) {
	store, work, _, root, step, doneStep := seedConvoyWorkflow(t)

	if err := store.Close(work.ID); err != nil {
		t.Fatalf("close work: %v", err)
	}
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, work.ID, &out, beads.GraphStore{Store: store})

	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if got.Status != "closed" {
			t.Errorf("%s: status=%q, want closed (stdout=%q)", id, got.Status, out.String())
			continue
		}
		if got.Metadata["close_reason"] != moleculeInputClosedReason {
			t.Errorf("%s: close_reason=%q, want %q", id, got.Metadata["close_reason"], moleculeInputClosedReason)
		}
	}
	// The step closes through the skip path (outcome=skipped); the root
	// closes through the shared announcer, which stamps close_reason only —
	// the same convention moleculeSourceAutocloseReason follows.
	gotRoot, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("reading root: %v", err)
	}
	if gotRoot.Metadata[beadmeta.OutcomeMetadataKey] != "" {
		t.Errorf("root outcome=%q, want no outcome stamp (announcer convention)", gotRoot.Metadata[beadmeta.OutcomeMetadataKey])
	}
	gotStep, err := store.Get(step.ID)
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if gotStep.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped {
		t.Errorf("step outcome=%q, want skipped", gotStep.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if !strings.Contains(out.String(), "Auto-closed molecule "+root.ID) {
		t.Errorf("stdout = %q, want the auto-close announcement for %s", out.String(), root.ID)
	}
	survivor, err := store.Get(doneStep.ID)
	if err != nil {
		t.Fatalf("reading the closed step: %v", err)
	}
	if survivor.Metadata["close_reason"] == moleculeInputClosedReason || survivor.Metadata[beadmeta.OutcomeMetadataKey] == beadmeta.OutcomeSkipped {
		t.Errorf("closed step %s was re-stamped: outcome=%q close_reason=%q; a recorded outcome must survive",
			doneStep.ID, survivor.Metadata[beadmeta.OutcomeMetadataKey], survivor.Metadata["close_reason"])
	}
}

// A convoy with a member still open is not fully terminal: the instance
// waits on the whole tracked set and must not be retired by this member's
// close.
func TestMoleculeAutocloseLeavesInFlightWorkflowWhenConvoyStillOpen(t *testing.T) {
	store, work, conv, root, step, _ := seedConvoyWorkflow(t)
	other, err := store.Create(beads.Bead{Title: "the other work", Type: "task"})
	if err != nil {
		t.Fatalf("create second work: %v", err)
	}
	if err := convoy.TrackItem(store, conv.ID, other.ID); err != nil {
		t.Fatalf("track second member: %v", err)
	}

	if err := store.Close(work.ID); err != nil {
		t.Fatalf("close work: %v", err)
	}
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, work.ID, &out, beads.GraphStore{Store: store})

	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if got.Status == "closed" {
			t.Errorf("%s: closed (close_reason=%q); the convoy still tracks open work", id, got.Metadata["close_reason"])
		}
	}
}

// An unresolved placeholder in the tracked set is not terminal: the close
// leaves the instance alone and lets the claim path's purge arm own purge
// evidence.
func TestMoleculeAutocloseLeavesInFlightWorkflowOnPlaceholderMember(t *testing.T) {
	store, work, conv, root, step, _ := seedConvoyWorkflow(t)
	dangling, err := store.Create(beads.Bead{Title: "soon purged", Type: "task"})
	if err != nil {
		t.Fatalf("create dangling work: %v", err)
	}
	if err := convoy.TrackItem(store, conv.ID, dangling.ID); err != nil {
		t.Fatalf("track dangling member: %v", err)
	}
	if err := store.Delete(dangling.ID); err != nil {
		t.Fatalf("purge dangling work: %v", err)
	}

	if err := store.Close(work.ID); err != nil {
		t.Fatalf("close work: %v", err)
	}
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, work.ID, &out, beads.GraphStore{Store: store})

	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if got.Status == "closed" {
			t.Errorf("%s: closed (close_reason=%q); the convoy has an unresolved member and is not terminal", id, got.Metadata["close_reason"])
		}
	}
}

// A root with no descendants is left alone — still instantiating, or the
// stepless-wisp shape the source-bead trigger owns — so closing the input
// bead cannot race a pour that is still landing its steps.
func TestMoleculeAutocloseLeavesSteplessWorkflowRootOnInputBeadClose(t *testing.T) {
	store, work, conv, root, _, _ := seedConvoyWorkflow(t)
	steps, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
		IncludeClosed: true,
	})
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("seed produced no steps to clear")
	}
	for _, s := range steps {
		if err := store.Delete(s.ID); err != nil {
			t.Fatalf("delete step %s: %v", s.ID, err)
		}
	}
	_ = conv

	if err := store.Close(work.ID); err != nil {
		t.Fatalf("close work: %v", err)
	}
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, work.ID, &out, beads.GraphStore{Store: store})

	got, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("reading root: %v", err)
	}
	if got.Status == "closed" {
		t.Errorf("stepless root %s: closed (close_reason=%q); the input trigger must leave it to the source trigger",
			root.ID, got.Metadata["close_reason"])
	}
}

// Closing an unrelated bead (nothing tracks it) is a no-op for the input
// trigger.
func TestMoleculeAutocloseInputTriggerNoopWithoutConvoy(t *testing.T) {
	store, work, _, root, step, _ := seedConvoyWorkflow(t)
	bystander, err := store.Create(beads.Bead{Title: "untracked bead", Type: "task"})
	if err != nil {
		t.Fatalf("create bystander: %v", err)
	}
	_ = work

	if err := store.Close(bystander.ID); err != nil {
		t.Fatalf("close bystander: %v", err)
	}
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, bystander.ID, &out, beads.GraphStore{Store: store})

	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if got.Status == "closed" {
			t.Errorf("%s: closed by a bystander close; nothing tracks the closed bead", id)
		}
	}
}
