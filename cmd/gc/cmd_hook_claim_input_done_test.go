package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// seedDoneWorkflow builds a store holding a workflow root whose input convoy
// tracks a single member, plus one open step under the root. memberStatus
// controls whether the tracked work is terminal.
func seedDoneWorkflow(t *testing.T, memberStatus string) (*beads.MemStore, beads.Bead, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "the work"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if memberStatus == "closed" {
		if err := store.Close(work.ID); err != nil {
			t.Fatalf("close work: %v", err)
		}
	}
	if memberStatus == "handed_off" {
		if err := store.Update(work.ID, beads.UpdateOpts{Status: strPtr("open"), Assignee: strPtr("sysadmin/gastown.refinery")}); err != nil {
			t.Fatalf("update work: %v", err)
		}
	}
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	if err := convoy.TrackItem(store, conv.ID, work.ID); err != nil {
		t.Fatalf("track: %v", err)
	}
	root, err := store.Create(beads.Bead{Title: "mol", Metadata: map[string]string{
		beadmeta.KindMetadataKey:          beadmeta.KindWorkflow,
		beadmeta.InputConvoyIDMetadataKey: conv.ID,
		beadmeta.RoutedToMetadataKey:      "rig/worker",
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step, err := store.Create(beads.Bead{Title: "Load context", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:        root.ID,
		beadmeta.ContinuationGroupMetadataKey: "pool-workflow",
		beadmeta.RoutedToMetadataKey:          "rig/worker",
	}})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	return store, root, step
}

func TestWorkflowInputDoneDetectsTerminalConvoyMember(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "closed")

	eval, err := workflowInputDone(store, root, nil, nil)
	if err != nil {
		t.Fatalf("workflowInputDone(root): %v", err)
	}
	if eval.Verdict != inputVerdictDone || eval.RootID != root.ID {
		t.Fatalf("root: verdict=%d rootID=%q, want done rootID=%q", eval.Verdict, eval.RootID, root.ID)
	}

	eval, err = workflowInputDone(store, step, nil, nil)
	if err != nil {
		t.Fatalf("workflowInputDone(step): %v", err)
	}
	if eval.Verdict != inputVerdictDone || eval.RootID != root.ID {
		t.Fatalf("step: verdict=%d rootID=%q, want done rootID=%q", eval.Verdict, eval.RootID, root.ID)
	}
}

func TestWorkflowInputDoneFalseWhileMemberOpen(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")
	eval, err := workflowInputDone(store, root, nil, nil)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if eval.Verdict != inputVerdictPending {
		t.Fatalf("verdict = %d for an open convoy member, want pending", eval.Verdict)
	}
}

func TestWorkflowInputDoneFalseWithoutInputConvoy(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Metadata: map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
	}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	eval, err := workflowInputDone(store, root, nil, nil)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if eval.Verdict != inputVerdictPending {
		t.Fatal("verdict = with no input convoy, want false")
	}
}

func TestWorkflowInputDoneTrueWhenMemberHandedOffToOtherLane(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "handed_off")
	eval, err := workflowInputDone(store, root, []string{"gastown__polecat-gc-xxxx"}, nil)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if eval.Verdict != inputVerdictDone {
		t.Fatalf("verdict = %d for a member handed off to another lane, want done", eval.Verdict)
	}
}

// A member still assigned to a seat that ran a step of this same workflow was
// not handed off: that seat is the workflow's own pool worker (typically dead,
// its routed steps already reopened). A successor seat must adopt the workflow,
// not retire it (sys-pxryan.44).
func TestWorkflowInputDoneFalseWhenMemberHeldBySiblingSeatOfSameWorkflow(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "open")
	const deadSeat = "homeops__homeops-luna-gc-5g2aze"
	claimStep, err := store.Create(beads.Bead{Title: "claim input", Assignee: deadSeat, Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:        root.ID,
		beadmeta.ContinuationGroupMetadataKey: "pool-workflow",
		beadmeta.RoutedToMetadataKey:          "rig/worker",
	}})
	if err != nil {
		t.Fatalf("create claim step: %v", err)
	}
	if err := store.Close(claimStep.ID); err != nil {
		t.Fatalf("close claim step: %v", err)
	}
	members, err := convoy.Members(store, root.Metadata[beadmeta.InputConvoyIDMetadataKey], true)
	if err != nil || len(members) != 1 {
		t.Fatalf("convoy.Members = %d, %v; want 1 member", len(members), err)
	}
	if err := store.Update(members[0].ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr(deadSeat)}); err != nil {
		t.Fatalf("update work: %v", err)
	}

	eval, err := workflowInputDone(store, step, []string{"homeops__homeops-luna-gc-9b2b0a"}, nil)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if eval.Verdict != inputVerdictPending {
		t.Fatalf("verdict = %d for a member held by a sibling seat of the same workflow, want pending", eval.Verdict)
	}
}

