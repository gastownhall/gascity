package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
)

// 21q: the lane's parity with the legacy poller for a session whose
// activity clock never quiets (#8544) and for the Codex mid-turn hold
// (#8840). The quiescence rule alone holds a keystroke runtime's boundary
// closed while its pane keeps drawing (an idle TUI that repaints, back-to-back
// turns), so lane content to such a session would wait until its start_by
// closed it undelivered. Content that has been deliverable for
// laneBoundaryStarvedAfter asks the screen's turn look instead, as the poller's
// starved queue does, and the program record's hold vetoes that bypass for a
// provider that declares hold_mid_turn under program_status = "on".

// starvedLane is a lane over one conversation whose pane never quiets: every
// pass reads its last activity as now.
type starvedLane struct {
	f    *laneFixture
	deps laneExecDeps
	st   *opjournal.Store
	l    *opjournal.ConversationLane
	// prep adjusts each pass's executor.
	prep func(*conversationLaneExec)
	// looks counts the starved boundary's looks.
	looks int
}

func newStarvedLane(t *testing.T, provider string) *starvedLane {
	t.Helper()
	f, deps, st := newScreenTurnFixture(t, provider)
	f.showTurn(false)
	return &starvedLane{f: f, deps: deps, st: st, l: f.lane(t, st)}
}

// prompt proposes a boundary prompt (gc session nudge's wait-idle) now.
func (s *starvedLane) prompt(t *testing.T, key, payload string) opjournal.Op {
	t.Helper()
	return s.f.propose(t, s.st, s.f.msg(key, opjournal.RolePrompt, payload, func(r *opjournal.Request) {
		r.Policy = cliLaneNudgePolicy(nudgeDeliveryWaitIdle)
		r.Producer = opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator}
	}))
}

// pass runs one lane pass with the pane's activity reading now, so the
// quiescence rule reads it busy, and returns op as the pass left it.
func (s *starvedLane) pass(t *testing.T, op opjournal.Op) opjournal.Op {
	t.Helper()
	s.f.sp.SetActivity(s.f.name, s.f.clk.Now())
	v, row := s.f.view(t)
	e := newConversationLaneExec(s.deps, row)
	quiescenceOnFixtureClock(s.f, e)
	look := e.starvedHold
	e.starvedHold = func(t nudgeTarget, now time.Time) (bool, error) {
		s.looks++
		return look(t, now)
	}
	if s.prep != nil {
		s.prep(e)
	}
	if _, err := s.l.Pass(context.Background(), v, e); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return s.f.get(t, s.st, op.ID)
}

// screenLooks counts the starved boundary's looks at the session's turn.
func (s *starvedLane) screenLooks() int {
	return s.looks
}

// TestLaneContentToANeverQuietSessionDeliversAfterStarvation: a boundary
// prompt to a session whose pane draws on every look and whose screen shows
// no turn waits while it is fresh and is typed once, as soon as it has been
// deliverable for laneBoundaryStarvedAfter.
func TestLaneContentToANeverQuietSessionDeliversAfterStarvation(t *testing.T) {
	s := newStarvedLane(t, "claude")
	const payload = "blocker gc-1 closed — routed dependent gc-2 may be ready to claim"
	op := s.prompt(t, "k-starved", payload)
	proposed := s.f.clk.Now()
	var got opjournal.Op
	for range 40 {
		if got = s.pass(t, op); got.Status == opjournal.StatusClosed {
			break
		}
		s.f.clk.Advance(laneBusyMax)
	}
	waited := s.f.clk.Now().Sub(proposed)
	if got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("after %s of a never-quiet pane: op %s %+v; want it closed succeeded once starved", waited, got.Status, got.Outcome)
	}
	if waited < laneBoundaryStarvedAfter || waited > laneBoundaryStarvedAfter+laneBusyMax {
		t.Fatalf("delivered after %s; want within one busy backoff of %s", waited, laneBoundaryStarvedAfter)
	}
	if typed := s.f.nudges(); len(typed) != 1 || !strings.Contains(typed[0].Message, payload) {
		t.Fatalf("typed %+v; want the prompt once", typed)
	}
}

