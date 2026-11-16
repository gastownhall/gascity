package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/convoy"
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
	if err := convoy.TrackItemIn(convoy.MemberClasses{Convoy: beadstest.Work(store)}, conv.ID, work.ID); err != nil {
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

// workflowInputCompleteIn is workflowInputDone over store with no absence
// prover, as the root id it resolved and whether the input is complete.
func workflowInputCompleteIn(store beads.Store, candidate beads.Bead, identityCandidates []string) (string, bool, error) {
	verdict, err := workflowInputDone(context.Background(), beadstest.Work(store), candidate, identityCandidates, nil)
	return verdict.RootID, verdict.State == workflowInputComplete, err
}

func TestWorkflowInputDoneDetectsTerminalConvoyMember(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "closed")

	rootID, done, err := workflowInputCompleteIn(store, root, nil)
	if err != nil {
		t.Fatalf("workflowInputDone(root): %v", err)
	}
	if !done || rootID != root.ID {
		t.Fatalf("root: done=%v rootID=%q, want done=true rootID=%q", done, rootID, root.ID)
	}

	rootID, done, err = workflowInputCompleteIn(store, step, nil)
	if err != nil {
		t.Fatalf("workflowInputDone(step): %v", err)
	}
	if !done || rootID != root.ID {
		t.Fatalf("step: done=%v rootID=%q, want done=true rootID=%q", done, rootID, root.ID)
	}

	// A step that carries the input convoy id beside its root id (a
	// formula's step metadata may stamp it) resolves to the same root, so
	// retiring the workflow closes the root and not that step alone.
	stamped, err := store.Create(beads.Bead{Title: "Review", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:    root.ID,
		beadmeta.InputConvoyIDMetadataKey: root.Metadata[beadmeta.InputConvoyIDMetadataKey],
		beadmeta.RoutedToMetadataKey:      "rig/worker",
	}})
	if err != nil {
		t.Fatalf("create stamped step: %v", err)
	}
	rootID, done, err = workflowInputCompleteIn(store, stamped, nil)
	if err != nil {
		t.Fatalf("workflowInputDone(stamped step): %v", err)
	}
	if !done || rootID != root.ID {
		t.Fatalf("stamped step: done=%v rootID=%q, want done=true rootID=%q", done, rootID, root.ID)
	}
}

