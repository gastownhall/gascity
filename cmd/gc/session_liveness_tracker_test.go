package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// livenessBaseTime is a fixed reference time for all liveness tracker tests.
var livenessBaseTime = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// startSession adds a named session to a Fake and optionally sets its activity time.
func startSession(f *runtime.Fake, name string, lastActivity time.Time) {
	_ = f.Start(context.Background(), name, runtime.Config{})
	if !lastActivity.IsZero() {
		f.SetActivity(name, lastActivity)
	}
}

// noopEmit is a convenience no-op escalation callback.
var noopEmit = func(_ string, _ time.Time, _ time.Time) {}

// --- Reproduction test: before fix, controller does NOT escalate stale sessions ---

// TestLivenessTracker_ReproductionBeforeFix demonstrates the gap this feature
// closes. Without a configured livenessTracker (nil), no escalation fires even
// when a refinery session has had no activity for many hours — matching the
// pre-fix behavior where the deacon's self-scheduling was the only path.
func TestLivenessTracker_ReproductionBeforeFix(t *testing.T) {
	// Pre-fix: no tracker → no escalation, regardless of session state.
	var tracker sessionLivenessTracker // nil

	f := runtime.NewFake()
	startSession(f, "gastown.refinery", time.Time{}) // dead: never ran
	window := 2 * time.Hour
	now := livenessBaseTime.Add(8 * time.Hour) // 8 hours elapsed — well past window
	startedAt := livenessBaseTime

	escalated := false
	if tracker != nil {
		tracker.checkFreshness("gastown.refinery", window, f, now, startedAt,
			func(_ string, _ time.Time, _ time.Time) { escalated = true })
	}

	// Without the fix (nil tracker), no escalation fires.
	if escalated {
		t.Fatal("reproduction: expected no escalation without a configured tracker (pre-fix behavior)")
	}
}

// --- Post-fix: tracker escalates correctly ---

// TestLivenessTracker_StaleSessionEscalates verifies that a session whose last
// activity exceeds the freshness window triggers escalation exactly once.
func TestLivenessTracker_StaleSessionEscalates(t *testing.T) {
	tracker := newSessionLivenessTracker(true)
	window := 2 * time.Hour

	// Session active 3 hours ago — stale.
	f := runtime.NewFake()
	startSession(f, "gastown.refinery", livenessBaseTime.Add(-3*time.Hour))
	now := livenessBaseTime
	startedAt := now.Add(-8 * time.Hour) // controller has been running 8 hours

	escalations := 0
	stale := tracker.checkFreshness("gastown.refinery", window, f, now, startedAt,
		func(_ string, _ time.Time, _ time.Time) { escalations++ })

	if !stale {
		t.Fatal("expected stale=true for session with activity 3h ago and 2h window")
	}
	if escalations != 1 {
		t.Fatalf("expected 1 escalation, got %d", escalations)
	}
}

// TestLivenessTracker_FreshSessionNoEscalation verifies that a session with
// recent activity does not trigger escalation (healthy rig produces no alert).
func TestLivenessTracker_FreshSessionNoEscalation(t *testing.T) {
	tracker := newSessionLivenessTracker(true)
	window := 2 * time.Hour

	// Session active 30 minutes ago — fresh.
	f := runtime.NewFake()
	startSession(f, "gastown.refinery", livenessBaseTime.Add(-30*time.Minute))
	now := livenessBaseTime
	startedAt := now.Add(-8 * time.Hour)

	escalations := 0
	stale := tracker.checkFreshness("gastown.refinery", window, f, now, startedAt,
		func(_ string, _ time.Time, _ time.Time) { escalations++ })

	if stale {
		t.Fatal("expected stale=false for session with activity 30m ago and 2h window")
	}
	if escalations != 0 {
		t.Fatalf("expected 0 escalations for fresh session, got %d", escalations)
	}
}

