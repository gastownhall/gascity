package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/session"
)

// TestDetectAbandonedCreateMatchesReconcilerLeaves is the drift guard for the
// shared abandoned-create rule. The reconciler keeps its clock-injected leaf
// forms (hot path, destructive), so two implementations of the same decision
// exist; this pins them to each other over an exhaustive grid of shapes,
// including +-1s around the 1m staleness, 10m never-started and in-flight
// window boundaries. With an explicit now the two must agree on every row.
func TestDetectAbandonedCreateMatchesReconcilerLeaves(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	states := []string{"creating", "start-pending", "active", "asleep", ""}
	lastWokes := map[string]string{
		"none":    "",
		"20s":     ago(20 * time.Second),
		"66s":     ago(66 * time.Second),
		"67s":     ago(67 * time.Second),
		"68s":     ago(68 * time.Second),
		"5m":      ago(5 * time.Minute),
		"62h":     ago(62 * time.Hour),
		"corrupt": "not-a-time",
	}
	anchors := map[string]struct {
		startedAt string
		createdAt time.Time
	}{
		"unanchored":    {},
		"59s":           {startedAt: ago(59 * time.Second)},
		"60s":           {startedAt: ago(60 * time.Second)},
		"61s":           {startedAt: ago(61 * time.Second)},
		"9m59s":         {startedAt: ago(10*time.Minute - time.Second)},
		"10m":           {startedAt: ago(10 * time.Minute)},
		"10m1s":         {startedAt: ago(10*time.Minute + time.Second)},
		"62h":           {startedAt: ago(62 * time.Hour)},
		"future":        {startedAt: now.Add(time.Hour).Format(time.RFC3339)},
		"created-60s":   {createdAt: now.Add(-60 * time.Second)},
		"created-59s":   {createdAt: now.Add(-59 * time.Second)},
		"corrupt-start": {startedAt: "garbage", createdAt: now.Add(-time.Hour)},
	}
	timeouts := []time.Duration{0, 30 * time.Second, time.Minute}

	checked, mismatches := 0, 0
	for _, state := range states {
		for _, claim := range []bool{false, true} {
			for lwName, lastWoke := range lastWokes {
				for aName, a := range anchors {
					for _, st := range timeouts {
						info := session.Info{
							ID:                     "gc-1",
							MetadataState:          state,
							PendingCreateClaim:     claim,
							LastWokeAt:             lastWoke,
							PendingCreateStartedAt: a.startedAt,
							CreatedAt:              a.createdAt,
						}
						fake := &clock.Fake{Time: now}
						want := sessionWakeStuckInFlightInfo(info) &&
							!pendingCreateLeaseActiveInfo(info, fake, st) &&
							pendingCreateAttemptStaleInfo(info, fake)
						_, got := session.DetectAbandonedCreate(info, now, st)
						checked++
						if got != want {
							mismatches++
							if mismatches <= 20 {
								t.Errorf("state=%q claim=%v last_woke=%s anchor=%s startupTimeout=%s: DetectAbandonedCreate=%v, reconciler leaves=%v",
									state, claim, lwName, aName, st, got, want)
							}
						}
					}
				}
			}
		}
	}
	if mismatches > 20 {
		t.Errorf("... and %d more mismatches", mismatches-20)
	}
	if checked < 1000 {
		t.Fatalf("grid shrank: only %d shapes checked", checked)
	}
}

// TestSessionAbandonedCreateBoundsAreShared pins that cmd/gc's bounds are the
// session package's, so the numbers live in one place.
func TestSessionAbandonedCreateBoundsAreShared(t *testing.T) {
	if staleCreatingStateTimeout != session.StaleCreatingTimeout {
		t.Errorf("staleCreatingStateTimeout = %s, want session.StaleCreatingTimeout %s", staleCreatingStateTimeout, session.StaleCreatingTimeout)
	}
	if pendingCreateNeverStartedTimeout != session.NeverStartedCreateLease {
		t.Errorf("pendingCreateNeverStartedTimeout = %s, want session.NeverStartedCreateLease %s", pendingCreateNeverStartedTimeout, session.NeverStartedCreateLease)
	}
	if staleKeyDetectDelay != session.StaleKeyDetectDelay {
		t.Errorf("staleKeyDetectDelay = %s, want session.StaleKeyDetectDelay %s", staleKeyDetectDelay, session.StaleKeyDetectDelay)
	}
}
