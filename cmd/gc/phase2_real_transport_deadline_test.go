//go:build integration

package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// This file guards the fix for ga-cv2tf0: phase2RealTransportBound and
// phase2RealTransportMarkerBound were fixed, sub-floor deadlines (5s / 500ms)
// racing a real tmux session start. Sized for an idle box, they fired under
// CI/fleet contention even though nothing was wedged — WC-TRANSPORT-001 (see
// its description in internal/worker/workertest/catalog.go) proves real
// transport delivery, not startup speed. This is the same flake shape
// TestHangBudgetStaysAHangDetector already guards for the rest of this
// package (ga-h51wa1, hangbudget_helpers_test.go); these tests apply the
// identical two-floor pattern to the real-transport proof's own deadlines,
// plus a pure-function proof that widening the deadline could not also
// widen it into tolerating a genuinely broken transport.

// TestPhase2RealTransportBoundsStayAHangDetector guards the two deadlines
// against exactly the regression that caused ga-cv2tf0: shrinking either one
// back down to a fixed value sized for an idle box.
func TestPhase2RealTransportBoundsStayAHangDetector(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		bound time.Duration
	}{
		{"phase2RealTransportBound", phase2RealTransportBound},
		{"phase2RealTransportMarkerBound", phase2RealTransportMarkerBound},
	} {
		if tc.bound < testutil.ExecRaceTimeout {
			t.Errorf("%s = %s, want >= testutil.ExecRaceTimeout (%s); it races a real tmux session start, so TESTING.md's Test deadline rule applies",
				tc.name, tc.bound, testutil.ExecRaceTimeout)
		}
		if tc.bound < hangBudget {
			t.Errorf("%s = %s, want >= hangBudget (%s); it is a hang detector for a real transport start, not a latency assertion (ga-cv2tf0) — reuse this package's existing hang budget instead of a second bespoke deadline",
				tc.name, tc.bound, hangBudget)
		}
	}
}

// phase2CorrectRun returns a phase2RealTransportRun that satisfies every
// content assertion in phase2RealTransportResult except (deliberately) the
// elapsed-time bound, so callers can isolate that one axis.
func phase2CorrectRun(tc phase2ProviderCase, startElapsed time.Duration) phase2RealTransportRun {
	return phase2RealTransportRun{
		Transport:         "tmux",
		Started:           true,
		ObservedProvider:  tc.family,
		AutonomousStarted: true,
		RunningAfterInput: true,
		StartElapsed:      startElapsed,
	}
}

// TestPhase2RealTransportResultToleratesASlowButCorrectStart is the direct
// expression of ga-cv2tf0's acceptance criterion that the proof must pass
// under host load: a startup that took longer than the old 5s bound but
// completed correctly, and stays within the hang budget, must be a Pass —
// WC-TRANSPORT-001 proves delivery, not speed. Before the fix this failed:
// phase2RealTransportBound was 5s, so an 8s-but-correct run was a false Fail.
func TestPhase2RealTransportResultToleratesASlowButCorrectStart(t *testing.T) {
	t.Parallel()

	tc := phase2ProviderCase{profileID: "phase2-deadline-fixture", family: "fixture"}
	const slowButUnderBudget = 8 * time.Second // > old 5s bound, < hangBudget
	run := phase2CorrectRun(tc, slowButUnderBudget)

	result := phase2RealTransportResult(tc, run)
	if !result.Passed() {
		t.Fatalf("phase2RealTransportResult() = %+v, want Pass for a startup that took %s (over the old 5s bound but within hangBudget %s) with every other proof correct",
			result, slowButUnderBudget, hangBudget)
	}
}

// TestPhase2RealTransportResultStillFailsWhenGenuinelyBroken is ga-cv2tf0's
// required negative case: widening the deadline to tolerate load must not
// also widen it into tolerating an actually-broken transport. A run that
// never completed Start (or otherwise never caught up within the hang
// budget) still reports Fail regardless of how generous the bound is.
func TestPhase2RealTransportResultStillFailsWhenGenuinelyBroken(t *testing.T) {
	t.Parallel()

	tc := phase2ProviderCase{profileID: "phase2-deadline-fixture", family: "fixture"}

	t.Run("start_never_completes", func(t *testing.T) {
		t.Parallel()
		run := phase2RealTransportRun{
			ErrorStage:   "start",
			Error:        "context deadline exceeded",
			StartElapsed: hangBudget,
		}
		result := phase2RealTransportResult(tc, run)
		if result.Passed() {
			t.Fatalf("phase2RealTransportResult() = %+v, want Fail for a Start() that never completed even within hangBudget", result)
		}
	})

	t.Run("exceeds_even_the_hang_budget", func(t *testing.T) {
		t.Parallel()
		const overBudget = hangBudget + time.Second
		run := phase2CorrectRun(tc, overBudget)
		result := phase2RealTransportResult(tc, run)
		if result.Passed() {
			t.Fatalf("phase2RealTransportResult() = %+v, want Fail when elapsed (%s) exceeds even hangBudget (%s) — the hang detector must still detect a genuine hang",
				result, overBudget, hangBudget)
		}
	})
}
