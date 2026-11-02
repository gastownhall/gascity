package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
)

// seedPurgeWorkflow builds a store holding a workflow root whose input convoy
// tracks a single work bead, plus one open and one closed step under the
// root. The caller decides what to purge from it.
func seedPurgeWorkflow(t *testing.T) (*beads.MemStore, beads.Bead, beads.Bead, beads.Bead, beads.Bead, beads.Bead) {
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
	doneStep, err := store.Create(beads.Bead{Title: "finished step", Metadata: map[string]string{
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

func TestWorkflowInputPurgedDetectsPurgedConvoy(t *testing.T) {
	store, _, conv, root, step, _ := seedPurgeWorkflow(t)
	if err := store.Delete(conv.ID); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}

	for _, candidate := range []beads.Bead{root, step} {
		rootID, purged, err := workflowInputPurged(store, store, candidate)
		if err != nil {
			t.Fatalf("workflowInputPurged(%s): %v", candidate.ID, err)
		}
		if !purged || rootID != root.ID {
			t.Fatalf("candidate %s: purged=%v rootID=%q, want purged=true rootID=%q", candidate.ID, purged, rootID, root.ID)
		}
	}
}

func TestWorkflowInputPurgedDetectsAllMembersPurged(t *testing.T) {
	store, work, _, root, step, _ := seedPurgeWorkflow(t)
	if err := store.Delete(work.ID); err != nil {
		t.Fatalf("purge work: %v", err)
	}

	// The convoy survives with a dangling tracks dep, so MembersIn reports
	// the member as an unresolved placeholder; every placeholder whose target
	// is confirmed absent everywhere is a purge.
	for _, candidate := range []beads.Bead{root, step} {
		rootID, purged, err := workflowInputPurged(store, store, candidate)
		if err != nil {
			t.Fatalf("workflowInputPurged(%s): %v", candidate.ID, err)
		}
		if !purged || rootID != root.ID {
			t.Fatalf("candidate %s: purged=%v rootID=%q, want purged=true rootID=%q", candidate.ID, purged, rootID, root.ID)
		}
	}
}

func TestWorkflowInputPurgedFalseForMemberResolvingInGraphStore(t *testing.T) {
	// Split-city shape, properly split: the convoy and the member bead live
	// in the work store, the root and its step in the graph store. The
	// member is purged from the work store — where the convoy's read sees a
	// placeholder — but still resolves in the graph store, so this is not
	// evidence of a purge.
	workStore := beads.NewMemStore()
	work, err := workStore.Create(beads.Bead{Title: "the work", Type: "task"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	conv, err := workStore.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	if err := convoy.TrackItem(workStore, conv.ID, work.ID); err != nil {
		t.Fatalf("track: %v", err)
	}
	graphStore := beads.NewMemStore()
	root, err := graphStore.Create(beads.Bead{Title: "mol", Metadata: map[string]string{
		beadmeta.KindMetadataKey:          beadmeta.KindWorkflow,
		beadmeta.InputConvoyIDMetadataKey: conv.ID,
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step, err := graphStore.Create(beads.Bead{Title: "Load context", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
	}})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	if err := workStore.Delete(work.ID); err != nil {
		t.Fatalf("purge work from the work store: %v", err)
	}
	ghost, err := graphStore.Create(beads.Bead{ID: work.ID, Title: "the work", Type: "task"})
	if err != nil {
		t.Fatalf("create work in the graph store: %v", err)
	}
	_ = ghost

	for _, candidate := range []beads.Bead{root, step} {
		_, purged, err := workflowInputPurged(graphStore, workStore, candidate)
		if err != nil {
			t.Fatalf("workflowInputPurged(%s): %v", candidate.ID, err)
		}
		if purged {
			t.Fatalf("candidate %s: purged=true, want false: the member resolves in the graph store", candidate.ID)
		}
	}
}

func TestWorkflowInputPurgedFalseForLiveOrTerminalMembers(t *testing.T) {
	store, work, _, root, _, _ := seedPurgeWorkflow(t)
	// Open member: live input.
	if _, purged, err := workflowInputPurged(store, store, root); err != nil || purged {
		t.Fatalf("open member: purged=%v err=%v, want false/nil", purged, err)
	}
	// Terminal member: the done-check's territory, and not a purge.
	if err := store.Close(work.ID); err != nil {
		t.Fatalf("close work: %v", err)
	}
	if _, purged, err := workflowInputPurged(store, store, root); err != nil || purged {
		t.Fatalf("closed member: purged=%v err=%v, want false/nil", purged, err)
	}
}

func TestWorkflowInputPurgedFalseForConvoyWithoutMembers(t *testing.T) {
	store, work, _, root, _, _ := seedPurgeWorkflow(t)
	// A degenerate convoy (no resolvable members, no dangling deps) is
	// ambiguous, and ambiguity is never a purge.
	if err := store.DepRemove(root.Metadata[beadmeta.InputConvoyIDMetadataKey], work.ID); err != nil {
		t.Fatalf("untrack: %v", err)
	}
	if _, purged, err := workflowInputPurged(store, store, root); err != nil || purged {
		t.Fatalf("memberless convoy: purged=%v err=%v, want false/nil", purged, err)
	}
}

func TestWorkflowInputPurgedFailsOpenOnStoreError(t *testing.T) {
	store, _, _, _, step, _ := seedPurgeWorkflow(t)
	// A root the graph store answers with a non-not-found error is
	// ambiguous: workflowInputPurged returns the error for the caller to
	// fail open on. The candidate must be a step so the root load happens.
	broken := brokenGetStore{Store: store, err: errors.New("dolt is on fire")}
	if _, _, err := workflowInputPurged(broken, store, step); err == nil {
		t.Fatal("workflowInputPurged on a failing root read: err=nil, want the read error")
	}
}

// brokenGetStore fails every Get with err, passing everything else through.
type brokenGetStore struct {
	beads.Store
	err error
}

func (b brokenGetStore) Get(id string) (beads.Bead, error) {
	return beads.Bead{}, b.err
}

func TestCancelInputPurgedWorkflowStampsHardFailure(t *testing.T) {
	store, _, _, root, step, doneStep := seedPurgeWorkflow(t)
	// Pre-assert the closed step's recorded outcome so the test proves it
	// survives the cancel.
	closedStep, err := store.Get(doneStep.ID)
	if err != nil {
		t.Fatalf("reading the closed step: %v", err)
	}
	if closedStep.Status != "closed" {
		t.Fatalf("seed closed step status=%q", closedStep.Status)
	}
	closedID := closedStep.ID

	if err := cancelInputPurgedWorkflow(store, root.ID); err != nil {
		t.Fatalf("cancelInputPurgedWorkflow: %v", err)
	}

	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if got.Status != "closed" {
			t.Errorf("%s: status=%q, want closed", id, got.Status)
		}
		if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
			t.Errorf("%s: outcome=%q, want fail", id, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
		if got.Metadata[beadmeta.FailureClassMetadataKey] != beadmeta.FailureClassHard {
			t.Errorf("%s: failure_class=%q, want hard", id, got.Metadata[beadmeta.FailureClassMetadataKey])
		}
		if got.Metadata[beadmeta.FailureReasonMetadataKey] != hookInputPurgedFailureReason {
			t.Errorf("%s: failure_reason=%q, want %q", id, got.Metadata[beadmeta.FailureReasonMetadataKey], hookInputPurgedFailureReason)
		}
		if got.Metadata["close_reason"] != hookCancelInputPurgedWorkflowCloseReason {
			t.Errorf("%s: close_reason=%q, want %q", id, got.Metadata["close_reason"], hookCancelInputPurgedWorkflowCloseReason)
		}
	}
	survivor, err := store.Get(closedID)
	if err != nil {
		t.Fatalf("reading the closed step: %v", err)
	}
	if survivor.Metadata[beadmeta.OutcomeMetadataKey] != "" || survivor.Metadata["close_reason"] == hookCancelInputPurgedWorkflowCloseReason {
		t.Errorf("closed step %s was re-stamped: outcome=%q close_reason=%q; a recorded outcome must survive",
			closedID, survivor.Metadata[beadmeta.OutcomeMetadataKey], survivor.Metadata["close_reason"])
	}
}

// purgedRetireOps builds hookClaimOps whose purge arm runs the production
// functions over store, with cancel counting invocations.
func purgedRetireOps(store *beads.MemStore, cancels *int) hookClaimOps {
	return hookClaimOps{
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead, ids []string) (string, bool, error) {
			return workflowInputDone(store, candidate, ids)
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			return skipDoneWorkflow(store, rootID)
		},
		InputPurged: func(_ context.Context, _ string, _ []string, candidate beads.Bead, _ []string) (string, bool, error) {
			return workflowInputPurged(store, store, candidate)
		},
		CancelInputPurgedWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			*cancels++
			return cancelInputPurgedWorkflow(store, rootID)
		},
	}
}

// The retire loop fails (not skips) a served candidate whose input convoy is
// purged, and drops the candidate from what it serves.
func TestRetireDoneWorkflowCandidatesFailsPurgedInputWorkflow(t *testing.T) {
	store, _, _, root, step, _ := seedPurgeWorkflow(t)
	if err := store.Delete(root.Metadata[beadmeta.InputConvoyIDMetadataKey]); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}
	var cancels int
	ops := purgedRetireOps(store, &cancels)
	// Serving is identity-based here: the candidate is the session's own
	// assignment.
	if err := store.Update(step.ID, beads.UpdateOpts{Assignee: strPtr("worker-1")}); err != nil {
		t.Fatalf("assign step: %v", err)
	}
	assigned, err := store.Get(step.ID)
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}

	var stderr bytes.Buffer
	opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}}
	kept := retireDoneWorkflowCandidates([]beads.Bead{assigned}, opts, ops, "/work", &stderr)

	if len(kept) != 0 {
		t.Fatalf("candidate kept (stderr=%q); its input convoy is purged and must be failed", stderr.String())
	}
	if cancels != 1 {
		t.Errorf("CancelInputPurgedWorkflow called %d times, want 1", cancels)
	}
	gotRoot, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("reading root: %v", err)
	}
	if gotRoot.Status != "closed" || gotRoot.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail ||
		gotRoot.Metadata[beadmeta.FailureReasonMetadataKey] != hookInputPurgedFailureReason {
		t.Fatalf("root: status=%q outcome=%q failure_reason=%q; want closed/fail/%s",
			gotRoot.Status, gotRoot.Metadata[beadmeta.OutcomeMetadataKey], gotRoot.Metadata[beadmeta.FailureReasonMetadataKey], hookInputPurgedFailureReason)
	}
	if !strings.Contains(stderr.String(), "input convoy is purged") {
		t.Errorf("stderr = %q, want a purge note", stderr.String())
	}
}

