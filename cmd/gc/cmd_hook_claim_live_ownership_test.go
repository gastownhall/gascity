package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// gk-nmxnj: on 2026-11-03 the gk polecat pool twice served work that other
// sessions already held — a mid-flight molecule root whose steps were
// in_progress under two other seats (gk-2f6b2, under nux's steps), and a
// completed work bead whose submit step had stamped handoff_stage and
// reassigned it to the refinery (gk-vld2m, re-slung to a fresh polecat that
// then died). The claim path read neither ownership surface; these tests pin
// the two fences: the foreign-workflow-hold gate and the handoff marker.

// hold puts a created bead into a live status: MemStore.Create always lands
// rows open, so in_progress states must be a transition, exactly as
// cmd_hook_claim_workflow_stop_test.go does it.
func hold(t *testing.T, f *hookWorkflowStopFixture, id, status string) {
	t.Helper()
	st := status
	if err := f.store.Update(id, beads.UpdateOpts{Status: &st}); err != nil {
		t.Fatalf("hold %s: %v", id, err)
	}
}

func TestHookClaimWithholdsRootWithForeignInProgressStep(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	root := f.create(beads.Bead{Title: "mol root", Type: "molecule", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "worker",
	}})
	foreign := f.create(beads.Bead{Title: "implement step", Type: "task", Assignee: "nux", Metadata: map[string]string{
		"gc.root_bead_id": root.ID, "gc.routed_to": "worker",
	}})
	hold(t, f, foreign.ID, "in_progress")
	other := f.create(beads.Bead{Title: "unrelated pool work", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}})
	f.served = []string{root.ID, other.ID}

	handed, stderr := f.run()
	if handed == root.ID {
		t.Fatalf("hook served the foreign-held root %s; a third polecat must not be handed a molecule mid-flight elsewhere", root.ID)
	}
	if handed != other.ID {
		t.Fatalf("hook handed out %q, want the live pool bead %q behind the withheld root", handed, other.ID)
	}
	f.wantNeverClaimed(root.ID)
	if got := f.get(root.ID); got.Status != "open" {
		t.Fatalf("withheld root %s became status %q; withholding must not settle live foreign work", root.ID, got.Status)
	}
	if got := f.get(foreign.ID); got.Status != "in_progress" || got.Assignee != "nux" {
		t.Fatalf("held step mutated: status=%q assignee=%q", got.Status, got.Assignee)
	}
	if !strings.Contains(stderr, "not serving workflow root") || !strings.Contains(stderr, "in_progress under nux") {
		t.Fatalf("stderr missing the withhold disclosure: %s", stderr)
	}
}

func TestHookClaimForeignHoldIgnoresOwnStepsAndOpenSiblings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		assignee string
		session  string
	}{
		{name: "step held by this session", status: "in_progress", assignee: "worker-1"},
		{name: "step provenance is this session", status: "in_progress", session: "worker-1"},
		{name: "open unassigned siblings", status: "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookWorkflowStopFixture(t)
			root := f.create(beads.Bead{Title: "mol root", Type: "molecule", Metadata: map[string]string{
				"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "worker",
			}})
			meta := map[string]string{"gc.root_bead_id": root.ID, "gc.routed_to": "worker"}
			if tc.session != "" {
				meta["gc.session_id"] = tc.session
			}
			step := f.create(beads.Bead{Title: "a step", Type: "task", Assignee: tc.assignee, Metadata: meta})
			if tc.status == "in_progress" {
				hold(t, f, step.ID, tc.status)
			}
			f.served = []string{root.ID}
			handed, stderr := f.run()
			if handed != root.ID {
				t.Fatalf("root withheld on non-foreign evidence (%s): handed=%q stderr=%s", tc.name, handed, stderr)
			}
		})
	}
}

func TestHookClaimForeignHoldDeadSeatProvenanceBlocks(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	root := f.create(beads.Bead{Title: "mol root", Type: "molecule", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "worker",
	}})
	// in_progress with the assignee already cleared but foreign claim
	// provenance: the holder died mid-step and salvage has not landed yet.
	// Taking the root now would re-run the molecule under a witness that may
	// still recover it.
	orphaned := f.create(beads.Bead{Title: "orphaned step", Type: "task", Metadata: map[string]string{
		"gc.root_bead_id": root.ID, "gc.routed_to": "worker", "gc.session_id": "city-wisp-dead",
	}})
	hold(t, f, orphaned.ID, "in_progress")
	other := f.create(beads.Bead{Title: "live pool work", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}})
	f.served = []string{root.ID, other.ID}

	handed, stderr := f.run()
	if handed != other.ID {
		t.Fatalf("handed=%q, want the pool bead past the root with a dead-seat held step; stderr=%s", handed, stderr)
	}
	f.wantNeverClaimed(root.ID)
	if !strings.Contains(stderr, "in_progress under city-wisp-dead") {
		t.Fatalf("stderr missing the provenance holder disclosure: %s", stderr)
	}
}

