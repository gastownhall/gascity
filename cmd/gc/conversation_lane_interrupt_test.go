package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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

// The worker's answers for an interrupt held back on this side of the
// runtime, nothing sent, and for a restart that could not start the runtime
// it stopped.
var (
	// errLaneInterruptLockBusy is a session mutation lock still held at the
	// effect's deadline.
	errLaneInterruptLockBusy = fmt.Errorf("stopping the turn of gc-1: %w: %w", runtime.ErrRuntimeUnavailable,
		fmt.Errorf("session gc-1: %w: %w", session.ErrSessionMutationBusy, context.DeadlineExceeded))
	// errLaneInterruptLeaseBusy is the restart's runtime lease held by another.
	errLaneInterruptLeaseBusy = fmt.Errorf("stopping the turn of gc-1: taking the runtime lease for its restart on failure, nothing sent: %w: %w",
		runtime.ErrRuntimeUnavailable, &session.RuntimeLeaseBusyError{Name: "gc-1", Holder: "a start", Local: true})
	// errLaneInterruptReadOutstanding is a check whose read was refused at
	// once: an earlier check's read of its key, given up on, is still out.
	errLaneInterruptReadOutstanding = fmt.Errorf("stopping the turn of gc-1: checking turn %q of gc-1 before interrupting it: %w: %w",
		"t1", runtime.ErrRuntimeUnavailable, fmt.Errorf("%w: %w", worker.ErrObservationOutstanding, runtime.ErrRuntimeUnavailable))
	// errLaneRestartNotStarted is an on_failure restart that stopped the
	// runtime and could not start it again.
	errLaneRestartNotStarted = fmt.Errorf("restarting session gc-1 after its interrupted turn was not seen to end: %w (resuming session: cannot start)",
		session.ErrRestartNotStarted)
	// errLaneTranscriptRowUnread and errLaneProtocolRowUnread are a turn
	// interrupt whose session row would not load under the session mutation
	// lock, on a transcript source (the checked stop) and a protocol source
	// (the protocol turn interrupt): nothing sent, which the worker reports
	// unavailable too.
	errLaneTranscriptRowUnread = fmt.Errorf("turns of session gc-1: %w: %w", runtime.ErrRuntimeUnavailable,
		fmt.Errorf("stopping the turn of gc-1: %w: %w", session.ErrStopNotSent, errors.New("store: connection reset by peer")))
	errLaneProtocolRowUnread = fmt.Errorf("turns of session gc-1: %w: %w", runtime.ErrRuntimeUnavailable,
		fmt.Errorf("interrupting turn %q of gc-1: %w: %w", "t1", session.ErrStopNotSent, errors.New("store: connection reset by peer")))
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
	// fresh, when set, answers the interrupt's fresh read
	// (worker.TurnHandle.FreshTurn), else read does.
	fresh  func(context.Context) (runtime.TurnHistory, error)
	answer func(worker.InterruptRequest) (worker.InterruptResult, error)
	during func(e *conversationLaneExec)
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
	return c.passWith(context.Background(), t)
}

// passWith is pass under ctx, which the effect's context derives from.
func (c *interruptCase) passWith(ctx context.Context, t *testing.T) opjournal.PassResult {
	t.Helper()
	deps := c.f.deps(c.st)
	deps.interrupts = c.ints
	v, info := c.f.view(t)
	e := newConversationLaneExec(deps, info)
	e.sessionTurns = func(string) (worker.TurnHandle, error) {
		return laneStubTurns{caps: c.caps, capsErr: c.capsErr, read: c.read, fresh: c.fresh}, nil
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
	res, err := c.l.Pass(ctx, v, e)
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
// could not read, release it as unavailable, nothing sent: it never runs
// without the fresh read (X11 §5.4); any other read error counts a failure,
// nothing sent. A
// session whose turns cannot carry it, or whose turns carry no identity (a
// transcript without turn ids), runs it session-scoped and marks it.
func TestLaneInterruptSessionScope(t *testing.T) {
	cases := []struct {
		name        string
		caps        worker.TurnCapabilities
		capsErr     error
		read        func(context.Context) (runtime.TurnHistory, error)
		wantReqs    []string // the turns the requests named
		released    bool
		failure     bool // released with a failure counted
		unavailable bool // released as unavailable, which start_by closes provider_unavailable
		arm         opjournal.Arm
		reason      string
		turn        string
		deg         string
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
			released: true, unavailable: true,
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
			released: true, unavailable: true,
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
				if marked := strings.HasPrefix(got.LastError, "unavailable"); marked != tc.unavailable {
					t.Fatalf("op released with last error %q, want it marked unavailable %v", got.LastError, tc.unavailable)
				}
			} else {
				assertOutcome(t, got, tc.arm, tc.reason, tc.turn, tc.deg)
			}
			reqs := c.requests()
			for _, r := range reqs {
				if r.Turn == "" && r.Fence != c.fence() {
					t.Fatalf("a session-scoped interrupt carried fence %v, want %v", r.Fence, c.fence())
				}
			}
			if turns := requestTurns(reqs); !slices.Equal(turns, tc.wantReqs) {
				t.Fatalf("requests named %q, want %q", turns, tc.wantReqs)
			}
		})
	}
}

