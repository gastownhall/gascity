package main

import (
	"testing"
	"time"
)

// deferTestEpoch is the fixed reconciler-clock reading the count-only tests
// pass to recordDefer: every defer lands on the same instant, so the
// wall-clock ceiling never trips and only the tick count is under test.
var deferTestEpoch = time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

// TestAssignedWorkDeferTracker_UnconfiguredSessionUsesDefault is the core
// divergence from idleTracker/maxSessionAgeTracker: an unregistered session
// is NOT treated as "feature off". recordDefer must still exceed the limit
// once defaultAssignedWorkDeferLimit consecutive same-anchor defers have
// been recorded, so the backstop is live without requiring any config.
func TestAssignedWorkDeferTracker_UnconfiguredSessionUsesDefault(t *testing.T) {
	t.Parallel()

	adt := newAssignedWorkDeferTracker()

	var exhausted bool
	for i := 0; i < defaultAssignedWorkDeferLimit; i++ {
		exhausted = adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch)
		if exhausted {
			t.Fatalf("recordDefer exhausted early on call %d of %d (limit %d)", i+1, defaultAssignedWorkDeferLimit, defaultAssignedWorkDeferLimit)
		}
	}
	if exhausted = adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch); !exhausted {
		t.Fatalf("recordDefer did not exceed default limit %d after %d consecutive same-anchor defers", defaultAssignedWorkDeferLimit, defaultAssignedWorkDeferLimit+1)
	}
}

// TestAssignedWorkDeferTracker_ResetsOnAnchorChange verifies that a fresh
// anchor bead ID starts a new count rather than continuing the streak, even
// when the previous streak was one defer away from the limit.
func TestAssignedWorkDeferTracker_ResetsOnAnchorChange(t *testing.T) {
	t.Parallel()

	adt := newAssignedWorkDeferTracker()
	adt.setLimit("worker-1", 3)

	for i := 0; i < 2; i++ {
		if adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch) {
			t.Fatalf("recordDefer exhausted early on anchor a, call %d", i+1)
		}
	}
	// Switch anchor bead: the streak must restart, not continue at count 3.
	if adt.recordDefer("worker-1", "", "wq-bead-b", deferTestEpoch) {
		t.Fatalf("recordDefer exhausted immediately after anchor change; want fresh count of 1")
	}
}

// TestAssignedWorkDeferTracker_ResetClearsState verifies that an explicit
// reset (called when the session was not idle-kill-eligible on a tick)
// clears the streak even when the next defer reuses the same anchor bead.
func TestAssignedWorkDeferTracker_ResetClearsState(t *testing.T) {
	t.Parallel()

	adt := newAssignedWorkDeferTracker()
	adt.setLimit("worker-1", 3)

	for i := 0; i < 2; i++ {
		if adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch) {
			t.Fatalf("recordDefer exhausted early before reset, call %d", i+1)
		}
	}
	adt.reset("worker-1")
	if adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch) {
		t.Fatalf("recordDefer exhausted immediately after reset; want fresh count of 1 even on the same anchor")
	}
}

// TestAssignedWorkDeferTracker_PerNameTakesPrecedenceOverTemplate mirrors
// idleTracker's precedence rule: a direct per-session limit wins over a
// registered template limit.
func TestAssignedWorkDeferTracker_PerNameTakesPrecedenceOverTemplate(t *testing.T) {
	t.Parallel()

	adt := newAssignedWorkDeferTracker()
	adt.setLimit("worker-1", 1)
	adt.setLimitForTemplate("local-core/builder", 100)

	if adt.recordDefer("worker-1", "local-core/builder", "wq-bead-a", deferTestEpoch) {
		t.Fatalf("first defer exhausted with limit 1 (should take exactly 1 defer to exceed)")
	}
	if !adt.recordDefer("worker-1", "local-core/builder", "wq-bead-a", deferTestEpoch) {
		t.Fatalf("recordDefer did not honor per-name limit 1 (template fallback of 100 masked it?)")
	}
}

