package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// These rows pin the retirement half of the purge arm of `gc hook --claim`
// (#8729): where its writes land, which rows it leaves alone, and how a write
// that stops part-way converges on the next claim. Each runs in both shapes a
// workflow takes: single-store, where the root and its steps share the work
// store with the input, and binding-resident (R6/B1), where the root and its
// steps live in the class binding while the input convoy and the work it
// tracks stay in the work store.

// purgeSplitOps are the claim ops of the split fixture with the purge arm
// wired the way the claim path wires it: the complete-city proof over the
// work store the fixture's rigless city holds, and the retirement over the
// work store, which the route redirects to the binding for a resident root.
func purgeSplitOps(t *testing.T, f doneWorkflowSplitFixture) hookClaimOps {
	t.Helper()
	base := f.baseOps(t)
	base.InputVerdict = func(_ context.Context, _ string, _ []string, candidate beads.Bead, identityCandidates []string) (workflowInputVerdict, error) {
		return workflowInputDoneAcross(f.work, beadstest.Work(f.work), candidate, identityCandidates)
	}
	base.InputPurged = proveOver(purgeFrameOverWork(t, f.work))
	base.RetirePurgedWorkflow = func(ctx context.Context, _ string, _ []string, rootID string) error {
		return retirePurgedWorkflow(ctx, f.work, rootID)
	}
	return classRoutedHookClaimOps(base, f.route)
}

// purgeSplitConvoy purges the fixture's input convoy. A purge takes the
// convoy's tracks edge with it; the store's Delete does not cascade, so the
// edge goes first.
func purgeSplitConvoy(t *testing.T, f doneWorkflowSplitFixture) {
	t.Helper()
	if err := f.work.DepRemove(f.convoyID, f.source.ID); err != nil {
		t.Fatalf("purging the input convoy's tracks edge: %v", err)
	}
	if err := f.work.Delete(f.convoyID); err != nil {
		t.Fatalf("purging the input convoy: %v", err)
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

// The root itself as a candidate (it carries the input convoy) is retired in
// the binding the same way its step is.
func TestClassRoutedHookRetiresABindingResidentPurgedWorkflowRoot(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, false)
	purgeSplitConvoy(t, f)
	if err := f.binding.SetMetadata(f.root.ID, beadmeta.RoutedToMetadataKey, "worker"); err != nil {
		t.Fatal(err)
	}
	root, err := f.binding.Get(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}

	kept, stderr := f.retire(t, purgeSplitOps(t, f), root)
	if len(kept) != 0 {
		t.Fatalf("root candidate %s kept (stderr=%q), want retired", root.ID, stderr)
	}
	assertRetiredAsInputPurged(t, f.binding, f.root.ID, f.step.ID)
	assertTerminalDispositionKept(t, f.binding, f.done.ID)
}

// The #8729 original shape on a split city: the synthetic convoy row survives
// but the one work bead it records was purged. The proof reads the recorded
// target as evidence, and the retirement lands in the binding while the work
// store's convoy is left alone.
func TestClassRoutedHookRetiresABindingResidentWorkflowWhoseTrackedWorkWasPurged(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, false)
	for key, value := range map[string]string{
		beadmeta.SyntheticMetadataKey:     "true",
		beadmeta.InputTargetIDMetadataKey: f.source.ID,
	} {
		if err := f.work.SetMetadata(f.convoyID, key, value); err != nil {
			t.Fatalf("marking the input convoy synthetic: %v", err)
		}
	}
	if err := f.work.DepRemove(f.convoyID, f.source.ID); err != nil {
		t.Fatalf("purging the tracked work's edge: %v", err)
	}
	if err := f.work.Delete(f.source.ID); err != nil {
		t.Fatalf("purging the tracked work: %v", err)
	}

	kept, stderr := f.retire(t, purgeSplitOps(t, f), f.step)
	if len(kept) != 0 {
		t.Fatalf("candidate %s kept (stderr=%q); the tracked input of workflow %s is gone and it must be retired", f.step.ID, stderr, f.root.ID)
	}
	assertRetiredAsInputPurged(t, f.binding, f.root.ID, f.step.ID)
	assertTerminalDispositionKept(t, f.binding, f.done.ID)
	if conv, err := f.work.Get(f.convoyID); err != nil || conv.Status != "open" {
		t.Errorf("input convoy %s: status=%q err=%v; the retirement must not touch the work store", f.convoyID, conv.Status, err)
	}
}