// A session-scoped op's fresh read is a read of its own
// (worker.TurnHandle.FreshTurn), never a read already in flight: one a turn
// watch started before t2 did still holds t1, and an interrupt aimed at t1
// would be refused not current, leaving t2 running. The op targets t2, the
// turn its own read finds.
func TestLaneInterruptSessionScopeReadsTheTurnsItself(t *testing.T) {
	for _, caps := range []worker.TurnCapabilities{laneAtomicTurnCaps, laneCheckedTurnCaps} {
		t.Run(string(caps.Source), func(t *testing.T) {
			c := newInterruptCase(t, caps)
			c.read = func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}}, nil
			}
			c.fresh = func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t2"}}, nil
			}
			op := c.propose(t, "click-1", "", "")
			c.pass(t)
			if turns := requestTurns(c.requests()); !slices.Equal(turns, []string{"t2"}) {
				t.Fatalf("requests named %q, want [t2], the turn the op's own read finds", turns)
			}
			assertOutcome(t, c.f.get(t, c.st, op.ID), opjournal.ArmSucceeded, "", "t2", opjournal.DegradationUnfencedInterrupt)
		})
	}
}

// A session-scoped op's fresh read refused because an earlier read is still
// out (worker.ErrObservationOutstanding) asked the runtime nothing: a read a
// budget gave up on, wedged in a stalled store or a call that ignores its
// context, holds its key until it returns. Whether that is the lane's own
// read, its key held past the budget, or one the worker makes inside it,
// the op is released held back, nothing sent, and its start_by closes it
// expired_before_start, never provider_unavailable.
func TestLaneInterruptReadStillOutIsHeldBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold func(t *testing.T, c *interruptCase)
	}{
		{"the lane's own read", func(t *testing.T, c *interruptCase) {
			entered, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { close(release) })
			go func() {
				_, _ = worker.ObserveBounded(context.Background(), laneInterruptReadKey(c.f.dir, c.f.id), func(context.Context) (runtime.TurnHistory, error) {
					close(entered)
					<-release
					return runtime.TurnHistory{}, nil
				})
			}()
			<-entered
		}},
		{"a read inside the worker", func(_ *testing.T, c *interruptCase) {
			c.fresh = func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{}, fmt.Errorf("turns of session gc-1: %w: %w", worker.ErrObservationOutstanding, runtime.ErrRuntimeUnavailable)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newInterruptCase(t, laneCheckedTurnCaps)
			tc.hold(t, c)
			op := c.propose(t, "click-1", "", "")
			c.pass(t)
			got := c.f.get(t, c.st, op.ID)
			if got.Status != opjournal.StatusAccepted || got.MaybeDelivered != 0 || got.Failures != 0 || strings.HasPrefix(got.LastError, "unavailable") {
				t.Fatalf("op %s maybe_delivered %d failures %d last error %q, want a transient release held back, nothing sent", got.Status, got.MaybeDelivered, got.Failures, got.LastError)
			}
			c.f.clk.Advance(2 * time.Minute)
			c.pass(t)
			assertOutcome(t, c.f.get(t, c.st, op.ID), opjournal.ArmFailed, opjournal.ReasonExpiredBeforeStart, "", "")
			if n := len(c.requests()); n != 0 {
				t.Fatalf("%d requests, want none", n)
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

// The interrupt's InterruptRef is the conversation's interrupt from before
// the effect: an effect that may have run leaves it, so a retry of its
// session-scoped op stays on its turn, and one that sent nothing takes it
// back. A turn not seen to end is recorded on the op too (Result.Turn), from
// which the turn observer credits it.
func TestLaneInterruptKeepsItsRefUnlessItSentNothing(t *testing.T) {
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
		{"unavailable, nothing sent", func(worker.InterruptRequest) (worker.InterruptResult, error) {
			return worker.InterruptResult{}, runtime.ErrRuntimeUnavailable
		}, false},
		{"sent, outcome unknown: a protocol dispatch went unanswered", func(req worker.InterruptRequest) (worker.InterruptResult, error) {
			return worker.InterruptResult{}, fmt.Errorf("interrupting turn %q of gc-1: %w: %w", req.Turn,
				session.ErrInterruptOutcomeUnknown, fmt.Errorf("%w: asking the owner of gc-1: i/o timeout", runtime.ErrRuntimeUnavailable))
		}, true},
		{"sent, outcome unknown: ctx ended, no restart", func(worker.InterruptRequest) (worker.InterruptResult, error) {
			return worker.InterruptResult{}, fmt.Errorf("stopping the turn of gc-1: the caller's context ended, not restarting it: %w: %w: %w",
				session.ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, context.Canceled)
		}, true},
		{"sent, outcome unknown: send failed after the check", func(worker.InterruptRequest) (worker.InterruptResult, error) {
			return worker.InterruptResult{}, fmt.Errorf("stopping the turn of gc-1: %w: %w: %w",
				session.ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, errors.New("tmux: broken pipe"))
		}, true},
		{"sent, the restart stopped the runtime and could not start it", func(worker.InterruptRequest) (worker.InterruptResult, error) {
			return worker.InterruptResult{}, errLaneRestartNotStarted
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
				t.Fatalf("interrupt after the effect = %+v, want %+v kept for a retry", after, want)
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

// requestTurns lists the turns reqs named.
func requestTurns(reqs []worker.InterruptRequest) []string {
	var turns []string
	for _, r := range reqs {
		turns = append(turns, r.Turn)
	}
	return turns
}

// A retry of a session-scoped op whose first attempt may have reached the
// runtime keeps the turn that attempt aimed at. While the op waits out its
// backoff, t1 ends and content starts t2; the retry still names t1, so the
// runtime refuses it, the op closes skipped{stale}, and t2 is never
// interrupted. The ref stays the first attempt's, which only aims the
// retry: if that attempt ended t1, nothing credits t1's end to the op, as
// an op closed skipped{stale} did not run (ga-6plx6a).
func TestLaneInterruptSessionScopedRetryKeepsItsTurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first error // the first attempt's answer: it may have reached the runtime
	}{
		{"send failed after the check", fmt.Errorf("stopping the turn of gc-1: %w: %w: %w",
			session.ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, errors.New("tmux: broken pipe"))},
		{"protocol dispatch unanswered", fmt.Errorf("interrupting turn %q of gc-1: %w: %w", "t1",
			session.ErrInterruptOutcomeUnknown, fmt.Errorf("%w: asking the owner of gc-1: i/o timeout", runtime.ErrRuntimeUnavailable))},
		{"unclassified error", errors.New("boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newInterruptCase(t, laneAtomicTurnCaps)
			current, attempts := "t1", 0
			c.read = func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{Current: runtime.TurnState{TurnID: current}}, nil
			}
			c.answer = func(req worker.InterruptRequest) (worker.InterruptResult, error) {
				if attempts++; attempts == 1 {
					return worker.InterruptResult{}, tc.first
				}
				if req.Turn != current {
					return worker.InterruptResult{}, &runtime.TurnNotCurrentError{Session: c.f.name, TurnID: req.Turn, Current: runtime.TurnState{TurnID: current}}
				}
				return worker.InterruptResult{Unfenced: true}, nil
			}
			op := c.propose(t, "click-1", "", "")
			c.pass(t)
			want := opjournal.InterruptRef{Op: op.ID, Turn: "t1"}
			got := c.f.get(t, c.st, op.ID)
			if got.Status != opjournal.StatusAccepted || got.MaybeDelivered == 0 || c.ints.current(c.f.id) != want {
				t.Fatalf("after the first attempt op %s maybe_delivered %d ref %+v, want released maybe delivered with ref %+v",
					got.Status, got.MaybeDelivered, c.ints.current(c.f.id), want)
			}
			current = "t2"
			c.f.clk.Advance(opjournal.ExecutorErrorBackoff + time.Second)
			c.pass(t)
			assertOutcome(t, c.f.get(t, c.st, op.ID), opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
			if turns := requestTurns(c.requests()); !slices.Equal(turns, []string{"t1", "t1"}) {
				t.Fatalf("requests named %q, want t1 twice: the retry is never re-aimed at t2", turns)
			}
			if after := c.ints.current(c.f.id); after != want {
				t.Fatalf("ref after the retry = %+v, want the first attempt's %+v kept", after, want)
			}
		})
	}
}

// A retry of a session-scoped op whose first attempt may have reached the
// runtime, with the record lost (a controller restart), cannot tell the turn
// that attempt aimed at from the next one: it closes the op
// failed{ambiguous_delivery} and sends nothing, rather than interrupt
// whatever a fresh read finds.
func TestLaneInterruptSessionScopedRetryWithoutItsRefIsAmbiguous(t *testing.T) {
	c := newInterruptCase(t, laneCheckedTurnCaps)
	current := "t1"
	c.read = func(context.Context) (runtime.TurnHistory, error) {
		return runtime.TurnHistory{Current: runtime.TurnState{TurnID: current}}, nil
	}
	c.answer = func(worker.InterruptRequest) (worker.InterruptResult, error) {
		return worker.InterruptResult{}, errors.New("boom")
	}
	op := c.propose(t, "click-1", "", "")
	c.pass(t)
	if got := c.f.get(t, c.st, op.ID); got.Status != opjournal.StatusAccepted || got.MaybeDelivered == 0 {
		t.Fatalf("after the first attempt op %s maybe_delivered %d, want released maybe delivered", got.Status, got.MaybeDelivered)
	}
	c.ints = newLaneInterrupts()
	current = "t2"
	c.f.clk.Advance(opjournal.ExecutorErrorBackoff + time.Second)
	c.pass(t)
	assertOutcome(t, c.f.get(t, c.st, op.ID), opjournal.ArmFailed, opjournal.ReasonAmbiguousDelivery, "", "")
	if turns := requestTurns(c.requests()); !slices.Equal(turns, []string{"t1"}) {
		t.Fatalf("requests named %q, want only the first attempt's t1", turns)
	}
}

// A retry of a session-scoped op whose first attempt may have reached the
// runtime, on a session whose turns carry no turn-scoped interrupt or no
// identity, has no turn to keep: its first attempt named none. While it
// waited out its backoff, content may have started the next turn, which a
// second session-scoped interrupt would stop, so the retry closes the op
// failed{ambiguous_delivery} and sends nothing.
func TestLaneInterruptSessionScopedRetryWithoutATurnIsAmbiguous(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps worker.TurnCapabilities
		read func(context.Context) (runtime.TurnHistory, error)
	}{
		{"turns carry no turn-scoped interrupt", worker.TurnCapabilities{}, nil},
		{"turns carry no identity", laneCheckedTurnCaps, func(context.Context) (runtime.TurnHistory, error) {
			return runtime.TurnHistory{}, fmt.Errorf("transcript records the open turn without an id: %w", runtime.ErrTurnIdentityUnsupported)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newInterruptCase(t, tc.caps)
			if tc.read != nil {
				c.read = tc.read
			}
			c.answer = func(worker.InterruptRequest) (worker.InterruptResult, error) {
				return worker.InterruptResult{}, fmt.Errorf("interrupting session: %w", runtime.ErrNudgeDeliveryUnconfirmed)
			}
			op := c.propose(t, "click-1", "", "")
			c.pass(t)
			if got := c.f.get(t, c.st, op.ID); got.Status != opjournal.StatusAccepted || got.MaybeDelivered == 0 {
				t.Fatalf("after the first attempt op %s maybe_delivered %d, want released maybe delivered", got.Status, got.MaybeDelivered)
			}
			c.answer = nil
			c.f.clk.Advance(opjournal.ExecutorErrorBackoff + time.Second)
			c.pass(t)
			assertOutcome(t, c.f.get(t, c.st, op.ID), opjournal.ArmFailed, opjournal.ReasonAmbiguousDelivery, "", "")
			if turns := requestTurns(c.requests()); !slices.Equal(turns, []string{""}) {
				t.Fatalf("requests named %q, want only the first attempt's session-scoped one", turns)
			}
		})
	}
}

// An interrupt released with nothing sent, and not taken again before its
// start_by, closes by why it was released: provider_unavailable only when
// the runtime could not answer; expired_before_start when it was held back
// on this side of the runtime, by the session mutation lock or the
// restart's runtime lease held by another, by a read refused because an
// earlier one of its key is still out, or by the effect's context ending
// before it could be sent.
func TestLaneInterruptReleaseAtStartByNamesItsCause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		cancel bool // the effect's context ends during the call
		want   string
	}{
		{"the runtime could not answer", fmt.Errorf("session gc-1: fence read: %w", runtime.ErrRuntimeUnavailable), false, opjournal.ReasonProviderUnavailable},
		{"the session mutation lock held past the deadline", errLaneInterruptLockBusy, false, opjournal.ReasonExpiredBeforeStart},
		{"the restart's runtime lease held", errLaneInterruptLeaseBusy, false, opjournal.ReasonExpiredBeforeStart},
		{"the effect's context ended before the dispatch", fmt.Errorf("interrupting turn %q of gc-1: nothing sent: %w: %w", "t1", runtime.ErrRuntimeUnavailable, context.Canceled), true, opjournal.ReasonExpiredBeforeStart},
		{"an earlier check's read still out", errLaneInterruptReadOutstanding, false, opjournal.ReasonExpiredBeforeStart},
		{"the row would not load under the lock, transcript source", errLaneTranscriptRowUnread, false, opjournal.ReasonExpiredBeforeStart},
		{"the row would not load under the lock, protocol source", errLaneProtocolRowUnread, false, opjournal.ReasonExpiredBeforeStart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newInterruptCase(t, laneCheckedTurnCaps)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.answer = func(worker.InterruptRequest) (worker.InterruptResult, error) {
				if tc.cancel {
					cancel()
				}
				return worker.InterruptResult{}, tc.err
			}
			op := c.propose(t, "stop:1:t1", "t1", "")
			c.passWith(ctx, t)
			got := c.f.get(t, c.st, op.ID)
			if got.Status != opjournal.StatusAccepted || got.MaybeDelivered != 0 || got.Failures != 0 {
				t.Fatalf("op %s maybe_delivered %d failures %d, want a transient release, nothing sent", got.Status, got.MaybeDelivered, got.Failures)
			}
			unavailable := tc.want == opjournal.ReasonProviderUnavailable
			if marked := strings.HasPrefix(got.LastError, "unavailable"); marked != unavailable {
				t.Fatalf("op released with last error %q, want it marked unavailable %v", got.LastError, unavailable)
			}
			c.f.clk.Advance(2 * time.Minute)
			c.pass(t)
			assertOutcome(t, c.f.get(t, c.st, op.ID), opjournal.ArmFailed, tc.want, "", "")
			if n := len(c.requests()); n != 1 {
				t.Fatalf("%d requests, want the one released", n)
			}
		})
	}
}

