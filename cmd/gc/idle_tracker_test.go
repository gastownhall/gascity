package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// errIdleSnapshotProbe is a stand-in transient snapshot failure used by the
// content-idle-clock tests.
var errIdleSnapshotProbe = errors.New("idle snapshot probe failed")

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

	if !it.checkIdle("mayor", "", "", "", sp, now) {
		t.Fatalf("checkIdle(mayor, \"\", sp, now) = false, want true (last activity 10m old, timeout 5m)")
	}

	sp.SetActivity("mayor", now.Add(-1*time.Minute))
	if it.checkIdle("mayor", "", "", "", sp, now) {
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

	if !it.checkIdle(sessionName, template, "", "", sp, now) {
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

	if it.checkIdle(sessionName, "", "", "", sp, now) {
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
	if !it.checkIdle("mayor", "mayor", "", "", sp, now) {
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

	if it.checkIdle(sessionName, template, "", "", sp, now) {
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

// fakeIdleSnapshotProvider embeds the standard fake provider and adds a
// controllable IdleSnapshotProvider so the content-based idle clock can be
// exercised without a real TUI. It models the ga-07mi8 defect: the coarse
// pane-activity clock stays perpetually fresh (an idle Claude TUI keeps
// repainting its status line) while the pane content is genuinely idle.
type fakeIdleSnapshotProvider struct {
	*runtime.Fake
	idle map[string]bool
	err  map[string]error
}

func newFakeIdleSnapshotProvider() *fakeIdleSnapshotProvider {
	return &fakeIdleSnapshotProvider{
		Fake: runtime.NewFake(),
		idle: make(map[string]bool),
		err:  make(map[string]error),
	}
}

func (f *fakeIdleSnapshotProvider) SnapshotIdle(name string) (bool, error) {
	if e := f.err[name]; e != nil {
		return false, e
	}
	return f.idle[name], nil
}

// TestIdleTracker_ContentClockFiresDespiteFreshActivity is the ga-07mi8
// regression: a Claude non-ACP session whose pane-activity clock never goes
// stale (the TUI repaints every tick) must still idle-timeout once its pane
// content has shown a continuous idle prompt for longer than the timeout.
func TestIdleTracker_ContentClockFiresDespiteFreshActivity(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	it.setTimeout("deacon", 1*time.Hour)

	sp := newFakeIdleSnapshotProvider()
	startFakeSession(t, sp.Fake, "deacon")
	sp.idle["deacon"] = true // pane content: idle prompt, no busy indicator

	base := time.Now()
	// Activity clock is ALWAYS fresh — the exact signal that defeated the old
	// activity-clock logic and left the heartbeat awake for hours.
	sp.SetActivity("deacon", base)
	if it.checkIdle("deacon", "", "claude", "", sp, base) {
		t.Fatalf("checkIdle fired on the first idle observation; want anchor-only")
	}

	sp.SetActivity("deacon", base.Add(30*time.Minute))
	if it.checkIdle("deacon", "", "claude", "", sp, base.Add(30*time.Minute)) {
		t.Fatalf("checkIdle fired at 30m into a 1h timeout")
	}

	// Past the timeout of continuous content-idle: must fire despite the
	// perpetually fresh activity clock.
	sp.SetActivity("deacon", base.Add(61*time.Minute))
	if !it.checkIdle("deacon", "", "claude", "", sp, base.Add(61*time.Minute)) {
		t.Fatalf("checkIdle did NOT fire after 61m of continuous content-idle (ga-07mi8 regression)")
	}
}

// TestIdleTracker_ContentClockResetsOnBusy verifies a busy observation resets
// the idle-since anchor, so a session that resumes work is not idle-timed-out
// on stale idle accumulation.
func TestIdleTracker_ContentClockResetsOnBusy(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	it.setTimeout("deacon", 1*time.Hour)

	sp := newFakeIdleSnapshotProvider()
	base := time.Now()

	sp.idle["deacon"] = true
	if it.checkIdle("deacon", "", "claude", "", sp, base) {
		t.Fatalf("unexpected fire on first idle observation")
	}

	// Session goes busy well before the timeout — the anchor must reset.
	sp.idle["deacon"] = false
	if it.checkIdle("deacon", "", "claude", "", sp, base.Add(40*time.Minute)) {
		t.Fatalf("checkIdle fired while busy")
	}

	// Idle again: the clock restarts here, so 40m later (< 1h) it must not fire.
	sp.idle["deacon"] = true
	if it.checkIdle("deacon", "", "claude", "", sp, base.Add(50*time.Minute)) {
		t.Fatalf("checkIdle fired on the first idle observation after a busy reset")
	}
	if it.checkIdle("deacon", "", "claude", "", sp, base.Add(90*time.Minute)) {
		t.Fatalf("checkIdle fired at 40m into the post-reset idle run (timeout 1h)")
	}
	// 61m of continuous post-reset idle: fires.
	if !it.checkIdle("deacon", "", "claude", "", sp, base.Add(111*time.Minute)) {
		t.Fatalf("checkIdle did not fire after 61m of continuous post-reset idle")
	}
}

// TestIdleTracker_ContentClockErrorLeavesAnchor verifies a transient snapshot
// error neither fires nor resets a long idle accumulation.
func TestIdleTracker_ContentClockErrorLeavesAnchor(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	it.setTimeout("deacon", 1*time.Hour)

	sp := newFakeIdleSnapshotProvider()
	base := time.Now()

	sp.idle["deacon"] = true
	if it.checkIdle("deacon", "", "claude", "", sp, base) {
		t.Fatalf("unexpected fire on first idle observation")
	}

	// A transient read failure mid-run: must not fire and must not reset.
	sp.err["deacon"] = errIdleSnapshotProbe
	if it.checkIdle("deacon", "", "claude", "", sp, base.Add(30*time.Minute)) {
		t.Fatalf("checkIdle fired on a snapshot error")
	}

	// Recover: the original anchor is intact, so 61m from base still fires.
	delete(sp.err, "deacon")
	if !it.checkIdle("deacon", "", "claude", "", sp, base.Add(61*time.Minute)) {
		t.Fatalf("checkIdle did not fire after recovery; the error reset the anchor")
	}
}

// TestIdleTracker_NonClaudeUsesActivityClock verifies the content clock is
// gated: a non-Claude provider, and Claude over ACP, keep the coarse
// activity-clock behavior even when the runtime can snapshot idle.
func TestIdleTracker_NonClaudeUsesActivityClock(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	it.setTimeout("worker", 1*time.Hour)

	sp := newFakeIdleSnapshotProvider()
	startFakeSession(t, sp.Fake, "worker")
	sp.idle["worker"] = true // content idle, but the gate must ignore it here

	base := time.Now()
	check := base.Add(2 * time.Hour)
	// Fresh activity at check time: the activity clock says NOT idle. If the
	// content clock were (wrongly) used, the session would fire.
	sp.SetActivity("worker", check)
	if it.checkIdle("worker", "", "codex", "", sp, check) {
		t.Fatalf("non-claude session used the content clock; want activity-clock (fresh => not idle)")
	}
	if it.checkIdle("worker", "", "claude", "acp", sp, check) {
		t.Fatalf("claude/acp session used the content clock; want activity-clock")
	}

	// Stale activity: the activity path still fires for a non-claude session.
	sp.SetActivity("worker", base)
	if !it.checkIdle("worker", "", "codex", "", sp, base.Add(2*time.Hour)) {
		t.Fatalf("non-claude session with stale activity did not idle-timeout via the activity clock")
	}
}