// A seat that died before closing its step leaves that step reopened with the
// assignee cleared; only the step's gc.session_name/gc.session_id claim
// provenance still names it. That seat is still this workflow's own worker.
func TestWorkflowInputDoneFalseWhenMemberHeldBySiblingSeatWhoseStepWasReopened(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "open")
	const deadSeat = "homeops__homeops-luna-gc-5g2aze"
	if _, err := store.Create(beads.Bead{Title: "claim input", Status: "open", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:        root.ID,
		beadmeta.ContinuationGroupMetadataKey: "pool-workflow",
		beadmeta.RoutedToMetadataKey:          "rig/worker",
		beadmeta.SessionNameMetadataKey:       deadSeat,
		beadmeta.SessionIDMetadataKey:         "gc-5g2aze",
	}}); err != nil {
		t.Fatalf("create reopened claim step: %v", err)
	}
	members, err := convoy.Members(store, root.Metadata[beadmeta.InputConvoyIDMetadataKey], true)
	if err != nil || len(members) != 1 {
		t.Fatalf("convoy.Members = %d, %v; want 1 member", len(members), err)
	}
	if err := store.Update(members[0].ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr(deadSeat)}); err != nil {
		t.Fatalf("update work: %v", err)
	}

	eval, err := workflowInputDone(store, step, []string{"homeops__homeops-luna-gc-9b2b0a"}, nil)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if eval.Verdict != inputVerdictPending {
		t.Fatalf("verdict = %d for a member held by a sibling seat whose step was reopened, want pending", eval.Verdict)
	}
}

// A reopened step may keep only gc.session_id as provenance; a member whose
// assignee is that session id is still held by this workflow's own seat.
func TestWorkflowInputDoneFalseWhenMemberHeldBySessionIDOnlyProvenance(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "open")
	const deadSessionID = "gc-5g2aze"
	if _, err := store.Create(beads.Bead{Title: "claim input", Status: "open", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:        root.ID,
		beadmeta.ContinuationGroupMetadataKey: "pool-workflow",
		beadmeta.RoutedToMetadataKey:          "rig/worker",
		beadmeta.SessionIDMetadataKey:         deadSessionID,
	}}); err != nil {
		t.Fatalf("create reopened claim step: %v", err)
	}
	members, err := convoy.Members(store, root.Metadata[beadmeta.InputConvoyIDMetadataKey], true)
	if err != nil || len(members) != 1 {
		t.Fatalf("convoy.Members = %d, %v; want 1 member", len(members), err)
	}
	if err := store.Update(members[0].ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr(deadSessionID)}); err != nil {
		t.Fatalf("update work: %v", err)
	}

	eval, err := workflowInputDone(store, step, []string{"homeops__homeops-luna-gc-9b2b0a"}, nil)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if eval.Verdict != inputVerdictPending {
		t.Fatalf("verdict = %d for a member held by a seat named only by gc.session_id provenance, want pending", eval.Verdict)
	}
}

func TestWorkflowInputDoneFalseWhenMemberAssignedToIdentityCandidate(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "handed_off")
	convoyID := root.Metadata[beadmeta.InputConvoyIDMetadataKey]
	members, err := convoy.Members(store, convoyID, true)
	if err != nil {
		t.Fatalf("convoy.Members: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1", len(members))
	}
	if err := store.Update(members[0].ID, beads.UpdateOpts{Assignee: strPtr("gastown__polecat-gc-xxxx")}); err != nil {
		t.Fatalf("update work: %v", err)
	}
	eval, err := workflowInputDone(store, root, []string{"gastown__polecat-gc-xxxx"}, nil)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if eval.Verdict != inputVerdictPending {
		t.Fatalf("verdict = %d for a member assigned to one of identityCandidates, want pending", eval.Verdict)
	}
}