// A restart that stopped the runtime and could not start it again keeps its
// cause on the op: the turn ended with the runtime, so the op is released
// maybe delivered, the start's error its last error, and its retry, which
// finds the turn gone, sends nothing and closes skipped{stale} with that
// last error still on the row.
func TestLaneInterruptFailedRestartKeepsItsCause(t *testing.T) {
	c := newInterruptCase(t, laneCheckedTurnCaps)
	c.answer = func(worker.InterruptRequest) (worker.InterruptResult, error) {
		return worker.InterruptResult{}, errLaneRestartNotStarted
	}
	op := c.propose(t, "stop:1:t1", "t1", opjournal.OnFailureRestart)
	c.pass(t)
	got := c.f.get(t, c.st, op.ID)
	if got.Status != opjournal.StatusAccepted || got.MaybeDelivered == 0 || !strings.HasPrefix(got.LastError, "unknown") || !strings.Contains(got.LastError, "cannot start") {
		t.Fatalf("op %s maybe_delivered %d last error %q, want released maybe delivered with the start's error", got.Status, got.MaybeDelivered, got.LastError)
	}
	c.answer = func(req worker.InterruptRequest) (worker.InterruptResult, error) {
		return worker.InterruptResult{}, &runtime.TurnNotCurrentError{Session: c.f.name, TurnID: req.Turn}
	}
	c.f.clk.Advance(opjournal.ExecutorErrorBackoff + time.Second)
	c.pass(t)
	got = c.f.get(t, c.st, op.ID)
	assertOutcome(t, got, opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
	if !strings.Contains(got.LastError, "cannot start") {
		t.Fatalf("closed op's last error %q, want the failed start's cause kept", got.LastError)
	}
}

// Only a ref naming an op and a turn is recorded; a newer ref replaces it,
// an executor that takes its ref back restores the one it replaced, and a
// nil record records nothing.
func TestLaneInterruptsRecord(t *testing.T) {
	r := newLaneInterrupts()
	r.record("c", opjournal.InterruptRef{Op: "op-1"})
	if got := r.current("c"); got != (opjournal.InterruptRef{}) {
		t.Fatalf("a ref without a turn was recorded: %+v", got)
	}
	first, second := opjournal.InterruptRef{Op: "op-1", Turn: "t1"}, opjournal.InterruptRef{Op: "op-2", Turn: "t2"}
	r.record("c", first)
	prev := r.record("c", second)
	if got := r.current("c"); prev != first || got != second {
		t.Fatalf("record returned %+v as the replaced ref and left %+v, want %+v replaced by %+v", prev, got, first, second)
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
	none.restore("c", first, second)
	if got := none.current("c"); got != (opjournal.InterruptRef{}) {
		t.Fatalf("a nil record answered %+v", got)
	}
}

// The lane and the API's stop write an unfenced interrupt into one op table,
// the lane with opjournal's token and the API with the session layer's: the
// two spell it the same.
func TestLaneUnfencedInterruptTokenIsTheSessionLayers(t *testing.T) {
	if opjournal.DegradationUnfencedInterrupt != session.DegradationUnfencedInterrupt {
		t.Fatalf("opjournal.DegradationUnfencedInterrupt = %q, session.DegradationUnfencedInterrupt = %q; want one token",
			opjournal.DegradationUnfencedInterrupt, session.DegradationUnfencedInterrupt)
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
	maybeSent := fmt.Errorf("interrupting turn %q of s: %w: %w", "t1", session.ErrInterruptOutcomeUnknown,
		fmt.Errorf("%w: asking the owner of s: i/o timeout", runtime.ErrRuntimeUnavailable))
	lostAfterInput := fmt.Errorf("%w after input was applied: %w", runtime.ErrNudgeDeliveryUnconfirmed,
		fmt.Errorf("tmux server lost: %w", runtime.ErrRuntimeUnavailable))
	rowClosed := fmt.Errorf("turns of session gc-1: %w: %w", runtime.ErrSessionNotFound,
		fmt.Errorf("interrupting turn %q of gc-1: %w: %w", "t1", session.ErrStopNotSent, session.ErrSessionClosed))
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
		{"unavailable, nothing sent", worker.InterruptResult{}, runtime.ErrRuntimeUnavailable, "t1", false, opjournal.NotDelivered{Transient: true, Unavailable: true, Cause: runtime.ErrRuntimeUnavailable}},
		{"unavailable, maybe sent", worker.InterruptResult{}, maybeSent, "t1", false, opjournal.Unknown{Cause: maybeSent}},
		{"unavailable, lost after input", worker.InterruptResult{}, lostAfterInput, "", false, opjournal.Unknown{Cause: lostAfterInput}},
		{"unavailable, the session mutation lock held", worker.InterruptResult{}, errLaneInterruptLockBusy, "t1", false, opjournal.NotDelivered{Transient: true, Cause: errLaneInterruptLockBusy}},
		{"unavailable, the restart's runtime lease held", worker.InterruptResult{}, errLaneInterruptLeaseBusy, "t1", false, opjournal.NotDelivered{Transient: true, Cause: errLaneInterruptLeaseBusy}},
		{"unavailable, an earlier read still out", worker.InterruptResult{}, errLaneInterruptReadOutstanding, "t1", false, opjournal.NotDelivered{Transient: true, Cause: errLaneInterruptReadOutstanding}},
		{"restart not started", worker.InterruptResult{}, errLaneRestartNotStarted, "t1", false, opjournal.Unknown{Cause: errLaneRestartNotStarted}},
		{"row unread under the lock, transcript source", worker.InterruptResult{}, errLaneTranscriptRowUnread, "t1", false, opjournal.NotDelivered{Transient: true, Cause: errLaneTranscriptRowUnread}},
		{"row unread under the lock, protocol source", worker.InterruptResult{}, errLaneProtocolRowUnread, "t1", false, opjournal.NotDelivered{Transient: true, Cause: errLaneProtocolRowUnread}},
		{"row closed under the lock", worker.InterruptResult{}, rowClosed, "t1", false, skipped},
		{"session gone", worker.InterruptResult{}, session.ErrSessionNotFound, "", false, skipped},
		{"other", worker.InterruptResult{}, boom, "", false, opjournal.Unknown{Cause: boom}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := laneInterruptResult(context.Background(), tc.res, tc.err, tc.turn, tc.exact)
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
	// An unavailable answer that sent nothing once the effect's context
	// ended is the effect's budget, not the runtime: a transient release
	// not marked unavailable. One that may have been sent stays unknown.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	notAsked := fmt.Errorf("interrupting turn %q of s: nothing sent: %w: %w", "t1", runtime.ErrRuntimeUnavailable, context.Canceled)
	if got, want := laneInterruptResult(ended, worker.InterruptResult{}, notAsked, "t1", false), (opjournal.NotDelivered{Transient: true, Cause: notAsked}); fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", want) {
		t.Fatalf("result under an ended context = %#v, want %#v", got, want)
	}
	if got := laneInterruptResult(ended, worker.InterruptResult{}, maybeSent, "t1", false); fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", opjournal.Unknown{Cause: maybeSent}) {
		t.Fatalf("a maybe-sent answer under an ended context = %#v, want it unknown", got)
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

// beforeInterruptExec runs before ahead of its executor's Interrupt.
type beforeInterruptExec struct {
	*conversationLaneExec
	before func()
}

func (e beforeInterruptExec) Interrupt(ctx context.Context, c opjournal.InterruptCall) opjournal.ControlResult {
	e.before()
	return e.conversationLaneExec.Interrupt(ctx, c)
}

// laneStaleRunning is the fixture's fake runtime whose IsRunning reads a
// live session not running once stale is set, as the tmux state cache can,
// until the runtime is started again (a provider's own start refreshes its
// cache). Its fresh liveness read (runtime.FreshLivenessObserver) is the
// fake's own.
type laneStaleRunning struct {
	*runtime.Fake
	stale *atomic.Bool
}

func (p laneStaleRunning) IsRunning(name string) bool {
	restarted := slices.ContainsFunc(p.SnapshotCalls(), func(c runtime.Call) bool { return c.Method == "Start" })
	if p.stale.Load() && !restarted {
		return false
	}
	return p.Fake.IsRunning(name)
}

func (p laneStaleRunning) ObserveLivenessSince(name string, processNames []string, since time.Time) (runtime.Liveness, error) {
	return runtime.FullFake{Fake: p.Fake}.ObserveLivenessSince(name, processNames, since)
}

// A session-scoped op with on_failure restart on a session whose row names
// no token, end to end on the session's own handle, interrupts and restarts
// a turn that does not settle even when the runtime's cache reads the live
// session not running as the interrupt starts: the stop takes a fresh
// liveness read, so the op closes succeeded{restarted_after_failed_interrupt},
// never succeeded with nothing sent and the turn left running.
func TestLaneInterruptRestartTrustsNoStaleCache(t *testing.T) {
	f := newLaneFixture(t, noLaneToken, func(meta map[string]string) {
		meta["provider"], meta["command"] = "claude", "claude" // an idle wait after the interrupt; the restart resumes the command
	})
	f.sp.WaitForIdleErrors[f.name] = errors.New("still busy")
	st := f.laneStore(t)
	op := f.propose(t, st, opjournal.Request{
		Conversation: f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "click-1",
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
		Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1},
		Policy:   opjournal.Policy{Interrupt: opjournal.InterruptPolicy{OnFailure: opjournal.OnFailureRestart}},
	})
	var stale atomic.Bool
	deps := f.deps(st)
	deps.sp = laneStaleRunning{Fake: f.sp, stale: &stale}
	v, info := f.view(t)
	e := newConversationLaneExec(deps, info)
	f.sp.Calls = nil
	if _, err := f.lane(t, st).Pass(context.Background(), v, beforeInterruptExec{e, func() { stale.Store(true) }}); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	assertOutcome(t, f.get(t, st, op.ID), opjournal.ArmSucceeded, "", "", opjournal.DegradationRestartedAfterFailedInterrupt)
	var calls []string
	for _, c := range f.sp.SnapshotCalls() {
		switch c.Method {
		case "Interrupt", "Stop", "Start":
			calls = append(calls, c.Method)
		}
	}
	if want := []string{"Interrupt", "Stop", "Start"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v: the turn interrupted, then the runtime restarted", calls, want)
	}
}

// laneLockedLoadStore fails the first load of row id made under the session
// mutation lock, the stop's own, with onLocked's error; nil lets the load
// through. Every other read goes to the store.
type laneLockedLoadStore struct {
	laneTestStore
	id       string
	once     sync.Once
	onLocked func() error
}

func (s *laneLockedLoadStore) Get(id string) (beads.Bead, error) {
	var err error
	if id == s.id && laneSessionMutationLockHeld(id) {
		s.once.Do(func() { err = s.onLocked() })
	}
	if err != nil {
		return beads.Bead{}, err
	}
	return s.laneTestStore.Get(id)
}

// laneSessionMutationLockHeld reports whether session id's mutation lock is
// held now, without waiting for it.
func laneSessionMutationLockHeld(id string) bool {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return errors.Is(session.WithSessionMutationLockContext(ctx, id, func() error { return nil }), session.ErrSessionMutationBusy)
}

// A session-scoped op whose stop, end to end on the session's own handle,
// cannot load the session row under the session mutation lock sent nothing,
// so it is released, never marked maybe delivered, and its retry interrupts
// the session and closes succeeded; one whose row was closed by then closes
// skipped{stale}. Neither closes failed{ambiguous_delivery} with nothing
// sent.
func TestLaneInterruptSessionScopedRowLoadUnderTheLock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		closed bool
	}{
		{"unreadable", false},
		{"closed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &laneLockedLoadStore{laneTestStore: newLaneTestStore()}
			f := newLaneFixtureOver(t, store)
			store.id = f.id
			store.onLocked = func() error {
				if tc.closed {
					return store.Close(f.id)
				}
				return errors.New("store: connection reset by peer")
			}
			st := f.laneStore(t)
			op := f.propose(t, st, opjournal.Request{
				Conversation: f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "click-1",
				Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
				Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1},
			})
			interrupts := func() int {
				n := 0
				for _, c := range f.sp.SnapshotCalls() {
					if c.Method == "Interrupt" || c.Method == "SendKeys" {
						n++
					}
				}
				return n
			}
			f.sp.Calls = nil
			if _, err := f.pass(context.Background(), t, f.lane(t, st), st, nil); err != nil {
				t.Fatalf("Pass: %v", err)
			}
			got := f.get(t, st, op.ID)
			if tc.closed {
				assertOutcome(t, got, opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
				if n := interrupts(); n != 0 {
					t.Fatalf("sent %d interrupts into a closed session, want none", n)
				}
				return
			}
			if got.Status != opjournal.StatusAccepted || got.MaybeDelivered != 0 || got.Failures != 0 || interrupts() != 0 {
				t.Fatalf("op %s maybe_delivered %d failures %d after %d interrupts, want a transient release, nothing sent",
					got.Status, got.MaybeDelivered, got.Failures, interrupts())
			}
			f.clk.Advance(opjournal.ExecutorErrorBackoff + time.Second)
			if _, err := f.pass(context.Background(), t, f.lane(t, st), st, nil); err != nil {
				t.Fatalf("Pass: %v", err)
			}
			assertOutcome(t, f.get(t, st, op.ID), opjournal.ArmSucceeded, "", "", opjournal.DegradationUnfencedInterrupt)
			if n := interrupts(); n == 0 {
				t.Fatal("the retry sent no interrupt")
			}
		})
	}
}