// A workflow that lives in the work store keeps the unrouted seams even under
// a route: the retirement lands in the work store, and the binding-resident
// workflow beside it is untouched.
func TestClassRoutedHookRetiresAWorkResidentPurgedWorkflowThroughTheBaseSeams(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, false)
	purgeSplitConvoy(t, f)
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

	kept, stderr := f.retire(t, purgeSplitOps(t, f), step)
	if len(kept) != 0 {
		t.Fatalf("candidate %s kept (stderr=%q); its workflow %s has a purged input and must be retired", step.ID, stderr, root.ID)
	}
	assertRetiredAsInputPurged(t, f.work, root.ID, step.ID)
	f.assertOpen(t, f.root.ID, f.step.ID)
	assertTerminalDispositionKept(t, f.binding, f.done.ID)
}

// purgePartialCloseStore lands only the first id of a CloseAll and then fails,
// for as long as fail is set. It forwards the compare-and-swap claim so a
// class route admits it as a binding.
type purgePartialCloseStore struct {
	beads.Store
	fail bool
}

func (s *purgePartialCloseStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if !s.fail {
		return s.Store.CloseAll(ids, metadata)
	}
	n, err := s.Store.CloseAll(ids[:1], metadata)
	if err != nil {
		return n, err
	}
	return n, errors.New("injected close failure")
}

func (s *purgePartialCloseStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	claimer, ok := s.Store.(interface {
		Claim(id, assignee string) (beads.Bead, bool, error)
	})
	if !ok {
		return beads.Bead{}, false, fmt.Errorf("%T has no compare-and-swap claim", s.Store)
	}
	return claimer.Claim(id, assignee)
}

// purgeRetryShape is one workflow shape the partial-write row runs in: the
// store that holds the workflow (wrapped to fail part-way), its candidate
// step, the steps the first write may leave open, and its claim ops.
type purgeRetryShape struct {
	holder    beads.Store
	partial   *purgePartialCloseStore
	root      beads.Bead
	candidate beads.Bead
	open      []string
	finished  string
	ops       hookClaimOps
}

func singleStorePurgeRetryShape(t *testing.T) purgeRetryShape {
	store := beads.NewMemStore()
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	root, open, inProgress, finished := seedInputWorkflow(t, store, conv.ID)
	if err := store.Delete(conv.ID); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}
	partial := &purgePartialCloseStore{Store: store, fail: true}
	ops := purgeClaimOps(t, store, proveOver(purgeFrameOverWork(t, store)), []beads.Bead{open}, new(string))
	ops.RetirePurgedWorkflow = func(ctx context.Context, _ string, _ []string, rootID string) error {
		return retirePurgedWorkflow(ctx, partial, rootID)
	}
	return purgeRetryShape{holder: store, partial: partial, root: root, candidate: open, open: []string{open.ID, inProgress.ID}, finished: finished.ID, ops: ops}
}

