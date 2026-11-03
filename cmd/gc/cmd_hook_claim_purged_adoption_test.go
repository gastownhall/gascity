package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The work query can serve a row for a bead the canonical store has already
// purged (the caching store outlives the purge), and the existing-assignment
// tier used to mint an existing_assignment receipt on that row alone — handing
// the seat a bead no bd mutation can address, the city-578k loop only a full
// session drain ever ended. These tests pin the confirmed-missing arm: the
// receipt is withheld, the session's pointer to the dead bead is cleared (only
// when it names the dead bead), and the claim falls through to the normal
// tiers, whose shared end is a drain the seat can actually leave on.

func purgedAdoptionRunner(deadID, liveID string) func(string, string) (string, error) {
	return func(string, string) (string, error) {
		rows := `[{"id":"` + deadID + `","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.continuation_group":"body"}}`
		if liveID != "" {
			rows += `,{"id":"` + liveID + `","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.continuation_group":"body"}}`
		}
		return rows + `]`, nil
	}
}

// purgedAdoptionReadWorkMeta answers the canonical readback per bead: the dead
// bead is confirmed absent, everything else resolves as owned by worker-1.
func purgedAdoptionReadWorkMeta(deadID string) func(context.Context, string, []string, string, string) (beads.Bead, error) {
	return func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
		if beadID == deadID {
			return beads.Bead{}, beads.ErrNotFound
		}
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "worker-1"}, nil
	}
}