func TestWorkflowInputDoneFalseWhenMemberUnassignedOpen(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")
	eval, err := workflowInputDone(store, root, []string{"gastown__polecat-gc-xxxx"}, nil)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if eval.Verdict != inputVerdictPending {
		t.Fatalf("verdict = %d for an unassigned open member, want pending", eval.Verdict)
	}
}

func TestSkipDoneWorkflowClosesRootAndSteps(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "closed")
	if err := store.Update(step.ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr("worker-1")}); err != nil {
		t.Fatalf("update step: %v", err)
	}

	if err := skipDoneWorkflow(store, root.ID); err != nil {
		t.Fatalf("skipDoneWorkflow: %v", err)
	}
	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
		if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped {
			t.Fatalf("%s gc.outcome = %q, want skipped", id, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
}

func TestDoHookClaimSkipsWorkflowWhoseInputIsDone(t *testing.T) {
	candidates := []beads.Bead{{
		ID:     "root-1",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:          beadmeta.KindWorkflow,
			beadmeta.InputConvoyIDMetadataKey: "convoy-1",
			beadmeta.RoutedToMetadataKey:      "worker",
		},
	}}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	claimed := false
	skipped := ""
	drained := false
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimed = true
			return beads.Bead{ID: beadID, Assignee: assignee}, true, nil
		},
		InputDone: func(_ context.Context, _ string, _ []string, _ beads.Bead, _ []string) (workflowInputEvaluation, error) {
			return workflowInputEvaluation{RootID: "root-1", Verdict: inputVerdictDone}, nil
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			skipped = rootID
			return nil
		},
		DrainAck: func(io.Writer) error {
			drained = true
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if claimed {
		t.Fatal("claimed a workflow whose input convoy work is already terminal")
	}
	if skipped != "root-1" {
		t.Fatalf("skipped root = %q, want root-1", skipped)
	}
	if !drained {
		t.Fatal("no-work drain not acknowledged after skipping the done workflow")
	}
	if !strings.Contains(stdout.String(), `"reason":"no_work"`) {
		t.Fatalf("stdout = %q, want no_work drain", stdout.String())
	}
}

