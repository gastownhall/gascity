package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// workflowRootCandidates returns a slice of candidates suitable for testing
// the hook-claim-continuation-nudge path: a workflow root that routes to
// route-1 and carries a continuation group so preassignHookContinuationGroup
// will assign siblings.
func workflowRootCandidates() []beads.Bead {
	return []beads.Bead{{
		ID:     "root-1",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "workflow",
			"gc.run_target":         "route-1",
			"gc.root_bead_id":       "root-1",
			"gc.continuation_group": "group-a",
		},
	}}
}

// stepBeadCandidates returns candidates whose gc.kind is "task" (a step
// bead), which must NOT trigger the hook-claim-continuation nudge even when
// siblings are assigned. Step beads are routed via gc.routed_to (not
// gc.run_target), so we set that for route matching.
func stepBeadCandidates() []beads.Bead {
	return []beads.Bead{{
		ID:     "step-1",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "task",
			"gc.routed_to":          "route-1",
			"gc.root_bead_id":       "root-1",
			"gc.continuation_group": "group-a",
		},
	}}
}

// buildContinuationNudgeOps returns a hookClaimOps that captures nudge
// enqueue calls. siblingIDs controls what ListContinuation returns.
func buildContinuationNudgeOps(candidates []beads.Bead, siblingIDs []string, enqueued *[]string) hookClaimOps {
	siblings := make([]beads.Bead, 0, len(siblingIDs))
	for _, id := range siblingIDs {
		siblings = append(siblings, beads.Bead{
			ID:     id,
			Status: "open",
			Metadata: map[string]string{
				"gc.kind":               "workflow",
				"gc.run_target":         "route-1",
				"gc.root_bead_id":       "root-1",
				"gc.continuation_group": "group-a",
			},
		})
	}
	return hookClaimOps{
		Runner: func(string, string) (string, error) {
			out, _ := json.Marshal(candidates)
			return string(out), nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			meta := candidates[0].Metadata
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: meta}, true, nil
		},
		ListContinuation: func(_ context.Context, _ string, _ []string, _, _ string) ([]beads.Bead, error) {
			return siblings, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, _, _ string) error {
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
		EnqueueContinuationNudge: func(assignee string) {
			*enqueued = append(*enqueued, assignee)
		},
	}
}

// TestHookClaimContinuationNudge_WorkflowRootEnqueuesNudge verifies that
// claiming a workflow root that pre-assigns at least one sibling enqueues a
// hook-claim-continuation nudge for the claiming session name.
func TestHookClaimContinuationNudge_WorkflowRootEnqueuesNudge(t *testing.T) {
	var enqueued []string
	ops := buildContinuationNudgeOps(workflowRootCandidates(), []string{"sib-1", "sib-2"}, &enqueued)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "gascity/worker/slot-0",
		IdentityCandidates: []string{"gascity/worker/slot-0"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(enqueued) != 1 || enqueued[0] != "gascity/worker/slot-0" {
		t.Fatalf("continuation nudge enqueued for %v, want [gascity/worker/slot-0]", enqueued)
	}
}

// TestHookClaimContinuationNudge_StepBeadNoNudge verifies that claiming a
// step bead (gc.kind="task") does NOT enqueue a hook-claim-continuation
// nudge even when siblings are pre-assigned. The nudge is only needed for
// self-propelling pool workflow roots; step claims happen while the session
// is already executing and will naturally poll.
func TestHookClaimContinuationNudge_StepBeadNoNudge(t *testing.T) {
	var enqueued []string
	ops := buildContinuationNudgeOps(stepBeadCandidates(), []string{"sib-1"}, &enqueued)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "gascity/worker/slot-0",
		IdentityCandidates: []string{"gascity/worker/slot-0"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(enqueued) != 0 {
		t.Fatalf("continuation nudge must not be enqueued for a step-bead claim, got %v", enqueued)
	}
}

// TestHookClaimContinuationNudge_NoSiblingsNoNudge verifies that claiming a
// workflow root that has NO unassigned siblings does NOT enqueue a nudge.
// An empty continuation means the session has no further work queued and
// should not be immediately propelled.
func TestHookClaimContinuationNudge_NoSiblingsNoNudge(t *testing.T) {
	var enqueued []string
	ops := buildContinuationNudgeOps(workflowRootCandidates(), nil /* no siblings */, &enqueued)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "gascity/worker/slot-0",
		IdentityCandidates: []string{"gascity/worker/slot-0"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(enqueued) != 0 {
		t.Fatalf("continuation nudge must not be enqueued when no siblings assigned, got %v", enqueued)
	}
}
