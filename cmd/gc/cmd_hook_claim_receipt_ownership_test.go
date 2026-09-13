package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The receipt invariant, pinned at every layer that can emit one.
//
// A `gc hook --claim` work receipt is a promise: the bead named in it is owned
// by the session reading it. The specimen's rogue seat executed beads#6038's
// preflight while the graph store recorded that step in_progress under a
// DIFFERENT seat — a receipt naming work its holder did not own. gc's own claim
// path was not the surface that produced it, but the only reason is these
// checks, and nothing failed loudly if one were deleted. Now something does.

// ownershipClaimResult is what a rigged Claim op hands back: a "successful"
// mutation whose readback disagrees about who owns the bead. Production ops
// cannot produce this — hookClaimThroughStore collapses it to ok=false — so
// these cases are exactly the regression each tier's own check exists to catch.
func ownershipClaimOp(status, assignee string) hookClaimFunc {
	return func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
		return beads.Bead{ID: beadID, Status: status, Assignee: assignee}, true, nil
	}
}

// Tier 1: the ready-assignment promoter. It promotes a bead this session already
// owns from open to in_progress, so BOTH halves are checkable — the status the
// CAS was supposed to move and the assignee it was supposed to keep.
func TestHookClaimReadyTierReceiptRequiresStoreOwnership(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		assignee string
	}{
		{"readback owned by another seat", "in_progress", "other-seat"},
		{"readback never left open", "open", "worker-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", "/rig", hookClaimOptions{
				Assignee:           "worker-1",
				IdentityCandidates: []string{"worker-1"},
				RouteTargets:       []string{"worker"},
				JSON:               true,
			}, hookClaimOps{
				Runner: func(string, string) (string, error) {
					return `[{"id":"work-1","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`, nil
				},
				Claim: ownershipClaimOp(tc.status, tc.assignee),
			}, &stdout, &stderr)

			if code != 1 {
				t.Fatalf("code = %d, want 1; a disputed readback is not a claim", code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want no receipt for a bead this session cannot prove it owns", stdout.String())
			}
			if !strings.Contains(stderr.String(), "claim readback") {
				t.Errorf("stderr = %q, want the readback diagnostic", stderr.String())
			}
		})
	}
}

// Tier 2: the fresh-claim path, which is where the overwhelming majority of
// receipts are minted. Its ownership guarantee came only from the op it calls;
// the tier trusted `ok` alone, so a regression in the op — or a new op that
// forgot the check — would have shipped a receipt naming another seat's bead.
func TestHookClaimEligibleTierReceiptRequiresStoreOwnership(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}, hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		Claim: ownershipClaimOp("in_progress", "other-seat"),
	}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1; a disputed readback is not a claim", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want no receipt naming another seat's bead", stdout.String())
	}
	if !strings.Contains(stderr.String(), "claim readback") {
		t.Errorf("stderr = %q, want the readback diagnostic", stderr.String())
	}
}

// A healthy claim still reports. Without this control the two pins above are
// satisfied by a path that refuses everything.
func TestHookClaimEligibleTierReportsAnOwnedClaim(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}, hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		Claim: ownershipClaimOp("in_progress", "worker-1"),
	}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"bead_id":"work-1"`) {
		t.Fatalf("stdout = %q, want the work receipt", stdout.String())
	}
}

// Layer 3: the shared post-mutation classifier every production op funnels
// through. This is where the eligible tier's ownership guarantee actually comes
// from, and it screens BOTH reads — the mutation's own projection and the
// canonical re-read — because either one can be the stale side of a lost race.
func TestHookClaimThroughStoreRefusesForeignOwnershipOnEitherRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutated   string
		canonical string
	}{
		{"mutation projection names another seat", "other-seat", "worker-1"},
		{"canonical readback names another seat", "worker-1", "other-seat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claimed, ok, err := hookClaimThroughStore("work-1", "worker-1",
				func() (beads.Bead, bool, error) {
					return beads.Bead{ID: "work-1", Status: "in_progress", Assignee: tc.mutated}, true, nil
				},
				func(string) (beads.Bead, error) {
					return beads.Bead{ID: "work-1", Status: "in_progress", Assignee: tc.canonical}, nil
				})
			if err != nil {
				t.Fatalf("hookClaimThroughStore: %v", err)
			}
			if ok {
				t.Fatalf("ok = true for a bead owned by %q/%q, want a non-claim", tc.mutated, tc.canonical)
			}
			if strings.TrimSpace(claimed.Assignee) == "worker-1" {
				t.Errorf("returned bead = %+v, want the foreign owner surfaced for the rejection event", claimed)
			}
		})
	}
}
