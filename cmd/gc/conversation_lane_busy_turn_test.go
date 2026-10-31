package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/worker"
)

// ga-8n8fja: the lane typed boundary prompts into Claude Code turns that had
// been running for minutes, up to five times each.
//
// A keystroke runtime's boundary is the quiescence rule: no pane output for
// defaultNudgePollQuiescence (3 s). Over tmux, the rule reads the activity
// clock from the controller's shared fleet snapshot, which is cached for its
// TTL (2 s), is stamped after its process-table scan, and carries
// #{window_activity} in whole seconds. A Claude pane that redraws its spinner
// every second can therefore read 3 s quiet. The lane also re-passes exactly
// at last activity + 3 s (Probe.BoundaryAt), where the same snapshot reads 3 s
// quiet by construction. Nothing after the probe waits for idle: gc's tmux
// provider runs with NudgeIdleTimeout 0, so a default nudge types at once.
//
// What the rule cannot see, the agent's screen shows: the runtime's one-look
// turn reading (runtime.TurnRunningProvider, which tmux answers from the busy
// indicator) reports the turn as running.

// newScreenTurnFixture is a lane fixture for a session of provider over a
// tmux profile whose screen readings the fixture's fake answers
// (runtime.Fake.ScreenTurnRunning, runtime.Fake.ScreenReadErrors).
func newScreenTurnFixture(t *testing.T, provider string) (*laneFixture, laneExecDeps, *opjournal.Store) {
	t.Helper()
	f := newLaneFixture(t, func(meta map[string]string) { meta["provider"] = provider })
	st := f.laneStore(t)
	deps := f.deps(st)
	deps.sp = runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: f.sp})
	return f, deps, st
}

// showTurn sets whether f's agent's screen shows a turn in progress, as a
// Claude Code pane mid-turn does ("✻ Working… (16m 1s · …)", "esc to
// interrupt").
func (f *laneFixture) showTurn(running bool) {
	if f.sp.ScreenTurnRunning == nil {
		f.sp.ScreenTurnRunning = map[string]bool{}
	}
	f.sp.ScreenTurnRunning[f.name] = running
}

// quiescenceOnFixtureClock makes e apply the production quiescence rule
// (pollerSessionIdleEnough's LastActivity branch) on f's clock.
func quiescenceOnFixtureClock(f *laneFixture, e *conversationLaneExec) {
	e.now = f.clk.Now
	e.idle = func(_ nudgeTarget, obs worker.LiveObservation) bool {
		return obs.LastActivity != nil && f.clk.Now().Sub(*obs.LastActivity) >= defaultNudgePollQuiescence
	}
}

