package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/fsys"
)

// These rows pin the done-workflow retirement and the stale reclaim of
// `gc hook --claim` on a split city (R6/B1, the #6475 treadmill).
//
// A graph.v2 workflow root and its steps live in the relocated binding; its
// synthetic input convoy and the source work it tracks stay in the work store.
// The production InputDone / SkipDoneWorkflow / ReclaimStale seams run a bd
// subprocess in the agent's work directory, so on a split city they could not
// read the root: the "input work already terminal, retire the workflow" path
// never fired, and the pool re-served the same step every tick.
//
// The work store here is a real FileStore and the binding a real SQLite store,
// fenced to the infrastructure namespaces. The base seams are the production
// functions (workflowInputDone, skipDoneWorkflow) over the work store, exactly
// as hookInputDoneWithBdStore runs them over the bd store.

type doneWorkflowSplitFixture struct {
	work     beads.Store
	binding  beads.Store
	route    *hookClaimClassRoute
	source   beads.Bead
	convoyID string
	root     beads.Bead
	step     beads.Bead
	done     beads.Bead
}

func newDoneWorkflowSplitFixture(t *testing.T, sourceClosed bool) doneWorkflowSplitFixture {
	t.Helper()
	work, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), ".gc", "beads.json"), beads.WithFileStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("opening the file work store: %v", err)
	}
	binding := newSplitEnvClassLeaf(t)
	f := doneWorkflowSplitFixture{work: work, binding: binding}

	f.source, err = work.Create(beads.Bead{Title: "source work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if sourceClosed {
		if err := work.Close(f.source.ID); err != nil {
			t.Fatal(err)
		}
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

// baseOps are the work-scope seams: the production functions over the work
// store, the way the default seams run them over the agent's bd store.
func (f doneWorkflowSplitFixture) baseOps(t *testing.T) hookClaimOps {
	t.Helper()
	return hookClaimOps{
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead, ids []string) (string, bool, error) {
			return workflowInputDone(f.work, candidate, ids)
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			return skipDoneWorkflow(f.work, rootID)
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
			return f.work.Get(beadID)
		},
		WorkflowStore: func(context.Context, string, []string, string, string) beads.Store {
			return f.work
		},
		ReclaimStale: func(_ context.Context, _ string, _ []string, beadID string) (bool, string, error) {
			if _, err := f.work.Get(beadID); err != nil {
				return false, "", err
			}
			return false, "", nil
		},
	}
}

func (f doneWorkflowSplitFixture) retire(t *testing.T, ops hookClaimOps, candidate beads.Bead) ([]beads.Bead, string) {
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

func (f doneWorkflowSplitFixture) assertSkipped(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		got, err := f.binding.Get(id)
		if err != nil {
			t.Fatalf("reading %s from the binding: %v", id, err)
		}
		if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped {
			t.Errorf("%s in the binding: status=%q outcome=%q, want closed/%s", id, got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], beadmeta.OutcomeSkipped)
		}
	}
}

func (f doneWorkflowSplitFixture) assertOpen(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		got, err := f.binding.Get(id)
		if err != nil {
			t.Fatalf("reading %s from the binding: %v", id, err)
		}
		if got.Status == "closed" {
			t.Errorf("%s in the binding was closed (outcome=%q); its input work is not done", id, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
}

// A step of a binding-resident workflow whose input work is terminal is
// retired: the root and its open steps close as skipped in the binding, and
// the candidate is dropped.
func TestClassRoutedHookRetiresABindingResidentDoneWorkflow(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, true)
	ops := classRoutedHookClaimOps(f.baseOps(t), f.route)

	kept, stderr := f.retire(t, ops, f.step)
	if len(kept) != 0 {
		t.Fatalf("candidate %s kept (stderr=%q); its workflow %s has terminal input work and must be retired", f.step.ID, stderr, f.root.ID)
	}
	f.assertSkipped(t, f.root.ID, f.step.ID)
	if src, err := f.work.Get(f.source.ID); err != nil || src.Status != "closed" {
		t.Errorf("source %s: status=%q err=%v; the retirement must not touch the work store", f.source.ID, src.Status, err)
	}
}

// The root itself as a candidate (it carries the input convoy) is retired the
// same way.
func TestClassRoutedHookRetiresABindingResidentDoneWorkflowRoot(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, true)
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
	f.assertSkipped(t, f.root.ID, f.step.ID)
}

