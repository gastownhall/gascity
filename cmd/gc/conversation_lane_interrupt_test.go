package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// Turn capabilities the interrupt tests run on.
var (
	// laneAtomicTurnCaps are a protocol source that fences a turn interrupt
	// atomically (ACP).
	laneAtomicTurnCaps = worker.TurnCapabilities{
		TurnCapabilities: runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic},
		Source:           worker.TurnSourceProtocol,
		Interrupt:        runtime.TurnInterruptProtocol,
	}
	// laneCheckedTurnCaps are a transcript source, whose turn interrupt is
	// checked (tmux).
	laneCheckedTurnCaps = worker.TurnCapabilities{
		TurnCapabilities: runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceChecked},
		Source:           worker.TurnSourceTranscript,
		Interrupt:        runtime.TurnInterruptKeystroke,
	}
)

// interruptStub is a worker.Handle whose InterruptAs records each request
// and answers from answer; during runs inside the call.
type interruptStub struct {
	worker.Handle
	mu     *sync.Mutex
	reqs   *[]worker.InterruptRequest
	by     *[]session.Actor
	answer func(worker.InterruptRequest) (worker.InterruptResult, error)
	during func()
}

func (s interruptStub) InterruptAs(_ context.Context, by session.Actor, req worker.InterruptRequest) (worker.InterruptResult, error) {
	s.mu.Lock()
	*s.reqs = append(*s.reqs, req)
	*s.by = append(*s.by, by)
	s.mu.Unlock()
	if s.during != nil {
		s.during()
	}
	if s.answer == nil {
		return worker.InterruptResult{}, nil
	}
	return s.answer(req)
}

// interruptCase runs one interrupt op through a pass of the live executor,
// over turns answering caps and read, and a stubbed interrupt handle.
type interruptCase struct {
	f      *laneFixture
	st     *opjournal.Store
	l      *opjournal.ConversationLane
	ints   *laneInterrupts
	caps   worker.TurnCapabilities
	read   func(context.Context) (runtime.TurnHistory, error)
	answer func(worker.InterruptRequest) (worker.InterruptResult, error)
	during func(e *conversationLaneExec)
	mu     sync.Mutex
	reqs   []worker.InterruptRequest
	by     []session.Actor
}

func newInterruptCase(t *testing.T, caps worker.TurnCapabilities) *interruptCase {
	t.Helper()
	f := newLaneFixture(t)
	st := f.laneStore(t)
	return &interruptCase{
		f: f, st: st, l: f.lane(t, st), ints: newLaneInterrupts(), caps: caps,
		read: func(context.Context) (runtime.TurnHistory, error) { return runtime.TurnHistory{}, nil },
	}
}

// propose proposes an interrupt keyed key, scoped to turn when one is
// named, with onFailure.
func (c *interruptCase) propose(t *testing.T, key, turn string, onFailure opjournal.OnFailure) opjournal.Op {
	t.Helper()
	return c.f.propose(t, c.st, opjournal.Request{
		Conversation: c.f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: key,
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
		Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1, Turn: turn},
		Policy:   opjournal.Policy{Interrupt: opjournal.InterruptPolicy{OnFailure: onFailure}},
	})
}

func (c *interruptCase) pass(t *testing.T) opjournal.PassResult {
	t.Helper()
	deps := c.f.deps(c.st)
	deps.interrupts = c.ints
	v, info := c.f.view(t)
	e := newConversationLaneExec(deps, info)
	e.sessionTurns = func(string) (worker.TurnHandle, error) {
		return laneStubTurns{caps: c.caps, read: c.read}, nil
	}
	e.interruptHandle = func() (worker.Handle, error) {
		stub := interruptStub{mu: &c.mu, reqs: &c.reqs, by: &c.by, answer: c.answer}
		if c.during != nil {
			stub.during = func() { c.during(e) }
		}
		return stub, nil
	}
	res, err := c.l.Pass(context.Background(), v, e)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return res
}

func (c *interruptCase) requests() []worker.InterruptRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reqs)
}

// fence is the fixture's incarnation fence as the worker gets it.
func (c *interruptCase) fence() runtime.InputFence {
	return runtime.InputFence{Generation: 1, Epoch: 1, Token: c.f.token}
}