// TestLaneFreshBusyDeferralStillWaits: short of laneBoundaryStarvedAfter a
// busy pane's content waits as before, and nothing reads the screen for it.
func TestLaneFreshBusyDeferralStillWaits(t *testing.T) {
	s := newStarvedLane(t, "claude")
	op := s.prompt(t, "k-fresh", "fresh")
	for range 11 {
		if got := s.pass(t, op); got.Status == opjournal.StatusClosed {
			t.Fatalf("op closed %+v short of starvation; want it waiting", got.Outcome)
		}
		s.f.clk.Advance(laneBusyMax)
	}
	if typed := s.f.nudges(); len(typed) != 0 {
		t.Fatalf("typed %+v into a busy pane short of starvation; want nothing", typed)
	}
	if n := s.screenLooks(); n != 0 {
		t.Fatalf("%d screen looks short of starvation; want none", n)
	}
}

// TestLaneStarvedBoundaryHoldsARunningTurn: a starved prompt to a session
// whose screen shows a running turn is not typed into it; once the screen
// shows none, it is typed once.
func TestLaneStarvedBoundaryHoldsARunningTurn(t *testing.T) {
	s := newStarvedLane(t, "claude")
	s.f.showTurn(true)
	const payload = "starved while a turn runs"
	op := s.prompt(t, "k-turn", payload)
	s.f.clk.Advance(10 * time.Minute)
	for range 6 {
		if got := s.pass(t, op); got.Status == opjournal.StatusClosed {
			t.Fatalf("op closed %+v while the screen shows a turn; want it waiting", got.Outcome)
		}
		s.f.clk.Advance(laneBusyMax)
	}
	if typed := s.f.nudges(); len(typed) != 0 {
		t.Fatalf("typed %+v into a running turn; want nothing", typed)
	}
	if s.screenLooks() == 0 {
		t.Fatal("a starved prompt read no screen; want the turn look to decide")
	}
	s.f.showTurn(false)
	if got := s.pass(t, op); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("after the turn: op %s %+v; want closed succeeded", got.Status, got.Outcome)
	}
	if typed := s.f.nudges(); len(typed) != 1 || !strings.Contains(typed[0].Message, payload) {
		t.Fatalf("typed %+v; want the prompt once", typed)
	}
}

// TestLaneStarvedBoundaryHonorsTheCodexHold: under program_status = "on" a
// provider declaring hold_mid_turn has the starved bypass vetoed while a
// fresh read of its program's record says a turn is working, though the
// screen shows none; under "off" the record is not read and the screen
// decides.
func TestLaneStarvedBoundaryHonorsTheCodexHold(t *testing.T) {
	setup := func(t *testing.T, city string) (*starvedLane, opjournal.Op) {
		t.Helper()
		s := newStarvedLane(t, "codex")
		cfg := &config.City{Session: config.SessionConfig{ProgramStatus: city}}
		s.deps.cfg = cfg
		s.prep = func(e *conversationLaneExec) {
			e.target.cfg = cfg
			e.target.resolved = &config.ResolvedProvider{Name: "codex", ProgramStatus: holdBinding(true)}
		}
		s.f.sp.SetProgramPane(s.f.name, holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now()))
		op := s.prompt(t, "k-codex", "starved into a codex turn")
		s.f.clk.Advance(10 * time.Minute)
		return s, op
	}
	t.Run("on", func(t *testing.T) {
		s, op := setup(t, "on")
		for range 3 {
			if got := s.pass(t, op); got.Status == opjournal.StatusClosed {
				t.Fatalf("op closed %+v while the record says working; want it held", got.Outcome)
			}
			s.f.clk.Advance(laneBusyMax)
		}
		if typed := s.f.nudges(); len(typed) != 0 {
			t.Fatalf("typed %+v while the record says working; want nothing", typed)
		}
		if n := s.f.sp.CountCalls("ProgramStatusHoldRead", s.f.name); n == 0 {
			t.Fatal("no hold read; want the record read fresh before the bypass")
		}
		s.f.sp.SetProgramPane(s.f.name, holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramDone, time.Now()))
		if got := s.pass(t, op); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
			t.Fatalf("after the record says done: op %s %+v; want closed succeeded", got.Status, got.Outcome)
		}
		if typed := s.f.nudges(); len(typed) != 1 {
			t.Fatalf("typed %+v; want the prompt once", typed)
		}
	})
	t.Run("off", func(t *testing.T) {
		s, op := setup(t, "off")
		if got := s.pass(t, op); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
			t.Fatalf("switch off: op %s %+v; want the screen to decide and the prompt delivered", got.Status, got.Outcome)
		}
		if n := s.f.sp.CountCalls("ProgramStatusHoldRead", s.f.name); n != 0 {
			t.Fatalf("%d hold reads under the switch off; want none", n)
		}
	})
}

