package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// admissionOrderingBase anchors the fixtures below. Every timestamp is a real
// clock reading so a zero WorkCreatedAt can only come from a request that
// genuinely carries no work bead.
var admissionOrderingBase = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

// sessionRequestIDs renders the sorted identity of each request so an ordering
// failure names the requests rather than dumping whole structs.
func sessionRequestIDs(requests []SessionRequest) []string {
	ids := make([]string, 0, len(requests))
	for _, req := range requests {
		switch {
		case req.WorkBeadID != "":
			ids = append(ids, req.WorkBeadID)
		case req.SessionBeadID != "":
			ids = append(ids, req.SessionBeadID)
		default:
			ids = append(ids, "<anonymous>")
		}
	}
	return ids
}

// permuteSessionRequests returns every ordering of requests, so a sort assertion
// can prove the result is a property of the comparator and not of input order.
func permuteSessionRequests(requests []SessionRequest) [][]SessionRequest {
	if len(requests) <= 1 {
		return [][]SessionRequest{append([]SessionRequest(nil), requests...)}
	}
	var out [][]SessionRequest
	for i := range requests {
		rest := make([]SessionRequest, 0, len(requests)-1)
		rest = append(rest, requests[:i]...)
		rest = append(rest, requests[i+1:]...)
		for _, tail := range permuteSessionRequests(rest) {
			perm := make([]SessionRequest, 0, len(requests))
			perm = append(perm, requests[i])
			perm = append(perm, tail...)
			out = append(out, perm)
		}
	}
	return out
}

// TestSortSessionRequests_AnonymousDoesNotBreakBeadBackedOrder pins the fix for
// the comparator's violated transitivity of equivalence. An anonymous request
// (no WorkCreatedAt, no WorkBeadID) used to compare equal to every peer in its
// priority band, so with two real beads it produced A≡X, X≡B and yet B<A. That
// is not a strict weak ordering, and sort.SliceStable then let the newer bead
// keep a slot ahead of the older one purely because of input order.
func TestSortSessionRequests_AnonymousDoesNotBreakBeadBackedOrder(t *testing.T) {
	newer := SessionRequest{
		Template:      "claude",
		Tier:          "new",
		BeadPriority:  intPtr(2),
		WorkBeadID:    "a-newer",
		WorkCreatedAt: admissionOrderingBase.Add(2 * time.Hour),
	}
	anonymous := SessionRequest{
		Template: "claude",
		Tier:     "new",
	}
	older := SessionRequest{
		Template:      "claude",
		Tier:          "new",
		BeadPriority:  intPtr(2),
		WorkBeadID:    "b-older",
		WorkCreatedAt: admissionOrderingBase,
	}

	// Oldest bead-backed work first, then the newer bead, then the anonymous
	// filler: a request with no work identity is the least specific claim on
	// the slot and sorts last within its band.
	want := []string{"b-older", "a-newer", "<anonymous>"}

	perms := permuteSessionRequests([]SessionRequest{newer, anonymous, older})
	if len(perms) != 6 {
		t.Fatalf("len(perms) = %d, want 6", len(perms))
	}
	for _, perm := range perms {
		input := sessionRequestIDs(perm)
		sorted := append([]SessionRequest(nil), perm...)
		sortSessionRequests(sorted, beads.PolicyPriorityFIFO)
		got := sessionRequestIDs(sorted)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sortSessionRequests(%v) = %v, want %v", input, got, want)
			}
		}
	}
}