// Open input work keeps the workflow live.
func TestClassRoutedHookKeepsABindingResidentWorkflowWithOpenInput(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, false)
	ops := classRoutedHookClaimOps(f.baseOps(t), f.route)

	kept, stderr := f.retire(t, ops, f.step)
	if len(kept) != 1 {
		t.Fatalf("candidate %s dropped (stderr=%q); its input work is still open", f.step.ID, stderr)
	}
	f.assertOpen(t, f.root.ID, f.step.ID)
}

// An input member assigned to a seat that ran a step of THIS workflow was never
// handed off (sys-pxryan.44). The seats are read from the steps, which live in
// the binding: read from the work store they would be empty and the member
// would read as handed off, retiring a live workflow.
func TestClassRoutedHookReadsWorkflowSeatsFromTheBinding(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, false)
	assignee := "worker-old"
	if err := f.work.Update(f.source.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}
	ops := classRoutedHookClaimOps(f.baseOps(t), f.route)

	kept, stderr := f.retire(t, ops, f.step)
	if len(kept) != 1 {
		t.Fatalf("candidate %s dropped (stderr=%q); its input member is held by %s, a seat of this workflow", f.step.ID, stderr, assignee)
	}
	f.assertOpen(t, f.root.ID, f.step.ID)
}

// The unrouted seams cannot see the root at all, which is the treadmill.
func TestUnroutedHookCannotRetireABindingResidentDoneWorkflow(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, true)
	kept, stderr := f.retire(t, f.baseOps(t), f.step)
	if len(kept) != 1 || !strings.Contains(stderr, "not found") {
		t.Fatalf("unrouted retirement kept=%d stderr=%q; expected the work-scope seam to miss the root (this row documents the bug the route closes)", len(kept), stderr)
	}
}

// reclaimingBinding is a binding that exposes a stale-lease reclaim, so a row
// can see the routed seam reach it.
type reclaimingBinding struct {
	beads.Store
	calls []string
	err   error
}

func (s *reclaimingBinding) Claim(id, assignee string) (beads.Bead, bool, error) {
	return s.Store.(interface {
		Claim(id, assignee string) (beads.Bead, bool, error)
	}).Claim(id, assignee)
}

func (s *reclaimingBinding) ReclaimStale(id string) (bool, string, error) {
	s.calls = append(s.calls, id)
	if s.err != nil {
		return false, "", s.err
	}
	return true, "worker-dead", nil
}

// ReclaimStale of a binding-resident bead runs against the binding: the work
// store's not-found is not an error the hook reports, and a binding with a
// stale-lease reclaim is asked for it.
func TestClassRoutedHookReclaimStaleFollowsTheBinding(t *testing.T) {
	t.Run("binding-reclaims", func(t *testing.T) {
		f := newDoneWorkflowSplitFixture(t, false)
		binding := &reclaimingBinding{Store: f.binding}
		route := newClaimRouteFor(t, binding)
		ops := classRoutedHookClaimOps(f.baseOps(t), route)
		reclaimed, prev, err := ops.ReclaimStale(context.Background(), "/work", nil, f.step.ID)
		if err != nil || !reclaimed || prev != "worker-dead" {
			t.Fatalf("routed ReclaimStale(%s) = (%v, %q, %v), want (true, worker-dead, nil)", f.step.ID, reclaimed, prev, err)
		}
		if len(binding.calls) != 1 || binding.calls[0] != f.step.ID {
			t.Fatalf("binding reclaim calls = %v, want [%s]", binding.calls, f.step.ID)
		}
	})
	t.Run("binding-without-lease-authority", func(t *testing.T) {
		f := newDoneWorkflowSplitFixture(t, false)
		ops := classRoutedHookClaimOps(f.baseOps(t), f.route)
		reclaimed, _, err := ops.ReclaimStale(context.Background(), "/work", nil, f.step.ID)
		if err != nil || reclaimed {
			t.Fatalf("routed ReclaimStale(%s) on a binding with no stale-lease reclaim = (%v, %v), want (false, nil): nothing to reclaim, not a missing bead", f.step.ID, reclaimed, err)
		}
	})
	t.Run("work-resident", func(t *testing.T) {
		f := newDoneWorkflowSplitFixture(t, false)
		ops := classRoutedHookClaimOps(f.baseOps(t), f.route)
		if _, _, err := ops.ReclaimStale(context.Background(), "/work", nil, f.source.ID); err != nil {
			t.Fatalf("routed ReclaimStale of work-resident %s: %v", f.source.ID, err)
		}
	})
}