// foreignHoldListErrorStore wraps the fixture ledger so List fails the way a
// store read hiccup does — the gate must fail open, like the stop gate.
type foreignHoldListErrorStore struct {
	beads.Store
	calls int
}

func (s *foreignHoldListErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	s.calls++
	return nil, errors.New("store down")
}

func TestHookClaimForeignHoldFailsOpen(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	root := f.create(beads.Bead{Title: "mol root", Type: "molecule", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "worker",
	}})
	fstep := f.create(beads.Bead{Title: "foreign step", Type: "task", Assignee: "nux", Metadata: map[string]string{
		"gc.root_bead_id": root.ID, "gc.routed_to": "worker",
	}})
	hold(t, f, fstep.ID, "in_progress")
	candidate := f.get(root.ID)
	opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}}
	ctx := context.Background()

	// No probe is possible without the seam: the gate is inert, the claim proceeds.
	if hookClaimForeignWorkflowHold(ctx, candidate, opts, hookClaimOps{}, f.t.TempDir(), io.Discard) {
		t.Fatal("gate fired with WorkflowStore seam absent; a missing probe must not withhold work")
	}

	// A store fault names itself on stderr and still lets the claim through.
	wrapped := &foreignHoldListErrorStore{Store: f.store}
	ops := hookClaimOps{WorkflowStore: func(context.Context, string, []string, string, string) beads.Store { return wrapped }}
	var stderr bytes.Buffer
	if hookClaimForeignWorkflowHold(ctx, candidate, opts, ops, f.t.TempDir(), &stderr) {
		t.Fatal("gate withheld on a store fault; one read error must not idle every seat behind it")
	}
	if wrapped.calls != 1 || !strings.Contains(stderr.String(), "foreign-hold check unavailable") {
		t.Fatalf("probe called %d times, stderr=%q; want one List attempt and the disclosed fault", wrapped.calls, stderr.String())
	}
}

// foreignHoldCountingStore delegates to the fixture ledger and counts List
// calls: ordinary task candidates must never trigger a probe at all — the
// root rows used to skip store reads entirely, and a gate that reads the
// store on every candidate changes the query surface (this exact mistake
// panicked the caching-store path during development of this fix).
type foreignHoldCountingStore struct {
	beads.Store
	calls int
}

func (s *foreignHoldCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.calls++
	return s.Store.List(q)
}

func TestHookClaimForeignHoldOnlyProbesWorkflowRoots(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}}
	wrapped := &foreignHoldCountingStore{Store: f.store}
	ops := hookClaimOps{WorkflowStore: func(context.Context, string, []string, string, string) beads.Store { return wrapped }}

	// An ordinary task candidate is skipped without touching the store, even
	// if some unrelated bead happens to share its id space.
	task := f.create(beads.Bead{Title: "plain task", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}})
	if hookClaimForeignWorkflowHold(context.Background(), task, opts, ops, f.t.TempDir(), io.Discard) {
		t.Fatal("gate fired on a non-workflow candidate")
	}

	// A step (its root names a DIFFERENT bead) is the step fences' business,
	// never the whole-root hand-out gate.
	root := f.create(beads.Bead{Title: "mol root", Type: "molecule", Metadata: map[string]string{"gc.kind": "workflow"}})
	held := f.create(beads.Bead{Title: "foreign step", Type: "task", Assignee: "nux", Metadata: map[string]string{"gc.root_bead_id": root.ID}})
	hold(t, f, held.ID, "in_progress")
	stepish := f.create(beads.Bead{Title: "a step elsewhere", Type: "task", Metadata: map[string]string{"gc.kind": "workflow", "gc.root_bead_id": root.ID}})
	if hookClaimForeignWorkflowHold(context.Background(), stepish, opts, ops, f.t.TempDir(), io.Discard) {
		t.Fatal("gate fired on a linked step, not a root handed out whole")
	}
	if wrapped.calls != 0 {
		t.Fatalf("gate probed the store %d time(s) for non-root candidates; ordinary claims must not grow a store read", wrapped.calls)
	}

	// Contrast: the real unlinked root DOES probe, and fires on the foreign held step above it.
	if !hookClaimForeignWorkflowHold(context.Background(), f.get(root.ID), opts, ops, f.t.TempDir(), io.Discard) {
		t.Fatal("root with a foreign in_progress step must trigger the probe")
	}
	if wrapped.calls != 1 {
		t.Fatalf("root probe made %d List calls, want exactly 1", wrapped.calls)
	}
}