// admissionOrderingFixtures spans every branch of the comparator: recovery and
// fresh tiers, present and missing priority, present and missing timestamps,
// present and missing work bead IDs, and equal-priority peers.
func admissionOrderingFixtures() []SessionRequest {
	return []SessionRequest{
		{Template: "claude", Tier: "resume", SessionBeadID: "sess-resume", BeadPriority: intPtr(3), WorkBeadID: "w-resume", WorkCreatedAt: admissionOrderingBase.Add(time.Hour)},
		{Template: "claude", Tier: "wake-known-identity", BeadPriority: intPtr(3), WorkBeadID: "w-wake", WorkCreatedAt: admissionOrderingBase},
		{Template: "claude", Tier: "new", SessionBeadID: "sess-inflight", WorkBeadID: "w-inflight"},
		{Template: "claude", Tier: "new", SessionBeadID: "sess-inflight-bare"},
		{Template: "claude", Tier: "new", BeadPriority: intPtr(0), WorkBeadID: "w-p0", WorkCreatedAt: admissionOrderingBase.Add(3 * time.Hour)},
		{Template: "claude", Tier: "new", BeadPriority: intPtr(2), WorkBeadID: "w-p2-old", WorkCreatedAt: admissionOrderingBase},
		{Template: "claude", Tier: "new", BeadPriority: intPtr(2), WorkBeadID: "w-p2-new", WorkCreatedAt: admissionOrderingBase.Add(time.Hour)},
		// Equal priority and equal timestamp: separated by ID alone.
		{Template: "claude", Tier: "new", BeadPriority: intPtr(2), WorkBeadID: "w-p2-tie-a", WorkCreatedAt: admissionOrderingBase},
		{Template: "claude", Tier: "new", BeadPriority: intPtr(2), WorkBeadID: "w-p2-tie-b", WorkCreatedAt: admissionOrderingBase},
		// Nil priority resolves to P2, matching the explicit P2 band above.
		{Template: "claude", Tier: "new", WorkBeadID: "w-nil-priority", WorkCreatedAt: admissionOrderingBase},
		{Template: "claude", Tier: "new"},
		{Template: "codex", Tier: "new"},
		{Template: "codex", Tier: "new", BeadPriority: intPtr(1), WorkBeadID: "w-codex-p1", WorkCreatedAt: admissionOrderingBase},
	}
}

// TestSessionRequestLess_IsStrictWeakOrdering proves the comparator satisfies
// the contract sort.Slice requires: irreflexivity, asymmetry, transitivity of
// the ordering, and transitivity of the induced equivalence. The last one is
// what the zero-guards used to break, and it is the one that made sort results
// depend on input order.
func TestSessionRequestLess_IsStrictWeakOrdering(t *testing.T) {
	policies := []beads.AdmissionPolicy{beads.PolicyPriorityFIFO, beads.PolicyFIFO}
	fixtures := admissionOrderingFixtures()

	for _, policy := range policies {
		t.Run(string(policy), func(t *testing.T) {
			less := func(a, b SessionRequest) bool { return sessionRequestLess(a, b, policy) }
			equiv := func(a, b SessionRequest) bool { return !less(a, b) && !less(b, a) }
			name := func(req SessionRequest) string {
				return fmt.Sprintf("%s/%s/%s/%s", req.Template, req.Tier, req.WorkBeadID, req.SessionBeadID)
			}

			for _, a := range fixtures {
				if less(a, a) {
					t.Errorf("irreflexivity violated: less(%s, %s) = true", name(a), name(a))
				}
			}
			for _, a := range fixtures {
				for _, b := range fixtures {
					if less(a, b) && less(b, a) {
						t.Errorf("asymmetry violated: %s and %s each precede the other", name(a), name(b))
					}
				}
			}
			for _, a := range fixtures {
				for _, b := range fixtures {
					for _, c := range fixtures {
						if less(a, b) && less(b, c) && !less(a, c) {
							t.Errorf("transitivity violated: %s < %s < %s but not %s < %s",
								name(a), name(b), name(c), name(a), name(c))
						}
						if equiv(a, b) && equiv(b, c) && !equiv(a, c) {
							t.Errorf("transitivity of equivalence violated: %s ~ %s ~ %s but %s !~ %s",
								name(a), name(b), name(c), name(a), name(c))
						}
					}
				}
			}
		})
	}
}