// TestAssignedWorkDeferTracker_TemplateFallbackResolvesPoolSession exercises
// the bead-derived pool session case: no direct registration, but the
// session's template has a configured limit.
func TestAssignedWorkDeferTracker_TemplateFallbackResolvesPoolSession(t *testing.T) {
	t.Parallel()

	adt := newAssignedWorkDeferTracker()
	template := "local-core/builder"
	adt.setLimitForTemplate(template, 1)

	sessionName := sessionNameFromBeadID("fm-miv1io")
	if adt.recordDefer(sessionName, template, "wq-bead-a", deferTestEpoch) {
		t.Fatalf("first defer exhausted with template limit 1 (should take exactly 1 defer to exceed)")
	}
	if !adt.recordDefer(sessionName, template, "wq-bead-a", deferTestEpoch) {
		t.Fatalf("recordDefer did not honor template limit 1 via fallback")
	}
}

// TestAssignedWorkDeferTracker_ExemptionFallsBackToDefaultNotTemplate
// verifies the exemption's actual contract: a template-exempt named session
// with no direct limit does NOT inherit the (possibly inappropriate) pool
// template limit, but still falls back to defaultAssignedWorkDeferLimit
// rather than being treated as unregistered/off — the backstop stays live.
func TestAssignedWorkDeferTracker_ExemptionFallsBackToDefaultNotTemplate(t *testing.T) {
	t.Parallel()

	adt := newAssignedWorkDeferTracker()
	template := "local-core/builder"
	adt.setLimitForTemplate(template, 100)
	adt.exemptTemplateFallbackForSession("mayor")

	var exhausted bool
	for i := 0; i < defaultAssignedWorkDeferLimit; i++ {
		exhausted = adt.recordDefer("mayor", template, "wq-bead-a", deferTestEpoch)
		if exhausted {
			t.Fatalf("recordDefer exhausted early on call %d (want default limit %d, not template limit 100)", i+1, defaultAssignedWorkDeferLimit)
		}
	}
	if exhausted = adt.recordDefer("mayor", template, "wq-bead-a", deferTestEpoch); !exhausted {
		t.Fatalf("exempt session did not fall back to defaultAssignedWorkDeferLimit %d", defaultAssignedWorkDeferLimit)
	}
}

// TestAssignedWorkDeferTracker_SetLimitZeroClearsOverride verifies that
// configuring a non-positive limit removes the direct override, falling
// back to the template (or default) resolution — matching idleTracker's
// setTimeout(0) "clear" convention.
func TestAssignedWorkDeferTracker_SetLimitZeroClearsOverride(t *testing.T) {
	t.Parallel()

	adt := newAssignedWorkDeferTracker()
	adt.setLimit("worker-1", 1)
	adt.setLimit("worker-1", 0)

	var exhausted bool
	for i := 0; i < defaultAssignedWorkDeferLimit; i++ {
		exhausted = adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch)
		if exhausted {
			t.Fatalf("recordDefer exhausted early on call %d after clearing override (want default limit %d)", i+1, defaultAssignedWorkDeferLimit)
		}
	}
	if exhausted = adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch); !exhausted {
		t.Fatalf("recordDefer did not fall back to default limit after setLimit(0) cleared the override")
	}
}

// TestAssignedWorkDeferTracker_SetLimitForTemplateIgnoresEmptyTemplate
// mirrors idleTracker's defensive empty-template guard.
func TestAssignedWorkDeferTracker_SetLimitForTemplateIgnoresEmptyTemplate(t *testing.T) {
	t.Parallel()

	adt := newAssignedWorkDeferTracker()
	adt.setLimitForTemplate("", 1)

	if len(adt.templateLimits) != 0 {
		t.Fatalf("templateLimits = %v, want empty after empty-template config", adt.templateLimits)
	}
}

// TestAssignedWorkDeferTracker_IndependentSessionsDoNotShareState verifies
// two different session names accrue independent streaks even on the same
// anchor bead (e.g. two convoy members both deferring on a shared parent).
func TestAssignedWorkDeferTracker_IndependentSessionsDoNotShareState(t *testing.T) {
	t.Parallel()

	adt := newAssignedWorkDeferTracker()
	adt.setLimit("worker-1", 1)
	adt.setLimit("worker-2", 100)

	if adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch) {
		t.Fatalf("worker-1 first defer exhausted with limit 1")
	}
	if adt.recordDefer("worker-2", "", "wq-bead-a", deferTestEpoch) {
		t.Fatalf("worker-2 defer exhausted with limit 100 after only 1 call")
	}
	if !adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch) {
		t.Fatalf("worker-1 second defer did not exceed its own limit 1 (state bled from worker-2?)")
	}
}

