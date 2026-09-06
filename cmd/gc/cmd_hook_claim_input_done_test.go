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

	rootID, done, err := workflowInputDone(store, root)
	if err != nil {
		t.Fatalf("workflowInputDone(root): %v", err)
	}
	if !done || rootID != root.ID {
		t.Fatalf("root: done=%v rootID=%q, want done=true rootID=%q", done, rootID, root.ID)
	}

	rootID, done, err = workflowInputDone(store, step)
	if err != nil {
		t.Fatalf("workflowInputDone(step): %v", err)
	}
	if !done || rootID != root.ID {
		t.Fatalf("step: done=%v rootID=%q, want done=true rootID=%q", done, rootID, root.ID)
	}
}

func TestWorkflowInputDoneFalseWhileMemberOpen(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")
	_, done, err := workflowInputDone(store, root)
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
	_, done, err := workflowInputDone(store, root)
	if err != nil {
		t.Fatalf("workflowInputDone: %v", err)
	}
	if done {
		t.Fatal("done = true with no input convoy, want false")
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
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead) (string, bool, error) {
			return "root-1", true, nil
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
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			t.Fatalf("unexpected claim of %s", beadID)
			return beads.Bead{}, false, nil
		},
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead) (string, bool, error) {
			return "root-1", true, nil
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
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead) (string, bool, error) {
			return "root-1", false, nil
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			t.Fatalf("unexpected skip of %s", rootID)
			return nil
		},
		ListContinuation: func(_ context.Context, _ string, _ []string, _, _ string) ([]beads.Bead, error) {
			return nil, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, _, _ string) error { return nil },
		ResolveWorkBranch:  func(string) string { return "" },
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