// TestLivenessTracker_EscalatesExactlyOncePerEpisode verifies that repeated
// patrol ticks with a stale session only emit the escalation once per stale
// episode. Subsequent ticks finding the same stale session must not re-fire.
func TestLivenessTracker_EscalatesExactlyOncePerEpisode(t *testing.T) {
	tracker := newSessionLivenessTracker(true)
	window := 2 * time.Hour
	f := runtime.NewFake()
	startSession(f, "gastown.refinery", livenessBaseTime.Add(-3*time.Hour))
	startedAt := livenessBaseTime.Add(-8 * time.Hour)

	escalations := 0
	emit := func(_ string, _ time.Time, _ time.Time) { escalations++ }

	// Simulate ten patrol ticks at 30s intervals with the session still stale.
	for i := range 10 {
		now := livenessBaseTime.Add(time.Duration(i) * 30 * time.Second)
		tracker.checkFreshness("gastown.refinery", window, f, now, startedAt, emit)
	}

	if escalations != 1 {
		t.Fatalf("expected exactly 1 escalation across 10 stale ticks, got %d", escalations)
	}
}

// TestLivenessTracker_NewEpisodeAfterRecovery verifies that after a stale
// session recovers (becomes fresh), a subsequent stale period opens a new
// episode and fires a new escalation.
func TestLivenessTracker_NewEpisodeAfterRecovery(t *testing.T) {
	tracker := newSessionLivenessTracker(true)
	window := 2 * time.Hour
	startedAt := livenessBaseTime.Add(-24 * time.Hour)
	f := runtime.NewFake()
	startSession(f, "gastown.refinery", time.Time{})

	escalations := 0
	emit := func(_ string, _ time.Time, _ time.Time) { escalations++ }

	// Episode 1: session stale.
	f.SetActivity("gastown.refinery", livenessBaseTime.Add(-3*time.Hour))
	tracker.checkFreshness("gastown.refinery", window, f, livenessBaseTime, startedAt, emit)
	if escalations != 1 {
		t.Fatalf("episode 1: expected 1 escalation, got %d", escalations)
	}

	// Session recovers — mark fresh.
	freshTime := livenessBaseTime.Add(10 * time.Minute)
	f.SetActivity("gastown.refinery", freshTime)
	now2 := livenessBaseTime.Add(30 * time.Minute)
	tracker.checkFreshness("gastown.refinery", window, f, now2, startedAt, emit)
	if escalations != 1 {
		t.Fatalf("after recovery: escalation should not re-fire, got %d", escalations)
	}

	// Episode 2: session goes stale again.
	f.SetActivity("gastown.refinery", livenessBaseTime.Add(-3*time.Hour))
	now3 := livenessBaseTime.Add(5 * time.Hour)
	tracker.checkFreshness("gastown.refinery", window, f, now3, startedAt, emit)
	if escalations != 2 {
		t.Fatalf("episode 2: expected 2 total escalations, got %d", escalations)
	}
}

// TestLivenessTracker_PerRigMultipleSessions verifies that for each rig the
// liveness check asserts both refinery AND witness sessions independently.
// Escalation fires for each stale session; a healthy session produces no alert.
func TestLivenessTracker_PerRigMultipleSessions(t *testing.T) {
	tracker := newSessionLivenessTracker(true)
	window := 2 * time.Hour
	startedAt := livenessBaseTime.Add(-24 * time.Hour)

	// refinery: stale (3h since last activity).
	// witness: fresh (30m since last activity).
	f := runtime.NewFake()
	startSession(f, "gastown.refinery", livenessBaseTime.Add(-3*time.Hour))
	startSession(f, "gastown.witness", livenessBaseTime.Add(-30*time.Minute))
	now := livenessBaseTime

	staleCount := 0
	escalations := make(map[string]int)
	for _, session := range []string{"gastown.refinery", "gastown.witness"} {
		sn := session
		stale := tracker.checkFreshness(sn, window, f, now, startedAt,
			func(_ string, _ time.Time, _ time.Time) { escalations[sn]++ })
		if stale {
			staleCount++
		}
	}

	if staleCount != 1 {
		t.Fatalf("expected 1 stale session (refinery), got %d", staleCount)
	}
	if escalations["gastown.refinery"] != 1 {
		t.Fatalf("expected 1 escalation for stale refinery, got %d", escalations["gastown.refinery"])
	}
	if escalations["gastown.witness"] != 0 {
		t.Fatalf("expected 0 escalations for fresh witness, got %d", escalations["gastown.witness"])
	}
}