// deferStreak records n same-anchor defers for one session, gap apart on the
// reconciler clock starting at deferTestEpoch, and returns each call's
// exhausted verdict in order.
func deferStreak(adt *memoryAssignedWorkDeferTracker, session, template string, n int, gap time.Duration) []bool {
	got := make([]bool, 0, n)
	for i := 0; i < n; i++ {
		got = append(got, adt.recordDefer(session, template, "wq-bead-a", deferTestEpoch.Add(time.Duration(i)*gap)))
	}
	return got
}

func assertDeferVerdicts(t *testing.T, got, want []bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("verdicts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("defer %d exhausted = %v, want %v (all verdicts %v, want %v)", i+1, got[i], want[i], got, want)
		}
	}
}

// TestAssignedWorkDeferTracker_DefaultLimitWindowStopsSlowTicks: slow
// reconcile ticks (130 s apart) must not stretch the default budget. The
// 2nd defer is 130 s after the first, past the 120 s ceiling, so the streak
// is exhausted there, never on the 1st defer.
func TestAssignedWorkDeferTracker_DefaultLimitWindowStopsSlowTicks(t *testing.T) {
	t.Parallel()
	got := deferStreak(newAssignedWorkDeferTracker(), "worker-1", "", 2, 130*time.Second)
	assertDeferVerdicts(t, got, []bool{false, true})
}

// TestAssignedWorkDeferTracker_DefaultLimitNominalTicksUnchanged: at the
// nominal 30 s patrol tick the 4th defer is only 90 s in, so the count alone
// decides, exactly as before the ceiling existed.
func TestAssignedWorkDeferTracker_DefaultLimitNominalTicksUnchanged(t *testing.T) {
	t.Parallel()
	got := deferStreak(newAssignedWorkDeferTracker(), "worker-1", "", 4, 30*time.Second)
	assertDeferVerdicts(t, got, []bool{false, false, false, true})
}

// TestAssignedWorkDeferTracker_DefaultLimitWindowBoundaryAt60sGaps: with
// 60 s gaps the 3rd defer is exactly 120 s after the first, and the ceiling
// is >=, so the stop lands on the 3rd defer.
func TestAssignedWorkDeferTracker_DefaultLimitWindowBoundaryAt60sGaps(t *testing.T) {
	t.Parallel()
	got := deferStreak(newAssignedWorkDeferTracker(), "worker-1", "", 3, 60*time.Second)
	assertDeferVerdicts(t, got, []bool{false, false, true})
}

// TestAssignedWorkDeferTracker_DefaultLimitJustUnderWindowAt59sGaps: with
// 59 s gaps the 3rd defer is 118 s in, under the ceiling, so it is NOT
// exhausted; the 4th (count 4 > 3) is.
func TestAssignedWorkDeferTracker_DefaultLimitJustUnderWindowAt59sGaps(t *testing.T) {
	t.Parallel()
	got := deferStreak(newAssignedWorkDeferTracker(), "worker-1", "", 4, 59*time.Second)
	assertDeferVerdicts(t, got, []bool{false, false, false, true})
}

// TestAssignedWorkDeferTracker_DefaultLimitWindowExactBoundary pins the
// comparison itself: exactly 120 s since the first defer is exhausted
// (>=), 119.999 s is not.
func TestAssignedWorkDeferTracker_DefaultLimitWindowExactBoundary(t *testing.T) {
	t.Parallel()

	atWindow := newAssignedWorkDeferTracker()
	if atWindow.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch) {
		t.Fatal("first defer exhausted; one extra tick of defer is kept on purpose")
	}
	if !atWindow.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch.Add(defaultAssignedWorkDeferWindow)) {
		t.Fatalf("defer exactly %s after the first was not exhausted", defaultAssignedWorkDeferWindow)
	}

	underWindow := newAssignedWorkDeferTracker()
	underWindow.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch)
	if underWindow.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch.Add(defaultAssignedWorkDeferWindow-time.Millisecond)) {
		t.Fatalf("defer 1ms under %s exhausted", defaultAssignedWorkDeferWindow)
	}
}

// TestAssignedWorkDeferTracker_ExplicitSessionLimitHasNoWindow: an explicit
// per-session limit keeps pure tick-count semantics even when ticks are
// 130 s apart.
func TestAssignedWorkDeferTracker_ExplicitSessionLimitHasNoWindow(t *testing.T) {
	t.Parallel()
	adt := newAssignedWorkDeferTracker()
	adt.setLimit("worker-1", 3)
	got := deferStreak(adt, "worker-1", "", 4, 130*time.Second)
	assertDeferVerdicts(t, got, []bool{false, false, false, true})
}