// A purge verdict that never arrives (candidate not served, or input fine)
// leaves the candidate and the workflow untouched.
func TestRetireDoneWorkflowCandidatesKeepsLiveAndUnservedCandidates(t *testing.T) {
	store, _, _, _, step, _ := seedPurgeWorkflow(t)
	var cancels int
	ops := purgedRetireOps(store, &cancels)
	opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}}

	// Live input, served candidate: kept.
	var stderr bytes.Buffer
	if kept := retireDoneWorkflowCandidates([]beads.Bead{step}, opts, ops, "/work", &stderr); len(kept) != 1 {
		t.Fatalf("served candidate with live input dropped (stderr=%q)", stderr.String())
	}
	// Purged input but the candidate is not ours to serve: kept, untouched.
	if err := store.Update(step.ID, beads.UpdateOpts{Assignee: strPtr("someone-else")}); err != nil {
		t.Fatalf("reassign step: %v", err)
	}
	root, err := store.Get(step.Metadata[beadmeta.RootBeadIDMetadataKey])
	if err != nil {
		t.Fatalf("reading root: %v", err)
	}
	if err := store.Delete(root.Metadata[beadmeta.InputConvoyIDMetadataKey]); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}
	foreign, err := store.Get(step.ID)
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	stderr.Reset()
	if kept := retireDoneWorkflowCandidates([]beads.Bead{foreign}, opts, ops, "/work", &stderr); len(kept) != 1 {
		t.Fatalf("unserved candidate dropped (stderr=%q); retirement must only touch this session's served candidates", stderr.String())
	}
	if cancels != 0 {
		t.Errorf("CancelInputPurgedWorkflow called %d times, want 0", cancels)
	}
}

