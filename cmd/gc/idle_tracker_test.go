package main

import (
	"context"
	"errors"
	"fmt"
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

// TestIdleTracker_ContentClockIndeterminateResetsAnchor verifies a scan that
// sees neither a ready prompt nor a busy indicator (runtime.ErrIdleIndeterminate)
// resets the idle-since anchor the way a busy observation does, unlike a
// transient snapshot error: the anchor must not span a stretch the scan could
// not read as idle and then fire early once the prompt returns.
func TestIdleTracker_ContentClockIndeterminateResetsAnchor(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	it.setTimeout("deacon", 1*time.Hour)

	sp := newFakeIdleSnapshotProvider()
	base := time.Now()

	sp.idle["deacon"] = true
	if it.checkIdle("deacon", "", "claude", "", sp, base) {
		t.Fatalf("unexpected fire on first idle observation")
	}

	// The prompt leaves the screen mid-run: must not fire, and must reset.
	sp.err["deacon"] = fmt.Errorf("scanning pane: %w", runtime.ErrIdleIndeterminate)
	if it.checkIdle("deacon", "", "claude", "", sp, base.Add(30*time.Minute)) {
		t.Fatalf("checkIdle fired on an indeterminate snapshot")
	}

	// The prompt returns: the clock restarts here, so 61m from base must not
	// fire.
	delete(sp.err, "deacon")
	if it.checkIdle("deacon", "", "claude", "", sp, base.Add(61*time.Minute)) {
		t.Fatalf("checkIdle fired 61m from base; the indeterminate snapshot left the pre-gap anchor in place")
	}
	// 61m of continuous idle after the restart: fires.
	if !it.checkIdle("deacon", "", "claude", "", sp, base.Add(122*time.Minute)) {
		t.Fatalf("checkIdle did not fire after 61m of continuous idle following the reset")
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

// TestIdleTracker_ClearIdleAnchorRestartsMeasurement verifies that clearing a
// session's anchor — what the reconciler does when it observes the session dead
// — makes a same-name replacement re-measure its idleness from scratch. Without
// this, a restarted named session inherits its predecessor's accumulation and
// can be idle-killed on its first post-restart idle observation.
func TestIdleTracker_ClearIdleAnchorRestartsMeasurement(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	it.setTimeout("deacon", 1*time.Hour)

	sp := newFakeIdleSnapshotProvider()
	startFakeSession(t, sp.Fake, "deacon")
	sp.idle["deacon"] = true

	base := time.Now()
	// Activity stays fresh at every observation so only the content clock can
	// fire — the activity clock must never be the reason for a verdict here.
	check := func(at time.Time) bool {
		sp.SetActivity("deacon", at)
		return it.checkIdle("deacon", "", "claude", "", sp, at)
	}

	if check(base) {
		t.Fatalf("checkIdle fired on the first idle observation; want anchor-only")
	}

	// The session dies and a replacement takes the same runtime name.
	it.clearIdleAnchor("deacon")

	if check(base.Add(61 * time.Minute)) {
		t.Fatalf("checkIdle fired 61m after the CLEARED anchor; the replacement inherited its predecessor's accumulation")
	}
	// Re-anchored at 61m, so the timeout is reached only 61m later still.
	if check(base.Add(90 * time.Minute)) {
		t.Fatalf("checkIdle fired 29m into the re-measured idle run (timeout 1h)")
	}
	if !check(base.Add(123 * time.Minute)) {
		t.Fatalf("checkIdle did not fire after 62m of continuous re-measured idle")
	}
	if _, ok := it.idleSince["deacon"]; ok {
		t.Fatalf("idleSince retained an anchor for %q after firing", "deacon")
	}
}

// TestIdleTracker_ContentClockFallsBackToActivityClock verifies the content
// clock is ADDITIVE. A Claude non-ACP pane the scan can never read as idle (a
// session crashed to a shell, or an overridden prompt that never matches the
// configured prefix) must keep the stale-activity recycle route it had before
// the content clock existed, rather than reporting "not idle" forever.
func TestIdleTracker_ContentClockFallsBackToActivityClock(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	it.setTimeout("deacon", 1*time.Hour)

	sp := newFakeIdleSnapshotProvider()
	startFakeSession(t, sp.Fake, "deacon")
	sp.idle["deacon"] = false // pane never matches the ready-prompt prefix

	base := time.Now()
	sp.SetActivity("deacon", base)

	if it.checkIdle("deacon", "", "claude", "", sp, base.Add(30*time.Minute)) {
		t.Fatalf("checkIdle fired at 30m with a 1h timeout")
	}
	if !it.checkIdle("deacon", "", "claude", "", sp, base.Add(61*time.Minute)) {
		t.Fatalf("checkIdle did not fall back to the activity clock for an unreadable pane; the content clock must supplement it, not replace it")
	}

	// A snapshot error takes the same fallback route.
	sp.err["deacon"] = errIdleSnapshotProbe
	if !it.checkIdle("deacon", "", "claude", "", sp, base.Add(61*time.Minute)) {
		t.Fatalf("checkIdle did not fall back to the activity clock on a snapshot error")
	}
}

// The progress-stall recycle needs its own continuous-idle anchor: the Claude
// Code updater flash keeps the raw pane-activity clock fresh for a session that
// is idle at its prompt (ga-g9n7ov), and the idle_timeout anchor exists only
// for sessions with an idle timeout and is cleared when that timeout fires.

func TestIdleTracker_ProgressIdleSinceTracksContinuousIdleRun(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	snap := newFakeIdleSnapshotProvider()
	t0 := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	const name = "worker"

	snap.idle[name] = true
	if since, ok := it.progressIdleSince(name, snap, t0); !ok || !since.Equal(t0) {
		t.Fatalf("first idle observation = (%v, %v), want (%v, true)", since, ok, t0)
	}
	if since, ok := it.progressIdleSince(name, snap, t0.Add(10*time.Minute)); !ok || !since.Equal(t0) {
		t.Fatalf("second idle observation = (%v, %v), want the run to still start at %v", since, ok, t0)
	}

	snap.idle[name] = false
	if _, ok := it.progressIdleSince(name, snap, t0.Add(20*time.Minute)); ok {
		t.Fatal("a busy observation reported a continuous idle run")
	}

	snap.idle[name] = true
	t3 := t0.Add(30 * time.Minute)
	if since, ok := it.progressIdleSince(name, snap, t3); !ok || !since.Equal(t3) {
		t.Fatalf("idle after busy = (%v, %v), want a fresh run starting at %v", since, ok, t3)
	}
}

func TestIdleTracker_ProgressIdleSinceIndeterminateResetsAndErrorKeepsAnchor(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	snap := newFakeIdleSnapshotProvider()
	t0 := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	const name = "worker"

	snap.idle[name] = true
	it.progressIdleSince(name, snap, t0)

	snap.err[name] = errIdleSnapshotProbe
	if _, ok := it.progressIdleSince(name, snap, t0.Add(10*time.Minute)); ok {
		t.Fatal("an erroring snapshot reported a continuous idle run")
	}
	delete(snap.err, name)
	if since, ok := it.progressIdleSince(name, snap, t0.Add(20*time.Minute)); !ok || !since.Equal(t0) {
		t.Fatalf("after a transient error = (%v, %v), want the original run from %v", since, ok, t0)
	}

	snap.err[name] = runtime.ErrIdleIndeterminate
	if _, ok := it.progressIdleSince(name, snap, t0.Add(30*time.Minute)); ok {
		t.Fatal("an indeterminate snapshot reported a continuous idle run")
	}
	delete(snap.err, name)
	t4 := t0.Add(40 * time.Minute)
	if since, ok := it.progressIdleSince(name, snap, t4); !ok || !since.Equal(t4) {
		t.Fatalf("after an indeterminate read = (%v, %v), want a fresh run from %v", since, ok, t4)
	}
}

func TestIdleTracker_ProgressIdleSinceIsIndependentOfIdleTimeoutAnchor(t *testing.T) {
	t.Parallel()

	it := newIdleTracker()
	snap := newFakeIdleSnapshotProvider()
	t0 := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	const name = "worker"
	snap.idle[name] = true

	// No idle timeout registered: the anchor is kept anyway.
	it.progressIdleSince(name, snap, t0)

	// An idle_timeout firing clears its own anchor, not the progress one.
	it.setTimeout(name, 10*time.Minute)
	it.checkIdleByContent(name, snap, 10*time.Minute, t0)
	if !it.checkIdleByContent(name, snap, 10*time.Minute, t0.Add(11*time.Minute)) {
		t.Fatal("idle timeout did not fire; test setup is wrong")
	}
	if since, ok := it.progressIdleSince(name, snap, t0.Add(12*time.Minute)); !ok || !since.Equal(t0) {
		t.Fatalf("after the idle timeout fired = (%v, %v), want the run still from %v", since, ok, t0)
	}

	it.clearIdleAnchor(name)
	t3 := t0.Add(20 * time.Minute)
	if since, ok := it.progressIdleSince(name, snap, t3); !ok || !since.Equal(t3) {
		t.Fatalf("after clearIdleAnchor = (%v, %v), want a fresh run from %v", since, ok, t3)
	}
}

func TestContentAwareLastActivity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	raw := now.Add(-time.Minute) // the updater flash just touched the pane
	const name = "worker"

	// primed returns a tracker whose idle run for name began 40m before now.
	primed := func(snap *fakeIdleSnapshotProvider) *memoryIdleTracker {
		it := newIdleTracker()
		snap.idle[name] = true
		it.progressIdleSince(name, snap, now.Add(-40*time.Minute))
		return it
	}

	t.Run("claude non-acp idle uses the content anchor over a fresh raw clock", func(t *testing.T) {
		snap := newFakeIdleSnapshotProvider()
		got := contentAwareLastActivity(primed(snap), name, "claude", "", snap, raw, now)
		if want := now.Add(-40 * time.Minute); !got.Equal(want) {
			t.Fatalf("got %v, want the idle anchor %v", got, want)
		}
	})
	t.Run("a raw clock already older than the anchor wins", func(t *testing.T) {
		snap := newFakeIdleSnapshotProvider()
		old := now.Add(-3 * time.Hour)
		if got := contentAwareLastActivity(primed(snap), name, "claude", "", snap, old, now); !got.Equal(old) {
			t.Fatalf("got %v, want the older raw activity %v", got, old)
		}
	})
	t.Run("busy keeps the raw clock", func(t *testing.T) {
		snap := newFakeIdleSnapshotProvider()
		it := primed(snap)
		snap.idle[name] = false
		if got := contentAwareLastActivity(it, name, "claude", "", snap, raw, now); !got.Equal(raw) {
			t.Fatalf("got %v, want raw %v", got, raw)
		}
	})
	t.Run("snapshot error keeps the raw clock", func(t *testing.T) {
		snap := newFakeIdleSnapshotProvider()
		it := primed(snap)
		snap.err[name] = errIdleSnapshotProbe
		if got := contentAwareLastActivity(it, name, "claude", "", snap, raw, now); !got.Equal(raw) {
			t.Fatalf("got %v, want raw %v", got, raw)
		}
	})
	t.Run("acp keeps the raw clock", func(t *testing.T) {
		snap := newFakeIdleSnapshotProvider()
		if got := contentAwareLastActivity(primed(snap), name, "claude", "acp", snap, raw, now); !got.Equal(raw) {
			t.Fatalf("got %v, want raw %v", got, raw)
		}
	})
	t.Run("non-claude keeps the raw clock", func(t *testing.T) {
		snap := newFakeIdleSnapshotProvider()
		if got := contentAwareLastActivity(primed(snap), name, "codex", "", snap, raw, now); !got.Equal(raw) {
			t.Fatalf("got %v, want raw %v", got, raw)
		}
	})
	t.Run("a runtime without snapshots keeps the raw clock", func(t *testing.T) {
		snap := newFakeIdleSnapshotProvider()
		if got := contentAwareLastActivity(primed(snap), name, "claude", "", runtime.NewFake(), raw, now); !got.Equal(raw) {
			t.Fatalf("got %v, want raw %v", got, raw)
		}
	})
	t.Run("a nil tracker keeps the raw clock", func(t *testing.T) {
		snap := newFakeIdleSnapshotProvider()
		if got := contentAwareLastActivity(nil, name, "claude", "", snap, raw, now); !got.Equal(raw) {
			t.Fatalf("got %v, want raw %v", got, raw)
		}
	})
}

// progressIdleSince is part of the idleTracker interface; the scripted fake
// the reconciler tests use reports no content-idle run.
func (f *fakeIdleTracker) progressIdleSince(string, runtime.IdleSnapshotProvider, time.Time) (time.Time, bool) {
	return time.Time{}, false
}
