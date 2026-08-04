package main

import (
	"testing"
	"time"
)

// TestDemandMismatchTracker_BelowThresholdDoesNotPublish verifies that
// consecutive no-claim idle-kill cycles only start reporting once the
// configured threshold is reached, and that the count increments by one
// per cycle in the meantime.
func TestDemandMismatchTracker_BelowThresholdDoesNotPublish(t *testing.T) {
	t.Parallel()

	tr := newDemandMismatchTracker(3)
	now := time.Now()

	publish, count, _ := tr.recordIdleKillCycle("local-core/builder", false, 2, now)
	if publish || count != 1 {
		t.Fatalf("cycle 1: publish=%v count=%d, want false/1", publish, count)
	}

	publish, count, _ = tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(time.Minute))
	if publish || count != 2 {
		t.Fatalf("cycle 2: publish=%v count=%d, want false/2", publish, count)
	}
}

// TestDemandMismatchTracker_PublishesOnceAtThreshold verifies that crossing
// the threshold publishes exactly once — not on every cycle at or above it —
// per the bead's "bound event volume" requirement.
func TestDemandMismatchTracker_PublishesOnceAtThreshold(t *testing.T) {
	t.Parallel()

	tr := newDemandMismatchTracker(3)
	now := time.Now()

	tr.recordIdleKillCycle("local-core/builder", false, 2, now)
	tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(time.Minute))
	publish, count, firstSeen := tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(2*time.Minute))
	if !publish || count != 3 {
		t.Fatalf("cycle 3: publish=%v count=%d, want true/3 (threshold crossed)", publish, count)
	}
	if !firstSeen.Equal(now) {
		t.Fatalf("cycle 3: firstSeen=%v, want %v (episode start)", firstSeen, now)
	}

	publish, count, _ = tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(3*time.Minute))
	if publish || count != 4 {
		t.Fatalf("cycle 4: publish=%v count=%d, want false/4 (already published this episode)", publish, count)
	}
}

// TestDemandMismatchTracker_BacksOffByDoublingThreshold verifies the
// republish backoff: after the first publish at the threshold, the next
// publish only fires at 2x the threshold, then 4x, and so on — geometric
// backoff so a long-lived episode does not flood the event bus.
func TestDemandMismatchTracker_BacksOffByDoublingThreshold(t *testing.T) {
	t.Parallel()

	tr := newDemandMismatchTracker(3)
	now := time.Now()

	var lastPublish bool
	var lastCount int
	for i := 0; i < 6; i++ {
		lastPublish, lastCount, _ = tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(time.Duration(i)*time.Minute))
	}
	// Cycles 1..6: publishes expected at 3 and 6 only.
	if !lastPublish || lastCount != 6 {
		t.Fatalf("cycle 6: publish=%v count=%d, want true/6 (2x threshold backoff)", lastPublish, lastCount)
	}

	for i := 6; i < 11; i++ {
		publish, count, _ := tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(time.Duration(i)*time.Minute))
		if publish {
			t.Fatalf("cycle %d: publish=true count=%d, want false (before next backoff point 12)", i+1, count)
		}
	}

	publish, count, _ := tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(11*time.Minute))
	if !publish || count != 12 {
		t.Fatalf("cycle 12: publish=%v count=%d, want true/12 (4x threshold backoff)", publish, count)
	}
}

// TestDemandMismatchTracker_OpenWorkResetsEpisode verifies that a killed
// session which still held open assigned work is not treated as a
// stranded-demand cycle at all — it resets the streak rather than
// incrementing it, since FR-2 only describes sessions that claimed nothing.
func TestDemandMismatchTracker_OpenWorkResetsEpisode(t *testing.T) {
	t.Parallel()

	tr := newDemandMismatchTracker(3)
	now := time.Now()

	tr.recordIdleKillCycle("local-core/builder", false, 2, now)
	tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(time.Minute))

	publish, count, _ := tr.recordIdleKillCycle("local-core/builder", true, 2, now.Add(2*time.Minute))
	if publish || count != 0 {
		t.Fatalf("open-work cycle: publish=%v count=%d, want false/0 (reset)", publish, count)
	}

	// Next no-claim cycle starts a fresh episode at 1, not 4.
	publish, count, _ = tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(3*time.Minute))
	if publish || count != 1 {
		t.Fatalf("post-reset cycle: publish=%v count=%d, want false/1 (fresh episode)", publish, count)
	}
}

