package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
// and answers from answer, by default as the worker does: nil, reporting
// Unfenced for a fenced request that allows it on a protocol source's turn
// interrupt (unfencedTurns). during runs inside the call.
type interruptStub struct {
	worker.Handle
	mu     *sync.Mutex
	reqs   *[]worker.InterruptRequest
	by     *[]session.Actor
	answer func(worker.InterruptRequest) (worker.InterruptResult, error)
	during func()
	// unfencedTurns says the session's turn interrupts take no input
	// fence (a protocol source).
	unfencedTurns bool
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
		return worker.InterruptResult{Unfenced: s.unfencedTurns && req.Turn != "" && req.AllowUnfenced && !req.Fence.IsZero()}, nil
	}
	return s.answer(req)
}

// interruptCase runs one interrupt op through a pass of the live executor,
// over turns answering caps and read, and a stubbed interrupt handle.
type interruptCase struct {
	f       *laneFixture
	st      *opjournal.Store
	l       *opjournal.ConversationLane
	ints    *laneInterrupts
	caps    worker.TurnCapabilities
	capsErr error
	read    func(context.Context) (runtime.TurnHistory, error)
	answer  func(worker.InterruptRequest) (worker.InterruptResult, error)
	during  func(e *conversationLaneExec)
	// handleErr, when set, is the interrupt handle's resolution error.
	handleErr error
	mu        sync.Mutex
	reqs      []worker.InterruptRequest
	by        []session.Actor
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
		return laneStubTurns{caps: c.caps, capsErr: c.capsErr, read: c.read}, nil
	}
	e.interruptHandle = func() (worker.Handle, error) {
		if c.handleErr != nil {
			return nil, c.handleErr
		}
		stub := interruptStub{mu: &c.mu, reqs: &c.reqs, by: &c.by, answer: c.answer, unfencedTurns: c.caps.Source == worker.TurnSourceProtocol}
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
// The request carries the incarnation's fence and lets the worker drop it;
// a protocol turn interrupt, which takes none, is then marked
// unfenced_interrupt on time (the 2026-10-19 ruling). Only one that kept
// its fence is exact, with no degradation.
func TestLaneInterruptAtomicLateInterruptNeverKillsTheNextTurn(t *testing.T) {
	c := newInterruptCase(t, laneAtomicTurnCaps)
	c.read = func(context.Context) (runtime.TurnHistory, error) {
		return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t2"}}, nil
	}
	c.answer = func(req worker.InterruptRequest) (worker.InterruptResult, error) {
		if req.Turn != "t2" {
			return worker.InterruptResult{}, &runtime.TurnNotCurrentError{Session: c.f.name, TurnID: req.Turn, Current: runtime.TurnState{TurnID: "t2"}}
		}
		return worker.InterruptResult{Unfenced: req.Turn != "t3"}, nil
	}
	late := c.propose(t, "stop:1:t1", "t1", "")
	c.pass(t)
	assertOutcome(t, c.f.get(t, c.st, late.ID), opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
	reqs := c.requests()
	if len(reqs) != 1 || reqs[0].Turn != "t1" {
		t.Fatalf("requests %+v, want one naming t1", reqs)
	}
	if reqs[0].Fence != c.fence() || !reqs[0].AllowUnfenced {
		t.Fatalf("request %+v, want the fence %v, which the worker may drop", reqs[0], c.fence())
	}

	onTime := c.propose(t, "stop:1:t2", "t2", "")
	c.pass(t)
	assertOutcome(t, c.f.get(t, c.st, onTime.ID), opjournal.ArmSucceeded, "", "t2", opjournal.DegradationUnfencedInterrupt)

	c.read = func(context.Context) (runtime.TurnHistory, error) {
		return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t3"}}, nil
	}
	c.answer = func(worker.InterruptRequest) (worker.InterruptResult, error) { return worker.InterruptResult{}, nil }
	fenced := c.propose(t, "stop:1:t3", "t3", "")
	c.pass(t)
	assertOutcome(t, c.f.get(t, c.st, fenced.ID), opjournal.ArmSucceeded, "", "t3", "")
}

// An op whose row names no token goes without a fence, and is marked so
// even on an atomic turn fence.
func TestLaneInterruptWithoutATokenIsMarkedUnfenced(t *testing.T) {
	c := newInterruptCase(t, laneAtomicTurnCaps)
	c.answer = func(worker.InterruptRequest) (worker.InterruptResult, error) { return worker.InterruptResult{}, nil }
	op := c.propose(t, "stop:1:t1", "t1", "")
	deps := c.f.deps(c.st)
	deps.interrupts = c.ints
	v, info := c.f.view(t)
	v.Token = ""
	e := newConversationLaneExec(deps, info)
	e.sessionTurns = func(string) (worker.TurnHandle, error) { return laneStubTurns{caps: c.caps, read: c.read}, nil }
	e.interruptHandle = func() (worker.Handle, error) {
		return interruptStub{mu: &c.mu, reqs: &c.reqs, by: &c.by, answer: c.answer}, nil
	}
	if _, err := c.l.Pass(context.Background(), v, e); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	got := c.f.get(t, c.st, op.ID)
	t.Logf("op %s %+v", got.Status, got.Outcome)
	assertOutcome(t, got, opjournal.ArmSucceeded, "", "t1", opjournal.DegradationUnfencedInterrupt)
	for _, r := range c.requests() {
		if !r.Fence.IsZero() {
			t.Fatalf("request %+v carried a fence the row does not name", r)
		}
	}
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
// session's turns allow a turn-scoped interrupt; with none running, or the
// session gone, it closes skipped{stale}, nothing sent. A read that cannot
// answer (runtime.ErrRuntimeUnavailable), or turn capabilities the probe
// could not read, release it, nothing sent: it never runs without the fresh
// read (X11 §5.4); any other read error counts a failure, nothing sent. A
// session whose turns cannot carry it, or whose turns carry no identity (a
// transcript without turn ids), runs it session-scoped and marks it.
func TestLaneInterruptSessionScope(t *testing.T) {
	cases := []struct {
		name     string
		caps     worker.TurnCapabilities
		capsErr  error
		read     func(context.Context) (runtime.TurnHistory, error)
		wantReqs []string // the turns the requests named
		released bool
		failure  bool // released with a failure counted
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
			wantReqs: []string{"t7"}, arm: opjournal.ArmSucceeded, turn: "t7", deg: opjournal.DegradationUnfencedInterrupt,
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
			released: true,
		},
		{
			name: "read finds no turn identity", caps: laneCheckedTurnCaps,
			read: func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{}, fmt.Errorf("transcript records the open turn without an id: %w", runtime.ErrTurnIdentityUnsupported)
			},
			wantReqs: []string{""}, arm: opjournal.ArmSucceeded, deg: opjournal.DegradationUnfencedInterrupt,
		},
		{
			name: "read finds the session gone", caps: laneCheckedTurnCaps,
			read: func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{}, fmt.Errorf("turns of session: %w", session.ErrSessionNotFound)
			},
			arm: opjournal.ArmSkipped, reason: opjournal.ReasonStale,
		},
		{
			name: "read finds the runtime gone", caps: laneCheckedTurnCaps,
			read: func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{}, fmt.Errorf("turns of session: %w", runtime.ErrSessionNotFound)
			},
			arm: opjournal.ArmSkipped, reason: opjournal.ReasonStale,
		},
		{
			name: "read fails otherwise", caps: laneCheckedTurnCaps,
			read: func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{}, errors.New("transcript: unexpected end of JSON input")
			},
			released: true, failure: true,
		},
		{
			name: "caps unreadable", caps: laneCheckedTurnCaps, capsErr: fmt.Errorf("caps: %w", runtime.ErrRuntimeUnavailable),
			released: true,
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
			c.capsErr = tc.capsErr
			op := c.propose(t, "click-1", "", "")
			c.pass(t)
			if got := c.f.get(t, c.st, op.ID); tc.released {
				if got.Status != opjournal.StatusAccepted || got.MaybeDelivered != 0 {
					t.Fatalf("op %s maybe_delivered %d, want released, nothing sent", got.Status, got.MaybeDelivered)
				}
				if counted := got.Failures > 0; counted != tc.failure {
					t.Fatalf("op released with %d failures (last error %q), want a failure counted %v", got.Failures, got.LastError, tc.failure)
				}
			} else {
				assertOutcome(t, got, tc.arm, tc.reason, tc.turn, tc.deg)
			}
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

// The interrupt's InterruptRef is the conversation's interrupt for the turn
// observer from before the effect until the observer folds that turn's end:
// an effect that may have run leaves it (the observer folds turn updates
// only between passes), and one that sent nothing takes it back. A turn not
// seen to end is recorded on the op too, so the credit survives the record.
func TestLaneInterruptHandsItsRefToTheObserver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(worker.InterruptRequest) (worker.InterruptResult, error)
		kept   bool
	}{
		{"settled", nil, true},
		{"not ended", func(worker.InterruptRequest) (worker.InterruptResult, error) {
			return worker.InterruptResult{}, session.TurnNotEndedError("turn", errors.New("busy"))
		}, true},
		{"unknown", func(worker.InterruptRequest) (worker.InterruptResult, error) {
			return worker.InterruptResult{}, errors.New("boom")
		}, true},
		{"not current", func(req worker.InterruptRequest) (worker.InterruptResult, error) {
			return worker.InterruptResult{}, &runtime.TurnNotCurrentError{Session: "s", TurnID: req.Turn}
		}, false},
		{"unavailable", func(worker.InterruptRequest) (worker.InterruptResult, error) {
			return worker.InterruptResult{}, runtime.ErrRuntimeUnavailable
		}, false},
		{"sent, outcome unknown: ctx ended, no restart", func(worker.InterruptRequest) (worker.InterruptResult, error) {
			return worker.InterruptResult{}, fmt.Errorf("stopping the turn of gc-1: the caller's context ended, not restarting it: %w: %w: %w",
				session.ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, context.Canceled)
		}, true},
		{"sent, outcome unknown: send failed after the check", func(worker.InterruptRequest) (worker.InterruptResult, error) {
			return worker.InterruptResult{}, fmt.Errorf("stopping the turn of gc-1: %w: %w: %w",
				session.ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, errors.New("tmux: broken pipe"))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newInterruptCase(t, laneCheckedTurnCaps)
			c.read = func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}}, nil
			}
			c.answer = tc.answer
			op := c.propose(t, "click-1", "", "")
			var during opjournal.InterruptRef
			c.during = func(*conversationLaneExec) { during = c.ints.current(c.f.id) }
			c.pass(t)
			want := opjournal.InterruptRef{Op: op.ID, Turn: "t1"}
			if during != want {
				t.Fatalf("interrupt during the effect = %+v, want %+v", during, want)
			}
			after := c.ints.current(c.f.id)
			if tc.kept && after != want {
				t.Fatalf("interrupt after the effect = %+v, want %+v kept for the observer", after, want)
			}
			if !tc.kept && after != (opjournal.InterruptRef{}) {
				t.Fatalf("interrupt after an effect that sent nothing = %+v, want none", after)
			}
			got := c.f.get(t, c.st, op.ID)
			if got.Status == opjournal.StatusClosed && got.Outcome.Reason == opjournal.ReasonNoBoundary && got.Outcome.Result.Turn != "t1" {
				t.Fatalf("op closed failed{no_boundary} on turn %q, want t1", got.Outcome.Result.Turn)
			}
			if strings.HasPrefix(tc.name, "sent") && (got.Status != opjournal.StatusAccepted || got.MaybeDelivered == 0) {
				t.Fatalf("op %s maybe_delivered %d after a sent interrupt of unknown outcome, want released maybe delivered", got.Status, got.MaybeDelivered)
			}
		})
	}
}

