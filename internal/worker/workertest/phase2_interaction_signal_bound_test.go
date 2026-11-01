package workertest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
	workerfake "github.com/gastownhall/gascity/internal/worker/fake"
)

// This file guards the fix for ga-4pxoo4r (WC-INT-000,
// TestPhase2RequiredInteractions): fakeInteractionSignalBound was a fixed 2s
// deadline on a whole standalone fake-worker run. Almost all of that run is
// the subprocess's own exec, Go runtime start and exit; the interaction signal
// itself typically takes a fraction of a millisecond. Under farm load a
// correct run took 2.37s and failed (review ga-2kxcvr4). The requirement (see
// RequirementInteractionSignal's description in catalog_phase2_data_test.go)
// promises that the blocked interaction signal and state are surfaced, not how
// fast, so the bound is a hang detector.
//
// This is the same fix and test shape as the sibling WC-BRINGUP-001 bound
// (ga-e4bhca, phase2_startup_bound_deadline_test.go): a bound-invariant guard,
// a positive case proving a slow-but-correct run passes, and a negative case
// proving a run that exceeds the hang budget still fails.

// TestPhase2InteractionSignalBoundStaysAHangDetector guards
// fakeInteractionSignalBound against shrinking back to a value sized for an
// idle box.
func TestPhase2InteractionSignalBoundStaysAHangDetector(t *testing.T) {
	t.Parallel()

	if fakeInteractionSignalBound < testutil.ExecRaceTimeout {
		t.Errorf("fakeInteractionSignalBound = %s, want >= testutil.ExecRaceTimeout (%s); "+
			"it bounds a real fake-worker subprocess from exec to exit, so TESTING.md's "+
			"Test deadline rule applies (ga-4pxoo4r)",
			fakeInteractionSignalBound, testutil.ExecRaceTimeout)
	}
	if fakeInteractionSignalBound < hangBudget {
		t.Errorf("fakeInteractionSignalBound = %s, want >= hangBudget (%s); "+
			"it is a hang detector for the fake worker's run, not a latency assertion — "+
			"reuse this package's hang budget instead of a bespoke deadline (ga-4pxoo4r)",
			fakeInteractionSignalBound, hangBudget)
	}
}

// interactionRunFixture builds a fakeStartupRun that satisfies every
// interactionSignalResult check except the elapsed-time one: a real on-disk
// "blocked" state file and the startup and interaction events a correct run
// emits. Only the elapsed time varies.
func interactionRunFixture(t *testing.T, profile ProfileID, elapsed time.Duration) fakeStartupRun {
	t.Helper()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.txt")
	if err := os.WriteFile(statePath, []byte("blocked\n"), 0o644); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}

	ready := time.Now()
	return fakeStartupRun{
		StatePath: statePath,
		EventPath: filepath.Join(dir, "events.jsonl"),
		Elapsed:   elapsed,
		Events: []workerfake.Event{
			{Kind: "state_transition", Provider: string(profile), State: "ready", Time: ready},
			{
				Kind:        "interaction",
				Provider:    string(profile),
				State:       "blocked",
				Time:        ready.Add(time.Millisecond),
				Interaction: &workerfake.InteractionEvent{Kind: "approval", RequestID: "req-1", State: "blocked"},
			},
		},
	}
}

// TestPhase2InteractionSignalResultToleratesASlowButCorrectRun is the direct
// expression of ga-4pxoo4r: a correct run that took longer than the old 2s
// bound, but stayed within the hang budget, must be a Pass.
func TestPhase2InteractionSignalResultToleratesASlowButCorrectRun(t *testing.T) {
	t.Parallel()

	const profile = ProfileID("phase2-interaction-deadline-fixture")
	for _, elapsed := range []time.Duration{
		2371800554 * time.Nanosecond, // what ga-2kxcvr4's remote run measured
		30 * time.Second,             // far over the old 2s bound, still under hangBudget
	} {
		result := interactionSignalResult(profile, interactionRunFixture(t, profile, elapsed))
		if !result.Passed() {
			t.Errorf("interactionSignalResult() = %+v, want Pass for a correct run that took "+
				"%s (over the old 2s bound but within hangBudget %s)",
				result, elapsed, hangBudget)
		}
	}
}

// TestPhase2InteractionSignalResultStillFailsWhenGenuinelyBroken is the
// required negative case: widening the bound to tolerate load must not also
// widen it into tolerating a stuck fake worker. A run that took longer than
// the hang budget still reports Fail, on the elapsed-time check itself. The
// case uses a synthetic elapsed time, so it runs without waiting out the
// budget.
func TestPhase2InteractionSignalResultStillFailsWhenGenuinelyBroken(t *testing.T) {
	t.Parallel()

	const profile = ProfileID("phase2-interaction-deadline-fixture")
	const overBudget = hangBudget + time.Second

	result := interactionSignalResult(profile, interactionRunFixture(t, profile, overBudget))
	if result.Passed() || !strings.Contains(result.Detail, "exceeded bound") {
		t.Fatalf("interactionSignalResult() = %+v, want an elapsed-time Fail when the run (%s) "+
			"exceeds even hangBudget (%s) — the hang detector must still detect a genuine hang",
			result, overBudget, hangBudget)
	}
}
