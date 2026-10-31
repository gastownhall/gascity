package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func runLatchClaim(t *testing.T, candidates []beads.Bead, drainAck bool) (hookClaimJSONResult, []string, bool) {
	t.Helper()
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]beads.Bead, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.ID] = candidate
	}
	var claims []string
	acked := false
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claims = append(claims, beadID)
			claimed := byID[beadID]
			claimed.Status = "in_progress"
			claimed.Assignee = assignee
			return claimed, true, nil
		},
		DrainAck: func(io.Writer) error { acked = true; return nil },
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
			return byID[beadID], nil
		},
		ResolveWorkBranch:        func(string) string { return "" },
		StampWorkMeta:            func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		PublishRunMap:            func(string, string, ...string) error { return nil },
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           drainAck,
		JSON:               true,
	}, ops, &stdout, &stderr)
	if drainAck && code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	return decodeTurnBoundResult(t, stdout.String()), claims, acked
}

func TestHookClaimSkipsWorkflowAndScopeLatchesAtEveryClaimTier(t *testing.T) {
	for _, kind := range []string{beadmeta.KindWorkflow, beadmeta.KindScope} {
		for _, tier := range []string{"existing", "ready", "fresh"} {
			t.Run(kind+"/"+tier, func(t *testing.T) {
				latch := beads.Bead{
					ID:       "latch",
					Status:   "open",
					Metadata: map[string]string{beadmeta.KindMetadataKey: kind, beadmeta.RoutedToMetadataKey: "worker"},
				}
				switch tier {
				case "existing":
					latch.Status = "in_progress"
					latch.Assignee = "worker-1"
				case "ready":
					latch.Assignee = "worker-1"
				}
				result, claims, _ := runLatchClaim(t, []beads.Bead{
					latch,
					{ID: "step", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}},
				}, false)
				if result.Action != "work" || result.BeadID != "step" {
					t.Fatalf("result = %+v, want executable step", result)
				}
				if len(claims) != 1 || claims[0] != "step" {
					t.Fatalf("claim attempts = %v, want [step]", claims)
				}
			})
		}
	}
}

func TestHookClaimLeavesStaleLatchesUnchangedWhenNoWorkExists(t *testing.T) {
	for _, kind := range []string{beadmeta.KindWorkflow, beadmeta.KindScope} {
		for _, drainAck := range []bool{false, true} {
			policy := "without-drain-ack"
			if drainAck {
				policy = "with-drain-ack"
			}
			t.Run(kind+"/"+policy, func(t *testing.T) {
				result, claims, acked := runLatchClaim(t, []beads.Bead{{
					ID:       "latch",
					Status:   "in_progress",
					Assignee: "worker-1",
					Metadata: map[string]string{beadmeta.KindMetadataKey: kind, beadmeta.RoutedToMetadataKey: "worker"},
				}}, drainAck)
				if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
					t.Fatalf("result = %+v, want drain/no_work", result)
				}
				if len(claims) != 0 {
					t.Fatalf("claim attempts = %v, want none", claims)
				}
				if acked != drainAck || result.DrainAcknowledged != drainAck {
					t.Fatalf("ack = (%t, %t), want %t", acked, result.DrainAcknowledged, drainAck)
				}
			})
		}
	}
}

func TestHookClaimPreservesControlDispatcherEligibility(t *testing.T) {
	root := beads.Bead{
		ID:       "workflow-root",
		Status:   "in_progress",
		Assignee: "worker-1",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.RunTargetMetadataKey: "worker"},
	}
	result, claims, _ := runLatchClaim(t, []beads.Bead{root}, false)
	if result.Action != "work" || result.BeadID != root.ID || result.Reason != "existing_assignment" {
		t.Fatalf("result = %+v, want legacy workflow control assignment", result)
	}
	if len(claims) != 0 {
		t.Fatalf("claim attempts = %v, want none for existing assignment", claims)
	}

	for _, kind := range beadmeta.ControlKinds {
		candidate := beads.Bead{
			ID:       "control",
			Status:   "open",
			Metadata: map[string]string{beadmeta.KindMetadataKey: kind, beadmeta.RoutedToMetadataKey: "worker"},
		}
		if !hookCandidateClaimable(candidate, []string{"worker"}, hookClaimBudgetDeferredTestNow) {
			t.Errorf("control kind %q is not claimable", kind)
		}
	}
}
