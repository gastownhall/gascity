package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// hookClaimBudgetDeferredTestNow is the fixed reference instant for every test
// in this file, so future/past framing never depends on wall-clock time.
var hookClaimBudgetDeferredTestNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func TestHookCandidateBudgetDeferredFutureTimestamp(t *testing.T) {
	candidate := beads.Bead{
		ID: "hw-future",
		Metadata: map[string]string{
			beadmeta.BudgetDeferredUntilMetadataKey: hookClaimBudgetDeferredTestNow.Add(time.Hour).Format(time.RFC3339),
		},
	}
	if !hookCandidateBudgetDeferred(candidate, hookClaimBudgetDeferredTestNow) {
		t.Fatal("want a candidate with a future gc.budget_deferred_until to be reported deferred")
	}
}

func TestHookCandidateBudgetDeferredPastTimestamp(t *testing.T) {
	candidate := beads.Bead{
		ID: "hw-past",
		Metadata: map[string]string{
			beadmeta.BudgetDeferredUntilMetadataKey: hookClaimBudgetDeferredTestNow.Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if hookCandidateBudgetDeferred(candidate, hookClaimBudgetDeferredTestNow) {
		t.Fatal("want a candidate whose gc.budget_deferred_until has already passed to no longer be deferred")
	}
}

func TestHookCandidateBudgetDeferredAbsentMetadata(t *testing.T) {
	candidate := beads.Bead{ID: "hw-absent"}
	if hookCandidateBudgetDeferred(candidate, hookClaimBudgetDeferredTestNow) {
		t.Fatal("want a candidate with no gc.budget_deferred_until metadata to not be deferred")
	}
}

func TestHookCandidateBudgetDeferredMalformedTimestamp(t *testing.T) {
	candidate := beads.Bead{
		ID: "hw-malformed",
		Metadata: map[string]string{
			beadmeta.BudgetDeferredUntilMetadataKey: "not-a-timestamp",
		},
	}
	if hookCandidateBudgetDeferred(candidate, hookClaimBudgetDeferredTestNow) {
		t.Fatal("want a malformed gc.budget_deferred_until to fail open (not deferred) rather than wedge the candidate forever")
	}
}

func TestHookCandidateClaimableFalseForFutureBudgetDeferred(t *testing.T) {
	candidate := beads.Bead{
		ID: "hw-routed",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:            "worker",
			beadmeta.BudgetDeferredUntilMetadataKey: hookClaimBudgetDeferredTestNow.Add(time.Hour).Format(time.RFC3339),
		},
	}
	if hookCandidateClaimable(candidate, []string{"worker"}, hookClaimBudgetDeferredTestNow) {
		t.Fatal("want a route-matched but future-budget-deferred candidate to be unclaimable")
	}
}

func TestHookCandidateClaimableTrueForPastBudgetDeferred(t *testing.T) {
	candidate := beads.Bead{
		ID: "hw-routed",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:            "worker",
			beadmeta.BudgetDeferredUntilMetadataKey: hookClaimBudgetDeferredTestNow.Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if !hookCandidateClaimable(candidate, []string{"worker"}, hookClaimBudgetDeferredTestNow) {
		t.Fatal("want a route-matched candidate whose budget deferral has expired to be claimable")
	}
}

// TestClaimFirstReadyHookAssignmentSkipsFutureBudgetDeferredCandidate locks in
// the gm-o45y5y fix at the ready-assignment tier: a candidate this session
// already owns must not be promoted while its budget deferral is still in the
// future. The injected Claim hard-fails the test if it is ever invoked, so a
// regression that drops the new skip condition is caught even though the
// candidate would otherwise claim successfully.
func TestClaimFirstReadyHookAssignmentSkipsFutureBudgetDeferredCandidate(t *testing.T) {
	candidates := []beads.Bead{
		{
			ID:       "hw-deferred",
			Status:   "open",
			Assignee: "worker",
			Metadata: map[string]string{
				beadmeta.BudgetDeferredUntilMetadataKey: hookClaimBudgetDeferredTestNow.Add(time.Hour).Format(time.RFC3339),
			},
		},
	}
	opts := hookClaimOptions{IdentityCandidates: []string{"worker"}}
	ops := hookClaimOps{
		Now: func() time.Time { return hookClaimBudgetDeferredTestNow },
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			t.Fatalf("Claim must not be called on a future-budget-deferred candidate, got %s", beadID)
			return beads.Bead{}, false, nil
		},
	}
	var stdout, stderr bytes.Buffer
	result := claimFirstReadyHookAssignment(candidates, opts, ops, t.TempDir(), &stdout, &stderr)
	if result.terminal {
		t.Fatalf("want a non-terminal result when the only candidate is budget-deferred, got terminal code=%d stderr=%s", result.code, stderr.String())
	}
}

// TestClaimFirstEligibleHookCandidateSkipsFutureBudgetDeferredCandidate is the
// fresh-claim-tier analog: a route-matched, unassigned candidate must not be
// handed out while gc.budget_deferred_until is still in the future.
func TestClaimFirstEligibleHookCandidateSkipsFutureBudgetDeferredCandidate(t *testing.T) {
	candidates := []beads.Bead{
		{
			ID: "hw-deferred",
			Metadata: map[string]string{
				beadmeta.RoutedToMetadataKey:            "worker",
				beadmeta.BudgetDeferredUntilMetadataKey: hookClaimBudgetDeferredTestNow.Add(time.Hour).Format(time.RFC3339),
			},
		},
	}
	opts := hookClaimOptions{RouteTargets: []string{"worker"}}
	ops := hookClaimOps{
		Now: func() time.Time { return hookClaimBudgetDeferredTestNow },
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			t.Fatalf("Claim must not be called on a future-budget-deferred candidate, got %s", beadID)
			return beads.Bead{}, false, nil
		},
	}
	var stdout, stderr bytes.Buffer
	result := claimFirstEligibleHookCandidate(candidates, opts, ops, t.TempDir(), &stdout, &stderr)
	if result.terminal {
		t.Fatalf("want a non-terminal result when the only candidate is budget-deferred, got terminal code=%d stderr=%s", result.code, stderr.String())
	}
}