// TestAssignedWorkDeferTracker_ExplicitTemplateLimitHasNoWindow: same for a
// per-template limit resolved through setLimitForTemplate.
func TestAssignedWorkDeferTracker_ExplicitTemplateLimitHasNoWindow(t *testing.T) {
	t.Parallel()
	adt := newAssignedWorkDeferTracker()
	adt.setLimitForTemplate("local-core/builder", 3)
	got := deferStreak(adt, "worker-1", "local-core/builder", 4, 130*time.Second)
	assertDeferVerdicts(t, got, []bool{false, false, false, true})
}

// TestAssignedWorkDeferTracker_ExemptFallbackGetsWindow: a session exempt
// from the template limit falls to the built-in default limit, and so gets
// the wall-clock ceiling even though its template registered a large limit.
func TestAssignedWorkDeferTracker_ExemptFallbackGetsWindow(t *testing.T) {
	t.Parallel()
	adt := newAssignedWorkDeferTracker()
	adt.setLimitForTemplate("local-core/builder", 100)
	adt.exemptTemplateFallbackForSession("mayor")
	got := deferStreak(adt, "mayor", "local-core/builder", 2, 130*time.Second)
	assertDeferVerdicts(t, got, []bool{false, true})
}

// TestAssignedWorkDeferTracker_AnchorChangeRestartsWindow: a new anchor bead
// discards the first-defer time along with the count. Without that, the
// third defer (200 s after the first anchor's first defer) would trip the
// ceiling.
func TestAssignedWorkDeferTracker_AnchorChangeRestartsWindow(t *testing.T) {
	t.Parallel()
	adt := newAssignedWorkDeferTracker()
	steps := []struct {
		anchor string
		at     time.Duration
		want   bool
	}{
		{"wq-bead-a", 0, false},
		{"wq-bead-a", 100 * time.Second, false},
		{"wq-bead-b", 200 * time.Second, false}, // fresh streak starts here
		{"wq-bead-b", 300 * time.Second, false}, // 100 s into the new streak
		{"wq-bead-b", 320 * time.Second, true},  // 120 s into the new streak
	}
	for i, s := range steps {
		if got := adt.recordDefer("worker-1", "", s.anchor, deferTestEpoch.Add(s.at)); got != s.want {
			t.Fatalf("step %d (%s at +%s) exhausted = %v, want %v", i+1, s.anchor, s.at, got, s.want)
		}
	}
}

// TestAssignedWorkDeferTracker_ResetRestartsWindow: reset() discards the
// first-defer time with the count, so a later streak on the same anchor
// starts its own 120 s window.
func TestAssignedWorkDeferTracker_ResetRestartsWindow(t *testing.T) {
	t.Parallel()
	adt := newAssignedWorkDeferTracker()
	adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch)
	adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch.Add(100*time.Second))
	adt.reset("worker-1")

	steps := []struct {
		at   time.Duration
		want bool
	}{
		{200 * time.Second, false}, // fresh streak starts here
		{300 * time.Second, false}, // 100 s in
		{320 * time.Second, true},  // 120 s in
	}
	for i, s := range steps {
		if got := adt.recordDefer("worker-1", "", "wq-bead-a", deferTestEpoch.Add(s.at)); got != s.want {
			t.Fatalf("post-reset step %d (+%s) exhausted = %v, want %v", i+1, s.at, got, s.want)
		}
	}
}

// TestAssignedWorkDeferTracker_EmptyAnchorFirstSightSetsWindowStart: a first
// defer with an empty anchor skips the anchor-change re-init (the zero state
// already has an empty anchor) but must still stamp the streak's start;
// otherwise the zero time makes every later defer look decades old.
func TestAssignedWorkDeferTracker_EmptyAnchorFirstSightSetsWindowStart(t *testing.T) {
	t.Parallel()
	adt := newAssignedWorkDeferTracker()
	steps := []struct {
		at   time.Duration
		want bool
	}{
		{0, false},
		{60 * time.Second, false},
		{120 * time.Second, true},
	}
	for i, s := range steps {
		if got := adt.recordDefer("worker-1", "", "", deferTestEpoch.Add(s.at)); got != s.want {
			t.Fatalf("empty-anchor step %d (+%s) exhausted = %v, want %v", i+1, s.at, got, s.want)
		}
	}
}
