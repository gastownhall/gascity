package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// --- fenceTurn: the exhaustive property -------------------------------------

const fenceSess = "s-fence"

// fenceScript scripts one fenceTurn run against a TurnFenceProvider double.
type fenceScript struct {
	sighting string // skipped, renamed, failed, none, active
	alive    string // up, down, error
	observe  string // none, t2, not_found, error
	fence    string // t1, none, t2, bare, not_found, gone_after_attempt, error
	dispatch string // ok, error
	fenced   bool
	wait     string // none, t3, not_ended, ctx, not_found
}

func (c fenceScript) String() string {
	return fmt.Sprintf("sighting=%s alive=%s observe=%s fence=%s dispatch=%s fenced=%t wait=%s",
		c.sighting, c.alive, c.observe, c.fence, c.dispatch, c.fenced, c.wait)
}

func (c fenceScript) seen() turnSighting {
	switch c.sighting {
	case "renamed":
		return turnSighting{taken: true, name: "s-other", state: runtime.TurnState{TurnID: "t1"}}
	case "failed":
		return turnSighting{taken: true, name: fenceSess, err: runtime.ErrRuntimeUnavailable}
	case "none":
		return turnSighting{taken: true, name: fenceSess}
	case "active":
		return turnSighting{taken: true, name: fenceSess, state: runtime.TurnState{TurnID: "t1"}}
	}
	return turnSighting{}
}

// fenceCall is one call the double saw.
type fenceCall struct {
	method   string
	turn     string
	deadline time.Duration // remaining ctx deadline; 0 when none
	timeout  time.Duration // WaitTurnEnded's
	value    string        // InterruptTurn: sent, not_current or error
}

// scriptedTurnFence is a TurnFenceProvider answering from a fenceScript.
type scriptedTurnFence struct {
	c     fenceScript
	calls []fenceCall
}

func remaining(ctx context.Context) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		return time.Until(d)
	}
	return 0
}

func (s *scriptedTurnFence) ObserveTurn(ctx context.Context, _ string) (runtime.TurnState, error) {
	s.calls = append(s.calls, fenceCall{method: "ObserveTurn", deadline: remaining(ctx)})
	switch s.c.observe {
	case "t2":
		return runtime.TurnState{TurnID: "t2"}, nil
	case "not_found":
		return runtime.TurnState{}, fmt.Errorf("%w: gone", runtime.ErrSessionNotFound)
	case "error":
		return runtime.TurnState{}, runtime.ErrRuntimeUnavailable
	}
	return runtime.TurnState{}, nil
}

func (s *scriptedTurnFence) InterruptTurn(ctx context.Context, name, turnID string) (runtime.TurnInterrupt, error) {
	call := fenceCall{method: "InterruptTurn", turn: turnID, deadline: remaining(ctx), value: "error"}
	defer func() { s.calls = append(s.calls, call) }()
	if turnID == "" {
		return runtime.TurnInterrupt{}, runtime.ErrTurnUnnamed
	}
	switch s.c.fence {
	case "none":
		call.value = "not_current"
		return runtime.TurnInterrupt{}, &runtime.TurnNotCurrentError{Session: name, TurnID: turnID}
	case "t2":
		call.value = "not_current"
		return runtime.TurnInterrupt{}, &runtime.TurnNotCurrentError{Session: name, TurnID: turnID, Current: runtime.TurnState{TurnID: "t2"}}
	case "bare":
		call.value = "not_current"
		return runtime.TurnInterrupt{}, fmt.Errorf("bridge: %w", runtime.ErrTurnNotCurrent)
	case "not_found":
		return runtime.TurnInterrupt{}, fmt.Errorf("%w: gone", runtime.ErrSessionNotFound)
	case "gone_after_attempt":
		// A dispatch attempt failed, maybe after reaching the runtime, and
		// the re-read before the retry found the session gone (t3bridge's
		// refence): both sentinels at once.
		return runtime.TurnInterrupt{}, fmt.Errorf("%w: dispatch failed: %w: gone", runtime.ErrRuntimeUnavailable, runtime.ErrSessionNotFound)
	case "error":
		return runtime.TurnInterrupt{}, runtime.ErrRuntimeUnavailable
	}
	if turnID != "t1" {
		call.value = "not_current"
		return runtime.TurnInterrupt{}, &runtime.TurnNotCurrentError{Session: name, TurnID: turnID, Current: runtime.TurnState{TurnID: "t1"}}
	}
	if s.c.dispatch == "error" {
		return runtime.TurnInterrupt{}, errors.New("dispatch: connection reset")
	}
	call.value = "sent"
	return runtime.TurnInterrupt{Fenced: s.c.fenced}, nil
}

func (s *scriptedTurnFence) WaitTurnEnded(ctx context.Context, _, turnID string, timeout time.Duration) (runtime.TurnState, error) {
	s.calls = append(s.calls, fenceCall{method: "WaitTurnEnded", turn: turnID, deadline: remaining(ctx), timeout: timeout})
	if turnID == "" {
		return runtime.TurnState{}, runtime.ErrTurnUnnamed
	}
	switch s.c.wait {
	case "t3":
		return runtime.TurnState{TurnID: "t3"}, nil
	case "not_ended":
		return runtime.TurnState{TurnID: turnID}, runtime.ErrTurnNotEnded
	case "ctx":
		return runtime.TurnState{}, context.DeadlineExceeded
	}
	return runtime.TurnState{}, nil // none, and not_found: a thread gone has ended its turn
}

// fenceOracle is the design's decision table (des-t3-1b §5), written out
// row by row: the outcome, whether the message is sent now, and whether an
// interrupt is sent.
func fenceOracle(c fenceScript) (outcome TurnInterruptOutcome, send, interrupt bool) {
	switch c.sighting {
	case "skipped", "renamed", "failed":
		switch c.observe {
		case "none", "not_found":
			return TurnInterruptUnavailable, true, false
		}
		return TurnInterruptUnavailable, false, false
	case "none":
		switch c.observe {
		case "none", "not_found":
			return TurnInterruptNoTurn, true, false
		case "t2":
			return TurnInterruptStale, false, false
		}
		return TurnInterruptUnavailable, false, false
	}
	switch c.alive {
	case "error":
		return TurnInterruptUnavailable, false, false
	case "down":
		return TurnInterruptStale, true, false
	}
	switch c.fence {
	case "none", "not_found":
		return TurnInterruptStale, true, false
	case "t2", "bare":
		return TurnInterruptStale, false, false
	case "error", "gone_after_attempt": // the interrupt may have reached the runtime
		return TurnInterruptUnavailable, false, false
	}
	if c.dispatch == "error" {
		return TurnInterruptUnavailable, false, false
	}
	switch c.wait {
	case "none", "not_found":
		return TurnInterruptSettled, true, true
	case "t3":
		return TurnInterruptSettled, false, true
	}
	return TurnInterruptUnsettled, false, true
}

