package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// End-to-end doHookClaim coverage for fresh workflow-root admission (#6461).
// TestWorkflowRootAdmissionAtClaimBoundary pins the predicates directly; these
// run the full claim command so the drain result, claim_rejected emission and
// candidate ordering are asserted as the worker sees them.

const (
	wfRootID = "wf-root"
	wfStepID = "wf-step"
	wfRoute  = "rig/worker"
)

func expandedWorkflowRootRow(assignee, status string) string {
	row := map[string]any{
		"id":       wfRootID,
		"status":   status,
		"assignee": assignee,
		"metadata": map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
			beadmeta.WorkflowExpandedMetadataKey: "true",
			beadmeta.RoutedToMetadataKey:         wfRoute,
		},
	}
	raw, _ := json.Marshal(row)
	return string(raw)
}

func readyWorkflowStepRow() string {
	row := map[string]any{
		"id":     wfStepID,
		"status": "open",
		"metadata": map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindTask,
			beadmeta.RootBeadIDMetadataKey: wfRootID,
			beadmeta.RoutedToMetadataKey:   wfRoute,
		},
	}
	raw, _ := json.Marshal(row)
	return string(raw)
}

func workflowRootClaimOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "worker-b",
		IdentityCandidates: []string{"worker-b"},
		RouteTargets:       []string{wfRoute},
		JSON:               true,
	}
}

func decodeClaimResult(t *testing.T, stdout *bytes.Buffer) hookClaimJSONResult {
	t.Helper()
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	return result
}

// Worker A owns the executable child, so the only unassigned row a custom or
// stale query can still hand worker B is the open root: no work, no claim.
func TestDoHookClaimRefusesExpandedWorkflowRootWhenChildIsOwned(t *testing.T) {
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return `[` + expandedWorkflowRootRow("", "open") + `]`, nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			t.Fatalf("claim must not run for expanded workflow root %s", beadID)
			return beads.Bead{}, false, nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", t.TempDir(), workflowRootClaimOpts(), ops, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookClaim(expanded root only) = %d, want 1 (no work); stderr=%s", code, stderr.String())
	}
	if result := decodeClaimResult(t, &stdout); result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v, want drain/no_work", result)
	}
}

// B discovers both rows, A wins the child between discovery and claim, and B's
// candidate walk must skip the root rather than take it as fallback ownership.
func TestDoHookClaimLostChildRaceDoesNotFallThroughToExpandedRoot(t *testing.T) {
	var attempted, rejected []string
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[` + readyWorkflowStepRow() + `,` + expandedWorkflowRootRow("", "open") + `]`, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			attempted = append(attempted, beadID)
			if beadID == wfStepID {
				return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "worker-a"}, false, nil
			}
			t.Fatalf("claim fell through to expanded workflow root %s after losing the child", beadID)
			return beads.Bead{}, false, nil
		},
		EmitClaimRejected: func(beadID, _, _ string) { rejected = append(rejected, beadID) },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", t.TempDir(), workflowRootClaimOpts(), ops, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookClaim(lost child race) = %d, want 1 (no work); stderr=%s", code, stderr.String())
	}
	if len(attempted) != 1 || attempted[0] != wfStepID {
		t.Fatalf("claim attempts = %v, want exactly [%s]", attempted, wfStepID)
	}
	if len(rejected) != 1 || rejected[0] != wfStepID {
		t.Fatalf("claim_rejected emissions = %v, want exactly [%s]", rejected, wfStepID)
	}
	if result := decodeClaimResult(t, &stdout); result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v, want drain/no_work (a lost race is not an error)", result)
	}
}

// Both rows unassigned: the child is claimed, the root is never touched.
func TestDoHookClaimClaimsReadyStepAheadOfExpandedRoot(t *testing.T) {
	var attempted []string
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[` + expandedWorkflowRootRow("", "open") + `,` + readyWorkflowStepRow() + `]`, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			attempted = append(attempted, beadID)
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: wfRoute}}, true, nil
		},
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", t.TempDir(), workflowRootClaimOpts(), ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim(root then step) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(attempted) != 1 || attempted[0] != wfStepID {
		t.Fatalf("claim attempts = %v, want exactly [%s]", attempted, wfStepID)
	}
	if result := decodeClaimResult(t, &stdout); result.BeadID != wfStepID || result.Reason != "claimed" {
		t.Fatalf("result = %+v, want %s claimed", result, wfStepID)
	}
}

// A never-expanded root is the unit of work (#2763) and is claimed as before.
func TestDoHookClaimStillClaimsRootOnlyWorkflowRoot(t *testing.T) {
	var row map[string]any
	if err := json.Unmarshal([]byte(expandedWorkflowRootRow("", "open")), &row); err != nil {
		t.Fatal(err)
	}
	delete(row["metadata"].(map[string]any), beadmeta.WorkflowExpandedMetadataKey)
	row["metadata"].(map[string]any)[beadmeta.NativeStepDependenciesMetadataKey] = "[]"
	raw, _ := json.Marshal([]any{row})
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(raw), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: wfRoute}}, true, nil
		},
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", t.TempDir(), workflowRootClaimOpts(), ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim(root-only root) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if result := decodeClaimResult(t, &stdout); result.BeadID != wfRootID || result.Reason != "claimed" {
		t.Fatalf("result = %+v, want %s claimed", result, wfRootID)
	}
}

// A root this session already owns is adopted as an existing assignment
// (#5474); admission gates fresh claims only.
func TestDoHookClaimAdoptsOwnedExpandedWorkflowRoot(t *testing.T) {
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[` + expandedWorkflowRootRow("worker-b", "in_progress") + `]`, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			t.Fatalf("claim must not run for an already-owned root %s", beadID)
			return beads.Bead{}, false, nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", t.TempDir(), workflowRootClaimOpts(), ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim(owned root) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if result := decodeClaimResult(t, &stdout); result.Reason != "existing_assignment" || result.BeadID != wfRootID {
		t.Fatalf("result = %+v, want existing_assignment on %s", result, wfRootID)
	}
}

// An open expanded root assigned to this session is a continuation anchor, not
// a fresh root claim. It must be promoted through the idempotent assigned-ready
// path even though an unassigned expanded root is not independently claimable.
func TestDoHookClaimPromotesOwnedOpenExpandedWorkflowRoot(t *testing.T) {
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[` + expandedWorkflowRootRow("worker-b", "open") + `]`, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			if beadID != wfRootID || assignee != "worker-b" {
				t.Fatalf("claim(%q, %q), want (%q, worker-b)", beadID, assignee, wfRootID)
			}
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{
				beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
				beadmeta.WorkflowExpandedMetadataKey: "true",
				beadmeta.RoutedToMetadataKey:         wfRoute,
			}}, true, nil
		},
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", t.TempDir(), workflowRootClaimOpts(), ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim(owned open root) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if result := decodeClaimResult(t, &stdout); result.Reason != "ready_assignment" || result.BeadID != wfRootID {
		t.Fatalf("result = %+v, want ready_assignment on %s", result, wfRootID)
	}
}
