package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// These rows pin the purge arm of `gc hook --claim` (#8729) on a split city
// (R6/B1), beside the done-workflow rows in hook_claim_done_workflow_split_test.go.
//
// A graph.v2 workflow root and its steps live in the binding while the
// synthetic input convoy and the work it tracks stay in the work store. The
// done arm already reads across the two halves (workflowInputDoneAcross), but
// the purge probe and its retirement ran over the WorkflowStore seam alone,
// which the route answers per BEAD: probed with a binding-resident STEP the
// seam returns the work store, the root read fails there, and the probe
// errors into serving — the split-city shape of the #8729 treadmill, one
// burned claim cycle per seat. The routed seams read the root from the store
// that holds it, frame the proof over both halves, and land the retirement in
// the binding.

// assertRetiredPurged asserts each id is closed with the purged-input
// disposition in store: fail/hard/input_purged under the shared close reason.
func assertRetiredPurged(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if got.Status != "closed" ||
			got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail ||
			got.Metadata[beadmeta.FailureClassMetadataKey] != beadmeta.FailureClassHard ||
			got.Metadata[beadmeta.FailureReasonMetadataKey] != beadmeta.FailureReasonInputPurged ||
			got.Metadata["close_reason"] != hookRetirePurgedWorkflowCloseReason {
			t.Errorf("%s: status=%q outcome=%q failure_class=%q failure_reason=%q close_reason=%q; want closed/%s/%s/%s/%s",
				id, got.Status,
				got.Metadata[beadmeta.OutcomeMetadataKey],
				got.Metadata[beadmeta.FailureClassMetadataKey],
				got.Metadata[beadmeta.FailureReasonMetadataKey],
				got.Metadata["close_reason"],
				beadmeta.OutcomeFail, beadmeta.FailureClassHard, beadmeta.FailureReasonInputPurged, hookRetirePurgedWorkflowCloseReason)
		}
	}
}

// assertTerminalDispositionKept asserts a step the retirement must not touch:
// still closed, still carrying the disposition its own close wrote, with no
// purged-input stamp over it.
func assertTerminalDispositionKept(t *testing.T, store beads.Store, id string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	if got.Status != "closed" {
		t.Errorf("%s: status=%q; a terminal step keeps its own close", id, got.Status)
	}
	if got.Metadata[beadmeta.FailureReasonMetadataKey] != "" || got.Metadata[beadmeta.OutcomeMetadataKey] != "" {
		t.Errorf("%s: outcome=%q failure_reason=%q; the retirement must not restamp a terminal step", id,
			got.Metadata[beadmeta.OutcomeMetadataKey], got.Metadata[beadmeta.FailureReasonMetadataKey])
	}
}

// A step of a binding-resident workflow whose input convoy was purged is
// retired: the root and the open step close fail/hard/input_purged IN THE
// BINDING, the terminal step keeps its own close, and the work store is
// untouched.
func TestClassRoutedHookRetiresABindingResidentPurgedWorkflow(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, false)
	if err := f.work.Delete(f.convoyID); err != nil {
		t.Fatalf("purging the input convoy: %v", err)
	}
	ops := classRoutedHookClaimOps(f.baseOps(t), f.route)

	kept, stderr := f.retire(t, ops, f.step)
	if len(kept) != 0 {
		t.Fatalf("candidate %s kept (stderr=%q); its workflow %s has a purged input and must be retired", f.step.ID, stderr, f.root.ID)
	}
	assertRetiredPurged(t, f.binding, f.root.ID, f.step.ID)
	assertTerminalDispositionKept(t, f.binding, f.done.ID)
	if src, err := f.work.Get(f.source.ID); err != nil || src.Status != "open" {
		t.Errorf("source %s: status=%q err=%v; the retirement must not touch the work store", f.source.ID, src.Status, err)
	}
}

// The root itself as a candidate (it carries the input convoy) is retired the
// same way.
func TestClassRoutedHookRetiresABindingResidentPurgedWorkflowRoot(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, false)
	if err := f.work.Delete(f.convoyID); err != nil {
		t.Fatalf("purging the input convoy: %v", err)
	}
	if err := f.binding.SetMetadata(f.root.ID, beadmeta.RoutedToMetadataKey, "worker"); err != nil {
		t.Fatal(err)
	}
	root, err := f.binding.Get(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	ops := classRoutedHookClaimOps(f.baseOps(t), f.route)

	kept, stderr := f.retire(t, ops, root)
	if len(kept) != 0 {
		t.Fatalf("root candidate %s kept (stderr=%q), want retired", root.ID, stderr)
	}
	assertRetiredPurged(t, f.binding, f.root.ID, f.step.ID)
	assertTerminalDispositionKept(t, f.binding, f.done.ID)
}

// The #8729 original shape on a split city: the convoy row survives but the
// one work bead it tracks was purged, leaving a dangling tracks edge. The
// routed probe reads the edge's target as evidence and retires in the binding.
func TestClassRoutedHookRetiresABindingResidentWorkflowWhoseTrackedWorkWasPurged(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, false)
	if err := f.work.Delete(f.source.ID); err != nil {
		t.Fatalf("purging the tracked work: %v", err)
	}
	ops := classRoutedHookClaimOps(f.baseOps(t), f.route)

	kept, stderr := f.retire(t, ops, f.step)
	if len(kept) != 0 {
		t.Fatalf("candidate %s kept (stderr=%q); every tracked input of workflow %s is gone and it must be retired", f.step.ID, stderr, f.root.ID)
	}
	assertRetiredPurged(t, f.binding, f.root.ID, f.step.ID)
	assertTerminalDispositionKept(t, f.binding, f.done.ID)
}

// A workflow that lives in the work store keeps the unrouted seams even under
// a route: the retirement lands in the work store, and the binding-resident
// workflow beside it is untouched.
func TestClassRoutedHookRetiresAWorkResidentPurgedWorkflowThroughTheBaseSeams(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, false)
	if err := f.work.Delete(f.convoyID); err != nil {
		t.Fatalf("purging the input convoy: %v", err)
	}
	root, err := f.work.Create(beads.Bead{Title: "work workflow root", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   f.convoyID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	step, err := f.work.Create(beads.Bead{Title: "work step", Type: "task", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.RoutedToMetadataKey:   "worker",
	}})
	if err != nil {
		t.Fatal(err)
	}
	ops := classRoutedHookClaimOps(f.baseOps(t), f.route)

	kept, stderr := f.retire(t, ops, step)
	if len(kept) != 0 {
		t.Fatalf("candidate %s kept (stderr=%q); its workflow %s has a purged input and must be retired", step.ID, stderr, root.ID)
	}
	assertRetiredPurged(t, f.work, root.ID, step.ID)
	f.assertOpen(t, f.root.ID, f.step.ID)
	assertTerminalDispositionKept(t, f.binding, f.done.ID)
}