// TestFenceTurnHoldsTheDeliverNowInvariants runs fenceTurn over its whole
// input space and checks the decision table and the invariants: at most one
// interrupt, sent only on a usable active sighting the fence read confirmed
// with the agent up, never naming an empty turn; a wait only after a sent
// interrupt, bounded by the settle timeout; every read bounded; the report's
// outcome, degradation and turn exactly as specified.
func TestFenceTurnHoldsTheDeliverNowInvariants(t *testing.T) {
	cases := 0
	for _, sighting := range []string{"skipped", "renamed", "failed", "none", "active"} {
		for _, alive := range []string{"up", "down", "error"} {
			for _, observe := range []string{"none", "t2", "not_found", "error"} {
				for _, fence := range []string{"t1", "none", "t2", "bare", "not_found", "gone_after_attempt", "error"} {
					for _, dispatch := range []string{"ok", "error"} {
						for _, fenced := range []bool{true, false} {
							for _, wait := range []string{"none", "t3", "not_ended", "ctx", "not_found"} {
								cases++
								checkFenceTurn(t, fenceScript{sighting, alive, observe, fence, dispatch, fenced, wait})
							}
						}
					}
				}
			}
		}
	}
	if cases != 8400 {
		t.Fatalf("enumerated %d cases, want 8400", cases)
	}
}

func checkFenceTurn(t *testing.T, c fenceScript) {
	t.Helper()
	tf := &scriptedTurnFence{c: c}
	aliveReads := 0
	report, now := fenceTurn(context.Background(), tf, fenceSess, c.seen(), func() (bool, error) {
		aliveReads++
		switch c.alive {
		case "down":
			return false, nil
		case "error":
			return false, runtime.ErrRuntimeUnavailable
		}
		return true, nil
	})
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("%s: %s (report %+v, now %t, calls %+v)", c, fmt.Sprintf(format, args...), report, now, tf.calls)
	}
	wantOutcome, wantSend, wantInterrupt := fenceOracle(c)
	if report.Outcome != wantOutcome || now != wantSend {
		fail("got (%s, now %t), want (%s, now %t)", report.Outcome, now, wantOutcome, wantSend)
	}
	active := c.sighting == "active"
	sent, interrupts, waits := 0, 0, 0
	interruptReturnedNil := false
	for i, call := range tf.calls {
		if call.method != "WaitTurnEnded" && (call.deadline <= 0 || call.deadline > turnFenceReadTimeout) {
			fail("%s ran with deadline %v, want within (0, %v]", call.method, call.deadline, turnFenceReadTimeout)
		}
		switch call.method {
		case "InterruptTurn":
			interrupts++
			if call.turn != "t1" {
				fail("InterruptTurn named %q, want the sighted t1", call.turn)
			}
			if call.value == "sent" {
				sent++
				interruptReturnedNil = true
			}
		case "WaitTurnEnded":
			waits++
			if i == 0 || tf.calls[i-1].method != "InterruptTurn" || tf.calls[i-1].value != "sent" {
				fail("WaitTurnEnded not right after a sent interrupt")
			}
			if call.turn != "t1" || call.timeout != turnSettleTimeout {
				fail("WaitTurnEnded(%q, %v), want (t1, %v)", call.turn, call.timeout, turnSettleTimeout)
			}
		}
	}
	// Invariant 1: one interrupt, fenced twice.
	if interrupts > 1 || sent > 1 {
		fail("%d InterruptTurn calls, %d sent; want at most one", interrupts, sent)
	}
	if interrupts == 1 && (!active || c.alive != "up") {
		fail("InterruptTurn without a usable active sighting and a live agent")
	}
	if (sent == 1) != wantInterrupt {
		fail("sent %d interrupts, want interrupt %t", sent, wantInterrupt)
	}
	if aliveReads > 0 && !active {
		fail("liveness read on an unusable or idle sighting")
	}
	if waits > 1 || (waits == 1) != (sent == 1) {
		fail("%d waits for %d sent interrupts", waits, sent)
	}
	// Invariant 3: one outcome, exactly specified.
	settled := report.Outcome == TurnInterruptSettled || report.Outcome == TurnInterruptUnsettled
	if settled != interruptReturnedNil {
		fail("outcome %s, InterruptTurn returned nil %t", report.Outcome, interruptReturnedNil)
	}
	if wantDeg := interruptReturnedNil && !c.fenced; (report.Degradation == DegradationUnfencedInterrupt) != wantDeg ||
		(report.Degradation != "" && report.Degradation != DegradationUnfencedInterrupt) {
		fail("degradation %q, want unfenced %t", report.Degradation, wantDeg)
	}
	if wantTurn := map[bool]string{true: "t1"}[active]; report.TurnID != wantTurn {
		fail("TurnID %q, want %q", report.TurnID, wantTurn)
	}
}

// --- the Manager: deliver-now through Submit -------------------------------

const fenceMsg = "msg"

// fenceFixture is a claude session on a t3bridge runtime (bare or behind a
// composite) with the submit poller stubbed.
type fenceFixture struct {
	mgr   *Manager
	f     *runtime.Fake
	info  Info
	city  string
	hints runtime.Config
	polls *atomic.Int32
}

type fenceFixtureOpts struct {
	wrap     func(*runtime.Fake) runtime.Provider // default: bare t3bridge
	store    beads.Store                          // default: a MemStore
	provider string                               // default: claude
	// noQueue gives the Manager no city path, so its follow-up queue
	// write fails; the actor still leases in the fixture's city.
	noQueue bool
}

