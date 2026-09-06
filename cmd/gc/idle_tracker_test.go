package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// startFakeSession marks `name` as running on the fake provider so the
// observe path in workerSessionTargetLastActivityWithConfig will read the
// activity timestamp set via SetActivity. Without a started session,
// obs.Running stays false and the activity lookup is skipped.
func startFakeSession(t *testing.T, sp *runtime.Fake, name string) {
	t.Helper()
	if err := sp.Start(context.Background(), name, runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("sp.Start(%q): %v", name, err)
	}
}

// TestIdleTracker_PerNameTimeoutTriggersOnLastActivity sanity-checks the
// original per-session-name registration: when a session has a registered
// timeout, checkIdle returns true once its last activity exceeds the
// threshold.
func TestIdleTracker_PerNameTimeoutTriggersOnLastActivity(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	it.setTimeout("mayor", 5*time.Minute)

	sp := runtime.NewFake()
	startFakeSession(t, sp, "mayor")
	now := time.Now()
	sp.SetActivity("mayor", now.Add(-10*time.Minute))

	if !it.checkIdle("mayor", "", sp, now) {
		t.Fatalf("checkIdle(mayor, \"\", sp, now) = false, want true (last activity 10m old, timeout 5m)")
	}

	sp.SetActivity("mayor", now.Add(-1*time.Minute))
	if it.checkIdle("mayor", "", sp, now) {
		t.Fatalf("checkIdle returned true for not-yet-idle session")
	}
}

// TestIdleTracker_TemplateFallbackResolvesPoolSession exercises the bug fix.
// A pool session has a bead-derived runtime name that is unknown at registration
// time. The tracker registers a per-template timeout and the call site supplies
// the template so the timeout still applies.
func TestIdleTracker_TemplateFallbackResolvesPoolSession(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	template := "local-core/builder"
	it.setTimeoutForTemplate(template, 1*time.Hour)

	sp := runtime.NewFake()
	sessionName := sessionNameFromBeadID("fm-miv1io")
	startFakeSession(t, sp, sessionName)
	now := time.Now()
	sp.SetActivity(sessionName, now.Add(-90*time.Minute))

	if !it.checkIdle(sessionName, template, sp, now) {
		t.Fatalf("checkIdle did not fire for pool session via template fallback (90m idle vs 1h timeout)")
	}
}

// TestIdleTracker_TemplateFallbackDoesNotApplyWithoutTemplate verifies that
// passing an empty template skips the per-template fallback. The call site
// has the template available, but extra defensive: if a future call site
// forgets to supply it, the old behavior (no fallback) is preserved rather
// than silently picking up a timeout from an unrelated template.
func TestIdleTracker_TemplateFallbackDoesNotApplyWithoutTemplate(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	it.setTimeoutForTemplate("local-core/builder", 1*time.Hour)

	sp := runtime.NewFake()
	sessionName := sessionNameFromBeadID("fm-anonymous")
	startFakeSession(t, sp, sessionName)
	now := time.Now()
	sp.SetActivity(sessionName, now.Add(-90*time.Minute))

	if it.checkIdle(sessionName, "", sp, now) {
		t.Fatalf("checkIdle should not have fired without a template argument")
	}
}

// TestIdleTracker_PerNameTakesPrecedenceOverTemplate ensures that a direct
// per-session registration overrides the per-template fallback, so named
// sessions retain their explicit timeouts even when their template also has
// one registered (the hybrid named+pool case, should it arise).
func TestIdleTracker_PerNameTakesPrecedenceOverTemplate(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	it.setTimeout("mayor", 5*time.Minute)
	it.setTimeoutForTemplate("mayor", 24*time.Hour)

	sp := runtime.NewFake()
	startFakeSession(t, sp, "mayor")
	now := time.Now()
	sp.SetActivity("mayor", now.Add(-10*time.Minute))

	// 10m idle should trip the per-name 5m timeout regardless of the larger
	// template fallback.
	if !it.checkIdle("mayor", "mayor", sp, now) {
		t.Fatalf("checkIdle did not honor per-name 5m timeout (template fallback masked it?)")
	}
}