func assertOutcome(t *testing.T, got opjournal.Op, arm opjournal.Arm, reason, turn, degradation string) {
	t.Helper()
	o := got.Outcome
	if got.Status != opjournal.StatusClosed || o.Arm != arm || o.Reason != reason || o.Result.Turn != turn || o.Degradation != degradation {
		t.Fatalf("op %s %s{reason %q, turn %q, degradation %q}, want %s{reason %q, turn %q, degradation %q}",
			got.Status, o.Arm, o.Reason, o.Result.Turn, o.Degradation, arm, reason, turn, degradation)
	}
}

// On an atomic turn fence, a late interrupt never kills the next turn: the
// op's turn ended and t2 runs, the executor still names t1 (never the turn
// it would probe), and the runtime's refusal closes the op skipped{stale}.
// On time, the same interrupt succeeds exact, with no degradation.
func TestLaneInterruptAtomicLateInterruptNeverKillsTheNextTurn(t *testing.T) {
	c := newInterruptCase(t, laneAtomicTurnCaps)
	c.read = func(context.Context) (runtime.TurnHistory, error) {
		return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t2"}}, nil
	}
	c.answer = func(req worker.InterruptRequest) (worker.InterruptResult, error) {
		if req.Turn != "t2" {
			return worker.InterruptResult{}, &runtime.TurnNotCurrentError{Session: c.f.name, TurnID: req.Turn, Current: runtime.TurnState{TurnID: "t2"}}
		}
		return worker.InterruptResult{}, nil
	}
	late := c.propose(t, "stop:1:t1", "t1", "")
	c.pass(t)
	assertOutcome(t, c.f.get(t, c.st, late.ID), opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
	reqs := c.requests()
	if len(reqs) != 1 || reqs[0].Turn != "t1" {
		t.Fatalf("requests %+v, want one naming t1", reqs)
	}
	if !reqs[0].Fence.IsZero() {
		t.Fatalf("a protocol turn interrupt carried fence %v; the worker refuses a fenced one", reqs[0].Fence)
	}

	onTime := c.propose(t, "stop:1:t2", "t2", "")
	c.pass(t)
	assertOutcome(t, c.f.get(t, c.st, onTime.ID), opjournal.ArmSucceeded, "", "t2", "")
}

// On a checked turn fence the same race is marked: an interrupt the check
// let through (the runtime interrupted whatever ran) closes succeeded with
// unfenced_interrupt, and every key carries the incarnation's input fence.
func TestLaneInterruptCheckedIsMarkedUnfenced(t *testing.T) {
	c := newInterruptCase(t, laneCheckedTurnCaps)
	op := c.propose(t, "stop:1:t1", "t1", "")
	c.pass(t)
	assertOutcome(t, c.f.get(t, c.st, op.ID), opjournal.ArmSucceeded, "", "t1", opjournal.DegradationUnfencedInterrupt)
	reqs := c.requests()
	if len(reqs) != 1 || reqs[0].Turn != "t1" || reqs[0].Fence != c.fence() {
		t.Fatalf("requests %+v, want t1 under fence %v", reqs, c.fence())
	}
}

// A session-scoped op names the turn a fresh read finds running where the
// session's turns allow a turn-scoped interrupt; with none running it closes
// skipped{stale}, nothing sent; a read that fails, or a session whose turns
// cannot carry it, runs it session-scoped and marks it.
func TestLaneInterruptSessionScope(t *testing.T) {
	cases := []struct {
		name     string
		caps     worker.TurnCapabilities
		read     func(context.Context) (runtime.TurnHistory, error)
		wantReqs []string // the turns the requests named
		arm      opjournal.Arm
		reason   string
		turn     string
		deg      string
	}{
		{
			name: "names the running turn", caps: laneAtomicTurnCaps,
			read: func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t7"}}, nil
			},
			wantReqs: []string{"t7"}, arm: opjournal.ArmSucceeded, turn: "t7",
		},
		{
			name: "no turn runs", caps: laneCheckedTurnCaps,
			read: func(context.Context) (runtime.TurnHistory, error) { return runtime.TurnHistory{}, nil },
			arm:  opjournal.ArmSkipped, reason: opjournal.ReasonStale,
		},
		{
			name: "read fails", caps: laneCheckedTurnCaps,
			read: func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{}, fmt.Errorf("scan: %w", runtime.ErrRuntimeUnavailable)
			},
			wantReqs: []string{""}, arm: opjournal.ArmSucceeded, deg: opjournal.DegradationUnfencedInterrupt,
		},
		{
			name: "no turn identity", caps: worker.TurnCapabilities{},
			wantReqs: []string{""}, arm: opjournal.ArmSucceeded, deg: opjournal.DegradationUnfencedInterrupt,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newInterruptCase(t, tc.caps)
			if tc.read != nil {
				c.read = tc.read
			}
			op := c.propose(t, "click-1", "", "")
			c.pass(t)
			assertOutcome(t, c.f.get(t, c.st, op.ID), tc.arm, tc.reason, tc.turn, tc.deg)
			var turns []string
			for _, r := range c.requests() {
				turns = append(turns, r.Turn)
				if r.Turn == "" && r.Fence != c.fence() {
					t.Fatalf("a session-scoped interrupt carried fence %v, want %v", r.Fence, c.fence())
				}
			}
			if !slices.Equal(turns, tc.wantReqs) {
				t.Fatalf("requests named %q, want %q", turns, tc.wantReqs)
			}
		})
	}
}