func newFenceFixture(t *testing.T, o fenceFixtureOpts) fenceFixture {
	t.Helper()
	if o.wrap == nil {
		o.wrap = t3bridgeComposites["t3bridge"]
	}
	if o.store == nil {
		o.store = beads.NewMemStore()
	}
	if o.provider == "" {
		o.provider = "claude"
	}
	f := runtime.NewFake()
	city := t.TempDir()
	sp := o.wrap(f)
	// The create's first start leases in the city (createStarted); a noQueue
	// fixture's Manager then has none.
	mgr := newTestManager(t, o.store, sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: o.provider, WorkDir: t.TempDir(), Provider: o.provider, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if o.noQueue {
		mgr = newTestManager(t, o.store, sp, WithCityPath(""))
	}
	polls := &atomic.Int32{}
	origPoller := startSessionSubmitPoller
	startSessionSubmitPoller = func(string, string, string) error { polls.Add(1); return nil }
	t.Cleanup(func() { startSessionSubmitPoller = origPoller })
	f.TurnScripts = map[string][]string{}
	f.TurnReadErrors = map[string][]error{}
	f.InterruptTurnErrors = map[string]error{}
	if f.NudgeErrors == nil {
		f.NudgeErrors = map[string]error{}
	}
	f.Calls = nil
	return fenceFixture{mgr: mgr, f: f, info: info, city: city, hints: runtime.Config{WorkDir: info.WorkDir}, polls: polls}
}

func (x fenceFixture) script(turns ...string) { x.f.TurnScripts[x.info.SessionName] = turns }

func (x fenceFixture) submit(t *testing.T, msg string) (SubmitOutcome, error) {
	t.Helper()
	return x.mgr.Submit(context.Background(), x.actor(t, ActorOperator), x.info.ID, msg, BuildResumeCommand(x.info), x.hints, SubmitIntentInterruptNow)
}

// actor is an Actor of kind leasing in the fixture's city.
func (x fenceFixture) actor(t *testing.T, kind ActorKind) Actor {
	t.Helper()
	city, err := NewCityDir(x.city)
	if err != nil {
		t.Fatalf("NewCityDir: %v", err)
	}
	return Actor{Kind: kind, City: city}
}

func (x fenceFixture) pending(t *testing.T) []nudgequeue.Item {
	t.Helper()
	state, err := nudgequeue.LoadState(x.city)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return state.Pending
}

// fenceEffects are the calls that act on the session: IT(turn:what it did)
// for InterruptTurn, N(text) for a delivery, and any untargeted interrupt,
// keys, stop, start or relaunch.
func fenceEffects(calls []runtime.Call) string {
	var out []string
	for _, c := range calls {
		switch c.Method {
		case "InterruptTurn":
			out = append(out, "IT("+c.Key+":"+c.Value+")")
		case "Nudge", "NudgeNow":
			out = append(out, "N("+c.Message+")")
		case "Interrupt", "SendKeys", "Stop", "Start", "Relaunch":
			out = append(out, c.Method)
		}
	}
	return strings.Join(out, " ")
}

// expectFence checks one deliver-now: its effects, its report, its queue
// flags, and the queued item when queued.
func expectFence(t *testing.T, x fenceFixture, out SubmitOutcome, err error, effects string, want TurnInterruptReport, queued, deferred bool) {
	t.Helper()
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	expectLeaseReleased(t, x)
	if got := fenceEffects(x.f.Calls); got != effects {
		t.Fatalf("effects = %q, want %q; calls %v", got, effects, callMethods(x.f.Calls))
	}
	if out.Interrupt == nil || *out.Interrupt != want {
		t.Fatalf("report = %+v, want %+v", out.Interrupt, want)
	}
	if out.Queued != queued || out.Deferred != deferred {
		t.Fatalf("outcome queued %t deferred %t, want %t %t", out.Queued, out.Deferred, queued, deferred)
	}
	pending := x.pending(t)
	if !queued {
		if len(pending) != 0 {
			t.Fatalf("pending %+v, want nothing queued", pending)
		}
		return
	}
	if len(pending) != 1 || pending[0].Message != fenceMsg || pending[0].SessionID != x.info.ID {
		t.Fatalf("pending %+v, want the message queued for %s", pending, x.info.ID)
	}
}

// expectLeaseReleased checks that the deliver-now gave its runtime lease
// back: a leaked lease blocks every later start, stop or delivery of the
// runtime until the process exits.
func expectLeaseReleased(t *testing.T, x fenceFixture) {
	t.Helper()
	l, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: x.city, Name: x.info.SessionName})
	if err != nil {
		t.Fatalf("runtime lease after Submit: %v, want it released", err)
	}
	l.Release()
}

// unfencedT1 is the report of an interrupt of t1 the runtime did not fence.
func unfencedT1(o TurnInterruptOutcome) TurnInterruptReport {
	return TurnInterruptReport{Outcome: o, TurnID: "t1", Degradation: DegradationUnfencedInterrupt}
}

func TestFencedDeliverNowNoTurnDeliversWithoutInterrupt(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "N(msg)", TurnInterruptReport{Outcome: TurnInterruptNoTurn}, false, false)
}

func TestFencedDeliverNowSettledDeliversAfterTurnEnds(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	x.script("t1", "t1", "")
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "IT(t1:sent) N(msg)", unfencedT1(TurnInterruptSettled), false, false)
}

func TestFencedDeliverNowFencedRuntimeHasNoDegradation(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	x.f.FencedTurnInterrupts = true
	x.script("t1", "t1", "")
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "IT(t1:sent) N(msg)", TurnInterruptReport{Outcome: TurnInterruptSettled, TurnID: "t1"}, false, false)
}

func TestFencedDeliverNowStaleTurnQueuesWithoutInterrupt(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	x.script("t1", "t2")
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "IT(t1:not_current)", TurnInterruptReport{Outcome: TurnInterruptStale, TurnID: "t1"}, true, false)
	if n := x.polls.Load(); n != 1 {
		t.Fatalf("submit poller started %d times, want 1", n)
	}
}

func TestFencedDeliverNowEndedTurnDeliversNow(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	x.script("t1", "")
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "IT(t1:not_current) N(msg)", TurnInterruptReport{Outcome: TurnInterruptStale, TurnID: "t1"}, false, false)
}

func TestFencedDeliverNowTurnStartedAfterRequestQueues(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	x.script("", "t2")
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "", TurnInterruptReport{Outcome: TurnInterruptStale}, true, false)
}

func TestFencedDeliverNowUnsettledQueuesWithoutRestart(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	x.script("t1")
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "IT(t1:sent)", unfencedT1(TurnInterruptUnsettled), true, false)
}

func TestFencedDeliverNowSettledIntoNewTurnQueues(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	x.script("t1", "t1", "t3")
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "IT(t1:sent)", unfencedT1(TurnInterruptSettled), true, false)
}