// TestLaneStarvedBoundaryReprobeKeepsThePassesContentAge: the host
// re-probes a busy pass's executor with its own view, which carries no
// content age; the re-probe judges starvation by the age the pass's probe
// read, so a never-quiet session's re-probe ends the busy wait once its
// content starves instead of re-probing until start_by.
func TestLaneStarvedBoundaryReprobeKeepsThePassesContentAge(t *testing.T) {
	s := newStarvedLane(t, "claude")
	op := s.prompt(t, "k-reprobe", "starved across a re-probe")
	s.f.sp.SetActivity(s.f.name, s.f.clk.Now())
	v, row := s.f.view(t)
	e := newConversationLaneExec(s.deps, row)
	quiescenceOnFixtureClock(s.f, e)
	if _, err := s.l.Pass(context.Background(), v, e); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if got := s.f.get(t, s.st, op.ID); got.Status == opjournal.StatusClosed {
		t.Fatalf("fresh op closed %+v on a busy pane", got.Outcome)
	}
	if !v.ContentSince.IsZero() {
		t.Fatalf("host view carries ContentSince %v; want zero", v.ContentSince)
	}
	s.f.clk.Advance(laneBoundaryStarvedAfter)
	s.f.sp.SetActivity(s.f.name, s.f.clk.Now())
	p, err := e.Reprobe(context.Background(), v)
	if err != nil {
		t.Fatalf("Reprobe: %v", err)
	}
	if p.Boundary != opjournal.TriYes || laneProbeStillBusy(p) {
		t.Fatalf("re-probe of starved content: boundary %v, still busy %v; want a boundary that ends the busy wait", p.Boundary, laneProbeStillBusy(p))
	}
}

// TestLaneStarvedBoundaryHoldsAnIndeterminateTurn: a family whose busy
// reading the runtime does not trust (runtime.ErrIdleIndeterminate) cannot
// show that no turn runs, so its starved content holds, as the legacy
// poller's does, and the host is not told of a failure on every look.
func TestLaneStarvedBoundaryHoldsAnIndeterminateTurn(t *testing.T) {
	s := newStarvedLane(t, "gemini")
	s.f.sp.ScreenReadErrors = map[string]error{s.f.name: runtime.ErrIdleIndeterminate}
	var noted []error
	s.deps.note = func(stage string, err error) {
		if stage == "boundary_starved" {
			noted = append(noted, err)
		}
	}
	op := s.prompt(t, "k-indeterminate", "starved on an unreadable turn")
	s.f.clk.Advance(10 * time.Minute)
	if got := s.pass(t, op); got.Status == opjournal.StatusClosed {
		t.Fatalf("op closed %+v with the turn indeterminate; want it held", got.Outcome)
	}
	if typed := s.f.nudges(); len(typed) != 0 || s.looks == 0 || len(noted) != 0 {
		t.Fatalf("typed %+v, looks %d, noted %v; want nothing typed after a look, nothing noted", typed, s.looks, noted)
	}
}