// TestSortSessionRequests_RecoveryFirstOutranksPriorityAndTime pins the
// invariant the ordering fix must not disturb: capacity already committed to a
// running session is preserved ahead of any fresh request, however urgent or
// old, and resume-like recovery outranks an in-flight create. Recovery is
// compared before priority and before time under every policy.
func TestSortSessionRequests_RecoveryFirstOutranksPriorityAndTime(t *testing.T) {
	resume := SessionRequest{
		Template:      "claude",
		Tier:          "resume",
		SessionBeadID: "sess-resume",
		BeadPriority:  intPtr(4),
		WorkBeadID:    "w-resume",
		WorkCreatedAt: admissionOrderingBase.Add(9 * time.Hour),
	}
	wake := SessionRequest{
		Template:      "claude",
		Tier:          "wake-known-identity",
		BeadPriority:  intPtr(4),
		WorkBeadID:    "w-wake",
		WorkCreatedAt: admissionOrderingBase.Add(9 * time.Hour),
	}
	inFlight := SessionRequest{
		Template:      "claude",
		Tier:          "new",
		SessionBeadID: "sess-inflight",
		BeadPriority:  intPtr(4),
		WorkBeadID:    "w-inflight",
	}
	urgentFresh := SessionRequest{
		Template:      "claude",
		Tier:          "new",
		BeadPriority:  intPtr(0),
		WorkBeadID:    "w-p0",
		WorkCreatedAt: admissionOrderingBase,
	}

	for _, policy := range []beads.AdmissionPolicy{beads.PolicyPriorityFIFO, beads.PolicyFIFO} {
		t.Run(string(policy), func(t *testing.T) {
			for _, perm := range permuteSessionRequests([]SessionRequest{urgentFresh, inFlight, wake, resume}) {
				input := sessionRequestIDs(perm)
				sorted := append([]SessionRequest(nil), perm...)
				sortSessionRequests(sorted, policy)
				got := sessionRequestIDs(sorted)

				// Resume-like recovery, then in-flight recovery, then the
				// urgent fresh request last.
				if got[3] != "w-p0" {
					t.Fatalf("sortSessionRequests(%v) = %v; fresh P0 must not displace committed capacity", input, got)
				}
				if got[2] != "w-inflight" {
					t.Fatalf("sortSessionRequests(%v) = %v; in-flight create must follow resume-like recovery", input, got)
				}
				if got[0] != "w-resume" || got[1] != "w-wake" {
					t.Fatalf("sortSessionRequests(%v) = %v; resume-like recovery must sort first", input, got)
				}
			}
		})
	}
}

// TestSortSessionRequests_FIFOPolicySkipsPriorityBand pins the policy gate: a
// pool on beads.PolicyFIFO admits strictly by arrival, so a newer P0 request
// must not jump an older P2 one. Under the default priority_fifo it must.
func TestSortSessionRequests_FIFOPolicySkipsPriorityBand(t *testing.T) {
	newerUrgent := SessionRequest{
		Template:      "claude",
		Tier:          "new",
		BeadPriority:  intPtr(0),
		WorkBeadID:    "w-p0-newer",
		WorkCreatedAt: admissionOrderingBase.Add(time.Hour),
	}
	olderRoutine := SessionRequest{
		Template:      "claude",
		Tier:          "new",
		BeadPriority:  intPtr(2),
		WorkBeadID:    "w-p2-older",
		WorkCreatedAt: admissionOrderingBase,
	}

	cases := []struct {
		policy beads.AdmissionPolicy
		want   []string
	}{
		{policy: beads.PolicyPriorityFIFO, want: []string{"w-p0-newer", "w-p2-older"}},
		{policy: beads.PolicyFIFO, want: []string{"w-p2-older", "w-p0-newer"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.policy), func(t *testing.T) {
			for _, perm := range permuteSessionRequests([]SessionRequest{newerUrgent, olderRoutine}) {
				sorted := append([]SessionRequest(nil), perm...)
				sortSessionRequests(sorted, tc.policy)
				got := sessionRequestIDs(sorted)
				for i := range tc.want {
					if got[i] != tc.want[i] {
						t.Fatalf("policy %q: sortSessionRequests(%v) = %v, want %v",
							tc.policy, sessionRequestIDs(perm), got, tc.want)
					}
				}
			}
		})
	}
}