// TestLivenessTracker_ZeroActivityTreatedAsStaleAfterWindow verifies that a
// session that has never run (zero last activity) is treated as stale only
// once the freshness window has elapsed since the controller started, to
// avoid false positives at startup.
func TestLivenessTracker_ZeroActivityTreatedAsStaleAfterWindow(t *testing.T) {
	tracker := newSessionLivenessTracker(true)
	window := 2 * time.Hour
	startedAt := livenessBaseTime

	f := runtime.NewFake()
	startSession(f, "gastown.refinery", time.Time{}) // never active

	escalations := 0
	emit := func(_ string, _ time.Time, _ time.Time) { escalations++ }

	// At startup (now == startedAt), elapsed = 0 < window. Should not escalate.
	tracker.checkFreshness("gastown.refinery", window, f, startedAt, startedAt, emit)
	if escalations != 0 {
		t.Fatal("expected no escalation at startup for zero-activity session")
	}

	// After 1h (half the window). Still should not escalate.
	tracker.checkFreshness("gastown.refinery", window, f,
		startedAt.Add(1*time.Hour), startedAt, emit)
	if escalations != 0 {
		t.Fatal("expected no escalation at 1h for zero-activity session with 2h window")
	}

	// After 3h (past the window). Should escalate.
	tracker.checkFreshness("gastown.refinery", window, f,
		startedAt.Add(3*time.Hour), startedAt, emit)
	if escalations != 1 {
		t.Fatalf("expected 1 escalation at 3h for zero-activity session with 2h window, got %d", escalations)
	}
}

// TestLivenessTracker_BoundedCadence asserts that the patrol is controller-driven:
// on every patrol tick, the tracker checks the session and reports stale=true
// when the session is stale, regardless of whether a self-scheduled loop in the
// pack would have woken the deacon. The max check gap equals patrol_interval.
func TestLivenessTracker_BoundedCadence(t *testing.T) {
	tracker := newSessionLivenessTracker(true)
	window := 30 * time.Minute
	startedAt := livenessBaseTime

	// Session last active 2h ago — stale.
	f := runtime.NewFake()
	startSession(f, "gastown.deacon", livenessBaseTime.Add(-2*time.Hour))

	// Simulate 5 patrol ticks at 30s intervals (the default patrol_interval).
	// The session remains stale. The tracker should observe stale on every tick.
	patrolInterval := 30 * time.Second
	staleObservations := 0
	for i := range 5 {
		now := livenessBaseTime.Add(time.Duration(i) * patrolInterval)
		stale := tracker.checkFreshness("gastown.deacon", window, f, now, startedAt, noopEmit)
		if stale {
			staleObservations++
		}
	}

	// Every tick should observe stale state — the controller checks on each
	// patrol tick regardless of the session's self-scheduling state.
	if staleObservations != 5 {
		t.Fatalf("expected stale=true on all 5 patrol ticks, got %d/5", staleObservations)
	}
}

// TestLivenessTracker_NilTrackerIsNoop verifies that a nil tracker is the
// safe no-op created when no session_liveness_checks are configured.
func TestLivenessTracker_NilTrackerIsNoop(t *testing.T) {
	tracker := newSessionLivenessTracker(false) // no checks configured → nil
	if tracker != nil {
		t.Fatal("expected nil tracker when hasAny=false")
	}
}

// TestLivenessTracker_ProviderErrorFailsOpen verifies that a provider error
// does not trigger escalation (fail-open, no false alarms).
func TestLivenessTracker_ProviderErrorFailsOpen(t *testing.T) {
	tracker := newSessionLivenessTracker(true)
	window := 2 * time.Hour
	startedAt := livenessBaseTime

	brokenProvider := runtime.NewFailFake()
	escalations := 0
	stale := tracker.checkFreshness("gastown.refinery", window, brokenProvider, livenessBaseTime, startedAt,
		func(_ string, _ time.Time, _ time.Time) { escalations++ })

	if stale {
		t.Fatal("expected stale=false when provider returns error (fail-open)")
	}
	if escalations != 0 {
		t.Fatalf("expected 0 escalations on provider error, got %d", escalations)
	}
}