// on_failure restart goes to the worker with the controller as the actor,
// and a restart the worker reports marks the outcome.
func TestLaneInterruptOnFailureRestart(t *testing.T) {
	c := newInterruptCase(t, laneCheckedTurnCaps)
	c.answer = func(worker.InterruptRequest) (worker.InterruptResult, error) {
		return worker.InterruptResult{Restarted: true}, nil
	}
	op := c.propose(t, "stop:1:t1", "t1", opjournal.OnFailureRestart)
	c.pass(t)
	assertOutcome(t, c.f.get(t, c.st, op.ID), opjournal.ArmSucceeded, "", "t1", opjournal.DegradationRestartedAfterFailedInterrupt)
	reqs := c.requests()
	if len(reqs) != 1 || reqs[0].OnFailure != worker.InterruptOnFailureRestart {
		t.Fatalf("requests %+v, want on_failure restart", reqs)
	}
	if c.by[0].Kind != session.ActorController || c.by[0].City.IsZero() {
		t.Fatalf("actor %+v, want the controller in the city", c.by[0])
	}
	failOp := c.propose(t, "stop:1:t2", "t2", opjournal.OnFailureFail)
	c.answer = nil
	c.pass(t)
	if got := c.requests(); got[1].OnFailure != "" {
		t.Fatalf("on_failure fail sent %q, want the default", got[1].OnFailure)
	}
	assertOutcome(t, c.f.get(t, c.st, failOp.ID), opjournal.ArmSucceeded, "", "t2", opjournal.DegradationUnfencedInterrupt)
}

// While the interrupt runs, its InterruptRef is the conversation's running
// interrupt for the turn observer; after, none is.
func TestLaneInterruptHandsItsRefToTheObserver(t *testing.T) {
	c := newInterruptCase(t, laneCheckedTurnCaps)
	op := c.propose(t, "stop:1:t1", "t1", "")
	var during opjournal.InterruptRef
	c.during = func(*conversationLaneExec) { during = c.ints.current(c.f.id) }
	c.pass(t)
	if want := (opjournal.InterruptRef{Op: op.ID, Turn: "t1"}); during != want {
		t.Fatalf("running interrupt during the effect = %+v, want %+v", during, want)
	}
	if got := c.ints.current(c.f.id); got != (opjournal.InterruptRef{}) {
		t.Fatalf("running interrupt after the effect = %+v, want none", got)
	}
}

// A session-scoped interrupt names no turn, so nothing is handed over.
func TestLaneInterruptsRecordOnlyANamedTurn(t *testing.T) {
	r := newLaneInterrupts()
	end := r.begin("c", opjournal.InterruptRef{Op: "op-1"})
	if got := r.current("c"); got != (opjournal.InterruptRef{}) {
		t.Fatalf("a ref without a turn was recorded: %+v", got)
	}
	end()
	var none *laneInterrupts
	none.begin("c", opjournal.InterruptRef{Op: "op-1", Turn: "t"})()
	if got := none.current("c"); got != (opjournal.InterruptRef{}) {
		t.Fatalf("a nil record answered %+v", got)
	}
}

