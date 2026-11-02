package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/fsys"
)

// These rows pin the input-purged cancel of `gc hook --claim` on a split
// city (city-4uo). The done-check twin (hook_claim_done_workflow_split_test.go)
// retires a workflow whose input work is terminal; this one covers the arm
// for an input that is GONE — the convoy bead or the work it tracks purged
// from the work store — where the done-check sees only placeholders and
// ambiguity, fails open, and the pool re-serves the step forever.
//
// Same topology as the done fixture: the workflow root and its steps live in
// the relocated binding, the synthetic input convoy and the work it tracks in
// the work store. The purge evidence lives in the work store; the cancel
// writes land in the binding.

type purgedWorkflowSplitFixture struct {
	work     beads.Store
	binding  beads.Store
	route    *hookClaimClassRoute
	source   beads.Bead
	convoyID string
	root     beads.Bead
	step     beads.Bead
	done     beads.Bead
}

func newPurgedWorkflowSplitFixture(t *testing.T) purgedWorkflowSplitFixture {
	t.Helper()
	work, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), ".gc", "beads.json"), beads.WithFileStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("opening the file work store: %v", err)
	}
	binding := newSplitEnvClassLeaf(t)
	f := purgedWorkflowSplitFixture{work: work, binding: binding}

	f.source, err = work.Create(beads.Bead{Title: "source work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := work.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	f.convoyID = conv.ID
	if err := convoy.TrackItem(work, conv.ID, f.source.ID); err != nil {
		t.Fatalf("tracking the source in the input convoy: %v", err)
	}

	f.root, err = binding.Create(beads.Bead{Title: "workflow root", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   conv.ID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.step, err = binding.Create(beads.Bead{Title: "open step", Type: "task", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: f.root.ID,
		beadmeta.RoutedToMetadataKey:   "worker",
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.done, err = binding.Create(beads.Bead{Title: "finished step", Type: "task", Assignee: "worker-old", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: f.root.ID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.Close(f.done.ID); err != nil {
		t.Fatal(err)
	}
	f.route = newClaimRouteFor(t, binding)
	return f
}

// purgeOps are the work-scope seams, the production functions over the work
// store, with the purge arm beside the done arm.
func (f purgedWorkflowSplitFixture) purgeOps() hookClaimOps {
	return hookClaimOps{
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead, ids []string) (string, bool, error) {
			return workflowInputDone(f.work, candidate, ids)
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			return skipDoneWorkflow(beads.GraphStore{Store: f.work}, rootID)
		},
		InputPurged: func(_ context.Context, _ string, _ []string, candidate beads.Bead, _ []string) (string, bool, error) {
			return workflowInputPurged(beads.GraphStore{Store: f.work}, beads.WorkStore{Store: f.work}, candidate)
		},
		CancelInputPurgedWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			return cancelInputPurgedWorkflow(beads.GraphStore{Store: f.work}, rootID)
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
			return f.work.Get(beadID)
		},
		WorkflowStore: func(context.Context, string, []string, string, string) beads.Store {
			return f.work
		},
	}
}

func (f purgedWorkflowSplitFixture) retire(t *testing.T, ops hookClaimOps, candidate beads.Bead) ([]beads.Bead, string) {
	t.Helper()
	var stderr bytes.Buffer
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
	}
	kept := retireDoneWorkflowCandidates([]beads.Bead{candidate}, opts, ops, "/work", &stderr)
	return kept, stderr.String()
}

func (f purgedWorkflowSplitFixture) assertFailedInBinding(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		got, err := f.binding.Get(id)
		if err != nil {
			t.Fatalf("reading %s from the binding: %v", id, err)
		}
		if got.Status != "closed" {
			t.Errorf("%s in the binding: status=%q, want closed", id, got.Status)
			continue
		}
		if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail ||
			got.Metadata[beadmeta.FailureClassMetadataKey] != beadmeta.FailureClassHard ||
			got.Metadata[beadmeta.FailureReasonMetadataKey] != hookInputPurgedFailureReason {
			t.Errorf("%s in the binding: outcome=%q class=%q reason=%q, want fail/hard/%s",
				id, got.Metadata[beadmeta.OutcomeMetadataKey], got.Metadata[beadmeta.FailureClassMetadataKey],
				got.Metadata[beadmeta.FailureReasonMetadataKey], hookInputPurgedFailureReason)
		}
	}
}

// A step of a binding-resident workflow whose input convoy was purged from
// the work store is failed, not served: the root and its open steps close
// with the hard input_purged record in the binding, the already-closed step
// keeps its recorded outcome, and the candidate is dropped.
func TestClassRoutedHookFailsABindingResidentWorkflowWithPurgedConvoy(t *testing.T) {
	f := newPurgedWorkflowSplitFixture(t)
	if err := f.work.Delete(f.convoyID); err != nil {
		t.Fatalf("purging the convoy: %v", err)
	}
	ops := classRoutedHookClaimOps(f.purgeOps(), f.route)

	kept, stderr := f.retire(t, ops, f.step)
	if len(kept) != 0 {
		t.Fatalf("candidate %s kept (stderr=%q); its input convoy is purged and must be failed", f.step.ID, stderr)
	}
	f.assertFailedInBinding(t, f.root.ID, f.step.ID)
	survivor, err := f.binding.Get(f.done.ID)
	if err != nil {
		t.Fatalf("reading the finished step: %v", err)
	}
	if survivor.Metadata[beadmeta.OutcomeMetadataKey] != "" || survivor.Metadata[beadmeta.FailureReasonMetadataKey] == hookInputPurgedFailureReason {
		t.Errorf("finished step %s re-stamped: outcome=%q reason=%q; a recorded outcome must survive the cancel",
			f.done.ID, survivor.Metadata[beadmeta.OutcomeMetadataKey], survivor.Metadata[beadmeta.FailureReasonMetadataKey])
	}
}

// The purged MEMBER (the convoy survives with a dangling tracks dep) is the
// same verdict through the placeholder arm.
func TestClassRoutedHookFailsABindingResidentWorkflowWithPurgedMember(t *testing.T) {
	f := newPurgedWorkflowSplitFixture(t)
	if err := f.work.Delete(f.source.ID); err != nil {
		t.Fatalf("purging the source work: %v", err)
	}
	ops := classRoutedHookClaimOps(f.purgeOps(), f.route)

	kept, stderr := f.retire(t, ops, f.step)
	if len(kept) != 0 {
		t.Fatalf("candidate %s kept (stderr=%q); its tracked work is purged and must be failed", f.step.ID, stderr)
	}
	f.assertFailedInBinding(t, f.root.ID, f.step.ID)
}

// Live input work keeps the workflow live: the purge arm must not fire on a
// member that still resolves.
func TestClassRoutedHookKeepsABindingResidentWorkflowWithLivePurgeProbe(t *testing.T) {
	f := newPurgedWorkflowSplitFixture(t)
	ops := classRoutedHookClaimOps(f.purgeOps(), f.route)

	kept, stderr := f.retire(t, ops, f.step)
	if len(kept) != 1 {
		t.Fatalf("candidate %s dropped (stderr=%q); its input work is still live", f.step.ID, stderr)
	}
	for _, id := range []string{f.root.ID, f.step.ID} {
		got, err := f.binding.Get(id)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if got.Status == "closed" {
			t.Errorf("%s was closed (outcome=%q); its input is live", id, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
}