// Only a ref naming an op and a turn is recorded; a newer ref replaces it,
// the observer's clear drops only the ref it names, and a nil record records
// nothing.
func TestLaneInterruptsRecord(t *testing.T) {
	r := newLaneInterrupts()
	r.record("c", opjournal.InterruptRef{Op: "op-1"})
	if got := r.current("c"); got != (opjournal.InterruptRef{}) {
		t.Fatalf("a ref without a turn was recorded: %+v", got)
	}
	first, second := opjournal.InterruptRef{Op: "op-1", Turn: "t1"}, opjournal.InterruptRef{Op: "op-2", Turn: "t2"}
	r.record("c", first)
	r.record("c", second)
	r.clear("c", first)
	if got := r.current("c"); got != second {
		t.Fatalf("after clearing a replaced ref the record is %+v, want %+v", got, second)
	}
	r.clear("c", second)
	if got := r.current("c"); got != (opjournal.InterruptRef{}) {
		t.Fatalf("after its clear the record is %+v, want none", got)
	}
	// An executor that takes its ref back restores the one it replaced, which
	// the observer has not folded yet.
	r.record("c", first)
	prev := r.record("c", second)
	if prev != first {
		t.Fatalf("record returned %+v as the replaced ref, want %+v", prev, first)
	}
	r.restore("c", second, prev)
	if got := r.current("c"); got != first {
		t.Fatalf("after taking back %+v the record is %+v, want %+v restored", second, got, first)
	}
	r.record("c", second)
	r.record("c", opjournal.InterruptRef{Op: "op-3", Turn: "t3"})
	r.restore("c", second, first)
	if got := r.current("c"); got.Op != "op-3" {
		t.Fatalf("taking back a replaced ref changed the record to %+v", got)
	}
	var none *laneInterrupts
	none.record("c", first)
	none.clear("c", first)
	none.restore("c", first, second)
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
	noBoundary := opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonNoBoundary, Turn: "t1"}
	notSent := fmt.Errorf("x: %w: %w", worker.ErrInterruptNotSent, worker.ErrHandleConfig)
	stopping := fmt.Errorf("x: %w", session.ErrSessionStopping)
	notPrepared := fmt.Errorf("x: %w: %w", session.ErrReplacementNotPrepared, miss)
	skipped := opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	outcomeUnknown := fmt.Errorf("x: %w: %w", session.ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable)
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
		{"not ended", worker.InterruptResult{}, session.TurnNotEndedError("turn", errors.New("busy")), "t1", true, noBoundary},
		{"not settled", worker.InterruptResult{}, fmt.Errorf("interrupting session: %w", fmt.Errorf("acp: turn still open: %w", runtime.ErrInterruptNotSettled)), "t1", true, noBoundary},
		{"not settled, checked", worker.InterruptResult{}, fmt.Errorf("stopping the turn of gc-1: %w: %w", runtime.ErrRuntimeUnavailable, runtime.ErrInterruptNotSettled), "t1", false, noBoundary},
		{"restarted and unfenced", worker.InterruptResult{Restarted: true, Unfenced: true}, nil, "t1", false, opjournal.Settled{Turn: "t1", Via: laneViaLive, Degradation: opjournal.DegradationRestartedAfterFailedInterrupt}},
		{"not sent", worker.InterruptResult{}, fmt.Errorf("x: %w: %w", worker.ErrInterruptNotSent, worker.ErrHandleConfig), "t1", false, opjournal.NotDelivered{Transient: true, Cause: notSent}},
		{"session stopping", worker.InterruptResult{}, stopping, "", false, opjournal.NotDelivered{Transient: true, Cause: stopping}},
		{"replacement not prepared", worker.InterruptResult{}, notPrepared, "t1", false, opjournal.Unknown{Cause: notPrepared}},
		{"sent, outcome unknown", worker.InterruptResult{}, outcomeUnknown, "t1", false, opjournal.Unknown{Cause: outcomeUnknown}},
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
			if tc.name == "not sent" {
				// The cause is wrapped afresh; compare its shape.
				nd, ok := got.(opjournal.NotDelivered)
				if !ok || !nd.Transient || !errors.Is(nd.Cause, worker.ErrInterruptNotSent) {
					t.Fatalf("result = %#v, want a transient release", got)
				}
				return
			}
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