func TestFencedDeliverNowSightingFailureNeverInterrupts(t *testing.T) {
	for _, tc := range []struct {
		turn    string
		effects string
		queued  bool
	}{{"", "N(msg)", false}, {"t1", "", true}} {
		t.Run("fence read "+tc.turn, func(t *testing.T) {
			x := newFenceFixture(t, fenceFixtureOpts{})
			x.f.TurnReadErrors[x.info.SessionName] = []error{runtime.ErrRuntimeUnavailable}
			x.script(tc.turn)
			out, err := x.submit(t, fenceMsg)
			expectFence(t, x, out, err, tc.effects, TurnInterruptReport{Outcome: TurnInterruptUnavailable}, tc.queued, false)
		})
	}
}

// A dormant row whose turn reads both failed gets the message queued
// Deferred: the follow-up poller delivers only to live sessions, so the
// caller must have the controller wake it (the API records the wake).
func TestFencedDeliverNowUnreadableDormantRowQueuesDeferred(t *testing.T) {
	for _, state := range []State{StateAsleep, StateDrained} {
		t.Run(string(state), func(t *testing.T) {
			x := newFenceFixture(t, fenceFixtureOpts{})
			if err := x.mgr.store.SetMetadataBatch(x.info.ID, map[string]string{"state": string(state)}); err != nil {
				t.Fatal(err)
			}
			x.f.TurnReadErrors[x.info.SessionName] = []error{runtime.ErrRuntimeUnavailable, runtime.ErrRuntimeUnavailable}
			x.script("t1")
			out, err := x.submit(t, fenceMsg)
			expectFence(t, x, out, err, "", TurnInterruptReport{Outcome: TurnInterruptUnavailable}, true, true)
		})
	}
}

func TestFencedDeliverNowFenceReadFailureQueues(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	x.f.TurnReadErrors[x.info.SessionName] = []error{nil, runtime.ErrRuntimeUnavailable}
	x.script("t1")
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "IT(t1:error)", TurnInterruptReport{Outcome: TurnInterruptUnavailable, TurnID: "t1"}, true, false)
	if n := countMethod(x.f.Calls, "WaitTurnEnded"); n != 0 {
		t.Fatalf("WaitTurnEnded calls = %d, want none without a sent interrupt", n)
	}
}

func TestFencedDeliverNowDispatchFailureQueues(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	x.f.InterruptTurnErrors[x.info.SessionName] = errors.New("dispatch: connection reset")
	x.script("t1")
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "IT(t1:error)", TurnInterruptReport{Outcome: TurnInterruptUnavailable, TurnID: "t1"}, true, false)
}

func TestFencedDeliverNowSendOutageQueues(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	x.f.NudgeErrors[x.info.SessionName] = fmt.Errorf("%w: no thread bound", runtime.ErrRuntimeUnavailable)
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "N(msg)", TurnInterruptReport{Outcome: TurnInterruptNoTurn}, true, false)
}

func TestFencedDeliverNowSendFailureReturnsTheError(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	rejected := errors.New("thread rejected the message")
	x.f.NudgeErrors[x.info.SessionName] = rejected
	out, err := x.submit(t, fenceMsg)
	if !errors.Is(err, rejected) {
		t.Fatalf("Submit = %v, want the send's error", err)
	}
	if out.Queued || out.Deferred || out.Interrupt == nil || out.Interrupt.Outcome != TurnInterruptNoTurn {
		t.Fatalf("outcome %+v (report %+v), want not queued with the no_turn report", out, out.Interrupt)
	}
	if pending := x.pending(t); len(pending) != 0 {
		t.Fatalf("pending %+v, want nothing queued", pending)
	}
}

func TestFencedDeliverNowOverADeadAgentQueuesDeferred(t *testing.T) {
	for _, tc := range []struct {
		turn string
		want TurnInterruptReport
	}{
		{"", TurnInterruptReport{Outcome: TurnInterruptNoTurn}},
		{"t1", TurnInterruptReport{Outcome: TurnInterruptStale, TurnID: "t1"}},
	} {
		t.Run("turn "+tc.turn, func(t *testing.T) {
			x := newFenceFixture(t, fenceFixtureOpts{})
			x.f.Zombies[x.info.SessionName] = true
			x.script(tc.turn)
			out, err := x.submit(t, fenceMsg)
			expectFence(t, x, out, err, "", tc.want, true, true)
		})
	}
}

func TestFencedDeliverNowWithNoThreadStartsTheSessionAndDelivers(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	if err := x.f.Stop(x.info.SessionName); err != nil {
		t.Fatal(err)
	}
	gone := fmt.Errorf("%w: no thread", runtime.ErrSessionNotFound)
	x.f.TurnReadErrors[x.info.SessionName] = []error{gone, gone}
	x.f.Calls = nil
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "Start N(msg)", TurnInterruptReport{Outcome: TurnInterruptNoTurn}, false, false)
}

// failingGetStore fails the first Get after it is armed.
type failingGetStore struct {
	beads.Store
	armed atomic.Bool
}

func (s *failingGetStore) Get(id string) (beads.Bead, error) {
	if s.armed.CompareAndSwap(true, false) {
		return beads.Bead{}, errors.New("store unreachable")
	}
	return s.Store.Get(id)
}

func TestFencedDeliverNowWithoutASightingNeverInterrupts(t *testing.T) {
	store := &failingGetStore{Store: beads.NewMemStore()}
	x := newFenceFixture(t, fenceFixtureOpts{store: store})
	x.script("t1")
	store.armed.Store(true)
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "", TurnInterruptReport{Outcome: TurnInterruptUnavailable}, true, false)
}

// probeBackend is a FullFake whose turn-fence and delivery calls first
// report to probe.
type probeBackend struct {
	runtime.FullFake
	probe func(method string, ctx context.Context, timeout time.Duration)
}

func (b probeBackend) ObserveTurn(ctx context.Context, name string) (runtime.TurnState, error) {
	b.probe("ObserveTurn", ctx, 0)
	return b.FullFake.ObserveTurn(ctx, name)
}

func (b probeBackend) InterruptTurn(ctx context.Context, name, turnID string) (runtime.TurnInterrupt, error) {
	b.probe("InterruptTurn", ctx, 0)
	return b.FullFake.InterruptTurn(ctx, name, turnID)
}

func (b probeBackend) WaitTurnEnded(ctx context.Context, name, turnID string, timeout time.Duration) (runtime.TurnState, error) {
	b.probe("WaitTurnEnded", ctx, timeout)
	return b.FullFake.WaitTurnEnded(ctx, name, turnID, timeout)
}

func (b probeBackend) Nudge(name string, content []runtime.ContentBlock) error {
	b.probe("Nudge", nil, 0)
	return b.FullFake.Nudge(name, content)
}