func TestDoHookClaimDropsPurgedExistingAssignmentAndClaimsNextWork(t *testing.T) {
	var stampCalls []string
	ops := hookClaimOps{
		Runner:       purgedAdoptionRunner("hw-dead", "hw-live"),
		ReadWorkMeta: purgedAdoptionReadWorkMeta("hw-dead"),
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			if beadID != "hw-live" {
				t.Fatalf("claim = %q, want the live bead hw-live (the purged one must not be served)", beadID)
			}
			return beads.Bead{
				ID:       beadID,
				Status:   "in_progress",
				Assignee: assignee,
				Metadata: map[string]string{"gc.routed_to": "worker"},
			}, true, nil
		},
		ReadSessionCurrentClaim: func(sessionID string) (string, error) {
			if sessionID != "sess-1" {
				t.Fatalf("stamp read on session %q, want sess-1", sessionID)
			}
			return "hw-dead", nil
		},
		StampSessionClaim: func(sessionID, beadID string) error {
			stampCalls = append(stampCalls, sessionID+"="+beadID)
			return nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_SESSION_ID=sess-1"},
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim(purged existing assignment) = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "work" || result.BeadID != "hw-live" || result.Reason != "ready_assignment" {
		t.Fatalf("claim fell through to %+v, want ready_assignment on hw-live", result)
	}
	if strings.Contains(result.BeadID, "hw-dead") {
		t.Fatalf("purged bead %s was re-served", "hw-dead")
	}
	// First the confirmed-missing arm clears the pointer to the dead bead,
	// then the claim tier's normal restamp re-points the session at hw-live.
	if got := strings.Join(stampCalls, ","); got != "sess-1=,sess-1=hw-live" {
		t.Fatalf("stamp calls = %q, want the clear of the dead pointer followed by the live restamp", got)
	}
	if !strings.Contains(stderr.String(), "dropping hw-dead") {
		t.Fatalf("stderr = %q, want the confirmed-missing drop note", stderr.String())
	}
}

func TestDoHookClaimPurgedExistingAssignmentDrains(t *testing.T) {
	drainAcked := false
	var stampCalls []string
	ops := hookClaimOps{
		Runner:       purgedAdoptionRunner("hw-dead", ""),
		ReadWorkMeta: purgedAdoptionReadWorkMeta("hw-dead"),
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			t.Fatalf("claim ran for %q; a purged bead must never be claimed", beadID)
			return beads.Bead{}, false, nil
		},
		DrainAck: func(io.Writer) error {
			drainAcked = true
			return nil
		},
		// An empty work_started cell: the drain fence proves no started claim.
		ReadWorkStarted:         func(string) (string, error) { return "", nil },
		ReadSessionCurrentClaim: func(string) (string, error) { return "hw-dead", nil },
		StampSessionClaim: func(sessionID, beadID string) error {
			stampCalls = append(stampCalls, sessionID+"="+beadID)
			return nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_SESSION_ID=sess-1"},
		DrainAck:           true,
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim(purged assignment, nothing else) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !drainAcked {
		t.Fatal("drain ack was not called")
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "drain" || result.BeadID != "" {
		t.Fatalf("drain fell through to %+v, want a drain with no bead served", result)
	}
	if got := strings.Join(stampCalls, ","); got != "sess-1=" {
		t.Fatalf("stamp calls = %q, want exactly one clear of sess-1's pointer", got)
	}
}

func TestDoHookClaimPurgedExistingAssignmentKeepsForeignStamp(t *testing.T) {
	var stampCalls []string
	ops := hookClaimOps{
		Runner:       purgedAdoptionRunner("hw-dead", ""),
		ReadWorkMeta: purgedAdoptionReadWorkMeta("hw-dead"),
		Claim: func(_ context.Context, _ string, _ []string, _, _ string) (beads.Bead, bool, error) {
			return beads.Bead{}, false, nil
		},
		DrainAck: func(io.Writer) error { return nil },
		// An empty work_started cell: the drain fence proves no started claim.
		ReadWorkStarted: func(string) (string, error) { return "", nil },
		// The session's latest stamp names OTHER live work, not the purged
		// bead: clearing it would drop the demand-class drain veto
		// sessionOwnsLiveClaim reads off this stamp.
		ReadSessionCurrentClaim: func(string) (string, error) { return "hw-elsewhere", nil },
		StampSessionClaim: func(sessionID, beadID string) error {
			stampCalls = append(stampCalls, sessionID+"="+beadID)
			return nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_SESSION_ID=sess-1"},
		DrainAck:           true,
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim(purged assignment, foreign stamp) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(stampCalls) != 0 {
		t.Fatalf("stamp calls = %q, want the pointer naming other live work left alone", strings.Join(stampCalls, ","))
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "drain" || result.BeadID != "" {
		t.Fatalf("result = %+v, want a drain with no bead served", result)
	}
}

func TestDoHookClaimPurgedExistingAssignmentKeepsStampWhenUnreadable(t *testing.T) {
	var stampCalls []string
	ops := hookClaimOps{
		Runner:       purgedAdoptionRunner("hw-dead", ""),
		ReadWorkMeta: purgedAdoptionReadWorkMeta("hw-dead"),
		Claim: func(_ context.Context, _ string, _ []string, _, _ string) (beads.Bead, bool, error) {
			return beads.Bead{}, false, nil
		},
		DrainAck: func(io.Writer) error { return nil },
		// An empty work_started cell: the drain fence proves no started claim.
		ReadWorkStarted: func(string) (string, error) { return "", nil },
		ReadSessionCurrentClaim: func(string) (string, error) {
			return "", errors.New("session store unavailable")
		},
		StampSessionClaim: func(sessionID, beadID string) error {
			stampCalls = append(stampCalls, sessionID+"="+beadID)
			return nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_SESSION_ID=sess-1"},
		DrainAck:           true,
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim(purged assignment, unreadable stamp) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(stampCalls) != 0 {
		t.Fatalf("stamp calls = %q, want no clear while the stamp read failed", strings.Join(stampCalls, ","))
	}
	if !strings.Contains(stderr.String(), "leaving the current-claim stamp on session sess-1 unreadable") {
		t.Fatalf("stderr = %q, want the unreadable-stamp note", stderr.String())
	}
}

func TestCertifyHookAdoptionVerdicts(t *testing.T) {
	bead := beads.Bead{ID: "hw-x", Status: "in_progress", Assignee: "worker-1"}
	opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}}
	dir := "/tmp/work"
	var stderr bytes.Buffer

	// Confirmed absent: the one error the store maps "no such bead" to.
	ops := hookClaimOps{ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
		return beads.Bead{}, beads.ErrNotFound
	}}
	verdict, assignee := certifyHookAdoption(bead, opts, ops, dir, &stderr)
	if verdict != hookAdoptionMissing || assignee != "" {
		t.Fatalf("verdict = (%v, %q), want (hookAdoptionMissing, \"\")", verdict, assignee)
	}

	// A read that could not be MADE stays fail-open: evidence of nothing.
	ops = hookClaimOps{ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
		return beads.Bead{}, errors.New("dolt restarted")
	}}
	stderr.Reset()
	verdict, _ = certifyHookAdoption(bead, opts, ops, dir, &stderr)
	if verdict != hookAdoptionUnverified {
		t.Fatalf("verdict = %v on an unreadable readback, want hookAdoptionUnverified", verdict)
	}
	if !strings.Contains(stderr.String(), "without a canonical ownership readback") {
		t.Fatalf("stderr = %q, want the unverified-readback warning", stderr.String())
	}

	// Owned: canonical readback names this session.
	ops = hookClaimOps{ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
		return beads.Bead{ID: "hw-x", Status: "in_progress", Assignee: "worker-1"}, nil
	}}
	verdict, canonical := certifyHookAdoption(bead, opts, ops, dir, &stderr)
	if verdict != hookAdoptionOwned || canonical != "worker-1" {
		t.Fatalf("verdict = (%v, %q), want (hookAdoptionOwned, worker-1)", verdict, canonical)
	}

	// Refused: the canonical store records a different owner.
	ops = hookClaimOps{ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
		return beads.Bead{ID: "hw-x", Status: "in_progress", Assignee: "worker-2"}, nil
	}}
	stderr.Reset()
	verdict, _ = certifyHookAdoption(bead, opts, ops, dir, &stderr)
	if verdict != hookAdoptionRefused {
		t.Fatalf("verdict = %v on a foreign owner, want hookAdoptionRefused", verdict)
	}
}
