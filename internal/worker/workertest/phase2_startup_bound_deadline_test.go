package workertest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
	workerfake "github.com/gastownhall/gascity/internal/worker/fake"
)

// This file guards the fix for ga-e4bhca (WC-BRINGUP-001,
// TestPhase2StartupOutcomeBounds): fakeStartupPostControlOverhead was a fixed
// 2s deadline racing the standalone fake worker's own OS-scheduling and
// file-I/O overhead between its control_observed and state_transition
// events. Sized for an idle box, it failed under ordinary host/CI
// contention even when nothing was actually stuck — the requirement itself
// (see RequirementStartupOutcomeBound's description in catalog_phase2_data_test.go)
// only promises "a bounded startup outcome," not a fast one, and the check
// has no matching lower bound, confirming it was always meant as a hang
// detector rather than a performance assertion.
//
// This mirrors the identical fix and test shape already applied to the
// sibling WC-TRANSPORT-001 bound (ga-cv2tf0, see
// cmd/gc/phase2_real_transport_deadline_test.go): a bound-invariant guard,
// a positive case proving a slow-but-correct run now passes, and a negative
// case proving a genuinely broken run still fails no matter how generous
// the bound is.

// TestPhase2StartupOutcomeBoundStaysAHangDetector guards
// fakeStartupPostControlOverhead against exactly the regression that caused
// ga-e4bhca: shrinking it back down to a value sized for an idle box.
func TestPhase2StartupOutcomeBoundStaysAHangDetector(t *testing.T) {
	t.Parallel()

	if fakeStartupPostControlOverhead < testutil.ExecRaceTimeout {
		t.Errorf("fakeStartupPostControlOverhead = %s, want >= testutil.ExecRaceTimeout (%s); "+
			"it races a real fake-worker subprocess's own startup scheduling, so TESTING.md's "+
			"Test deadline rule applies (ga-e4bhca)",
			fakeStartupPostControlOverhead, testutil.ExecRaceTimeout)
	}
	if fakeStartupPostControlOverhead < hangBudget {
		t.Errorf("fakeStartupPostControlOverhead = %s, want >= hangBudget (%s); "+
			"it is a hang detector for the fake worker's own post-control transition, not a "+
			"latency assertion — reuse this package's hang budget instead of a bespoke deadline "+
			"(ga-e4bhca)",
			fakeStartupPostControlOverhead, hangBudget)
	}
}

// startupRunFixture builds a fakeStartupRun with a real on-disk state file
// and the three synthetic events startupOutcomeResult requires
// (control_waiting, control_observed, state_transition), so every one of
// its other assertions (state content, event ordering and identity,
// launch-to-wait bound) is satisfied and only the post-control elapsed-time
// axis is under test.
func startupRunFixture(t *testing.T, profile ProfileID, outcome string, postControl time.Duration) fakeStartupRun {
	t.Helper()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.txt")
	if err := os.WriteFile(statePath, []byte(outcome), 0o644); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}

	waiting := time.Now()
	observed := waiting.Add(time.Millisecond)
	transition := observed.Add(postControl)
	return fakeStartupRun{
		StatePath:    statePath,
		EventPath:    filepath.Join(dir, "events.jsonl"),
		LaunchToWait: time.Second,
		Events: []workerfake.Event{
			{Kind: "control_waiting", Provider: string(profile), Time: waiting},
			{Kind: "control_observed", Provider: string(profile), Time: observed},
			{Kind: "state_transition", Provider: string(profile), State: outcome, Time: transition},
		},
	}
}

// TestPhase2StartupOutcomeResultToleratesASlowButCorrectTransition is the
// direct expression of ga-e4bhca's acceptance criterion that the proof must
// pass under host load: a post-control transition that took far longer than
// the old 2s overhead, but completed correctly and stayed within the hang
// budget, must be a Pass — WC-BRINGUP-001 proves the outcome is delivered,
// not how fast.
func TestPhase2StartupOutcomeResultToleratesASlowButCorrectTransition(t *testing.T) {
	t.Parallel()

	const profile = ProfileID("phase2-startup-deadline-fixture")
	const outcome = "ready"
	const delay = 10 * time.Millisecond
	const slowButUnderBudget = 30 * time.Second // > old 2s overhead, < hangBudget

	run := startupRunFixture(t, profile, outcome, slowButUnderBudget)
	result := startupOutcomeResult(profile, outcome, delay, run)
	if !result.Passed() {
		t.Fatalf("startupOutcomeResult() = %+v, want Pass for a post-control transition that took "+
			"%s (over the old 2s overhead but within hangBudget %s) with every other proof correct",
			result, slowButUnderBudget, hangBudget)
	}
}

// TestPhase2StartupOutcomeResultStillFailsWhenGenuinelyBroken is
// ga-e4bhca's required negative case: widening the bound to tolerate load
// must not also widen it into tolerating a genuinely stuck fake worker. A
// post-control transition that never caught up within the hang budget still
// reports Fail regardless of how generous the bound is. The case uses
// synthetic event timestamps to model the late transition, so it runs
// without waiting out the budget.
func TestPhase2StartupOutcomeResultStillFailsWhenGenuinelyBroken(t *testing.T) {
	t.Parallel()

	const profile = ProfileID("phase2-startup-deadline-fixture")
	const outcome = "ready"
	const delay = 10 * time.Millisecond
	const overBudget = hangBudget + time.Second

	run := startupRunFixture(t, profile, outcome, overBudget)
	result := startupOutcomeResult(profile, outcome, delay, run)
	if result.Passed() {
		t.Fatalf("startupOutcomeResult() = %+v, want Fail when the post-control transition (%s) "+
			"exceeds even hangBudget (%s) — the hang detector must still detect a genuine hang",
			result, overBudget, hangBudget)
	}
}
