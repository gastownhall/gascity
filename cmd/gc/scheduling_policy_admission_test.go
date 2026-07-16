package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

var admissionBase = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

func pri(p int) *int { return &p }

func routed(id string, p int, age time.Duration) beads.Bead {
	return beads.Bead{
		ID:        id,
		Status:    "open",
		Priority:  pri(p),
		CreatedAt: admissionBase.Add(age),
		Metadata:  map[string]string{"gc.routed_to": "worker"},
	}
}

// Path: hook claims (ordering within one store's result).
func TestOrderedHookCandidatesFollowsPolicy(t *testing.T) {
	// oldP2 arrived first; newP0 is more urgent.
	candidates := []beads.Bead{routed("new-p0", 0, time.Hour), routed("old-p2", 2, 0)}

	if got := orderedHookCandidates(candidates, beads.PolicyPriorityFIFO)[0].ID; got != "new-p0" {
		t.Errorf("priority_fifo admitted %q first, want new-p0", got)
	}
	if got := orderedHookCandidates(candidates, beads.PolicyFIFO)[0].ID; got != "old-p2" {
		t.Errorf("fifo admitted %q first, want old-p2", got)
	}
	if got := orderedHookCandidates(candidates, "")[0].ID; got != "new-p0" {
		t.Errorf("unset admitted %q first, want new-p0 (#4322 default)", got)
	}
}

// Path: lost-claim retry. #4322 latches the band so a lost P0 cannot fall
// through to P2. Under fifo that latch must be inert or older work is skipped.
func TestLostClaimRetryBandLatchIsPolicyGated(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     beads.AdmissionPolicy
		wantTried  int
		wantFirst  string
		wantSecond string
	}{
		{
			name: "priority_fifo stops at the band edge", policy: beads.PolicyPriorityFIFO,
			wantTried: 1, wantFirst: "new-p0",
		},
		{
			name: "fifo scans past the band change", policy: beads.PolicyFIFO,
			wantTried: 2, wantFirst: "old-p2", wantSecond: "new-p0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tried []string
			ops := hookClaimOps{
				Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
					tried = append(tried, beadID)
					return beads.Bead{}, false, nil // always lose the race
				},
			}
			opts := hookClaimOptions{
				Assignee:     "worker-1",
				RouteTargets: []string{"worker"},
				Policy:       tc.policy,
			}
			candidates := orderedHookCandidates(
				[]beads.Bead{routed("new-p0", 0, time.Hour), routed("old-p2", 2, 0)},
				tc.policy,
			)
			claimFirstEligibleHookCandidate(candidates, opts, ops, t.TempDir(), &nopWriter{}, &nopWriter{})

			if len(tried) != tc.wantTried {
				t.Fatalf("tried %v, want %d attempt(s)", tried, tc.wantTried)
			}
			if tried[0] != tc.wantFirst {
				t.Errorf("first attempt %q, want %q", tried[0], tc.wantFirst)
			}
			if tc.wantSecond != "" && tried[1] != tc.wantSecond {
				t.Errorf("second attempt %q, want %q", tried[1], tc.wantSecond)
			}
		})
	}
}

// Path: cross-store claims. The tier-2 fresh-priority gate is #4322's
// cross-store arm of the same band latch, and must be policy-gated too.
func TestCrossStoreFreshPriorityGateIsPolicyGated(t *testing.T) {
	candidate := routed("old-p2", 2, 0)
	required := 0 // an active P0 band

	_, ok := hookClaimRank(candidate, hookClaimOptions{
		RouteTargets: []string{"worker"},
		Policy:       beads.PolicyPriorityFIFO,
	}, &required)
	if ok {
		t.Error("priority_fifo must reject an out-of-band P2 while a P0 band is active")
	}

	_, ok = hookClaimRank(candidate, hookClaimOptions{
		RouteTargets: []string{"worker"},
		Policy:       beads.PolicyFIFO,
	}, &required)
	if !ok {
		t.Error("fifo has no bands; the fresh-priority gate must not reject older work")
	}
}

// Recovery-first must survive under both policies: work this session already
// owns outranks any fresh claim, whatever the ordering policy says.
func TestRecoveryFirstHoldsUnderBothPolicies(t *testing.T) {
	mine := beads.Bead{ID: "mine", Status: "in_progress", Assignee: "worker-1", Priority: pri(4), CreatedAt: admissionBase.Add(9 * time.Hour)}
	fresh := routed("fresh-p0", 0, 0)

	for _, policy := range []beads.AdmissionPolicy{beads.PolicyPriorityFIFO, beads.PolicyFIFO} {
		opts := hookClaimOptions{
			Assignee:           "worker-1",
			IdentityCandidates: []string{"worker-1"},
			RouteTargets:       []string{"worker"},
			Policy:             policy,
		}
		mineRank, ok := hookClaimRank(mine, opts, nil)
		if !ok {
			t.Fatalf("%s: recovery candidate was not rankable", policy)
		}
		freshRank, ok := hookClaimRank(fresh, opts, nil)
		if !ok {
			t.Fatalf("%s: fresh candidate was not rankable", policy)
		}
		if !hookClaimRankLess(mineRank, freshRank, policy) {
			t.Errorf("%s: recovery of owned work must outrank a fresh claim", policy)
		}
	}
}

// Path: nested-cap admission. Recovery still wins; among new requests the
// policy decides who gets scarce capacity.
func TestSortSessionRequestsFollowsPolicy(t *testing.T) {
	newP0 := SessionRequest{Template: "a", Tier: "new", WorkBeadID: "new-p0", BeadPriority: pri(0), WorkCreatedAt: admissionBase.Add(time.Hour)}
	oldP2 := SessionRequest{Template: "a", Tier: "new", WorkBeadID: "old-p2", BeadPriority: pri(2), WorkCreatedAt: admissionBase}
	resume := SessionRequest{Template: "a", Tier: "resume", WorkBeadID: "resume-p4", BeadPriority: pri(4), SessionBeadID: "s1", WorkCreatedAt: admissionBase.Add(9 * time.Hour)}

	for _, tc := range []struct {
		policy    beads.AdmissionPolicy
		wantAfter string
	}{
		{policy: beads.PolicyPriorityFIFO, wantAfter: "new-p0"},
		{policy: beads.PolicyFIFO, wantAfter: "old-p2"},
	} {
		reqs := []SessionRequest{newP0, oldP2, resume}
		sortSessionRequests(reqs, tc.policy)

		if reqs[0].WorkBeadID != "resume-p4" {
			t.Errorf("%s: recovery-first violated: head is %q, want resume-p4", tc.policy, reqs[0].WorkBeadID)
		}
		if reqs[1].WorkBeadID != tc.wantAfter {
			t.Errorf("%s: admitted %q after recovery, want %q", tc.policy, reqs[1].WorkBeadID, tc.wantAfter)
		}
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