// The acceptance case on an atomic fake runtime (ACP): a turn-scoped op
// goes to the session's own handle with its fence, which the worker drops
// for the protocol turn interrupt, so a caps read that failed in the probe
// never turns it into failed{unsupported}. With t1 no longer running, the
// op closes skipped{stale} whether or not the probe read the caps.
func TestLaneInterruptACPTurnAfterACapsReadHiccup(t *testing.T) {
	atomic := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact}
	for _, tc := range []struct {
		name    string
		capsErr error
	}{
		{"caps read unavailable", fmt.Errorf("turns of session: %w", runtime.ErrRuntimeUnavailable)},
		{"caps read", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTurnLaneFixture(t, runtime.ProfileACP, func(meta map[string]string) { meta["transport"] = "acp" })
			f.laneFixture.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{f.name: {atomic}}
			st := f.laneStore(t)
			op := f.propose(t, st, opjournal.Request{
				Conversation: f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "stop:1:t1",
				Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
				Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1, Turn: "t1"},
			})
			e, v := f.exec(t, st)
			capsErr := tc.capsErr
			e.turns = func() (worker.TurnHandle, error) {
				return laneStubTurns{
					caps:    worker.TurnCapabilities{TurnCapabilities: atomic, Source: worker.TurnSourceProtocol, Interrupt: runtime.TurnInterruptKeystroke},
					capsErr: capsErr,
					read:    func(context.Context) (runtime.TurnHistory, error) { return runtime.TurnHistory{}, nil },
				}, nil
			}
			if _, err := f.lane(t, st).Pass(context.Background(), v, e); err != nil {
				t.Fatalf("Pass: %v", err)
			}
			assertOutcome(t, f.get(t, st, op.ID), opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
		})
	}
}

