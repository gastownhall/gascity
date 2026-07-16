package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Regression for review finding 1 on PR #4322.
//
// The federated claim loop scans for the best store, then RE-QUERIES the winner
// to revalidate before mutating. If another claimant takes the P0 inside that
// window, reselection used to run with activePriority still unset — the first
// scan's rank was discarded into `_` — so no band lock applied and the loop fell
// through to a weaker band, then latched onto THAT band. The band we observed
// must be locked before revalidation, not after selection.
func TestClaimPreservesFirstScanBandThroughRevalidation(t *testing.T) {
	stores := []hookStore{{dir: "city", env: []string{"GC_STORE=city"}}}

	// First scan sees a P0. Revalidation shows it gone (another claimant took
	// it) and only a P1 remains — the race window this test exists for.
	scans := 0
	run := func(_, _ string, _ []string) (string, error) {
		scans++
		if scans == 1 {
			return `[{"id":"hw-p0","status":"open","priority":0,"created_at":"2026-07-15T12:00:00Z","metadata":{"gc.routed_to":"worker"}},
			          {"id":"hw-p1","status":"open","priority":1,"created_at":"2026-07-15T12:00:00Z","metadata":{"gc.routed_to":"worker"}}]`, nil
		}
		return `[{"id":"hw-p1","status":"open","priority":1,"created_at":"2026-07-15T12:00:00Z","metadata":{"gc.routed_to":"worker"}}]`, nil
	}

	var claimed []string
	ops := hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimed = append(claimed, beadID)
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(string) string { return "" },
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Policy:             beads.PolicyPriorityFIFO,
	}

	var stdout, stderr strings.Builder
	claimHookWorkWithRunner("wq", "city", nil, stores, opts, ops, run,
		func(string, error) {}, &stdout, &stderr)

	for _, id := range claimed {
		if id == "hw-p1" {
			t.Fatalf("claimed %v: a lost P0 fell through to P1 — the band observed on the first scan was not locked before revalidation", claimed)
		}
	}
}

// The same window under fifo must NOT lock a band: fifo has no bands, so the
// remaining older work stays claimable rather than being fenced out.
func TestClaimDoesNotLockBandUnderFIFO(t *testing.T) {
	stores := []hookStore{{dir: "city", env: []string{"GC_STORE=city"}}}

	scans := 0
	run := func(_, _ string, _ []string) (string, error) {
		scans++
		if scans == 1 {
			return `[{"id":"hw-old-p2","status":"open","priority":2,"created_at":"2026-07-15T10:00:00Z","metadata":{"gc.routed_to":"worker"}},
			          {"id":"hw-new-p0","status":"open","priority":0,"created_at":"2026-07-15T12:00:00Z","metadata":{"gc.routed_to":"worker"}}]`, nil
		}
		return `[{"id":"hw-new-p0","status":"open","priority":0,"created_at":"2026-07-15T12:00:00Z","metadata":{"gc.routed_to":"worker"}}]`, nil
	}

	var claimed []string
	ops := hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimed = append(claimed, beadID)
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(string) string { return "" },
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Policy:             beads.PolicyFIFO,
	}

	var stdout, stderr strings.Builder
	claimHookWorkWithRunner("wq", "city", nil, stores, opts, ops, run,
		func(string, error) {}, &stdout, &stderr)

	if len(claimed) == 0 {
		t.Fatal("fifo claimed nothing: a band lock fenced out work that fifo must still admit")
	}
}