// TestWorkflowRootIDForInputCheck pins which bead a candidate's input check
// reads as its workflow root: the root a step names wins over an input convoy
// id the step also carries; a bead is its own root only when it names no
// other root and carries the input convoy id.
func TestWorkflowRootIDForInputCheck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		want     string
	}{
		{name: "root with input convoy", metadata: map[string]string{beadmeta.InputConvoyIDMetadataKey: "gc-in"}, want: "gc-self"},
		{name: "root naming itself with input convoy", metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-self", beadmeta.InputConvoyIDMetadataKey: "gc-in"}, want: "gc-self"},
		{name: "step", metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-root"}, want: "gc-root"},
		{name: "step stamped with the input convoy id", metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-root", beadmeta.InputConvoyIDMetadataKey: "gc-in"}, want: "gc-root"},
		{name: "root naming itself without input convoy", metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-self"}},
		{name: "bead outside any workflow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workflowRootIDForInputCheck(beads.Bead{ID: "gc-self", Metadata: tc.metadata})
			if got != tc.want {
				t.Fatalf("workflowRootIDForInputCheck = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWorkflowInputDoneFalseWhileMemberOpen(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")
	_, done, err := workflowInputCompleteIn(store, root, nil)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if done {
		t.Fatal("done = true for an open convoy member, want false")
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
	_, done, err := workflowInputCompleteIn(store, root, nil)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if done {
		t.Fatal("done = true with no input convoy, want false")
	}
}

func TestWorkflowInputDoneTrueWhenMemberHandedOffToOtherLane(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "handed_off")
	_, done, err := workflowInputCompleteIn(store, root, []string{"gastown__polecat-gc-xxxx"})
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if !done {
		t.Fatal("done = false for a member handed off to another lane, want true")
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
	members, err := convoy.MembersIn(convoy.MemberClasses{Convoy: beadstest.Work(store)}, root.Metadata[beadmeta.InputConvoyIDMetadataKey], true)
	if err != nil || len(members) != 1 {
		t.Fatalf("convoy.Members = %d, %v; want 1 member", len(members), err)
	}
	if err := store.Update(members[0].ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr(deadSeat)}); err != nil {
		t.Fatalf("update work: %v", err)
	}

	_, done, err := workflowInputCompleteIn(store, step, []string{"homeops__homeops-luna-gc-9b2b0a"})
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if done {
		t.Fatal("done = true for a member held by a sibling seat of the same workflow, want false")
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
	members, err := convoy.MembersIn(convoy.MemberClasses{Convoy: beadstest.Work(store)}, root.Metadata[beadmeta.InputConvoyIDMetadataKey], true)
	if err != nil || len(members) != 1 {
		t.Fatalf("convoy.Members = %d, %v; want 1 member", len(members), err)
	}
	if err := store.Update(members[0].ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr(deadSeat)}); err != nil {
		t.Fatalf("update work: %v", err)
	}

	_, done, err := workflowInputCompleteIn(store, step, []string{"homeops__homeops-luna-gc-9b2b0a"})
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if done {
		t.Fatal("done = true for a member held by a sibling seat whose step was reopened, want false")
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
	members, err := convoy.MembersIn(convoy.MemberClasses{Convoy: beadstest.Work(store)}, root.Metadata[beadmeta.InputConvoyIDMetadataKey], true)
	if err != nil || len(members) != 1 {
		t.Fatalf("convoy.Members = %d, %v; want 1 member", len(members), err)
	}
	if err := store.Update(members[0].ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr(deadSessionID)}); err != nil {
		t.Fatalf("update work: %v", err)
	}

	_, done, err := workflowInputCompleteIn(store, step, []string{"homeops__homeops-luna-gc-9b2b0a"})
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if done {
		t.Fatal("done = true for a member held by a seat named only by gc.session_id provenance, want false")
	}
}

func TestWorkflowInputDoneFalseWhenMemberAssignedToIdentityCandidate(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "handed_off")
	convoyID := root.Metadata[beadmeta.InputConvoyIDMetadataKey]
	members, err := convoy.MembersIn(convoy.MemberClasses{Convoy: beadstest.Work(store)}, convoyID, true)
	if err != nil {
		t.Fatalf("convoy.Members: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1", len(members))
	}
	if err := store.Update(members[0].ID, beads.UpdateOpts{Assignee: strPtr("gastown__polecat-gc-xxxx")}); err != nil {
		t.Fatalf("update work: %v", err)
	}
	_, done, err := workflowInputCompleteIn(store, root, []string{"gastown__polecat-gc-xxxx"})
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if done {
		t.Fatal("done = true for a member assigned to one of identityCandidates, want false")
	}
}

func TestWorkflowInputDoneFalseWhenMemberUnassignedOpen(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")
	_, done, err := workflowInputCompleteIn(store, root, []string{"gastown__polecat-gc-xxxx"})
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if done {
		t.Fatal("done = true for an unassigned open member, want false")
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
		InputVerdict: func(_ context.Context, _ string, _ []string, _ beads.Bead, _ []string, _ workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputVerdict{RootID: "root-1", State: workflowInputComplete}, nil
		},
		RetireWorkflow: func(_ context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
			skipped = verdict.RootID
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
		InputVerdict: func(_ context.Context, _ string, _ []string, _ beads.Bead, _ []string, _ workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputVerdict{RootID: "root-1", State: workflowInputComplete}, nil
		},
		RetireWorkflow: func(_ context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
			skipped = verdict.RootID
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
		InputVerdict: func(_ context.Context, _ string, _ []string, _ beads.Bead, _ []string, _ workflowInputAbsenceProver) (workflowInputVerdict, error) {
			return workflowInputVerdict{RootID: "root-1"}, nil
		},
		RetireWorkflow: func(_ context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
			t.Fatalf("unexpected retirement of %s", verdict.RootID)
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