// A turn-scoped op whose protocol turn interrupt, end to end on the
// session's own handle over an atomic fake runtime (ACP), cannot load the
// session row under the session mutation lock sent nothing and asked the
// runtime nothing: it is released, never marked unavailable or maybe
// delivered, so start_by closes it expired_before_start, never
// provider_unavailable; one whose row was closed by then closes
// skipped{stale}.
func TestLaneInterruptTurnScopedRowLoadUnderTheLock(t *testing.T) {
	atomic := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact}
	for _, tc := range []struct {
		name   string
		closed bool
	}{
		{"unreadable", false},
		{"closed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &laneLockedLoadStore{laneTestStore: newLaneTestStore()}
			f := newTurnLaneFixtureOver(t, store, runtime.ProfileACP, func(meta map[string]string) { meta["transport"] = "acp" })
			store.id = f.id
			store.onLocked = func() error {
				if tc.closed {
					return store.Close(f.id)
				}
				return errors.New("store: connection reset by peer")
			}
			f.laneFixture.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{f.name: {atomic}}
			f.laneFixture.sp.TurnScripts = map[string][]string{f.name: {"t1"}}
			st := f.laneStore(t)
			op := f.propose(t, st, opjournal.Request{
				Conversation: f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "stop:1:t1",
				Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
				Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1, Turn: "t1"},
			})
			pass := func() {
				t.Helper()
				e, v := f.exec(t, st)
				e.turns = func() (worker.TurnHandle, error) {
					return laneStubTurns{
						caps: worker.TurnCapabilities{TurnCapabilities: atomic, Source: worker.TurnSourceProtocol, Interrupt: runtime.TurnInterruptKeystroke},
						read: func(context.Context) (runtime.TurnHistory, error) {
							return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}}, nil
						},
					}, nil
				}
				if _, err := f.lane(t, st).Pass(context.Background(), v, e); err != nil {
					t.Fatalf("Pass: %v", err)
				}
			}
			f.laneFixture.sp.Calls = nil
			pass()
			if slices.ContainsFunc(f.laneFixture.sp.SnapshotCalls(), func(c runtime.Call) bool { return c.Method == "InterruptTurn" }) {
				t.Fatalf("calls %+v, want no turn interrupt", f.laneFixture.sp.SnapshotCalls())
			}
			got := f.get(t, st, op.ID)
			if tc.closed {
				assertOutcome(t, got, opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
				return
			}
			if got.Status != opjournal.StatusAccepted || got.MaybeDelivered != 0 || got.Failures != 0 || strings.HasPrefix(got.LastError, "unavailable") {
				t.Fatalf("op %s maybe_delivered %d failures %d last error %q, want a transient release not marked unavailable, nothing sent",
					got.Status, got.MaybeDelivered, got.Failures, got.LastError)
			}
			f.clk.Advance(2 * time.Minute)
			pass()
			assertOutcome(t, f.get(t, st, op.ID), opjournal.ArmFailed, opjournal.ReasonExpiredBeforeStart, "", "")
		})
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