// The purge arm runs before the done arm, and an ambiguous purge verdict
// falls through to the done arm rather than suppressing the retirement.
func TestHookRetireIfInputDeadArmOrder(t *testing.T) {
	candidate := beads.Bead{ID: "step-1", Assignee: "worker-1"}
	opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}}
	var stderr bytes.Buffer

	// Purge wins over done: both report dead, only the cancel fires.
	var cancels, skips int
	ops := hookClaimOps{
		InputPurged: func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
			return "root-1", true, nil
		},
		CancelInputPurgedWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			cancels++
			if rootID != "root-1" {
				t.Errorf("cancel got rootID %q, want root-1", rootID)
			}
			return nil
		},
		InputDone: func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
			return "root-1", true, nil
		},
		SkipDoneWorkflow: func(context.Context, string, []string, string) error {
			skips++
			return nil
		},
	}
	if !hookRetireIfInputDead(candidate, opts, ops, "/work", &stderr) {
		t.Fatal("hookRetireIfInputDead = false, want true (purged)")
	}
	if cancels != 1 || skips != 0 {
		t.Fatalf("cancels=%d skips=%d, want the purge arm to win (1, 0)", cancels, skips)
	}

	// Ambiguous purge (error) falls through to the done arm.
	cancels, skips = 0, 0
	ops.InputPurged = func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
		return "", false, errors.New("store hiccup")
	}
	if !hookRetireIfInputDead(candidate, opts, ops, "/work", &stderr) {
		t.Fatal("hookRetireIfInputDead = false, want true (done arm after ambiguous purge)")
	}
	if cancels != 0 || skips != 1 {
		t.Fatalf("cancels=%d skips=%d, want the done arm (0, 1)", cancels, skips)
	}

	// A failed cancel keeps the candidate visible for the next poll.
	ops.InputPurged = func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
		return "root-1", true, nil
	}
	ops.CancelInputPurgedWorkflow = func(context.Context, string, []string, string) error {
		return errors.New("close refused")
	}
	if hookRetireIfInputDead(candidate, opts, ops, "/work", &stderr) {
		t.Fatal("hookRetireIfInputDead = true on a failed cancel; the bead must stay visible")
	}
}