func TestDoHookClaimSkipsExistingAssignmentWhoseInputIsDone(t *testing.T) {
	// The per-seat treadmill: the seat already holds an in_progress step of a
	// molecule whose work finished elsewhere. The hook must not hand that step
	// back as existing_assignment; it must close the molecule and drain.
	candidates := []beads.Bead{{
		ID:       "step-1",
		Status:   "in_progress",
		Assignee: "worker-1",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:        "root-1",
			beadmeta.ContinuationGroupMetadataKey: "pool-workflow",
			beadmeta.RoutedToMetadataKey:          "worker",
		},
	}}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	skipped := ""
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			t.Fatalf("unexpected claim of %s", beadID)
			return beads.Bead{}, false, nil
		},
		InputDone: func(_ context.Context, _ string, _ []string, _ beads.Bead, _ []string) (workflowInputEvaluation, error) {
			return workflowInputEvaluation{RootID: "root-1", Verdict: inputVerdictDone}, nil
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			skipped = rootID
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if skipped != "root-1" {
		t.Fatalf("skipped root = %q, want root-1", skipped)
	}
	if strings.Contains(stdout.String(), "existing_assignment") {
		t.Fatalf("stdout = %q, handed back a step of a done workflow", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"reason":"no_work"`) {
		t.Fatalf("stdout = %q, want no_work drain", stdout.String())
	}
}

// seedPurgedInputWorkflow builds a store holding a live graph.v2 workflow
// whose root references an input convoy that no longer exists: the convoy was
// purged, and its open steps otherwise loop in the ready pool forever because
// the done-check reads a missing convoy as "nothing to wait on". The finished
// step pins that a retirement preserves terminal outcomes.
func seedPurgedInputWorkflow(t *testing.T) (*beads.MemStore, beads.Bead, beads.Bead, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "mol", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   "gc-purged-convoy",
		beadmeta.RoutedToMetadataKey:        "worker",
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step, err := store.Create(beads.Bead{Title: "Load context", Status: "in_progress", Assignee: "worker-dead", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.RoutedToMetadataKey:   "worker",
	}})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	finished, err := store.Create(beads.Bead{Title: "done step", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
	}})
	if err != nil {
		t.Fatalf("create finished step: %v", err)
	}
	if err := store.Close(finished.ID); err != nil {
		t.Fatalf("close finished step: %v", err)
	}
	if err := store.SetMetadata(finished.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass); err != nil {
		t.Fatalf("stamp finished step outcome: %v", err)
	}
	return store, root, step, finished
}

// TestDoHookClaimRetiresWorkflowWhoseInputWasPurged is the #8729 regression:
// a graph.v2 candidate whose input convoy is proven absent from the supported
// topology retires at claim time — root and open steps close as a hard
// input_purged failure — instead of being served to a seat that burns a claim
// cycle discovering the spec no longer exists. Terminal steps keep their
// recorded outcome, no claim runs, and the retirement names the missing ids.
func TestDoHookClaimRetiresWorkflowWhoseInputWasPurged(t *testing.T) {
	store, root, step, finished := seedPurgedInputWorkflow(t)
	proveCalls := 0
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			output, err := json.Marshal([]beads.Bead{root})
			if err != nil {
				return "", err
			}
			return string(output), nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			t.Errorf("claimed %s; a workflow whose input was purged must retire, not be claimed", beadID)
			return beads.Bead{}, false, nil
		},
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead, ids []string) (workflowInputEvaluation, error) {
			return workflowInputDone(store, candidate, ids, func(missing []string) (bool, error) {
				proveCalls++
				if len(missing) != 1 || missing[0] != "gc-purged-convoy" {
					t.Errorf("absence proof asked for %v, want [gc-purged-convoy]", missing)
				}
				return true, nil
			})
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			t.Errorf("skipped %s; a purged input retires as a hard failure, not as skipped", rootID)
			return nil
		},
		CancelPurgedWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			return cancelPurgedWorkflow(store, rootID)
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if proveCalls != 1 {
		t.Fatalf("absence proof calls = %d, want 1", proveCalls)
	}
	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != "closed" {
			t.Errorf("%s status = %q, want closed", id, got.Status)
		}
		if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
			t.Errorf("%s gc.outcome = %q, want fail", id, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
		if got.Metadata[beadmeta.FailureClassMetadataKey] != beadmeta.FailureClassHard {
			t.Errorf("%s gc.failure_class = %q, want hard", id, got.Metadata[beadmeta.FailureClassMetadataKey])
		}
		if got.Metadata[beadmeta.FailureReasonMetadataKey] != "input_purged" {
			t.Errorf("%s gc.failure_reason = %q, want input_purged", id, got.Metadata[beadmeta.FailureReasonMetadataKey])
		}
		if got.Metadata["close_reason"] != sourceworkflow.HookPurgedInputWorkflowCloseReason {
			t.Errorf("%s close_reason = %q, want %q", id, got.Metadata["close_reason"], sourceworkflow.HookPurgedInputWorkflowCloseReason)
		}
	}
	done, err := store.Get(finished.ID)
	if err != nil {
		t.Fatalf("get finished step: %v", err)
	}
	if done.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Errorf("finished step gc.outcome = %q, want pass preserved by the retirement", done.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if !strings.Contains(stderr.String(), root.ID) || !strings.Contains(stderr.String(), "gc-purged-convoy") {
		t.Errorf("stderr = %q, want the retirement to log the root and the missing convoy id", stderr.String())
	}
	if !strings.Contains(stdout.String(), `"reason":"no_work"`) {
		t.Fatalf("stdout = %q, want no_work drain", stdout.String())
	}
}

// seedPurgedMembershipWorkflow builds a store holding a live graph.v2 workflow
// whose input convoy is still present but every bead it tracks was purged:
// each dangling tracks edge resolves to an unresolved placeholder in the
// membership listing, the shape the done-check read as live work forever
// (#8729). The finished step pins that a retirement preserves terminal
// outcomes.
func seedPurgedMembershipWorkflow(t *testing.T) (*beads.MemStore, beads.Bead, beads.Bead, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	for _, id := range []string{"gc-vanished-a", "gc-vanished-b"} {
		if err := store.DepAdd(conv.ID, id, convoy.TrackingDepType); err != nil {
			t.Fatalf("track %s: %v", id, err)
		}
	}
	root, err := store.Create(beads.Bead{Title: "mol", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   conv.ID,
		beadmeta.RoutedToMetadataKey:        "worker",
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step, err := store.Create(beads.Bead{Title: "Load context", Status: "in_progress", Assignee: "worker-dead", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.RoutedToMetadataKey:   "worker",
	}})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	finished, err := store.Create(beads.Bead{Title: "done step", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
	}})
	if err != nil {
		t.Fatalf("create finished step: %v", err)
	}
	if err := store.Close(finished.ID); err != nil {
		t.Fatalf("close finished step: %v", err)
	}
	if err := store.SetMetadata(finished.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass); err != nil {
		t.Fatalf("stamp finished step outcome: %v", err)
	}
	return store, root, step, finished
}

// TestWorkflowInputPurgedEveryTrackedMemberAbsent is the second #8729
// regression: the input convoy survives, but every bead it tracks was purged,
// so the membership listing is all unresolved placeholders. Once every member
// id is proven absent the retirement fires with the purge disposition — at
// base the placeholders blocked the done-check forever and the workflow looped
// in the ready pool, so the candidate is claimed instead of retired.
func TestWorkflowInputPurgedEveryTrackedMemberAbsent(t *testing.T) {
	store, root, step, finished := seedPurgedMembershipWorkflow(t)
	proveCalls := 0
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			output, err := json.Marshal([]beads.Bead{root})
			if err != nil {
				return "", err
			}
			return string(output), nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			t.Errorf("claimed %s; a workflow whose tracked input was purged must retire, not be claimed", beadID)
			return beads.Bead{}, false, nil
		},
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead, ids []string) (workflowInputEvaluation, error) {
			return workflowInputDone(store, candidate, ids, func(missing []string) (bool, error) {
				proveCalls++
				sort.Strings(missing)
				if len(missing) != 2 || missing[0] != "gc-vanished-a" || missing[1] != "gc-vanished-b" {
					t.Errorf("absence proof asked for %v, want [gc-vanished-a gc-vanished-b]", missing)
				}
				return true, nil
			})
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			t.Errorf("skipped %s; a purged input retires as a hard failure, not as skipped", rootID)
			return nil
		},
		CancelPurgedWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			return cancelPurgedWorkflow(store, rootID)
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if proveCalls != 1 {
		t.Fatalf("absence proof calls = %d, want 1", proveCalls)
	}
	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != "closed" {
			t.Errorf("%s status = %q, want closed", id, got.Status)
		}
		if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
			t.Errorf("%s gc.outcome = %q, want fail", id, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
		if got.Metadata[beadmeta.FailureClassMetadataKey] != beadmeta.FailureClassHard {
			t.Errorf("%s gc.failure_class = %q, want hard", id, got.Metadata[beadmeta.FailureClassMetadataKey])
		}
		if got.Metadata[beadmeta.FailureReasonMetadataKey] != "input_purged" {
			t.Errorf("%s gc.failure_reason = %q, want input_purged", id, got.Metadata[beadmeta.FailureReasonMetadataKey])
		}
		if got.Metadata["close_reason"] != sourceworkflow.HookPurgedInputWorkflowCloseReason {
			t.Errorf("%s close_reason = %q, want %q", id, got.Metadata["close_reason"], sourceworkflow.HookPurgedInputWorkflowCloseReason)
		}
	}
	done, err := store.Get(finished.ID)
	if err != nil {
		t.Fatalf("get finished step: %v", err)
	}
	if done.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Errorf("finished step gc.outcome = %q, want pass preserved by the retirement", done.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if !strings.Contains(stderr.String(), root.ID) || !strings.Contains(stderr.String(), "gc-vanished-a") {
		t.Errorf("stderr = %q, want the retirement to log the root and a missing member id", stderr.String())
	}
	if !strings.Contains(stdout.String(), `"reason":"no_work"`) {
		t.Fatalf("stdout = %q, want no_work drain", stdout.String())
	}
}

// TestWorkflowInputPendingWhenMixedResolvedAndMissingMembers pins the Q3
// policy: a convoy that still resolves one member stays pending even when
// every missing member id would be provably absent — a completed sibling does
// not establish a missing sibling's former status, so no absence scan runs.
func TestWorkflowInputPendingWhenMixedResolvedAndMissingMembers(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "the surviving work"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := store.Close(work.ID); err != nil {
		t.Fatalf("close work: %v", err)
	}
	conv, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	if err := convoy.TrackItem(store, conv.ID, work.ID); err != nil {
		t.Fatalf("track surviving work: %v", err)
	}
	if err := store.DepAdd(conv.ID, "gc-vanished-a", convoy.TrackingDepType); err != nil {
		t.Fatalf("track vanished work: %v", err)
	}
	root, err := store.Create(beads.Bead{Title: "mol", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   conv.ID,
		beadmeta.RoutedToMetadataKey:        "worker",
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}

	eval, err := workflowInputDone(store, root, nil, func(missing []string) (bool, error) {
		t.Errorf("absence proof asked for %v; a mixed membership must stay pending without a scan", missing)
		return true, nil
	})
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if eval.Verdict != inputVerdictPending {
		t.Fatalf("verdict = %d for a mixed resolved/missing membership, want pending", eval.Verdict)
	}
}

// TestDoHookClaimKeepsCandidateWhenAbsenceProofInconclusive pins the fail-open
// half of the purge verdict: a prover that cannot confirm the absence — one
// store unread, another still holding a copy, whatever the reason — answers
// not-proven, and the evaluation stays pending, so the candidate is served as
// it always was instead of being closed on a guess.
func TestDoHookClaimKeepsCandidateWhenAbsenceProofInconclusive(t *testing.T) {
	store, root, _, _ := seedPurgedInputWorkflow(t)
	claimedID := ""
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			output, err := json.Marshal([]beads.Bead{root})
			if err != nil {
				return "", err
			}
			return string(output), nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedID = beadID
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead, ids []string) (workflowInputEvaluation, error) {
			return workflowInputDone(store, candidate, ids, func([]string) (bool, error) {
				return false, nil
			})
		},
		CancelPurgedWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			t.Errorf("canceled workflow %s; an unproven absence must never retire", rootID)
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if claimedID != root.ID {
		t.Fatalf("claimed %q, want %q: an inconclusive proof keeps the candidate in the pool", claimedID, root.ID)
	}
	got, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("root was closed on an inconclusive absence proof; the workflow must stay alive")
	}
}

// TestDoHookClaimKeepsCandidateWhenAbsenceProofFails pins the error-reporting
// half: an operational fault in the proof — a store that will not open or
// read — is logged with the candidate named and treated as pending, never
// swallowed into a purge.
func TestDoHookClaimKeepsCandidateWhenAbsenceProofFails(t *testing.T) {
	store, root, _, _ := seedPurgedInputWorkflow(t)
	claimedID := ""
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			output, err := json.Marshal([]beads.Bead{root})
			if err != nil {
				return "", err
			}
			return string(output), nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedID = beadID
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead, ids []string) (workflowInputEvaluation, error) {
			return workflowInputDone(store, candidate, ids, func([]string) (bool, error) {
				return false, errors.New("store offline")
			})
		},
		CancelPurgedWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			t.Errorf("canceled workflow %s; a faulted absence proof must never retire", rootID)
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if claimedID != root.ID {
		t.Fatalf("claimed %q, want %q: a faulted proof keeps the candidate in the pool", claimedID, root.ID)
	}
	if !strings.Contains(stderr.String(), root.ID) || !strings.Contains(stderr.String(), "store offline") {
		t.Errorf("stderr = %q, want the operational fault logged with the candidate and the cause", stderr.String())
	}
	got, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("root was closed on a faulted absence proof; the workflow must stay alive")
	}
}

func TestDoHookClaimStillClaimsWhenInputNotDone(t *testing.T) {
	candidates := []beads.Bead{{
		ID:     "root-1",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:          beadmeta.KindWorkflow,
			beadmeta.InputConvoyIDMetadataKey: "convoy-1",
			beadmeta.RoutedToMetadataKey:      "worker",
		},
	}}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	claimedID := ""
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedID = beadID
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		InputDone: func(_ context.Context, _ string, _ []string, _ beads.Bead, _ []string) (workflowInputEvaluation, error) {
			return workflowInputEvaluation{RootID: "root-1"}, nil
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			t.Fatalf("unexpected skip of %s", rootID)
			return nil
		},
		ListContinuation: func(_ context.Context, _ string, _ []string, _, _ string) ([]beads.Bead, error) {
			return nil, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, _, _ string) error { return nil },
		ResolveWorkBranch:  func(hookClaimWorkTree) string { return "" },
		StampWorkMeta: func(_ context.Context, _ string, _ []string, _, _ string, _ map[string]string) error {
			return nil
		},
		PublishRunMap: func(_, _ string, _ ...string) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if claimedID != "root-1" {
		t.Fatalf("claimed = %q, want root-1", claimedID)
	}
}

// membershipListingStore counts the convoy-child listings an evaluation
// makes, so a row can pin the done verdict's read economy: the membership is
// listed once, never re-scanned.
type membershipListingStore struct {
	beads.Store
	childLists int
}

func (s *membershipListingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.ParentID != "" {
		s.childLists++
	}
	return s.Store.List(q)
}

// TestWorkflowInputDoneListsMembershipOnceAndNeverConsultsTheAbsenceProof pins
// the done path's economy: an input whose tracked members resolve terminal is
// answered by ONE membership listing, with no absence scan behind it — the
// purge verdict's proof is for the purged arm alone.
func TestWorkflowInputDoneListsMembershipOnceAndNeverConsultsTheAbsenceProof(t *testing.T) {
	mem, root, _ := seedDoneWorkflow(t, "closed")
	counting := &membershipListingStore{Store: mem}
	prove := func(ids []string) (bool, error) {
		t.Errorf("the absence proof was consulted (%v) for an input whose members resolve terminal; the done verdict needs no absence scan", ids)
		return false, nil
	}

	eval, err := workflowInputDone(counting, root, nil, prove)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if eval.Verdict != inputVerdictDone {
		t.Fatalf("verdict = %d for a terminal tracked member, want done", eval.Verdict)
	}
	if counting.childLists != 1 {
		t.Fatalf("membership was listed %d times, want exactly once", counting.childLists)
	}
}

// TestSkipDoneWorkflowPreservesRecordedOutcomesAndExistingMetadata pins the
// done retirement's write shape: an already-terminal step keeps the outcome it
// recorded, the open step and the root gain exactly the skipped disposition
// and its close_reason, and every metadata key they carried before survives.
func TestSkipDoneWorkflowPreservesRecordedOutcomesAndExistingMetadata(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "closed")
	finished, err := store.Create(beads.Bead{Title: "finished step", Assignee: "worker-old", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(finished.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(finished.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(step.ID, "gc.attempt", "2"); err != nil {
		t.Fatal(err)
	}

	if err := skipDoneWorkflow(store, root.ID); err != nil {
		t.Fatalf("skipDoneWorkflow: %v", err)
	}

	fin, err := store.Get(finished.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fin.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Errorf("the finished step's recorded outcome = %q, want %s; terminal beads keep what they recorded", fin.Metadata[beadmeta.OutcomeMetadataKey], beadmeta.OutcomePass)
	}
	if fin.Metadata["close_reason"] == hookSkipDoneWorkflowCloseReason {
		t.Errorf("the finished step %s was stamped with the skip close_reason; terminal beads are not rewritten", finished.ID)
	}
	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != "closed" {
			t.Errorf("%s status = %q, want closed", id, got.Status)
		}
		if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped {
			t.Errorf("%s gc.outcome = %q, want %s", id, got.Metadata[beadmeta.OutcomeMetadataKey], beadmeta.OutcomeSkipped)
		}
		if got.Metadata["close_reason"] != hookSkipDoneWorkflowCloseReason {
			t.Errorf("%s close_reason = %q, want %q", id, got.Metadata["close_reason"], hookSkipDoneWorkflowCloseReason)
		}
	}
	if got, _ := store.Get(step.ID); got.Metadata["gc.attempt"] != "2" {
		t.Errorf("the open step's pre-existing metadata was clobbered (gc.attempt = %q, want \"2\")", got.Metadata["gc.attempt"])
	}
	if got, _ := store.Get(root.ID); got.Metadata[beadmeta.InputConvoyIDMetadataKey] == "" {
		t.Errorf("the root's input convoy id was clobbered by the skip close")
	}
}
