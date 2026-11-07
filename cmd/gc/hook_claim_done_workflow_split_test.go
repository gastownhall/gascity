package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/fsys"
)

// These rows pin the done-workflow retirement and the stale reclaim of
// `gc hook --claim` on a split city (R6/B1, the #6475 treadmill).
//
// A graph.v2 workflow root and its steps live in the relocated binding; its
// synthetic input convoy and the source work it tracks stay in the work store.
// The production InputVerdict / RetireWorkflow / ReclaimStale seams run a bd
// subprocess in the agent's work directory, so on a split city they could not
// read the root: the "input work already terminal, retire the workflow" path
// never fired, and the pool re-served the same step every tick.
//
// The work store here is a real FileStore and the binding a real SQLite store,
// fenced to the infrastructure namespaces. The base seams are the production
// functions (workflowInputDone, skipDoneWorkflow, failPurgedWorkflow) over the
// work store, exactly as hookInputDoneWithBdStore and
// hookRetireWorkflowWithBdStore run them over the bd store.

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
	if err := convoy.TrackItemIn(convoy.MemberClasses{Convoy: beadstest.Work(work)}, conv.ID, f.source.ID); err != nil {
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

// stampedStep adds to the binding an open routed step that carries the input
// convoy id beside its root id, as a formula's step metadata may stamp it.
func (f doneWorkflowSplitFixture) stampedStep(t *testing.T) beads.Bead {
	t.Helper()
	step, err := f.binding.Create(beads.Bead{Title: "stamped step", Type: "task", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:    f.root.ID,
		beadmeta.InputConvoyIDMetadataKey: f.convoyID,
		beadmeta.RoutedToMetadataKey:      "worker",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return step
}

// baseOps are the work-scope seams: the production functions over the work
// store, the way the default seams run them over the agent's bd store.
func (f doneWorkflowSplitFixture) baseOps(t *testing.T) hookClaimOps {
	t.Helper()
	return hookClaimOps{
		InputVerdict: func(ctx context.Context, _ string, _ []string, candidate beads.Bead, ids []string, prove workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputDone(ctx, beadstest.Work(f.work), candidate, ids, prove)
		},
		RetireWorkflow: func(ctx context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
			if verdict.State == workflowInputPurged {
				return failPurgedWorkflow(ctx, f.work, verdict.RootID)
			}
			return skipDoneWorkflow(f.work, verdict.RootID)
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
// the candidate is dropped. A step that carries the input convoy id itself
// retires the same workflow through its root.
func TestClassRoutedHookRetiresABindingResidentDoneWorkflow(t *testing.T) {
	for _, stamped := range []bool{false, true} {
		t.Run(fmt.Sprintf("step stamped with the input id=%t", stamped), func(t *testing.T) {
			f := newDoneWorkflowSplitFixture(t, true)
			candidate := f.step
			if stamped {
				candidate = f.stampedStep(t)
			}
			ops := classRoutedHookClaimOps(f.baseOps(t), f.route)

			kept, stderr := f.retire(t, ops, candidate)
			if len(kept) != 0 {
				t.Fatalf("candidate %s kept (stderr=%q); its workflow %s has terminal input work and must be retired", candidate.ID, stderr, f.root.ID)
			}
			f.assertSkipped(t, f.root.ID, f.step.ID, candidate.ID)
			if src, err := f.work.Get(f.source.ID); err != nil || src.Status != "closed" {
				t.Errorf("source %s: status=%q err=%v; the retirement must not touch the work store", f.source.ID, src.Status, err)
			}
		})
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

// A binding-resident workflow whose input convoy is gone from the work store
// is retired only on the city-wide absence proof: input still held by another
// store keeps the candidate, and input absent everywhere closes the root and
// its open steps as purged in the binding, leaving the work store untouched.
func TestClassRoutedHookRetiresPurgedInput(t *testing.T) {
	for _, tc := range []struct {
		name         string
		absentInCity bool
		stamped      bool // the candidate is a step carrying the input convoy id
	}{
		{name: "input in another store", absentInCity: false},
		{name: "input absent everywhere", absentInCity: true},
		{name: "input absent everywhere, step stamped with the input id", absentInCity: true, stamped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDoneWorkflowSplitFixture(t, false)
			candidate := f.step
			if tc.stamped {
				candidate = f.stampedStep(t)
			}
			if err := f.work.Delete(f.convoyID); err != nil {
				t.Fatalf("purging the input convoy %s: %v", f.convoyID, err)
			}
			prover := &recordingProver{absent: map[string]bool{f.convoyID: tc.absentInCity}}
			ops := classRoutedHookClaimOps(f.baseOps(t), f.route)
			ops.ProveInputAbsent = prover.prove

			kept, stderr := f.retire(t, ops, candidate)
			if len(prover.calls) != 1 || !slices.Equal(prover.calls[0], []string{f.convoyID}) {
				t.Fatalf("prover calls = %v, want one proof of [%s]", prover.calls, f.convoyID)
			}
			if !tc.absentInCity {
				if len(kept) != 1 {
					t.Fatalf("candidate %s dropped (stderr=%q); its input convoy still exists in another store", candidate.ID, stderr)
				}
				f.assertOpen(t, f.root.ID, f.step.ID)
				return
			}
			if len(kept) != 0 {
				t.Fatalf("candidate %s kept (stderr=%q); its input convoy %s exists in no store", candidate.ID, stderr, f.convoyID)
			}
			assertPurgeRetired(t, f.binding, f.root.ID, f.step.ID, candidate.ID)
			if !strings.Contains(stderr, f.root.ID) || !strings.Contains(stderr, f.convoyID) {
				t.Errorf("stderr = %q, want it to name the retired root %s and the absent convoy %s", stderr, f.root.ID, f.convoyID)
			}
			if done, err := f.binding.Get(f.done.ID); err != nil || done.Status != "closed" ||
				done.Metadata[beadmeta.OutcomeMetadataKey] != "" || done.Metadata[beadmeta.FailureClassMetadataKey] != "" ||
				done.Metadata[beadmeta.FailureReasonMetadataKey] != "" || done.Metadata["close_reason"] != "" {
				t.Errorf("finished step %s: status=%q metadata=%v err=%v; its recorded outcome must survive", f.done.ID, done.Status, done.Metadata, err)
			}
			if src, err := f.work.Get(f.source.ID); err != nil || src.Status == "closed" {
				t.Errorf("source %s: status=%q err=%v; the retirement must not touch the work store", f.source.ID, src.Status, err)
			}
		})
	}
}

// fixtureProveAbsent proves ids absent from exactly stores: a not-found from
// each proves, one hit disproves, any other read error is returned. The
// production prover's own rules are pinned in hook_claim_input_purge_test.go;
// these rows need only a proof that spans the stores they stage.
func fixtureProveAbsent(stores ...workflowInputReader) workflowInputAbsenceProver {
	return func(ctx context.Context, ids []string) (bool, error) {
		for _, id := range ids {
			for _, store := range stores {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				_, err := store.Get(id)
				if err == nil {
					return false, nil
				}
				if !errors.Is(err, beads.ErrNotFound) {
					return false, fmt.Errorf("proving %s absent: %w", id, err)
				}
			}
		}
		return true, nil
	}
}

// A convoy gone from both legs the claim reads but still held by a third store
// the proof reads keeps a binding-resident workflow live: the third store is
// read once, the candidate stays, and nothing closes — even when the work
// store keeps the deleted convoy's edges to terminal work.
func TestClassRoutedHookKeepsPurgedInputWhenAThirdStoreHoldsTheConvoy(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, f doneWorkflowSplitFixture) string // returns the convoy id the root names
	}{{
		name: "convoy only in the third store",
		seed: func(t *testing.T, f doneWorkflowSplitFixture) string {
			if err := f.binding.SetMetadata(f.root.ID, beadmeta.InputConvoyIDMetadataKey, "gc-held-convoy"); err != nil {
				t.Fatal(err)
			}
			return "gc-held-convoy"
		},
	}, {
		name: "deleted convoy left terminal edges in the work store",
		seed: func(t *testing.T, f doneWorkflowSplitFixture) string {
			if err := f.work.Close(f.source.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.work.Delete(f.convoyID); err != nil {
				t.Fatalf("deleting the input convoy %s: %v", f.convoyID, err)
			}
			return f.convoyID
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDoneWorkflowSplitFixture(t, false)
			convoyID := tc.seed(t, f)
			third := &proveAbsentGetStore{id: convoyID, present: true}
			ops := classRoutedHookClaimOps(f.baseOps(t), f.route)
			ops.ProveInputAbsent = fixtureProveAbsent(f.work, f.binding, third)

			kept, stderr := f.retire(t, ops, f.step)

			if len(kept) != 1 {
				t.Fatalf("candidate %s dropped (stderr=%q); its input convoy still lives in a store the proof reads", f.step.ID, stderr)
			}
			f.assertOpen(t, f.root.ID, f.step.ID)
			if third.gets != 1 {
				t.Fatalf("third store read %d times, want the one read that found %s", third.gets, convoyID)
			}
		})
	}
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