func (b probeBackend) NudgeKeyed(name string, content []runtime.ContentBlock, key string) (runtime.DeliveryReceipt, error) {
	b.probe("Nudge", nil, 0)
	return b.FullFake.NudgeKeyed(name, content, key)
}

func probed(probe func(string, context.Context, time.Duration)) func(*runtime.Fake) runtime.Provider {
	return func(f *runtime.Fake) runtime.Provider {
		return runtime.NewFakeProfile(runtime.ProfileT3Bridge, probeBackend{FullFake: runtime.FullFake{Fake: f}, probe: probe})
	}
}

func TestFencedDeliverNowSightsOnceAcrossLeaseRetry(t *testing.T) {
	var released atomic.Bool
	var observes, interruptsAfterRelease, interruptsBefore atomic.Int32
	x := newFenceFixture(t, fenceFixtureOpts{wrap: probed(func(method string, _ context.Context, _ time.Duration) {
		switch method {
		case "ObserveTurn":
			observes.Add(1)
			if released.Load() {
				t.Errorf("ObserveTurn after the lease was released: the sighting must precede the lease wait")
			}
		case "InterruptTurn":
			if released.Load() {
				interruptsAfterRelease.Add(1)
			} else {
				interruptsBefore.Add(1)
			}
		}
	})})
	x.script("t1", "t1", "")
	held, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: x.city, Name: x.info.SessionName})
	if err != nil {
		t.Fatalf("holding the runtime lease: %v", err)
	}
	t.Cleanup(SetOperatorLeaseWaitForTest(goroutineHangBudget))
	time.AfterFunc(200*time.Millisecond, func() {
		released.Store(true)
		held.Release()
	})
	out, err := x.submit(t, fenceMsg)
	expectFence(t, x, out, err, "IT(t1:sent) N(msg)", unfencedT1(TurnInterruptSettled), false, false)
	if n := observes.Load(); n != 1 {
		t.Fatalf("ObserveTurn calls = %d, want the one sighting", n)
	}
	if interruptsBefore.Load() != 0 || interruptsAfterRelease.Load() != 1 {
		t.Fatalf("InterruptTurn before/after release = %d/%d, want 0/1", interruptsBefore.Load(), interruptsAfterRelease.Load())
	}
}

func TestFencedDeliverNowRefusesAStoppingRow(t *testing.T) {
	checkFencedRefusal(t, stoppingRow(), ErrSessionStopping)
}

func TestFencedDeliverNowRefusesAKillPendingRow(t *testing.T) {
	checkFencedRefusal(t, killPendingRow(), ErrSessionKillPending)
}

// A skipped sighting leaves the capability unread, and a row the fenced path
// refuses must not have it read: no turn read, no effect, the row's own
// refusal. Behind the composite the read never reaches the fake (the
// composite answers it as unsupported), so there only the kill-pending row
// shows an unguarded read: the unfenced path does not refuse it first.
func TestFencedDeliverNowRefusesWithoutASighting(t *testing.T) {
	for _, rt := range []struct {
		name string
		wrap func(*runtime.Fake) runtime.Provider
	}{
		{"t3bridge", t3bridgeComposites["t3bridge"]},
		{"composite without capability", compositeWithoutCapability},
	} {
		for _, row := range []struct {
			name string
			meta map[string]string
			want error
		}{
			{"stopping", stoppingRow(), ErrSessionStopping},
			{"kill pending", killPendingRow(), ErrSessionKillPending},
		} {
			t.Run(rt.name+"/"+row.name, func(t *testing.T) {
				store := &failingGetStore{Store: beads.NewMemStore()}
				x := newFenceFixture(t, fenceFixtureOpts{store: store, wrap: rt.wrap})
				store.armed.Store(true)
				expectFencedRefusal(t, x, row.meta, row.want)
				if n := countMethod(x.f.Calls, "ObserveTurn"); n != 0 {
					t.Fatalf("ObserveTurn calls = %d, want none; calls %v", n, callMethods(x.f.Calls))
				}
			})
		}
	}
}

// stoppingRow is the metadata of a row a drain-ack stop is retiring.
func stoppingRow() map[string]string {
	return map[string]string{"state": string(StateDraining), "state_reason": DrainAckStopPendingReason}
}

// killPendingRow is the metadata of a row a kill put to sleep.
func killPendingRow() map[string]string {
	return map[string]string{"state": string(StateAsleep), "state_reason": KillPendingReason, "sleep_reason": string(SleepReasonKilled), "slept_at": time.Now().UTC().Format(time.RFC3339)}
}

func checkFencedRefusal(t *testing.T, meta map[string]string, want error) {
	t.Helper()
	x := newFenceFixture(t, fenceFixtureOpts{})
	expectFencedRefusal(t, x, meta, want)
	if n := countMethod(x.f.Calls, "ObserveTurn"); n > 1 {
		t.Fatalf("ObserveTurn calls = %d, want at most the intake sighting", n)
	}
}

// expectFencedRefusal gives x's row meta and expects a deliver-now on it to be
// refused with want, having queued, reported and done nothing.
func expectFencedRefusal(t *testing.T, x fenceFixture, meta map[string]string, want error) {
	t.Helper()
	if err := x.mgr.store.SetMetadataBatch(x.info.ID, meta); err != nil {
		t.Fatal(err)
	}
	x.script("t1")
	// The row's stop holds the runtime lease: the refusal must come before
	// the lease wait, not as ErrSessionStarting after it.
	held, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: x.city, Name: x.info.SessionName})
	if err != nil {
		t.Fatalf("holding the runtime lease: %v", err)
	}
	defer held.Release()
	t.Cleanup(SetOperatorLeaseWaitForTest(50 * time.Millisecond))
	out, err := x.submit(t, fenceMsg)
	if !errors.Is(err, want) || out.Queued || out.Interrupt != nil {
		t.Fatalf("Submit = %+v, %v; want %v, nothing queued, no report", out, err, want)
	}
	if got := fenceEffects(x.f.Calls); got != "" {
		t.Fatalf("effects = %q, want none", got)
	}
	if pending := x.pending(t); len(pending) != 0 {
		t.Fatalf("pending %+v, want nothing queued", pending)
	}
}

