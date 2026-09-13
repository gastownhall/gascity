package main

import "testing"

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
		exhausted = adt.recordDefer("worker-1", "", "wq-bead-a")
		if exhausted {
			t.Fatalf("recordDefer exhausted early on call %d of %d (limit %d)", i+1, defaultAssignedWorkDeferLimit, defaultAssignedWorkDeferLimit)
		}
	}
	if exhausted = adt.recordDefer("worker-1", "", "wq-bead-a"); !exhausted {
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
		if adt.recordDefer("worker-1", "", "wq-bead-a") {
			t.Fatalf("recordDefer exhausted early on anchor a, call %d", i+1)
		}
	}
	// Switch anchor bead: the streak must restart, not continue at count 3.
	if adt.recordDefer("worker-1", "", "wq-bead-b") {
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
		if adt.recordDefer("worker-1", "", "wq-bead-a") {
			t.Fatalf("recordDefer exhausted early before reset, call %d", i+1)
		}
	}
	adt.reset("worker-1")
	if adt.recordDefer("worker-1", "", "wq-bead-a") {
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

	if adt.recordDefer("worker-1", "local-core/builder", "wq-bead-a") {
		t.Fatalf("first defer exhausted with limit 1 (should take exactly 1 defer to exceed)")
	}
	if !adt.recordDefer("worker-1", "local-core/builder", "wq-bead-a") {
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
	if adt.recordDefer(sessionName, template, "wq-bead-a") {
		t.Fatalf("first defer exhausted with template limit 1 (should take exactly 1 defer to exceed)")
	}
	if !adt.recordDefer(sessionName, template, "wq-bead-a") {
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
		exhausted = adt.recordDefer("mayor", template, "wq-bead-a")
		if exhausted {
			t.Fatalf("recordDefer exhausted early on call %d (want default limit %d, not template limit 100)", i+1, defaultAssignedWorkDeferLimit)
		}
	}
	if exhausted = adt.recordDefer("mayor", template, "wq-bead-a"); !exhausted {
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
		exhausted = adt.recordDefer("worker-1", "", "wq-bead-a")
		if exhausted {
			t.Fatalf("recordDefer exhausted early on call %d after clearing override (want default limit %d)", i+1, defaultAssignedWorkDeferLimit)
		}
	}
	if exhausted = adt.recordDefer("worker-1", "", "wq-bead-a"); !exhausted {
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

	if adt.recordDefer("worker-1", "", "wq-bead-a") {
		t.Fatalf("worker-1 first defer exhausted with limit 1")
	}
	if adt.recordDefer("worker-2", "", "wq-bead-a") {
		t.Fatalf("worker-2 defer exhausted with limit 100 after only 1 call")
	}
	if !adt.recordDefer("worker-1", "", "wq-bead-a") {
		t.Fatalf("worker-1 second defer did not exceed its own limit 1 (state bled from worker-2?)")
	}
}
