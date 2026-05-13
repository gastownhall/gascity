package main

import (
	"testing"
	"time"
)

// Acceptance tests for the spawn-storm safety net.
//
// Scenarios:
//
//   - Real storm (drain-without-claim sustained) triggers detection.
//   - Healthy churn (workers claim and finish) does NOT trigger.
//   - Pre-claim exploration (worker takes time, then claims) does NOT trigger.
//   - Throttle gates new spawns; in-flight workers untouched.
//   - One-storm-one-mail (single transition signal).
//   - Decay: throttle expires when storm stops; state resets.

func TestSafetyNet_RealStormTriggersDetection(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 5,
		InitialBackoff: 10 * time.Minute,
		MaxBackoff:     60 * time.Minute,
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

	// Five drain-without-claim outcomes spaced over ~4 minutes — within the
	// sliding window. The fifth crosses the threshold and triggers detection.
	var newStorm bool
	for i := 0; i < 5; i++ {
		now := base.Add(time.Duration(i) * 50 * time.Second)
		got := sn.RecordDrainOutcome("foundations/worker", "worker-1", false, now)
		if i == 4 {
			newStorm = got
		} else if got {
			t.Fatalf("RecordDrainOutcome(i=%d): unexpected newStorm=true before threshold", i)
		}
	}
	if !newStorm {
		t.Fatal("RecordDrainOutcome at threshold: want newStorm=true, got false")
	}

	// Throttle is active right after detection.
	if !sn.IsThrottled("foundations/worker", base.Add(5*time.Minute)) {
		t.Fatal("IsThrottled just after detection: want true, got false")
	}
}

func TestSafetyNet_HealthyChurnDoesNotTrigger(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 5,
		InitialBackoff: 10 * time.Minute,
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

	// Twenty workers spawn, claim, finish, recycle — all claimed=true.
	for i := 0; i < 20; i++ {
		now := base.Add(time.Duration(i) * 10 * time.Second)
		if sn.RecordDrainOutcome("foundations/worker", "worker", true, now) {
			t.Fatalf("RecordDrainOutcome(claimed=true, i=%d): unexpected newStorm=true", i)
		}
	}
	if sn.IsThrottled("foundations/worker", base.Add(10*time.Minute)) {
		t.Fatal("IsThrottled after healthy churn: want false, got true")
	}
}

func TestSafetyNet_ExplorationThenClaimDoesNotTrigger(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 5,
		InitialBackoff: 10 * time.Minute,
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

	// Five workers each take a couple of minutes thinking before claim, but
	// eventually claim and close. From the safety net's vantage these are
	// claimed=true outcomes; pre-claim exploration is unobservable.
	for i := 0; i < 5; i++ {
		now := base.Add(time.Duration(i) * 2 * time.Minute)
		if sn.RecordDrainOutcome("foundations/worker", "worker", true, now) {
			t.Fatalf("RecordDrainOutcome(exploring worker i=%d): unexpected newStorm=true", i)
		}
	}
	if sn.IsThrottled("foundations/worker", base.Add(15*time.Minute)) {
		t.Fatal("IsThrottled after exploration-then-claim: want false, got true")
	}
}

func TestSafetyNet_OneMailPerStormTransition(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 3,
		InitialBackoff: 10 * time.Minute,
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

	transitions := 0
	// 10 drains within the window. Only the threshold-crossing one should
	// report newStorm=true; subsequent ones within the same storm episode
	// must not retrigger the mail.
	for i := 0; i < 10; i++ {
		now := base.Add(time.Duration(i) * 20 * time.Second)
		if sn.RecordDrainOutcome("foundations/worker", "w", false, now) {
			transitions++
		}
	}
	if transitions != 1 {
		t.Fatalf("transitions across single storm episode: want 1, got %d", transitions)
	}
}

func TestSafetyNet_DecayAfterStormPasses(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 3,
		InitialBackoff: 10 * time.Minute,
		MaxBackoff:     60 * time.Minute,
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

	// Trigger one storm.
	for i := 0; i < 3; i++ {
		sn.RecordDrainOutcome("foundations/worker", "w", false, base.Add(time.Duration(i)*30*time.Second))
	}

	// Wait past the initial backoff with no further drain signals: throttle
	// must release; subsequent storm starts from the base window (not from
	// the elevated backoff of the previous episode).
	postBackoff := base.Add(11 * time.Minute)
	if sn.IsThrottled("foundations/worker", postBackoff) {
		t.Fatal("IsThrottled after backoff window with no new drains: want false, got true")
	}

	// A new storm now: backoff starts from InitialBackoff, not 2*Initial.
	// The threshold-crossing drain lands at base2+60s; throttle should
	// expire at base2+60s+10m (and NOT at base2+60s+20m).
	base2 := postBackoff
	for i := 0; i < 3; i++ {
		sn.RecordDrainOutcome("foundations/worker", "w", false, base2.Add(time.Duration(i)*30*time.Second))
	}
	thresholdAt := base2.Add(60 * time.Second)
	// 1 minute inside the InitialBackoff window: throttle active.
	if !sn.IsThrottled("foundations/worker", thresholdAt.Add(9*time.Minute)) {
		t.Fatal("IsThrottled at threshold+9m: want true, got false (second storm)")
	}
	// 1 minute past the InitialBackoff window with no new drains: throttle
	// clears. If the backoff had doubled from the prior episode (20m) this
	// would still be inside it — proves the second storm starts fresh.
	if sn.IsThrottled("foundations/worker", thresholdAt.Add(11*time.Minute)) {
		t.Fatal("IsThrottled at threshold+11m: want false, got true (decay didn't reset)")
	}
}