// A pending prompt is answered explicitly before deliver-now acts: a fenced
// runtime that also asks questions sends no turn interrupt over one.
func TestFencedDeliverNowRefusesOverAPendingInteraction(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{wrap: func(f *runtime.Fake) runtime.Provider {
		f.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		return runtime.FullFake{Fake: f}
	}})
	x.f.SetPendingInteraction(x.info.SessionName, &runtime.PendingInteraction{RequestID: "r1", Kind: "approval"})
	x.script("t1", "t1", "")
	out, err := x.submit(t, fenceMsg)
	if !errors.Is(err, ErrPendingInteraction) || out.Queued || out.Interrupt != nil {
		t.Fatalf("Submit = %+v, %v; want ErrPendingInteraction, nothing queued, no report", out, err)
	}
	if got := fenceEffects(x.f.Calls); got != "" {
		t.Fatalf("effects = %q, want none", got)
	}
	if pending := x.pending(t); len(pending) != 0 {
		t.Fatalf("pending %+v, want nothing queued", pending)
	}
}

// A suspended row has no turn to fence: deliver-now reads no turn and takes
// the unfenced path, whose send resumes it for an operator.
func TestFencedDeliverNowSkipsASuspendedRow(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	if err := x.mgr.store.SetMetadataBatch(x.info.ID, map[string]string{"state": string(StateSuspended)}); err != nil {
		t.Fatal(err)
	}
	x.script("t1")
	out, err := x.submit(t, fenceMsg)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if out.Interrupt != nil {
		t.Fatalf("report = %+v, want none for a suspended row", out.Interrupt)
	}
	for _, m := range []string{"ObserveTurn", "InterruptTurn", "WaitTurnEnded"} {
		if n := countMethod(x.f.Calls, m); n != 0 {
			t.Fatalf("%s calls = %d, want none; calls %v", m, n, callMethods(x.f.Calls))
		}
	}
}

// TestFencedDeliverNowAfterTheInterruptStopsTheRuntime pins the delivery
// when the runtime reads as not running once the interrupt landed, as a
// runtime that reports an interrupted session as absent would (a T3-2
// residual, des-t3-interim-interrupt §12; the pinned bridge does not). The
// send treats it as a stopped session: an operator's starts it and
// delivers; a background caller's is queued Deferred, for the controller.
func TestFencedDeliverNowAfterTheInterruptStopsTheRuntime(t *testing.T) {
	for _, tc := range []struct {
		name             string
		kind             ActorKind
		effects          string
		queued, deferred bool
	}{
		{"operator", ActorOperator, "IT(t1:sent) Start N(msg)", false, false},
		{"background", ActorBackground, "IT(t1:sent)", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var x fenceFixture
			x = newFenceFixture(t, fenceFixtureOpts{wrap: probed(func(method string, _ context.Context, _ time.Duration) {
				if method == "InterruptTurn" {
					// The runtime stops reporting the session as running.
					if err := x.f.Stop(x.info.SessionName); err != nil {
						t.Errorf("Stop: %v", err)
					}
				}
			})})
			x.script("t1", "t1", "")
			out, err := x.mgr.Submit(context.Background(), x.actor(t, tc.kind), x.info.ID, fenceMsg, BuildResumeCommand(x.info), x.hints, SubmitIntentInterruptNow)
			// The fixture's own Stop stands for the runtime's state change.
			calls := x.f.Calls
			if i := callIndex(calls, "Stop", x.info.SessionName); i >= 0 {
				calls = append(append([]runtime.Call{}, calls[:i]...), calls[i+1:]...)
			}
			x.f.Calls = calls
			expectFence(t, x, out, err, tc.effects, unfencedT1(TurnInterruptSettled), tc.queued, tc.deferred)
		})
	}
}