func bindingResidentPurgeRetryShape(t *testing.T) purgeRetryShape {
	f := newDoneWorkflowSplitFixture(t, false)
	inProgress, err := f.binding.Create(beads.Bead{Title: "in-progress step", Type: "task", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: f.root.ID,
		beadmeta.RoutedToMetadataKey:   "worker",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.binding.Update(inProgress.ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr("worker-2")}); err != nil {
		t.Fatalf("start step: %v", err)
	}
	purgeSplitConvoy(t, f)
	partial := &purgePartialCloseStore{Store: f.binding, fail: true}
	f.route = newClaimRouteFor(t, partial)
	return purgeRetryShape{holder: f.binding, partial: partial, root: f.root, candidate: f.step, open: []string{f.step.ID, inProgress.ID}, finished: f.done.ID, ops: purgeSplitOps(t, f)}
}

// A purge retirement whose write stops part-way keeps the candidate and logs
// the workflow and the error, leaving the root open; the retry after the store
// recovers retires the rest and keeps every disposition already recorded —
// the one the failed write left, one a worker recorded since, and one from
// before the purge.
func TestHookInputPurgeRetirementFailure(t *testing.T) {
	for name, newShape := range map[string]func(*testing.T) purgeRetryShape{
		"single-store":     singleStorePurgeRetryShape,
		"binding-resident": bindingResidentPurgeRetryShape,
	} {
		t.Run(name, func(t *testing.T) {
			s := newShape(t)
			opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}}

			var stderr bytes.Buffer
			kept := retireDoneWorkflowCandidates([]beads.Bead{s.candidate}, opts, s.ops, "/work", &stderr)
			if len(kept) != 1 || kept[0].ID != s.candidate.ID {
				t.Fatalf("kept = %v after a failed retirement write, want [%s] (stderr=%q)", kept, s.candidate.ID, stderr.String())
			}
			for _, want := range []string{s.root.ID, "injected close failure"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to name %q", stderr.String(), want)
				}
			}
			if root, err := s.holder.Get(s.root.ID); err != nil || root.Status == "closed" {
				t.Fatalf("root %s: status=%q err=%v; a failed write must leave it open for the retry", s.root.ID, root.Status, err)
			}
			var landed, left beads.Bead
			for _, id := range s.open {
				got, err := s.holder.Get(id)
				if err != nil {
					t.Fatalf("get %s: %v", id, err)
				}
				if got.Status == "closed" {
					landed = got
				} else {
					left = got
				}
			}
			if landed.ID == "" || left.ID == "" {
				t.Fatalf("after the partial write landed=%q left=%q; the fixture lands exactly the batch's first id", landed.ID, left.ID)
			}
			if _, err := s.holder.CloseAll([]string{left.ID}, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}); err != nil {
				t.Fatalf("finishing %s: %v", left.ID, err)
			}

			s.partial.fail = false
			stderr.Reset()
			if kept := retireDoneWorkflowCandidates([]beads.Bead{s.candidate}, opts, s.ops, "/work", &stderr); len(kept) != 0 {
				t.Fatalf("candidate %s kept on retry (stderr=%q), want retired", s.candidate.ID, stderr.String())
			}
			assertRetiredAsInputPurged(t, s.holder, s.root.ID, landed.ID)
			for id, outcome := range map[string]string{left.ID: beadmeta.OutcomePass, s.finished: ""} {
				got, err := s.holder.Get(id)
				if err != nil {
					t.Fatalf("get %s: %v", id, err)
				}
				if got.Status != "closed" || got.Metadata[beadmeta.FailureReasonMetadataKey] != "" {
					t.Errorf("%s = %s %v; a step that finished on its own keeps its disposition", id, got.Status, got.Metadata)
				}
				if outcome != "" && got.Metadata[beadmeta.OutcomeMetadataKey] != outcome {
					t.Errorf("%s outcome = %q, want %q kept", id, got.Metadata[beadmeta.OutcomeMetadataKey], outcome)
				}
			}
			if got, err := s.holder.Get(landed.ID); err != nil || got.Revision != landed.Revision {
				t.Errorf("step %s revision %d -> %d (err=%v); the retry must not rewrite a step the failed write already closed", landed.ID, landed.Revision, got.Revision, err)
			}
		})
	}
}

// A retirement keeps a root a re-read finds already terminal: its own close
// and disposition stand, and only the open steps under it are retired. When
// nothing under the root is open either, nothing is written.
func TestRetirePurgedWorkflowKeepsATerminalRoot(t *testing.T) {
	store := beads.NewMemStore()
	root, open, inProgress, finished := seedInputWorkflow(t, store, "gc-gone")
	if _, err := store.CloseAll([]string{root.ID}, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}); err != nil {
		t.Fatalf("closing root: %v", err)
	}
	closedRoot, err := store.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := retirePurgedWorkflow(context.Background(), store, root.ID); err != nil {
		t.Fatalf("retirePurgedWorkflow: %v", err)
	}
	assertRetiredAsInputPurged(t, store, open.ID, inProgress.ID)
	got, err := store.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != closedRoot.Revision || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass || got.Metadata[beadmeta.FailureReasonMetadataKey] != "" {
		t.Fatalf("root %s = rev %d %v; an already-terminal root keeps its own close (rev %d)", root.ID, got.Revision, got.Metadata, closedRoot.Revision)
	}
	finishedBefore, err := store.Get(finished.ID)
	if err != nil {
		t.Fatal(err)
	}

	refusing := &purgePartialCloseStore{Store: store, fail: true}
	if err := retirePurgedWorkflow(context.Background(), refusing, root.ID); err != nil {
		t.Fatalf("retirePurgedWorkflow over a fully terminal workflow wrote (or failed): %v", err)
	}
	if got, err := store.Get(finished.ID); err != nil || got.Revision != finishedBefore.Revision {
		t.Fatalf("finished step %s rewritten (rev %d -> %d, err=%v)", finished.ID, finishedBefore.Revision, got.Revision, err)
	}
}

// A retirement whose mutation deadline has passed by the time its reads are
// done writes nothing: the close is bounded by the fresh post-proof deadline,
// not merely started under it.
func TestRetirePurgedWorkflowWritesNothingPastItsDeadline(t *testing.T) {
	store := beads.NewMemStore()
	root, open, inProgress, _ := seedInputWorkflow(t, store, "gc-gone")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := retirePurgedWorkflow(ctx, store, root.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retirePurgedWorkflow past its deadline = %v, want context.DeadlineExceeded", err)
	}
	assertNotRetired(t, store, map[string]string{root.ID: "open", open.ID: "open", inProgress.ID: "in_progress"})
}