// laneInterruptResult maps the worker's answers to the lane's results (X11
// §5.4).
func TestLaneInterruptResultMapping(t *testing.T) {
	miss := &runtime.FenceMismatchError{Session: "s", Found: runtime.FenceFoundAbsent}
	partial := &runtime.FenceMismatchError{Session: "s", Found: runtime.FenceFoundAbsent, Partial: true}
	boom := errors.New("boom")
	failed := func(r string) opjournal.ControlResult { return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: r} }
	skipped := opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	cases := []struct {
		name  string
		res   worker.InterruptResult
		err   error
		turn  string
		exact bool
		want  opjournal.ControlResult
	}{
		{"exact settle", worker.InterruptResult{}, nil, "t1", true, opjournal.Settled{Turn: "t1", Via: laneViaLive}},
		{"checked settle", worker.InterruptResult{}, nil, "t1", false, opjournal.Settled{Turn: "t1", Via: laneViaLive, Degradation: opjournal.DegradationUnfencedInterrupt}},
		{"session settle", worker.InterruptResult{}, nil, "", false, opjournal.Settled{Via: laneViaLive, Degradation: opjournal.DegradationUnfencedInterrupt}},
		{"restarted", worker.InterruptResult{Restarted: true}, nil, "t1", true, opjournal.Settled{Turn: "t1", Via: laneViaLive, Degradation: opjournal.DegradationRestartedAfterFailedInterrupt}},
		{"not ended", worker.InterruptResult{}, session.TurnNotEndedError("turn", errors.New("busy")), "t1", true, failed(opjournal.ReasonNoBoundary)},
		{"not current", worker.InterruptResult{}, &runtime.TurnNotCurrentError{Session: "s", TurnID: "t1"}, "t1", true, skipped},
		{"identity unsupported", worker.InterruptResult{}, fmt.Errorf("x: %w", runtime.ErrTurnIdentityUnsupported), "t1", true, failed(opjournal.ReasonUnsupported)},
		{"interrupt unsupported", worker.InterruptResult{}, fmt.Errorf("x: %w", session.ErrTurnInterruptUnsupported), "", false, failed(opjournal.ReasonUnsupported)},
		{"restart unsupported", worker.InterruptResult{}, fmt.Errorf("x: %w", worker.ErrInterruptRestartUnsupported), "", false, failed(opjournal.ReasonUnsupported)},
		{"fence unsupported", worker.InterruptResult{}, fmt.Errorf("x: %w", runtime.ErrInputFenceUnsupported), "", false, failed(opjournal.ReasonUnsupported)},
		{"fence miss", worker.InterruptResult{}, miss, "", false, opjournal.NotDelivered{Transient: true, Cause: miss}},
		{"partial fence miss", worker.InterruptResult{}, partial, "", false, opjournal.Unknown{Cause: partial}},
		{"unavailable", worker.InterruptResult{}, runtime.ErrRuntimeUnavailable, "t1", false, opjournal.NotDelivered{Transient: true, Cause: runtime.ErrRuntimeUnavailable}},
		{"session gone", worker.InterruptResult{}, session.ErrSessionNotFound, "", false, skipped},
		{"other", worker.InterruptResult{}, boom, "", false, opjournal.Unknown{Cause: boom}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := laneInterruptResult(tc.res, tc.err, tc.turn, tc.exact)
			if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", tc.want) {
				t.Fatalf("result = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The production wiring, end to end on the fake runtime: a session-scoped
// interrupt op on a session with no turn source goes through the session's
// own handle as a fenced stop, and closes succeeded, marked unfenced.
func TestLaneInterruptThroughTheSessionHandle(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	op := f.propose(t, st, opjournal.Request{
		Conversation: f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "click-1",
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
		Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1},
	})
	f.sp.Calls = nil
	if _, err := f.pass(context.Background(), t, f.lane(t, st), st, nil); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	assertOutcome(t, f.get(t, st, op.ID), opjournal.ArmSucceeded, "", "", opjournal.DegradationUnfencedInterrupt)
	var fenced []runtime.InputFence
	for _, c := range f.sp.SnapshotCalls() {
		if c.Method == "Interrupt" || c.Method == "SendKeys" {
			fenced = append(fenced, c.Fence)
		}
	}
	want := runtime.InputFence{Generation: 1, Epoch: 1, Token: f.token}
	if len(fenced) == 0 || slices.ContainsFunc(fenced, func(got runtime.InputFence) bool { return got != want }) {
		t.Fatalf("interrupt inputs fenced %v, want each under %v", fenced, want)
	}
}