func TestFencedDeliverNowRunsAsOneUnit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script []string
		want   string
	}{
		{"delivered", []string{"t1", "t1", ""}, "InterruptTurn WaitTurnEnded Nudge"},
		{"queued", []string{"t1", "t1", "t3"}, "InterruptTurn WaitTurnEnded Queue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var x fenceFixture
			var seen []string
			check := func(method string) {
				seen = append(seen, method)
				if !sessionMutationLockHeld(x.info.ID) {
					t.Errorf("%s ran without the session mutation lock", method)
				}
				l, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: x.city, Name: x.info.SessionName})
				if err == nil {
					l.Release()
					t.Errorf("%s ran without the runtime lease", method)
				}
			}
			x = newFenceFixture(t, fenceFixtureOpts{wrap: probed(func(method string, _ context.Context, _ time.Duration) {
				if method != "ObserveTurn" {
					check(method)
				}
			})})
			startSessionSubmitPoller = func(string, string, string) error { check("Queue"); return nil }
			x.script(tc.script...)
			if _, err := x.submit(t, fenceMsg); err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if got := strings.Join(seen, " "); got != tc.want {
				t.Fatalf("checked calls = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFencedDeliverNowBoundsEveryRead(t *testing.T) {
	type bound struct {
		method   string
		deadline time.Duration
		hasDL    bool
		timeout  time.Duration
	}
	var bounds []bound
	x := newFenceFixture(t, fenceFixtureOpts{wrap: probed(func(method string, ctx context.Context, timeout time.Duration) {
		if ctx == nil {
			return
		}
		d, ok := ctx.Deadline()
		bounds = append(bounds, bound{method, time.Until(d), ok, timeout})
	})})
	x.script("t1", "t1", "")
	if _, err := x.submit(t, fenceMsg); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	var methods []string
	for _, b := range bounds {
		methods = append(methods, b.method)
		switch b.method {
		case "ObserveTurn", "InterruptTurn":
			if !b.hasDL || b.deadline <= 0 || b.deadline > turnFenceReadTimeout {
				t.Fatalf("%s deadline %v (set %t), want within (0, %v]", b.method, b.deadline, b.hasDL, turnFenceReadTimeout)
			}
		case "WaitTurnEnded":
			if b.timeout != 15*time.Second {
				t.Fatalf("WaitTurnEnded timeout %v, want 15s", b.timeout)
			}
		}
	}
	if got := strings.Join(methods, " "); got != "ObserveTurn InterruptTurn WaitTurnEnded" {
		t.Fatalf("bounded calls = %q", got)
	}
}

func TestFencedDeliverNowSendsTheMessageVerbatim(t *testing.T) {
	const turn = "thread-a/turn-9f2"
	const msg = "  stop — use the other branch\n\t<system-reminder>keep</system-reminder>\n%s %q {{.x}}  "
	for _, tc := range []struct {
		name   string
		script []string
		queued bool
	}{
		{"delivered", []string{turn, turn, ""}, false},
		{"queued", []string{turn, turn, "t3"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newFenceFixture(t, fenceFixtureOpts{})
			x.script(tc.script...)
			out, err := x.submit(t, msg)
			if err != nil || out.Queued != tc.queued {
				t.Fatalf("Submit = %+v, %v; want queued %t", out, err, tc.queued)
			}
			var texts []string
			for _, c := range x.f.Calls {
				if c.Method == "Nudge" || c.Method == "NudgeNow" {
					texts = append(texts, runtime.FlattenText(c.Content))
				}
			}
			for _, item := range x.pending(t) {
				texts = append(texts, item.Message)
			}
			if len(texts) != 1 || texts[0] != msg {
				t.Fatalf("delivered or queued texts = %q, want exactly %q", texts, msg)
			}
			if strings.Contains(texts[0], turn) {
				t.Fatalf("text %q carries the turn id", texts[0])
			}
		})
	}
}

// Every exit of a fenced deliver-now writes exactly one log line and one
// datapoint, its message disposition labeled as what became of the message.
func TestFencedDeliverNowRecordsOneOutcome(t *testing.T) {
	rejected := errors.New("thread rejected the message")
	for _, tc := range []struct {
		name    string
		opts    fenceFixtureOpts
		script  []string
		arrange func(*testing.T, fenceFixture)
		wantErr bool
		// line is the log line after `deliver now for "<session>": `.
		line, point string
	}{
		{
			name: "delivered", line: `interrupt no_turn (turn ""); message delivered`, point: "no_turn||delivered=1",
		},
		{
			name: "deferred over a dead agent", script: []string{"t1"},
			arrange: func(_ *testing.T, x fenceFixture) { x.f.Zombies[x.info.SessionName] = true },
			line:    `interrupt stale (turn "t1"); message deferred`, point: "stale||deferred=1",
		},
		{
			name: "queued at the boundary", script: []string{"t1", "t1", "t3"},
			line: `interrupt settled (turn "t1"), unfenced_interrupt; message queued`, point: "settled|unfenced_interrupt|queued=1",
		},
		{
			name: "deferred at the boundary on a dormant row",
			arrange: func(t *testing.T, x fenceFixture) {
				if err := x.mgr.store.SetMetadataBatch(x.info.ID, map[string]string{"state": string(StateAsleep)}); err != nil {
					t.Fatal(err)
				}
				x.f.TurnReadErrors[x.info.SessionName] = []error{runtime.ErrRuntimeUnavailable, runtime.ErrRuntimeUnavailable}
			},
			script: []string{"t1"},
			line:   `interrupt unavailable (turn ""); message deferred`, point: "unavailable||deferred=1",
		},
		{
			name: "send rejected", wantErr: true,
			arrange: func(_ *testing.T, x fenceFixture) { x.f.NudgeErrors[x.info.SessionName] = rejected },
			line:    `interrupt no_turn (turn ""); message failed: sending message to session: ` + rejected.Error(), point: "no_turn||failed=1",
		},
		{
			name: "queue write failed", opts: fenceFixtureOpts{noQueue: true}, script: []string{"t1", "t1", "t3"}, wantErr: true,
			line:  `interrupt settled (turn "t1"), unfenced_interrupt; message failed: deferred submit is unavailable without a city path`,
			point: "settled|unfenced_interrupt|failed=1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			prev := otel.GetMeterProvider()
			otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
			telemetry.ResetInstrumentsForTest()
			t.Cleanup(func() {
				otel.SetMeterProvider(prev)
				telemetry.ResetInstrumentsForTest()
			})
			logs := captureLog(t)
			x := newFenceFixture(t, tc.opts)
			x.script(tc.script...)
			if tc.arrange != nil {
				tc.arrange(t, x)
			}
			if _, err := x.submit(t, fenceMsg); (err != nil) != tc.wantErr {
				t.Fatalf("Submit error = %v, want error %t", err, tc.wantErr)
			}
			var lines []string
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "deliver now for") {
					lines = append(lines, line)
				}
			}
			wantLine := fmt.Sprintf("session: deliver now for %q: %s", x.info.SessionName, tc.line)
			if len(lines) != 1 || lines[0] != wantLine {
				t.Fatalf("log lines = %q, want [%q]", lines, wantLine)
			}
			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			var points []string
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name != "gc.session.turn_interrupts.total" {
						continue
					}
					for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
						var kv []string
						for _, k := range []attribute.Key{"outcome", "degradation", "message_disposition"} {
							v, _ := dp.Attributes.Value(k)
							kv = append(kv, v.AsString())
						}
						points = append(points, fmt.Sprintf("%s=%d", strings.Join(kv, "|"), dp.Value))
					}
				}
			}
			if len(points) != 1 || points[0] != tc.point {
				t.Fatalf("turn_interrupts datapoints = %q, want [%q]", points, tc.point)
			}
		})
	}
}

// compositeWithoutCapability routes the session to f, a protocol-mode backend
// that does not report turns, behind a composite that does.
func compositeWithoutCapability(f *runtime.Fake) runtime.Provider {
	f.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	return auto.New(f, runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: runtime.NewFake()}))
}

func TestFencedDeliverNowCompositeWithoutCapabilityKeepsProtocolSequence(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{wrap: compositeWithoutCapability})
	out, err := x.submit(t, fenceMsg)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if out.Interrupt != nil {
		t.Fatalf("report = %+v, want none on the unfenced path", out.Interrupt)
	}
	if got := fenceEffects(x.f.Calls); got != "Interrupt N(msg)" {
		t.Fatalf("effects = %q, want the protocol Interrupt then the message", got)
	}
	if n := countMethod(x.f.Calls, "NudgeNow"); n != 1 {
		t.Fatalf("NudgeNow calls = %d, want 1; calls %v", n, callMethods(x.f.Calls))
	}
}

// A composite routing to a backend without the capability keeps
// SESSION-RUNTIME-016 even when the sighting was skipped (here the unlocked
// row read failed): the capability is re-read under the lock.
func TestFencedDeliverNowCompositeWithoutCapabilityAndASkippedSightingKeepsProtocolSequence(t *testing.T) {
	store := &failingGetStore{Store: beads.NewMemStore()}
	x := newFenceFixture(t, fenceFixtureOpts{store: store, wrap: compositeWithoutCapability})
	store.armed.Store(true)
	out, err := x.submit(t, fenceMsg)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if out.Interrupt != nil || out.Queued {
		t.Fatalf("outcome %+v (report %+v), want the unfenced delivery", out, out.Interrupt)
	}
	if got := fenceEffects(x.f.Calls); got != "Interrupt N(msg)" {
		t.Fatalf("effects = %q, want the protocol Interrupt then the message", got)
	}
	if pending := x.pending(t); len(pending) != 0 {
		t.Fatalf("pending %+v, want nothing queued", pending)
	}
}