func TestSafetyNet_ExponentialBackoffDuringSustainedStorm(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 3,
		InitialBackoff: 10 * time.Minute,
		MaxBackoff:     60 * time.Minute,
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

	// Cross threshold -> InitialBackoff (10 min) -> throttledUntil = t0+10m
	for i := 0; i < 3; i++ {
		sn.RecordDrainOutcome("foundations/worker", "w", false, base.Add(time.Duration(i)*30*time.Second))
	}
	thrAt := base.Add(2*30*time.Second + 10*time.Minute)
	if !sn.IsThrottled("foundations/worker", thrAt.Add(-time.Second)) {
		t.Fatal("IsThrottled just before initial backoff expiry: want true")
	}

	// Another drain-without-claim WHILE throttled: backoff doubles to 20 min.
	mid := base.Add(2 * time.Minute)
	sn.RecordDrainOutcome("foundations/worker", "w2", false, mid)

	// Throttle now extends to mid+20m, well past the original thrAt.
	probe := mid.Add(19 * time.Minute)
	if !sn.IsThrottled("foundations/worker", probe) {
		t.Fatal("IsThrottled inside extended (2x) backoff: want true, got false")
	}
	// And expires after 20 min from `mid`.
	probe2 := mid.Add(20*time.Minute + time.Second)
	// Need to also be past the sliding window so old drains age out, otherwise
	// IsThrottled-driven clear may not trigger. The sliding window is 5 min.
	if sn.IsThrottled("foundations/worker", probe2) {
		t.Fatal("IsThrottled past extended backoff: want false, got true")
	}
}

func TestSafetyNet_BackoffCappedAtMax(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 2,
		InitialBackoff: 30 * time.Minute,
		MaxBackoff:     60 * time.Minute,
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

	// Trigger storm: initial backoff = 30 min.
	sn.RecordDrainOutcome("foundations/worker", "a", false, base)
	sn.RecordDrainOutcome("foundations/worker", "b", false, base.Add(10*time.Second))

	// 10 further drain-without-claim outcomes — backoff should double several
	// times but cap at 60 min.
	for i := 0; i < 10; i++ {
		sn.RecordDrainOutcome("foundations/worker", "c", false, base.Add(time.Duration(i)*20*time.Second))
	}

	// At base + (10*20s) + 61 min, throttle MUST have expired (cap = 60m).
	lastDrain := base.Add(10 * 20 * time.Second)
	if sn.IsThrottled("foundations/worker", lastDrain.Add(61*time.Minute)) {
		t.Fatal("IsThrottled past MaxBackoff cap: want false, got true")
	}
}

func TestSafetyNet_PerTemplateIsolation(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 3,
		InitialBackoff: 10 * time.Minute,
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

	// Storm on template A.
	for i := 0; i < 3; i++ {
		sn.RecordDrainOutcome("templateA", "w", false, base.Add(time.Duration(i)*30*time.Second))
	}
	if !sn.IsThrottled("templateA", base.Add(5*time.Minute)) {
		t.Fatal("templateA throttled after storm: want true, got false")
	}

	// templateB unaffected: no drains there.
	if sn.IsThrottled("templateB", base.Add(5*time.Minute)) {
		t.Fatal("templateB throttled by templateA's storm: want false, got true (cross-template leak)")
	}
}

func TestApplyThrottleToScaleCheckCounts_NilSafetyNet(t *testing.T) {
	counts := map[string]int{"templateA": 3, "templateB": 1}
	throttled := applyThrottleToScaleCheckCounts(nil, counts, time.Now())
	if throttled != nil {
		t.Fatalf("throttled = %v, want nil with nil safety net", throttled)
	}
	if counts["templateA"] != 3 || counts["templateB"] != 1 {
		t.Fatalf("counts mutated by nil safety net: %v", counts)
	}
}