// probeQuiet probes f's conversation once with the activity clock reading
// 3.4 s quiet: the cached, whole-second activity of a pane that redraws every
// second, read 3.4 s after its last counted output.
func probeQuiet(t *testing.T, f *laneFixture, deps laneExecDeps) opjournal.Probe {
	t.Helper()
	f.sp.SetActivity(f.name, f.clk.Now().Add(-3400*time.Millisecond))
	v, row := f.view(t)
	e := newConversationLaneExec(deps, row)
	quiescenceOnFixtureClock(f, e)
	p, err := e.Probe(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLaneExecRunningTurnOnScreenIsNoBoundary: a keystroke runtime whose
// activity clock reads quiet while its agent's screen shows a turn running is
// not at a boundary, whether the clock read quiet on the first look or the
// lane came back at the quiescence deadline the last look named.
func TestLaneExecRunningTurnOnScreenIsNoBoundary(t *testing.T) {
	t.Run("the activity clock reads quiet", func(t *testing.T) {
		f, deps, _ := newScreenTurnFixture(t, "claude")
		f.showTurn(true)
		p := probeQuiet(t, f, deps)
		if p.Boundary != opjournal.TriNo {
			t.Fatalf("boundary %v while the agent's screen shows a running turn, want no: a boundary prompt would be typed into it", p.Boundary)
		}
		if !p.BoundaryAt.IsZero() {
			t.Fatalf("BoundaryAt %v; want none, so the busy backoff paces the next look", p.BoundaryAt)
		}
	})
	t.Run("the quiescence deadline on the same snapshot", func(t *testing.T) {
		f, deps, _ := newScreenTurnFixture(t, "claude")
		f.showTurn(true)
		f.sp.SetActivity(f.name, f.clk.Now().Add(-time.Second))
		v, row := f.view(t)
		e := newConversationLaneExec(deps, row)
		quiescenceOnFixtureClock(f, e)
		p, err := e.Probe(context.Background(), v)
		if err != nil {
			t.Fatal(err)
		}
		if p.Boundary != opjournal.TriNo || p.BoundaryAt.IsZero() {
			t.Fatalf("first look: boundary %v at %v, want no with a quiescence deadline", p.Boundary, p.BoundaryAt)
		}
		// The worker passes again at BoundaryAt. The snapshot it reads there
		// is still the one this look read: the activity has not moved.
		f.clk.Advance(p.BoundaryAt.Sub(f.clk.Now()))
		p, err = e.Probe(context.Background(), v)
		if err != nil {
			t.Fatal(err)
		}
		if p.Boundary != opjournal.TriNo {
			t.Fatalf("at the deadline the look named: boundary %v while the agent's screen shows a running turn, want no", p.Boundary)
		}
	})
}

// TestLaneExecQuietBoundaryScreenReadings pins the rest of the screen check:
// a quiet clock over a screen with no turn is a boundary; a screen that
// cannot be read is no known boundary, and the host is told why; a family
// whose busy indicator the pane scan does not read keeps the quiescence rule,
// whatever its screen reading says.
func TestLaneExecQuietBoundaryScreenReadings(t *testing.T) {
	t.Run("no turn on screen", func(t *testing.T) {
		f, deps, _ := newScreenTurnFixture(t, "claude")
		if p := probeQuiet(t, f, deps); p.Boundary != opjournal.TriYes {
			t.Fatalf("boundary %v over a quiet clock and an idle screen, want yes", p.Boundary)
		}
	})
	t.Run("codex reads its screen too", func(t *testing.T) {
		f, deps, _ := newScreenTurnFixture(t, "codex")
		f.showTurn(true)
		if p := probeQuiet(t, f, deps); p.Boundary != opjournal.TriNo {
			t.Fatalf("boundary %v while a codex screen shows a running turn, want no", p.Boundary)
		}
	})
	t.Run("an unreadable screen", func(t *testing.T) {
		f, deps, _ := newScreenTurnFixture(t, "claude")
		f.sp.ScreenReadErrors = map[string]error{f.name: runtime.ErrRuntimeUnavailable}
		var noted []error
		deps.note = func(stage string, err error) {
			if stage == "turn_on_screen" {
				noted = append(noted, err)
			}
		}
		if p := probeQuiet(t, f, deps); p.Boundary != opjournal.TriUnknown {
			t.Fatalf("boundary %v over an unreadable screen, want unknown", p.Boundary)
		}
		if len(noted) != 1 || !errors.Is(noted[0], runtime.ErrRuntimeUnavailable) {
			t.Fatalf("noted %v; want the screen read's error once", noted)
		}
	})
	t.Run("a family without a readable busy indicator", func(t *testing.T) {
		if tmux.BusyIndicatorReadable("gemini") {
			t.Skip("gemini's busy indicator became readable; pick another family")
		}
		f, deps, _ := newScreenTurnFixture(t, "gemini")
		f.showTurn(true)
		if p := probeQuiet(t, f, deps); p.Boundary != opjournal.TriYes {
			t.Fatalf("boundary %v for a family whose busy reading the lane does not trust, want the quiescence rule's yes", p.Boundary)
		}
	})
}

// TestLaneBoundaryPromptIntoRunningTurnIsTypedOnce reproduces the soak's
// signature end to end at the lane: a gc session nudge (wait-idle, so a
// boundary prompt, at_least_once) to a Claude session in a long turn. A submit
// into the running turn would never see the idle->busy transition that
// confirms it (ErrNudgeSubmitUnconfirmed), so the lane would count it maybe
// delivered and retry. While the turn runs, the prompt must wait for the
// boundary: nothing is typed into the turn. Once the turn ends it is typed
// exactly once.
func TestLaneBoundaryPromptIntoRunningTurnIsTypedOnce(t *testing.T) {
	f, deps, st := newScreenTurnFixture(t, "claude")
	f.showTurn(true)
	f.sp.NudgeErrors = map[string]error{f.name: fmt.Errorf("%w: session %q", tmux.ErrNudgeSubmitUnconfirmed, f.name)}
	l := f.lane(t, st)
	const payload = "blocker gc-1 closed — routed dependent gc-2 may be ready to claim"
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, payload, func(r *opjournal.Request) {
		r.Policy = cliLaneNudgePolicy(nudgeDeliveryWaitIdle)
		r.Producer = opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator}
	}))
	pass := func() opjournal.Op {
		t.Helper()
		// Every look finds the activity clock 3.4 s quiet, as the cached
		// snapshot of a pane redrawing every second can.
		f.sp.SetActivity(f.name, f.clk.Now().Add(-3400*time.Millisecond))
		v, row := f.view(t)
		e := newConversationLaneExec(deps, row)
		quiescenceOnFixtureClock(f, e)
		if _, err := l.Pass(context.Background(), v, e); err != nil {
			t.Fatalf("Pass: %v", err)
		}
		f.clk.Advance(30 * time.Second)
		return f.get(t, st, op.ID)
	}
	for range 12 {
		if got := pass(); got.Status == opjournal.StatusClosed {
			t.Fatalf("op closed %+v while the turn runs; want it waiting for the boundary", got.Outcome)
		}
	}
	if typed := f.nudges(); len(typed) > 0 {
		got := f.get(t, st, op.ID)
		t.Fatalf("typed %d copies into the running turn; op %s attempt %d maybe_delivered %d failures %d; want none typed while the turn runs",
			len(typed), got.Status, got.Attempt, got.MaybeDelivered, got.Failures)
	}

	// The turn ends: the screen shows the idle composer and a submit is
	// confirmed again.
	f.showTurn(false)
	delete(f.sp.NudgeErrors, f.name)
	var got opjournal.Op
	for range 4 {
		if got = pass(); got.Status == opjournal.StatusClosed {
			break
		}
	}
	if got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("after the turn: op %s %+v; want closed succeeded", got.Status, got.Outcome)
	}
	typed := f.nudges()
	if len(typed) != 1 || !strings.Contains(typed[0].Message, payload) || strings.Contains(typed[0].Message, "may repeat an earlier one") {
		t.Fatalf("after the turn typed %+v; want the prompt exactly once, not marked as a repeat", typed)
	}
	if got.MaybeDelivered != 0 {
		t.Fatalf("maybe_delivered %d; want 0", got.MaybeDelivered)
	}
}