// on_failure restart end to end on an atomic fake runtime (ACP), which reads
// its turns by protocol and interrupts by keystroke: an op scoped to t1,
// whose end the runtime does not see, has its turn interrupted by protocol
// and then the runtime stopped and started in its place, so it closes
// succeeded, marked restarted_after_failed_interrupt, never
// failed{unsupported} with the turn left running.
func TestLaneInterruptACPRestartOnFailure(t *testing.T) {
	atomic := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact}
	f := newTurnLaneFixture(t, runtime.ProfileACP, func(meta map[string]string) {
		meta["transport"], meta["command"] = "acp", "claude" // the restart's start resumes the command
	})
	f.laneFixture.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{f.name: {atomic}}
	f.laneFixture.sp.TurnScripts = map[string][]string{f.name: {"t1"}}
	st := f.laneStore(t)
	op := f.propose(t, st, opjournal.Request{
		Conversation: f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "stop:1:t1",
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
		Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1, Turn: "t1"},
		Policy:   opjournal.Policy{Interrupt: opjournal.InterruptPolicy{OnFailure: opjournal.OnFailureRestart}},
	})
	e, v := f.exec(t, st)
	e.turns = func() (worker.TurnHandle, error) {
		return laneStubTurns{
			caps: worker.TurnCapabilities{TurnCapabilities: atomic, Source: worker.TurnSourceProtocol, Interrupt: runtime.TurnInterruptKeystroke},
			read: func(context.Context) (runtime.TurnHistory, error) {
				return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}}, nil
			},
		}, nil
	}
	f.laneFixture.sp.Calls = nil
	if _, err := f.lane(t, st).Pass(context.Background(), v, e); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	assertOutcome(t, f.get(t, st, op.ID), opjournal.ArmSucceeded, "", "t1", opjournal.DegradationRestartedAfterFailedInterrupt)
	var calls []string
	for _, c := range f.laneFixture.sp.SnapshotCalls() {
		switch c.Method {
		case "InterruptTurn":
			calls = append(calls, c.Method+"("+c.Key+"):"+c.Value)
		case "Stop", "Start":
			calls = append(calls, c.Method)
		}
	}
	if want := []string{"InterruptTurn(t1):sent", "Stop", "Start"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v: the turn interrupted, then the runtime restarted", calls, want)
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

// The retry end to end on an atomic fake runtime (ACP): a session-scoped
// op's first attempt names t1, the turn a fresh read finds, and its
// dispatch goes unanswered (an RPC timeout after the request left), so it
// may have reached the runtime: the op is released maybe delivered, its ref
// kept. By the retry t1 has ended and t2 runs; the retry still names t1,
// which the runtime refuses, so the op closes skipped{stale} and t2 is never
// interrupted.
func TestLaneInterruptAtomicFakeRetryNeverInterruptsTheNextTurn(t *testing.T) {
	atomic := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact}
	f := newTurnLaneFixture(t, runtime.ProfileACP, func(meta map[string]string) { meta["transport"] = "acp" })
	f.laneFixture.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{f.name: {atomic}}
	f.laneFixture.sp.TurnScripts = map[string][]string{f.name: {"t1"}}
	f.laneFixture.sp.InterruptTurnErrors = map[string]error{f.name: fmt.Errorf("%w: asking the owner of %q: i/o timeout", runtime.ErrRuntimeUnavailable, f.name)}
	st := f.laneStore(t)
	ints := newLaneInterrupts()
	op := f.propose(t, st, opjournal.Request{
		Conversation: f.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "click-1",
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
		Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1},
	})
	current := "t1"
	pass := func() {
		t.Helper()
		e, v := f.exec(t, st)
		e.deps.interrupts = ints
		e.turns = func() (worker.TurnHandle, error) {
			return laneStubTurns{
				caps: worker.TurnCapabilities{TurnCapabilities: atomic, Source: worker.TurnSourceProtocol, Interrupt: runtime.TurnInterruptKeystroke},
				read: func(context.Context) (runtime.TurnHistory, error) {
					return runtime.TurnHistory{Current: runtime.TurnState{TurnID: current}}, nil
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
	pass()
	want := opjournal.InterruptRef{Op: op.ID, Turn: "t1"}
	if got := f.get(t, st, op.ID); got.Status != opjournal.StatusAccepted || got.MaybeDelivered == 0 || ints.current(f.id) != want {
		t.Fatalf("after an unanswered dispatch op %s maybe_delivered %d ref %+v, want released maybe delivered with ref %+v",
			got.Status, got.MaybeDelivered, ints.current(f.id), want)
	}
	f.laneFixture.sp.InterruptTurnErrors = nil
	f.laneFixture.sp.TurnScripts = map[string][]string{f.name: {"t2"}}
	current = "t2"
	f.clk.Advance(opjournal.ExecutorErrorBackoff + time.Second)
	pass()
	assertOutcome(t, f.get(t, st, op.ID), opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
	if got := interrupts(); !slices.Equal(got, []string{"t1:error", "t1:not_current"}) {
		t.Fatalf("interrupts %v, want t1's twice, the retry refused not current", got)
	}
}