func TestFencedDeliverNowRefusedFamilyReadsNothing(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{provider: "antigravity"})
	x.script("t1")
	if _, err := x.submit(t, fenceMsg); !errors.Is(err, ErrInteractionUnsupported) {
		t.Fatalf("Submit = %v, want ErrInteractionUnsupported", err)
	}
	if n := countMethod(x.f.Calls, "ObserveTurn"); n != 0 {
		t.Fatalf("ObserveTurn calls = %d, want none", n)
	}
	if got := fenceEffects(x.f.Calls); got != "" {
		t.Fatalf("effects = %q, want none", got)
	}
}

// StopTurn is not deliver-now: it keeps the protocol Interrupt
// (SESSION-RUNTIME-016) and reads no turn.
func TestT3BridgeProfileStopTurnStaysUnfenced(t *testing.T) {
	x := newFenceFixture(t, fenceFixtureOpts{})
	x.script("t1")
	if err := x.mgr.StopTurn(x.info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}
	if n := countMethod(x.f.Calls, "Interrupt"); n != 1 {
		t.Fatalf("Interrupt calls = %d, want 1; calls %v", n, callMethods(x.f.Calls))
	}
	for _, m := range []string{"ObserveTurn", "InterruptTurn", "WaitTurnEnded"} {
		if n := countMethod(x.f.Calls, m); n != 0 {
			t.Fatalf("%s calls = %d, want none", m, n)
		}
	}
}

// TestFencedDeliverNowSettlesTheMessageExactlyOnce: over turn scripts, a dead
// or live agent, send failures and the composites, every deliver-now ends in
// exactly one of delivered, queued or an error with nothing queued; sends at
// most one interrupt and never names an empty turn; carries the text
// verbatim; and on these live rows reports Deferred only when the send queued
// it over a dead agent.
func TestFencedDeliverNowSettlesTheMessageExactlyOnce(t *testing.T) {
	scripts := [][]string{nil, {"t1", "t1", ""}, {"t1", "t2"}, {"t1"}, {"", "t2"}, {"t1", ""}, {"t1", "t1", "t3"}}
	sendErrs := map[string]error{
		"ok":          nil,
		"unavailable": fmt.Errorf("%w: no thread bound", runtime.ErrRuntimeUnavailable),
		"rejected":    errors.New("rejected"),
	}
	for _, wrapName := range []string{"t3bridge", "auto", "hybrid"} {
		for i, script := range scripts {
			for _, zombie := range []bool{false, true} {
				for sendName, sendErr := range sendErrs {
					name := fmt.Sprintf("%s/script%d/zombie=%t/send=%s", wrapName, i, zombie, sendName)
					t.Run(name, func(t *testing.T) {
						x := newFenceFixture(t, fenceFixtureOpts{wrap: t3bridgeComposites[wrapName]})
						x.script(script...)
						x.f.Zombies[x.info.SessionName] = zombie
						if sendErr != nil {
							x.f.NudgeErrors[x.info.SessionName] = sendErr
						}
						out, err := x.submit(t, fenceMsg)
						pending := x.pending(t)
						delivered := 0
						for _, c := range x.f.Calls {
							if (c.Method == "Nudge" || c.Method == "NudgeNow") && sendErr == nil {
								delivered++
								if c.Message != fenceMsg {
									t.Fatalf("delivered %q, want %q", c.Message, fenceMsg)
								}
							}
						}
						switch {
						case err != nil:
							if len(pending) != 0 || out.Queued || delivered != 0 {
								t.Fatalf("error %v with pending %d, queued %t, delivered %d", err, len(pending), out.Queued, delivered)
							}
						case out.Queued:
							if len(pending) != 1 || pending[0].Message != fenceMsg || delivered != 0 {
								t.Fatalf("queued with pending %+v, delivered %d", pending, delivered)
							}
						default:
							if len(pending) != 0 || delivered != 1 {
								t.Fatalf("delivered %d with pending %+v", delivered, pending)
							}
						}
						if out.Deferred && (!out.Queued || !zombie) {
							t.Fatalf("Deferred %+v without the send queueing over a dead agent", out)
						}
						if out.Interrupt == nil {
							t.Fatalf("outcome %+v (%v) carries no report", out, err)
						}
						sent := 0
						for _, c := range x.f.Calls {
							switch c.Method {
							case "InterruptTurn", "WaitTurnEnded":
								if c.Key == "" {
									t.Fatalf("%s named an empty turn", c.Method)
								}
								if c.Method == "InterruptTurn" && c.Value == "sent" {
									sent++
								}
							case "Interrupt", "SendKeys", "Stop", "Relaunch":
								t.Fatalf("untargeted %s on the fenced path", c.Method)
							}
						}
						if sent > 1 || (zombie && sent != 0) {
							t.Fatalf("%d interrupts sent (zombie %t)", sent, zombie)
						}
					})
				}
			}
		}
	}
}

// Only interrupt_now reads the turn: a default or follow_up submit to a
// session whose runtime reports turns, bare or behind a composite, makes no
// turn read, with a turn running or not.
func TestSubmitOtherIntentsReadNoTurn(t *testing.T) {
	for _, wrapName := range []string{"t3bridge", "auto", "hybrid"} {
		for _, intent := range []SubmitIntent{"", SubmitIntentDefault, SubmitIntentFollowUp} {
			for _, turn := range []string{"", "t1"} {
				t.Run(fmt.Sprintf("%s/%q/turn=%q", wrapName, intent, turn), func(t *testing.T) {
					x := newFenceFixture(t, fenceFixtureOpts{wrap: t3bridgeComposites[wrapName]})
					x.script(turn)
					if _, err := x.mgr.Submit(context.Background(), x.actor(t, ActorOperator), x.info.ID, fenceMsg, BuildResumeCommand(x.info), x.hints, intent); err != nil {
						t.Fatalf("Submit(%q): %v", intent, err)
					}
					for _, m := range []string{"ObserveTurn", "InterruptTurn", "WaitTurnEnded"} {
						if n := countMethod(x.f.Calls, m); n != 0 {
							t.Fatalf("%s calls = %d, want none; calls %v", m, n, callMethods(x.f.Calls))
						}
					}
				})
			}
		}
	}
}