// The interrupt handle's resolution sends nothing: a session gone closes
// the op skipped{stale}, and any other error releases it, nothing sent.
func TestLaneInterruptHandleResolution(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		stale bool
	}{
		{"session gone", fmt.Errorf("resolving: %w", session.ErrSessionNotFound), true},
		{"other", errors.New("config: command is required"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newInterruptCase(t, laneCheckedTurnCaps)
			c.handleErr = tc.err
			op := c.propose(t, "stop:1:t1", "t1", "")
			c.pass(t)
			got := c.f.get(t, c.st, op.ID)
			if tc.stale {
				assertOutcome(t, got, opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
			} else if got.Status != opjournal.StatusAccepted || got.MaybeDelivered != 0 || got.Failures != 0 {
				t.Fatalf("op %s maybe_delivered %d failures %d, want a transient release, nothing sent", got.Status, got.MaybeDelivered, got.Failures)
			}
			if after := c.ints.current(c.f.id); after != (opjournal.InterruptRef{}) {
				t.Fatalf("a handle that never resolved left the ref %+v", after)
			}
		})
	}
}

// The acceptance case end to end on an atomic fake runtime (ACP) with the
// next turn running: an op scoped to t1, which ended while t2 runs, reaches
// the runtime's turn interrupt naming t1, which refuses it, so it closes
// skipped{stale} and t2 is never interrupted; an op scoped to t2 then
// interrupts t2.
func TestLaneInterruptAtomicFakeNeverInterruptsTheNextTurn(t *testing.T) {
	atomic := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact}
	f := newTurnLaneFixture(t, runtime.ProfileACP, func(meta map[string]string) { meta["transport"] = "acp" })
	f.laneFixture.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{f.name: {atomic}}
	f.laneFixture.sp.TurnScripts = map[string][]string{f.name: {"t2", "t2", ""}}
	st := f.laneStore(t)
	propose := func(turn string) opjournal.Op {
		return f.propose(t, st, opjournal.Request{
			Conversation: f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "stop:1:" + turn,
			Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
			Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1, Turn: turn},
		})
	}
	pass := func() {
		t.Helper()
		e, v := f.exec(t, st)
		e.turns = func() (worker.TurnHandle, error) {
			return laneStubTurns{
				caps: worker.TurnCapabilities{TurnCapabilities: atomic, Source: worker.TurnSourceProtocol, Interrupt: runtime.TurnInterruptKeystroke},
				read: func(context.Context) (runtime.TurnHistory, error) {
					return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t2"}}, nil
				},
			}, nil
		}
		if _, err := f.lane(t, st).Pass(context.Background(), v, e); err != nil {
			t.Fatalf("Pass: %v", err)
		}
	}
	interrupts := func() []string {
		var out []string
		for _, c := range f.laneFixture.sp.SnapshotCalls() {
			switch c.Method {
			case "InterruptTurn":
				out = append(out, c.Key+":"+c.Value)
			case "Interrupt", "SendKeys":
				out = append(out, c.Method)
			}
		}
		return out
	}
	late := propose("t1")
	pass()
	assertOutcome(t, f.get(t, st, late.ID), opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
	if got := interrupts(); !slices.Equal(got, []string{"t1:not_current"}) {
		t.Fatalf("interrupts %v, want only t1's, refused not current", got)
	}
	onTime := propose("t2")
	pass()
	assertOutcome(t, f.get(t, st, onTime.ID), opjournal.ArmSucceeded, "", "t2", opjournal.DegradationUnfencedInterrupt)
	if got := interrupts(); !slices.Contains(got, "t2:sent") {
		t.Fatalf("interrupts %v, want t2's sent", got)
	}
}