// TestHookClaimSkipsHandedOffReadyAssignment covers hazard B end-to-end on
// the ready tier: submit-and-exit leaves a work bead OPEN, assigned to the
// refinery, with handoff_stage stamped. A polecat aliasing to that assignee
// (or a pool re-sling that clears the assignee while the marker survives)
// must not pull it back into in_progress — that is how gk-vld2m was
// re-claimed and its new claimant died.
func TestHookClaimSkipsHandedOffReadyAssignment(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	handed := f.create(beads.Bead{Title: "shipped work, awaiting refinery", Type: "task", Status: "open", Assignee: "worker-1", Metadata: map[string]string{
		"handoff_stage": "target_recorded", "gc.routed_to": "worker",
	}})
	other := f.create(beads.Bead{Title: "fresh pool work", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}})
	f.served = []string{handed.ID, other.ID}

	handedOut, _ := f.run()
	if handedOut == handed.ID {
		t.Fatalf("hook re-claimed the handed-off bead %s; its PR is already recorded and the refinery owns it", handed.ID)
	}
	if handedOut != other.ID {
		t.Fatalf("handed out %q, want the fresh pool bead %q behind it", handedOut, other.ID)
	}
	f.wantNeverClaimed(handed.ID)
	if got := f.get(handed.ID); got.Assignee != "worker-1" || got.Metadata["handoff_stage"] != "target_recorded" {
		t.Fatalf("handed-off bead mutated: assignee=%q marker=%q", got.Assignee, got.Metadata["handoff_stage"])
	}
}

// The same marker must also block the eligible tier's fresh hand-out once a
// re-sling clears the assignee: the marker is the surviving evidence.
func TestHookClaimEligibleTierSkipsHandedOffReSlung(t *testing.T) {
	f := newHookWorkflowStopFixture(t)
	resent := f.create(beads.Bead{Title: "assignee cleared, marker survives", Type: "task", Status: "open", Metadata: map[string]string{
		"handoff_stage": "target_recorded", "gc.routed_to": "worker",
	}})
	other := f.create(beads.Bead{Title: "fresh pool work", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}})
	f.served = []string{resent.ID, other.ID}

	handedOut, _ := f.run()
	if handedOut != other.ID {
		t.Fatalf("handed out %q, want %q; a handoff-stamped row must not be freshly claimable (gk-vld2m)", handedOut, other.ID)
	}
	f.wantNeverClaimed(resent.ID)
}

func TestHookCandidateHandedOffEligibility(t *testing.T) {
	now := time.Now().UTC()
	routed := func(meta map[string]string) map[string]string {
		out := map[string]string{"gc.routed_to": "gk/gastown.polecat"}
		for k, v := range meta {
			out[k] = v
		}
		return out
	}
	base := func(meta map[string]string) beads.Bead {
		return beads.Bead{ID: "gk-x1", Status: "open", Metadata: routed(meta)}
	}
	if !hookCandidateClaimable(base(nil), []string{"gk/gastown.polecat"}, now) {
		t.Fatal("plain routed candidate must stay claimable")
	}
	if hookCandidateClaimable(base(map[string]string{"handoff_stage": "target_recorded"}), []string{"gk/gastown.polecat"}, now) {
		t.Fatal("a handoff-stamped bead must not be freshly claimable")
	}
	if !hookCandidateClaimable(base(map[string]string{"handoff_stage": ""}), []string{"gk/gastown.polecat"}, now) {
		t.Fatal("an explicitly cleared handoff marker (the rejection path) must restore claimability")
	}
	held := base(map[string]string{"handoff_stage": "target_recorded"})
	held.Assignee = "gk/gastown.refinery"
	if hookCandidateReclaimEligible(held, []string{"gk/gastown.polecat"}, now) {
		t.Fatal("a handoff-stamped bead must not be stale-reclaimable — bd reclaim clears ANY assignee on lease TTL alone")
	}
	held.Metadata["handoff_stage"] = ""
	if !hookCandidateReclaimEligible(held, []string{"gk/gastown.polecat"}, now) {
		t.Fatal("without the marker the scoped reclaim path is unchanged")
	}
}