// TestIdleTracker_SetTimeoutForTemplateZeroClears verifies that calling
// setTimeoutForTemplate with a non-positive duration removes the entry —
// matching setTimeout's behavior for consistency.
func TestIdleTracker_SetTimeoutForTemplateZeroClears(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	template := "local-core/builder"
	it.setTimeoutForTemplate(template, 1*time.Hour)
	it.setTimeoutForTemplate(template, 0)

	sp := runtime.NewFake()
	sessionName := sessionNameFromBeadID("fm-x")
	startFakeSession(t, sp, sessionName)
	now := time.Now()
	sp.SetActivity(sessionName, now.Add(-2*time.Hour))

	if it.checkIdle(sessionName, template, sp, now) {
		t.Fatalf("checkIdle fired after template timeout was cleared")
	}
}

func TestIdleTracker_SetTimeoutForTemplateIgnoresEmptyTemplate(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	it.setTimeoutForTemplate("", 1*time.Hour)

	if len(it.templateTimeouts) != 0 {
		t.Fatalf("templateTimeouts = %v, want empty after empty-template config", it.templateTimeouts)
	}
}

// TestIdleTracker_NoAssignedWorkClockFiresAfterTimeout is the regression for
// pool sessions whose runtime activity clock never ages (a pi TUI repaints
// its pane once a second while idle): a session that holds no open assigned
// work continuously longer than its timeout is idle, regardless of pane
// activity.
func TestIdleTracker_NoAssignedWorkClockFiresAfterTimeout(t *testing.T) {
	it := newIdleTracker()
	it.setTimeoutForTemplate("hudson", 15*time.Minute)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	if it.checkNoAssignedWorkIdle("hudson-gc-1", "hudson", false, now) {
		t.Fatal("first no-work observation must anchor, not fire")
	}
	if it.checkNoAssignedWorkIdle("hudson-gc-1", "hudson", false, now.Add(14*time.Minute)) {
		t.Fatal("must not fire before the timeout elapses")
	}
	if !it.checkNoAssignedWorkIdle("hudson-gc-1", "hudson", false, now.Add(16*time.Minute)) {
		t.Fatal("must fire once no-work has been continuous for longer than the timeout")
	}
	// Firing clears the anchor so a replacement session re-measures from scratch.
	if it.checkNoAssignedWorkIdle("hudson-gc-1", "hudson", false, now.Add(17*time.Minute)) {
		t.Fatal("anchor must be cleared after firing")
	}
}

// TestIdleTracker_NoAssignedWorkClockResetsWhenWorkAppears verifies that a
// claim mid-run resets the no-work anchor: the clock measures CONTINUOUS
// absence of assigned work.
func TestIdleTracker_NoAssignedWorkClockResetsWhenWorkAppears(t *testing.T) {
	it := newIdleTracker()
	it.setTimeoutForTemplate("hudson", 15*time.Minute)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	it.checkNoAssignedWorkIdle("hudson-gc-1", "hudson", false, now)
	if it.checkNoAssignedWorkIdle("hudson-gc-1", "hudson", true, now.Add(10*time.Minute)) {
		t.Fatal("holding work must never fire")
	}
	if it.checkNoAssignedWorkIdle("hudson-gc-1", "hudson", false, now.Add(20*time.Minute)) {
		t.Fatal("work observation must have reset the anchor; 20m since first anchor is not 15m since reset")
	}
	if !it.checkNoAssignedWorkIdle("hudson-gc-1", "hudson", false, now.Add(36*time.Minute)) {
		t.Fatal("must fire 16m after the post-work re-anchor")
	}
}

// TestIdleTracker_NoAssignedWorkClockRequiresTimeout pins that the clock is
// inert for sessions with no registered timeout (per-name or template) and
// honors the always-named template-fallback exemption exactly as checkIdle does.
func TestIdleTracker_NoAssignedWorkClockRequiresTimeout(t *testing.T) {
	it := newIdleTracker()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, ts := range []time.Duration{0, time.Hour, 48 * time.Hour} {
		if it.checkNoAssignedWorkIdle("worker-1", "", false, now.Add(ts)) {
			t.Fatalf("no timeout configured: must never fire (at +%s)", ts)
		}
	}
	it.setTimeoutForTemplate("worker", time.Minute)
	it.exemptTemplateFallbackForSession("worker-named")
	for _, ts := range []time.Duration{0, time.Hour} {
		if it.checkNoAssignedWorkIdle("worker-named", "worker", false, now.Add(ts)) {
			t.Fatalf("exempt session must not inherit the template timeout (at +%s)", ts)
		}
	}
}