// TestDemandMismatchTracker_ZeroDemandResetsEpisode verifies that once pool
// demand for a template drops to zero, the streak resets — an idle pool
// with no outstanding work is not a stranded-demand cycle.
func TestDemandMismatchTracker_ZeroDemandResetsEpisode(t *testing.T) {
	t.Parallel()

	tr := newDemandMismatchTracker(3)
	now := time.Now()

	tr.recordIdleKillCycle("local-core/builder", false, 2, now)
	tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(time.Minute))

	publish, count, _ := tr.recordIdleKillCycle("local-core/builder", false, 0, now.Add(2*time.Minute))
	if publish || count != 0 {
		t.Fatalf("zero-demand cycle: publish=%v count=%d, want false/0 (reset)", publish, count)
	}
}

// TestDemandMismatchTracker_DemandDropStartsFreshEpisode verifies that a
// drop in template demand (while still positive) is treated as evidence
// that some session made progress, breaking the old episode — but the
// current no-claim observation still starts a new one at count 1 rather
// than being discarded outright.
func TestDemandMismatchTracker_DemandDropStartsFreshEpisode(t *testing.T) {
	t.Parallel()

	tr := newDemandMismatchTracker(3)
	now := time.Now()

	tr.recordIdleKillCycle("local-core/builder", false, 5, now)
	tr.recordIdleKillCycle("local-core/builder", false, 5, now.Add(time.Minute))

	publish, count, firstSeen := tr.recordIdleKillCycle("local-core/builder", false, 3, now.Add(2*time.Minute))
	if publish || count != 1 {
		t.Fatalf("demand-drop cycle: publish=%v count=%d, want false/1 (episode restarts)", publish, count)
	}
	if !firstSeen.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("demand-drop cycle: firstSeen=%v, want %v (new episode start)", firstSeen, now.Add(2*time.Minute))
	}
}

// TestDemandMismatchTracker_TemplatesAreIndependent verifies that per-
// template state does not leak across templates sharing the tracker.
func TestDemandMismatchTracker_TemplatesAreIndependent(t *testing.T) {
	t.Parallel()

	tr := newDemandMismatchTracker(3)
	now := time.Now()

	tr.recordIdleKillCycle("template-a", false, 2, now)
	tr.recordIdleKillCycle("template-a", false, 2, now.Add(time.Minute))
	publish, count, _ := tr.recordIdleKillCycle("template-a", false, 2, now.Add(2*time.Minute))
	if !publish || count != 3 {
		t.Fatalf("template-a cycle 3: publish=%v count=%d, want true/3", publish, count)
	}

	publish, count, _ = tr.recordIdleKillCycle("template-b", false, 1, now.Add(2*time.Minute))
	if publish || count != 1 {
		t.Fatalf("template-b cycle 1: publish=%v count=%d, want false/1 (independent of template-a)", publish, count)
	}
}

// TestDemandMismatchTracker_NonPositiveThresholdDefaultsToThree verifies the
// constructor's documented default so callers do not need to duplicate the
// "default suggestion: 3" from the bead at every call site.
func TestDemandMismatchTracker_NonPositiveThresholdDefaultsToThree(t *testing.T) {
	t.Parallel()

	tr := newDemandMismatchTracker(0)
	now := time.Now()

	tr.recordIdleKillCycle("local-core/builder", false, 2, now)
	tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(time.Minute))
	publish, count, _ := tr.recordIdleKillCycle("local-core/builder", false, 2, now.Add(2*time.Minute))
	if !publish || count != 3 {
		t.Fatalf("cycle 3 with threshold=0: publish=%v count=%d, want true/3 (defaulted to 3)", publish, count)
	}
}
