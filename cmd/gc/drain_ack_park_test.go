package main

import (
	"bytes"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// TestDrainAckParkReleasesTheHeldClaimAndRecordsNoAnomaly pins the park path
// the drain-ack-loop escalation prescribes (gastownhall/gascity#6940): a
// seat holding a started claim that is then put on hold:mayor — the recipe
// the escalation mails — runs its own `gc runtime drain-ack` and the ack
// succeeds, because a claim on a dispatch hold is never work the seat
// serves (the hook side of that fence is
// TestHookDrainAckFenceSkipsDispatchHolds); the release gives the claim
// back open and unassigned with its hold label kept; and the finalized
// stop records no session.drain_acked_with_assigned_work, because the
// released claim is no longer assigned work any arm of the classifier or
// the close gate finds. A regression that keeps the held claim refuses
// the seat's own ack and makes the finalize record the anomaly again, so
// the loop the escalation exists to end would never drain.
func TestDrainAckParkReleasesTheHeldClaimAndRecordsNoAnomaly(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = servingCity("worker")
	fake := events.NewFake()
	env.rec = fake

	row := env.createSessionBead("worker", "worker")
	held := seedHeldStartedClaim(t, env, &row)

	// The seat's own drain-ack runs the release with the started refusal
	// armed (releaseUnexecutedClaimsForSessionStore's !caller.operator): a
	// started claim on hold:mayor is not kept, so the ack proceeds instead
	// of refusing over it.
	seat, err := env.store.Get(row.ID)
	if err != nil {
		t.Fatalf("reading the session bead: %v", err)
	}
	var releaseStderr bytes.Buffer
	kept := releaseDrainAckClaims(
		testWorkLegs(env.city, env.cfg, env.store, nil), seat, true,
		drainAckReleaseBudget, &releaseStderr,
	)
	if len(kept) != 0 {
		t.Fatalf("the seat's own drain-ack was refused over %v; a claim on hold:mayor does not hold its seat's ack (stderr %s)", kept, releaseStderr.String())
	}

	claim, err := env.store.Get(held.ID)
	if err != nil {
		t.Fatalf("reading the released claim: %v", err)
	}
	if claim.Status != "open" || claim.Assignee != "" {
		t.Fatalf("the held claim is status=%q assignee=%q after the release, want open and unassigned (stderr %s)", claim.Status, claim.Assignee, releaseStderr.String())
	}
	if !slices.Contains(claim.Labels, beadmeta.HoldMayorLabel) {
		t.Fatalf("the released claim lost its hold label: %q", claim.Labels)
	}

	// The finalized stop of the acked seat: the released claim is open and
	// unassigned, so the close gate finds no assigned work and the finalize
	// records no drain-acked-with-assigned-work anomaly.
	env.setSessionMetadata(&row, session.DrainAckStopPendingPatch(env.clk.Now().UTC()))
	info := env.sessionInfo(row.ID)
	finalizeDrainAckStoppedSession(
		env.city, env.cfg, env.store, testSeatWork(env.city, env.cfg, env.store, nil),
		info, "worker", false, newFakeDrainOps(), env.dt, env.clk, fake, &env.stderr,
	)

	for _, ev := range fake.Events {
		if ev.Type == events.SessionDrainAckedWithAssignedWork {
			t.Fatalf("the finalized stop recorded %s for a parked, released claim (payload %s, stderr %s)", ev.Type, ev.Payload, env.stderr.String())
		}
	}
}
