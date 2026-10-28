package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// The wait helpers that take a NudgeShadows answer over the beads-mode shadow
// view as they do over the legacy shadows (DES-NQ §8 NQ-3 View): a live
// record is queued, so the wait reminder is not queued again, and a delivered
// or failed record resolves the attempt. A record the queue-mode shim retired
// migrated answers as the gc:nudge shadow the file queue saved for the moved
// nudge: queued while the file queue holds the nudge, resolved once it
// delivers it, and unknown when no shadow exists. The helpers never see the
// migrated code, which nudgequeue.ClassifyTerminalState does not classify
// (ga-0qe8xy).
func TestNudgeShadowViewDrivesTheWaitHelpers(t *testing.T) {
	store := beads.NudgesStore{Store: openShimTestBeadsStore(t)}
	fronts, err := storebinding.NewBeadsNudgeFrontDoors(store)
	if err != nil {
		t.Fatalf("NewBeadsNudgeFrontDoors: %v", err)
	}
	now := time.Now().UTC()
	enqueue := func(id string) {
		t.Helper()
		if err := fronts.Queue.Enqueue(shimItem(id, now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	enqueue("n-live")
	for _, outcome := range []string{"accepted_for_injection", "injected", "injected_unobserved", "failed"} {
		enqueue("n-" + outcome)
		if err := fronts.Queue.Ack([]string{"n-" + outcome}, outcome, "", ""); err != nil {
			t.Fatalf("Ack(%s): %v", outcome, err)
		}
	}
	enqueue("n-dead")
	if dead, err := fronts.Queue.RecordFailure([]string{"n-dead"}, storebinding.ErrNudgeSessionFenceMismatch, now); err != nil || len(dead) != 1 {
		t.Fatalf("RecordFailure = %v, %v", shimIDs(dead), err)
	}

	// The shim acks a record migrated once the file queue holds the nudge
	// under an open gc:nudge shadow, which the file queue retires when it
	// delivers the nudge.
	legacy := nudgequeue.NewStore(store)
	migrate := func(id string, shadowed bool) nudgequeue.Item {
		t.Helper()
		item := shimItem(id, now)
		enqueue(id)
		if shadowed {
			beadID, _, err := legacy.Save(item)
			if err != nil {
				t.Fatalf("Save(%s): %v", id, err)
			}
			item.BeadID = beadID
		}
		if err := fronts.Queue.Ack([]string{id}, nudgeQueueMigratedOutcome, nudgeQueueMigratedReason, nudgeQueueMigratedBoundary); err != nil {
			t.Fatalf("Ack(%s, %s): %v", id, nudgeQueueMigratedOutcome, err)
		}
		return item
	}
	migrate("n-migrated", true)
	if err := legacy.Terminalize(migrate("n-migrated-delivered", true), "injected", "", "", now); err != nil {
		t.Fatalf("Terminalize(n-migrated-delivered): %v", err)
	}
	migrate("n-migrated-unshadowed", false)

	for _, tc := range []struct {
		id       string
		resolved bool
		next     string
	}{
		{id: "n-live", resolved: true, next: ""},
		{id: "n-accepted_for_injection", resolved: true, next: "2"},
		{id: "n-injected", resolved: true, next: "2"},
		{id: "n-injected_unobserved", resolved: true, next: "2"},
		{id: "n-failed", resolved: true, next: "2"},
		{id: "n-dead", resolved: true, next: "2"},
		{id: "n-migrated", resolved: true, next: ""},
		{id: "n-migrated-delivered", resolved: true, next: "2"},
		{id: "n-migrated-unshadowed", resolved: false, next: "2"},
		{id: "n-unknown", resolved: false, next: "2"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			resolved, err := waitNudgeAttemptQueuedOrResolved(fronts.Shadows, tc.id)
			if err != nil {
				t.Fatalf("waitNudgeAttemptQueuedOrResolved: %v", err)
			}
			if resolved != tc.resolved {
				t.Fatalf("waitNudgeAttemptQueuedOrResolved = %v, want %v", resolved, tc.resolved)
			}
			wait := sessionpkg.WaitInfo{ID: "gc-wait-1", State: waitStateClosed, DeliveryAttempt: "1", NudgeID: tc.id}
			next, err := nextWaitDeliveryAttempt(fronts.Shadows, wait)
			if err != nil {
				t.Fatalf("nextWaitDeliveryAttempt: %v", err)
			}
			if next != tc.next {
				t.Fatalf("nextWaitDeliveryAttempt = %q, want %q", next, tc.next)
			}
		})
	}
}