func TestApplyThrottleToScaleCheckCounts_GatesOnlyThrottled(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 3,
		InitialBackoff: 10 * time.Minute,
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	// Storm on templateA only.
	for i := 0; i < 3; i++ {
		sn.RecordDrainOutcome("templateA", "w", false, base.Add(time.Duration(i)*30*time.Second))
	}

	counts := map[string]int{"templateA": 5, "templateB": 2}
	throttled := applyThrottleToScaleCheckCounts(sn, counts, base.Add(2*time.Minute))
	if len(throttled) != 1 || !throttled["templateA"] {
		t.Fatalf("throttled = %v, want {templateA:true} only", throttled)
	}
	if counts["templateA"] != 0 {
		t.Fatalf("counts[templateA] = %d, want 0 (gated)", counts["templateA"])
	}
	if counts["templateB"] != 2 {
		t.Fatalf("counts[templateB] = %d, want 2 (unaffected)", counts["templateB"])
	}
}

func TestApplyThrottleToScaleCheckCounts_SkipsZeroDemand(t *testing.T) {
	// A template that already has zero demand should NOT appear in the
	// throttled-set even if it's currently in a storm — there's nothing
	// to suppress, and surfacing it in diagnostics would be noisy.
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 2,
		InitialBackoff: 10 * time.Minute,
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	sn.RecordDrainOutcome("templateA", "w", false, base)
	sn.RecordDrainOutcome("templateA", "w", false, base.Add(30*time.Second))

	counts := map[string]int{"templateA": 0}
	throttled := applyThrottleToScaleCheckCounts(sn, counts, base.Add(1*time.Minute))
	if len(throttled) != 0 {
		t.Fatalf("throttled = %v, want empty (zero demand has nothing to gate)", throttled)
	}
}

func TestRegisterSpawnStormSafetyNet_RestoresOnRelease(t *testing.T) {
	sn1 := NewSpawnStormSafetyNet(SpawnStormConfig{})
	restore1 := RegisterSpawnStormSafetyNetForCurrentController(sn1)
	if got := safetyNetForGating(); got != sn1 {
		t.Fatalf("after register: got %p, want %p", got, sn1)
	}

	sn2 := NewSpawnStormSafetyNet(SpawnStormConfig{})
	restore2 := RegisterSpawnStormSafetyNetForCurrentController(sn2)
	if got := safetyNetForGating(); got != sn2 {
		t.Fatalf("after second register: got %p, want %p", got, sn2)
	}

	restore2()
	if got := safetyNetForGating(); got != sn1 {
		t.Fatalf("after restore2: got %p, want %p (sn1)", got, sn1)
	}

	restore1()
	if got := safetyNetForGating(); got != nil {
		t.Fatalf("after restore1: got %p, want nil", got)
	}
}

func TestSafetyNet_DefaultInitialBackoffIsOneMinute(t *testing.T) {
	// Threshold-of-5-in-5min already implies sustained badness; a 10-minute
	// initial backoff is overkill and the cost-of-being-wrong is high.
	// 1 minute keeps the throttle short enough that operator-driven (or
	// natural) recovery isn't penalized for a quarter-hour.
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 2,
		// InitialBackoff omitted -> defaults applied.
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	sn.RecordDrainOutcome("templateA", "w", false, base)
	sn.RecordDrainOutcome("templateA", "w", false, base.Add(10*time.Second))

	// Just before 1 minute past threshold-cross: still throttled.
	if !sn.IsThrottled("templateA", base.Add(10*time.Second).Add(59*time.Second)) {
		t.Fatal("default initial backoff: throttle expired before 1 minute")
	}
	// Just after 1 minute past threshold-cross with no further drains:
	// throttle clears. If the default were still 10 minutes this would
	// remain throttled and the test would fail.
	if sn.IsThrottled("templateA", base.Add(10*time.Second).Add(61*time.Second)) {
		t.Fatal("default initial backoff: throttle still active past 1 minute (default not lowered)")
	}
}

func TestSafetyNet_SlidingWindowExpiresOldDrains(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 5,
		InitialBackoff: 10 * time.Minute,
	})
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

	// Four drains, spaced out wide enough that by the time the 5th arrives,
	// the earliest has aged out of the 5-min window. Should NOT trigger.
	sn.RecordDrainOutcome("foundations/worker", "w", false, base)
	sn.RecordDrainOutcome("foundations/worker", "w", false, base.Add(1*time.Minute))
	sn.RecordDrainOutcome("foundations/worker", "w", false, base.Add(2*time.Minute))
	sn.RecordDrainOutcome("foundations/worker", "w", false, base.Add(3*time.Minute))
	// At base+6m, the base drain has aged out. Window now contains 4 entries
	// counting the new one — below threshold.
	if got := sn.RecordDrainOutcome("foundations/worker", "w", false, base.Add(6*time.Minute)); got {
		t.Fatal("RecordDrainOutcome at base+6m (with window expiry): want newStorm=false, got true")
	}
	if sn.IsThrottled("foundations/worker", base.Add(7*time.Minute)) {
		t.Fatal("IsThrottled after below-threshold sustained low rate: want false, got true")
	}
}
