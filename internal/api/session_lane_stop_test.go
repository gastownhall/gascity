package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// /stop and run cancel under lane authority (design slice 4): with interrupt
// listed, the API proposes interrupt ops the lane host runs and answers from
// their outcomes; it sends nothing to the runtime itself.

// laneStopEnv is a city whose lane host executes interrupt, over a protocol
// runtime that records turn starts (so a named turn passes the front door),
// with the host's passes scripted by closeWith: each poke closes every open
// lane interrupt op of the poked conversation with closeWith's outcome, or
// leaves it open when closeWith is nil.
func laneStopEnv(t *testing.T, closeWith func(op opjournal.Op) *opjournal.Outcome) (*opsEnv, *lanePokeState, session.Info) {
	t.Helper()
	e, st := newLaneOpsEnv(t, "message", "interrupt")
	e.fs.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	e.fs.sessionProvider = runtime.FullFake{Fake: e.fs.sp}
	info := e.session(t, "Lane stop")
	e.fs.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{info.SessionName: {{
		Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceChecked, Link: runtime.TurnLinkCorrelated,
	}}}
	st.onPoke = func(conv string) { scriptLanePass(t, st.ops, conv, closeWith) }
	return e, st, info
}

// scriptLanePass closes conv's open lane interrupt ops with closeWith's
// outcome; a canceled outcome cancels the op.
func scriptLanePass(t *testing.T, ops *opjournal.Store, conv string, closeWith func(op opjournal.Op) *opjournal.Outcome) {
	t.Helper()
	if closeWith == nil {
		return
	}
	open, err := ops.ListOpen(conv)
	if err != nil {
		t.Errorf("listing open ops: %v", err)
		return
	}
	for _, op := range open {
		if op.Executor != opjournal.ExecutorLane || op.Kind != opjournal.KindInterrupt {
			continue
		}
		o := closeWith(op)
		switch {
		case o == nil:
		case o.Arm == opjournal.ArmCanceled:
			if _, _, err := ops.Cancel(op.ID, "operator", ""); err != nil {
				t.Errorf("canceling op %s: %v", op.ID, err)
			}
		default:
			if _, err := ops.Close(op.ID, *o, "lane:ctl"); err != nil {
				t.Errorf("closing op %s: %v", op.ID, err)
			}
		}
	}
}

func outcome(arm opjournal.Arm, reason string) func(opjournal.Op) *opjournal.Outcome {
	return func(opjournal.Op) *opjournal.Outcome { return &opjournal.Outcome{Arm: arm, Reason: reason} }
}

// fencedOff is the outcome the lane's scope rule closes an op with once its
// session moved on to generation gen: skipped{stale}, unrun, recording gen
// (opjournal.FencedOff). The executor's close of an op that found no turn is
// skipped{stale} recording no generation.
func fencedOff(gen int64) *opjournal.Outcome {
	return &opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale, Result: opjournal.Result{Generation: gen}}
}

// staleViewLane is the lane's scope rule and incarnation wait (opjournal
// planRules) over session conv, whose lane runtime row leads its session row,
// the lane's view, at generation rowGen. Its pass (outcome) fences an open
// interrupt op scoped behind the effective incarnation, the later of rowGen
// and the session row's generation, off unrun, recording that incarnation;
// holds one scoped at it while the view lags (WaitIncarnation); and succeeds
// one once the view has caught up (catchUp). It fences off one op, the
// request's first: a follow-up it would fence off again fails the test and is
// left open, so the request answers at its bound instead of proposing more.
type staleViewLane struct {
	t      *testing.T
	store  beads.Store
	conv   string
	rowGen int64

	mu     sync.Mutex
	fenced bool
	held   map[string]bool
}

func newStaleViewLane(t *testing.T, store beads.Store, conv string, rowGen int64) *staleViewLane {
	return &staleViewLane{t: t, store: store, conv: conv, rowGen: rowGen, held: map[string]bool{}}
}

// outcome is the lane's pass over op (scriptLanePass): its close, or nil to
// leave it open.
func (l *staleViewLane) outcome(op opjournal.Op) *opjournal.Outcome {
	l.mu.Lock()
	defer l.mu.Unlock()
	scope, why, err := sessionOpScope(l.store, l.conv)
	if why != "" {
		l.t.Errorf("reading session %s's generation: %s %v", l.conv, why, err)
		return nil
	}
	view := scope.Generation
	inc := max(view, l.rowGen)
	switch {
	case op.Scope.Generation < inc && !l.fenced:
		l.fenced = true
		return fencedOff(inc)
	case op.Scope.Generation < inc:
		l.t.Errorf("op %s, a follow-up of generation %d, is behind the runtime row's %d: the lane would fence it off again", op.ID, op.Scope.Generation, inc)
		return nil
	case view < inc:
		l.held[op.ID] = true
		return nil
	}
	return &opjournal.Outcome{Arm: opjournal.ArmSucceeded}
}

// catchUp, once the lane holds op id for its view, moves the session row to
// the runtime row's generation, as the view catching up, and reports so: the
// lane's next pass runs the op.
func (l *staleViewLane) catchUp(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held[id] {
		return false
	}
	if err := l.store.SetMetadata(l.conv, "generation", strconv.FormatInt(l.rowGen, 10)); err != nil {
		l.t.Errorf("catching session %s's row up: %v", l.conv, err)
		return false
	}
	return true
}

// wasHeld reports whether the lane held op id for its view to catch up.
func (l *staleViewLane) wasHeld(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held[id]
}

// runtimeInterrupts counts every interrupt the API sent the runtime itself.
func runtimeInterrupts(sp *runtime.Fake, name string) int {
	return sp.CountCalls("Interrupt", name) + sp.CountCalls("InterruptTurn", name) + sp.CountCalls("SendKeys", name)
}

// unobservableRuntime is a runtime whose state cannot be read, as
// t3bridge's when its snapshot read fails past the startup grace: IsRunning
// reads false, and the error-bearing liveness read the lane's probe makes
// fails runtime.ErrRuntimeUnavailable.
type unobservableRuntime struct{ runtime.FullFake }

func (unobservableRuntime) IsRunning(string) bool { return false }

func (unobservableRuntime) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, fmt.Errorf("reading session %s's snapshot: %w", name, runtime.ErrRuntimeUnavailable)
}

// processNameLiveness is a FullFake whose liveness reads as that of a runtime
// with no liveness observer of its own (runtime.ObserveLiveness's fallback):
// running while started, and alive by the agent process names the reader
// passes, so a read that omits them takes a session whose agent died (a
// zombie) for alive.
type processNameLiveness struct{ runtime.FullFake }

func (p processNameLiveness) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	running := p.IsRunning(name)
	if len(processNames) == 0 {
		return runtime.Liveness{Running: running, Alive: running}, nil
	}
	return runtime.Liveness{Running: running, Alive: p.ProcessAlive(name, processNames)}, nil
}

// withAgentProcessNames gives the test sessions' provider, "test", the
// agent process names a liveness read checks.
func withAgentProcessNames(fs *fakeState) {
	fs.cfg.Providers["test"] = config.ProviderSpec{DisplayName: "Test", Command: "echo", ProcessNames: []string{"test-agent"}}
}

func TestStopUnderLaneProposesAnInterruptOp(t *testing.T) {
	e, st, info := laneStopEnv(t, outcome(opjournal.ArmSucceeded, ""))
	rec := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	wantStatus(t, "stop", rec, http.StatusOK)
	a := decodeCommandAnswer(t, rec)
	if a.Op == nil || a.IdempotencyKey != "stop-1" || a.Replayed {
		t.Fatalf("answer = %+v", a)
	}
	op, err := st.ops.Get(a.Op.ID)
	if err != nil {
		t.Fatal(err)
	}
	gen := generationOf(t, e.store, info.ID)
	if op.Executor != opjournal.ExecutorLane || op.Kind != opjournal.KindInterrupt || op.ClientKey != "stop-1" ||
		op.Producer.Principal != "operator" || op.Scope != (opjournal.Scope{Kind: opjournal.ScopeSession, Generation: gen}) ||
		op.Policy.Interrupt.OnFailure != opjournal.OnFailureFail {
		t.Fatalf("op = %+v", op)
	}
	if got := st.poked(); !slices.Contains(got, info.ID) {
		t.Fatalf("pokes = %v, want the session", got)
	}
	if n := runtimeInterrupts(e.fs.sp, info.SessionName); n != 0 {
		t.Fatalf("the API sent %d interrupts itself, want none", n)
	}
	if rows := e.opRows(t); len(rows) != 1 {
		t.Fatalf("op rows = %d, want the lane op alone (no legacy op)", len(rows))
	}
	// Without a key the op's key is minted.
	rec = e.post(t, "/session/"+info.ID+"/stop", "", "")
	wantStatus(t, "unkeyed stop", rec, http.StatusOK)
	if b := decodeCommandAnswer(t, rec); b.Op == nil || b.Op.ID == a.Op.ID || b.IdempotencyKey == "" {
		t.Fatalf("unkeyed answer = %+v, want a fresh op under a minted key", b)
	}
}

func TestStopUnderLaneAnswersFromTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		arm    opjournal.Arm
		reason string
		status int
		code   string
	}{
		{"succeeded", "", opjournal.ArmSucceeded, "", http.StatusOK, ""},
		{"stale, unnamed", "", opjournal.ArmSkipped, opjournal.ReasonStale, http.StatusOK, ""},
		{"stale, named", `{"turn":"t-1"}`, opjournal.ArmSkipped, opjournal.ReasonStale, http.StatusConflict, turnNotCurrentCode},
		{"conversation closed, unnamed", "", opjournal.ArmSkipped, opjournal.ReasonConversationClosed, http.StatusOK, ""},
		{"conversation closed, named", `{"turn":"t-1"}`, opjournal.ArmSkipped, opjournal.ReasonConversationClosed, http.StatusConflict, turnNotCurrentCode},
		{"not needed, named", `{"turn":"t-1"}`, opjournal.ArmSkipped, opjournal.ReasonNotNeeded, http.StatusOK, ""},
		{"unsupported", "", opjournal.ArmFailed, opjournal.ReasonUnsupported, http.StatusNotImplemented, ""},
		{"provider unavailable", "", opjournal.ArmFailed, opjournal.ReasonProviderUnavailable, http.StatusServiceUnavailable, "runtime_unavailable"},
		{"expired before start", "", opjournal.ArmFailed, opjournal.ReasonExpiredBeforeStart, http.StatusServiceUnavailable, "runtime_unavailable"},
		{"ambiguous", "", opjournal.ArmFailed, opjournal.ReasonAmbiguousDelivery, http.StatusConflict, opAlreadyFailedCode},
		{"restart not started", "", opjournal.ArmFailed, opjournal.ReasonRestartNotStarted, http.StatusConflict, opAlreadyFailedCode},
		{"other", "", opjournal.ArmFailed, opjournal.ReasonOther, http.StatusConflict, opAlreadyFailedCode},
		{"canceled", "", opjournal.ArmCanceled, "", http.StatusConflict, opAlreadyFailedCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, info := laneStopEnv(t, outcome(tc.arm, tc.reason))
			rec := e.post(t, "/session/"+info.ID+"/stop", "k-"+strings.ReplaceAll(tc.name, " ", "-"), tc.body)
			wantStatus(t, tc.name, rec, tc.status)
			if tc.code != "" && !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("body %s, want %s", rec.Body.String(), tc.code)
			}
			if n := runtimeInterrupts(e.fs.sp, info.SessionName); n != 0 {
				t.Fatalf("the API sent %d interrupts itself", n)
			}
		})
	}
	// failed{no_boundary} is the legacy not-ended answer.
	e, _, info := laneStopEnv(t, outcome(opjournal.ArmFailed, opjournal.ReasonNoBoundary))
	rec := e.post(t, "/session/"+info.ID+"/stop", "k-nb", "")
	if want := humaStopError(runtime.ErrTurnNotEnded); rec.Code != statusOf(want) {
		t.Fatalf("no_boundary: %d %s, want %d", rec.Code, rec.Body.String(), statusOf(want))
	}
}

// statusOf is a Huma error's HTTP status.
func statusOf(err error) int {
	if se, ok := err.(interface{ GetStatus() int }); ok {
		return se.GetStatus()
	}
	return 0
}

func TestStopUnderLaneOpenAtTheBoundIsInFlight(t *testing.T) {
	old := laneControlAnswerBound
	laneControlAnswerBound = 50 * time.Millisecond
	t.Cleanup(func() { laneControlAnswerBound = old })
	e, _, info := laneStopEnv(t, nil)
	rec := e.post(t, "/session/"+info.ID+"/stop", "stop-open", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in_flight") || !strings.Contains(rec.Body.String(), "stop-open") {
		t.Fatalf("open at the bound: %d %s, want 409 in_flight naming the key", rec.Code, rec.Body.String())
	}
}

func TestStopUnderLaneSameKeyRetryAdoptsOrReplays(t *testing.T) {
	old := laneControlAnswerBound
	laneControlAnswerBound = 2 * time.Second
	t.Cleanup(func() { laneControlAnswerBound = old })
	e, st, info := laneStopEnv(t, nil)
	laneControlAnswerBound = 50 * time.Millisecond
	first := e.post(t, "/session/"+info.ID+"/stop", "stop-r", "")
	wantStatus(t, "first", first, http.StatusConflict)
	open, err := st.ops.ListOpen(info.ID)
	if err != nil || len(open) != 1 {
		t.Fatalf("open ops = %v, %v; want the first stop's", open, err)
	}
	// The retry adopts the open op: it proposes nothing new and answers
	// once the lane closes it.
	laneControlAnswerBound = 2 * time.Second
	st.onAwait = func(id string) {
		_, _ = st.ops.Close(id, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:ctl")
	}
	retry := e.post(t, "/session/"+info.ID+"/stop", "stop-r", "")
	wantStatus(t, "adopting retry", retry, http.StatusOK)
	if a := decodeCommandAnswer(t, retry); a.Op == nil || a.Op.ID != open[0].ID || !a.Replayed {
		t.Fatalf("adopting retry = %+v, want op %s", a, open[0].ID)
	}
	// Across a restart the retry replays the closed op.
	e.restart(t)
	e.resolver.state = st
	replay := e.post(t, "/session/"+info.ID+"/stop", "stop-r", "")
	wantStatus(t, "replay", replay, http.StatusOK)
	if a := decodeCommandAnswer(t, replay); a.Op == nil || a.Op.ID != open[0].ID || !a.Replayed {
		t.Fatalf("replay = %+v", a)
	}
	if rows := e.opRows(t); len(rows) != 1 {
		t.Fatalf("op rows = %d, want one op", len(rows))
	}
}

func TestNamedStopUnderLaneKeysByTurnAndRecordsTheRequestKey(t *testing.T) {
	e, st, info := laneStopEnv(t, outcome(opjournal.ArmSucceeded, ""))
	rec := e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`)
	wantStatus(t, "named stop", rec, http.StatusOK)
	a := decodeCommandAnswer(t, rec)
	gen := generationOf(t, e.store, info.ID)
	wantKey, _ := opjournal.InterruptKey(gen, "t-1")
	if a.Op == nil || a.IdempotencyKey != wantKey {
		t.Fatalf("answer = %+v, want the turn's target key %s", a, wantKey)
	}
	op, err := st.ops.Get(a.Op.ID)
	if err != nil || op.Scope.Turn != "t-1" || op.Executor != opjournal.ExecutorLane {
		t.Fatalf("op = %+v, %v", op, err)
	}
	rowID, _ := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", "click-1")
	row, err := st.ops.Get(rowID)
	if err != nil || row.Status != opjournal.StatusClosed || row.Outcome.Result.CoalescedInto != op.ID || row.Executor == opjournal.ExecutorLane {
		t.Fatalf("row under the request key = %+v, %v; want a closed coalesced row", row, err)
	}
	// A retry under the request's key, with no turn, follows the row back,
	// across a restart (the Idempotency-Key cache is gone).
	e.restart(t)
	e.resolver.state = st
	retry := e.post(t, "/session/"+info.ID+"/stop", "click-1", "")
	wantStatus(t, "retry", retry, http.StatusOK)
	if b := decodeCommandAnswer(t, retry); b.Op == nil || b.Op.ID != op.ID || !b.Replayed {
		t.Fatalf("retry = %+v, want op %s replayed", b, op.ID)
	}
	// A named turn the runtime cannot identify is refused at the front door.
	e.fs.sp.TurnCapabilityScripts[info.SessionName] = []runtime.TurnCapabilities{{}}
	before := len(e.opRows(t))
	refused := e.post(t, "/session/"+info.ID+"/stop", "click-2", `{"turn":"t-2"}`)
	wantStatus(t, "unidentified turn", refused, http.StatusNotImplemented)
	if after := len(e.opRows(t)); after != before {
		t.Fatalf("a front-door refusal wrote %d rows", after-before)
	}
}

// A same-key /stop retry that names a turn the key's stop did not name is
// 422 idempotency_mismatch, as under OJ-4, nothing proposed: another named
// turn, and a turn named under the key of a stop that named none (its op
// session-scoped). Each retry comes after a restart, so the Idempotency-Key
// cache cannot answer it.
func TestStopUnderLaneKeyReusedForAnotherTurnIsMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, firstBody string
	}{
		{"named, then another turn", `{"turn":"t-1"}`},
		{"unnamed, then a turn", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, st, info := laneStopEnv(t, outcome(opjournal.ArmSucceeded, ""))
			wantStatus(t, "first", e.post(t, "/session/"+info.ID+"/stop", "k-1", tc.firstBody), http.StatusOK)
			e.restart(t)
			e.resolver.state = st
			before := len(e.opRows(t))
			rec := e.post(t, "/session/"+info.ID+"/stop", "k-1", `{"turn":"t-2"}`)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "idempotency_mismatch") {
				t.Fatalf("same key, other turn: %d %s, want 422 idempotency_mismatch", rec.Code, rec.Body.String())
			}
			if after := len(e.opRows(t)); after != before {
				t.Fatalf("the refused retry wrote %d rows", after-before)
			}
		})
	}
}

// A named stop whose turn's target-key op closed without success does not
// replay that failure: it stops the turn again under its own key.
func TestNamedStopUnderLaneRetriesAFailedStopOfItsTurn(t *testing.T) {
	fail := true
	e, st, info := laneStopEnv(t, func(opjournal.Op) *opjournal.Outcome {
		if fail {
			return &opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonOther}
		}
		return &opjournal.Outcome{Arm: opjournal.ArmSucceeded}
	})
	first := e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`)
	if first.Code != http.StatusConflict || !strings.Contains(first.Body.String(), opAlreadyFailedCode) {
		t.Fatalf("first stop: %d %s, want 409 %s", first.Code, first.Body.String(), opAlreadyFailedCode)
	}
	targetKey, _ := opjournal.InterruptKey(generationOf(t, e.store, info.ID), "t-1")
	targetID, _ := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", targetKey)
	fail = false
	rec := e.post(t, "/session/"+info.ID+"/stop", "click-2", `{"turn":"t-1"}`)
	wantStatus(t, "second stop", rec, http.StatusOK)
	a := decodeCommandAnswer(t, rec)
	if a.Op == nil || a.Op.ID == targetID || a.IdempotencyKey != "click-2" || a.Replayed {
		t.Fatalf("second stop = %+v, want a new op under its own key, not the turn's failed op %s", a, targetID)
	}
	op, err := st.ops.Get(a.Op.ID)
	if err != nil || op.Scope.Turn != "t-1" || op.Executor != opjournal.ExecutorLane || op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("retry op = %+v, %v; want a succeeded lane stop of turn t-1", op, err)
	}
	if target, err := st.ops.Get(targetID); err != nil || target.Outcome.Arm != opjournal.ArmFailed {
		t.Fatalf("the turn's target-key op = %+v, %v; want it left closed failed", target, err)
	}
}

func TestStopWithoutLaneInterruptKeepsOJ4(t *testing.T) {
	e, st := newLaneOpsEnv(t, "message")
	e.fs.cfg.Ops.ControlMode = "auto"
	e.restart(t)
	e.resolver.state = st
	info := e.session(t, "Legacy stop")
	rec := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	wantStatus(t, "legacy stop", rec, http.StatusOK)
	a := decodeCommandAnswer(t, rec)
	if a.Op == nil {
		t.Fatalf("answer = %+v, want OJ-4's recorded op", a)
	}
	op, err := st.ops.Get(a.Op.ID)
	if err != nil || op.Executor == opjournal.ExecutorLane {
		t.Fatalf("op = %+v, %v; want a legacy op", op, err)
	}
	if got := e.fs.sp.CountCalls("Interrupt", info.SessionName); got != 1 {
		t.Fatalf("Interrupt calls = %d, want the legacy stop's one", got)
	}
}

// AwaitConversationOp returns once the op closes, and the op as read when
// its context ends first.
func TestAwaitLaneOpReturnsAtCloseOrBound(t *testing.T) {
	e, st := newLaneOpsEnv(t, "interrupt")
	info := e.session(t, "Await")
	op, _, err := st.ops.Propose(opjournal.Request{
		Conversation: info.ID, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "a-1",
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
		Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := e.server(t)
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	got, err := s.awaitLaneOp(short, info.ID, op.ID)
	if err != nil || got.Status == opjournal.StatusClosed {
		t.Fatalf("await of an open op = %+v, %v; want it open at the bound", got, err)
	}
	st.onAwait = func(id string) {
		_, _ = st.ops.Close(id, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:ctl")
	}
	long, cancelLong := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelLong()
	got, err = s.awaitLaneOp(long, info.ID, op.ID)
	if err != nil || got.Status != opjournal.StatusClosed {
		t.Fatalf("await = %+v, %v; want it closed", got, err)
	}
}

// A /stop of a session with no turn to stop (suspended, or its runtime not
// running) gets no lane op, which the lane would hold not_running until its
// start_by, and answers at once, as OJ-4's StopTurn guard does: a named turn
// is 409 turn_not_current, nothing written; a stop naming none is 200,
// recorded as an API-executed row closed skipped{not_needed}, which a
// same-key retry replays. The lane here never closes an op, so a stop that
// proposed one would answer in_flight at the bound instead.
func TestStopUnderLaneOfASessionWithNoTurnAnswersAtOnce(t *testing.T) {
	old := laneControlAnswerBound
	laneControlAnswerBound = 2 * time.Second
	t.Cleanup(func() { laneControlAnswerBound = old })
	for _, tc := range []struct {
		name   string
		noTurn func(t *testing.T, e *opsEnv, info session.Info)
	}{
		{"suspended", func(t *testing.T, e *opsEnv, info session.Info) {
			suspendSessionForPermissionModeTest(t, e.fs, info.ID)
		}},
		{"runtime not running", func(t *testing.T, e *opsEnv, info session.Info) {
			if err := e.fs.sp.Stop(info.SessionName); err != nil {
				t.Fatal(err)
			}
		}},
		{"agent dead", func(_ *testing.T, e *opsEnv, info session.Info) {
			// The runtime runs, but its agent died: the lane's probe reads
			// it not running (Running && Alive), its liveness read with
			// the session's agent process names.
			withAgentProcessNames(e.fs)
			e.fs.sessionProvider = processNameLiveness{runtime.FullFake{Fake: e.fs.sp}}
			e.fs.sp.Zombies[info.SessionName] = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, st, info := laneStopEnv(t, nil)
			tc.noTurn(t, e, info)
			named := e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`)
			if named.Code != http.StatusConflict || !strings.Contains(named.Body.String(), turnNotCurrentCode) {
				t.Fatalf("named stop: %d %s, want 409 %s", named.Code, named.Body.String(), turnNotCurrentCode)
			}
			if rows := e.opRows(t); len(rows) != 0 {
				t.Fatalf("the named stop wrote %d op rows, want none", len(rows))
			}
			rec := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
			wantStatus(t, "unnamed stop", rec, http.StatusOK)
			a := decodeCommandAnswer(t, rec)
			if a.Op == nil || a.IdempotencyKey != "stop-1" || a.Replayed {
				t.Fatalf("answer = %+v, want a new op under the request's key", a)
			}
			op, err := st.ops.Get(a.Op.ID)
			if err != nil || op.Executor == opjournal.ExecutorLane || op.Kind != opjournal.KindInterrupt ||
				op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmSkipped || op.Outcome.Reason != opjournal.ReasonNotNeeded {
				t.Fatalf("op = %+v, %v; want an API-executed interrupt closed skipped{not_needed}", op, err)
			}
			if n := runtimeInterrupts(e.fs.sp, info.SessionName); n != 0 {
				t.Fatalf("the API sent %d interrupts itself", n)
			}
			// A same-key retry, across a restart, replays the row.
			e.restart(t)
			e.resolver.state = st
			replay := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
			wantStatus(t, "replay", replay, http.StatusOK)
			if b := decodeCommandAnswer(t, replay); b.Op == nil || b.Op.ID != op.ID || !b.Replayed {
				t.Fatalf("replay = %+v, want op %s replayed", b, op.ID)
			}
			if rows := e.opRows(t); len(rows) != 1 {
				t.Fatalf("op rows = %d, want the one row, no lane op", len(rows))
			}
		})
	}
}

// A named stop's row under its own key that was written but never closed
// (its close failed, or its API died between the two writes) names a row no
// executor runs. A same-key retry, naming the turn or not, finishes the row,
// closed skipped{coalesced_into} the turn's op, and answers from that op
// rather than in_flight.
func TestStopUnderLaneSameKeyRetryFinishesTheRowItsCloseMissed(t *testing.T) {
	old := laneControlAnswerBound
	laneControlAnswerBound = 2 * time.Second
	t.Cleanup(func() { laneControlAnswerBound = old })
	e, st, info := laneStopEnv(t, nil)
	// The lane here closes nothing, so the row's close is the stop's only
	// op-row update.
	e.store.set(func(s *opsTestStore) { s.updateFails = 1 })
	first := e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`)
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), "op_journal_unavailable") {
		t.Fatalf("first stop: %d %s, want 503 op_journal_unavailable", first.Code, first.Body.String())
	}
	rowID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", "click-1")
	if err != nil {
		t.Fatal(err)
	}
	if row, err := st.ops.Get(rowID); err != nil || row.Status == opjournal.StatusClosed || row.Executor == opjournal.ExecutorLane {
		t.Fatalf("row under the request key = %+v, %v; want an API-executed row left open", row, err)
	}
	targetKey, err := opjournal.InterruptKey(generationOf(t, e.store, info.ID), "t-1")
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", targetKey)
	if err != nil {
		t.Fatal(err)
	}
	st.onAwait = func(id string) {
		_, _ = st.ops.Close(id, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:ctl")
	}
	for _, body := range []string{"", `{"turn":"t-1"}`} {
		e.restart(t)
		e.resolver.state = st
		retry := e.post(t, "/session/"+info.ID+"/stop", "click-1", body)
		wantStatus(t, "retry "+body, retry, http.StatusOK)
		if a := decodeCommandAnswer(t, retry); a.Op == nil || a.Op.ID != targetID || !a.Replayed {
			t.Fatalf("retry %q = %+v, want the turn's op %s replayed", body, a, targetID)
		}
	}
	row, err := st.ops.Get(rowID)
	if err != nil || row.Status != opjournal.StatusClosed || row.Outcome.Reason != opjournal.ReasonCoalescedInto || row.Outcome.Result.CoalescedInto != targetID {
		t.Fatalf("row = %+v, %v; want it closed skipped{coalesced_into: %s}", row, err, targetID)
	}
}

// A /stop of a session whose runtime cannot be observed (a runtime snapshot
// that does not read: IsRunning false, the lane's error-bearing probe
// runtime.ErrRuntimeUnavailable) does not know the session has no turn: it
// proposes its lane op, which the lane's probe holds until the runtime
// answers, and answers from that op, never "nothing to stop" recorded
// skipped{not_needed} and never turn_not_current. The lane here closes no
// op, so each stop answers in_flight at the bound.
func TestStopUnderLaneOfAnUnobservableRuntimeGoesToTheLane(t *testing.T) {
	old := laneControlAnswerBound
	laneControlAnswerBound = 50 * time.Millisecond
	t.Cleanup(func() { laneControlAnswerBound = old })
	e, st, info := laneStopEnv(t, nil)
	e.fs.sessionProvider = unobservableRuntime{runtime.FullFake{Fake: e.fs.sp}}
	for _, tc := range []struct{ name, key, body string }{
		{"named", "click-1", `{"turn":"t-1"}`},
		{"unnamed", "stop-1", ""},
	} {
		rec := e.post(t, "/session/"+info.ID+"/stop", tc.key, tc.body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in_flight") {
			t.Fatalf("%s stop: %d %s, want 409 in_flight, its op left to the lane", tc.name, rec.Code, rec.Body.String())
		}
	}
	open, err := st.ops.ListOpen(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("open ops = %+v, want the named and the unnamed stop's lane ops", open)
	}
	for _, op := range open {
		if op.Executor != opjournal.ExecutorLane || op.Kind != opjournal.KindInterrupt {
			t.Fatalf("open op %+v, want a lane interrupt", op)
		}
	}
	for _, row := range e.opRows(t) {
		op, err := opjournal.DecodeOp(row)
		if err != nil {
			t.Fatalf("decode op row %s: %v", row.ID, err)
		}
		if op.Outcome.Reason == opjournal.ReasonNotNeeded {
			t.Fatalf("op %+v records nothing to stop", op)
		}
	}
	if n := runtimeInterrupts(e.fs.sp, info.SessionName); n != 0 {
		t.Fatalf("the API sent %d interrupts itself", n)
	}
}

// A named stop's row under its own key that the sweep closed
// failed{deadline_in_flight}, its close having missed with no retry before
// its finish_by, says nothing of the stop: a same-key retry, naming the turn
// or not, answers from the turn's lane op rather than 409
// op_already_failed, which would ask for a second stop under a new key.
func TestStopUnderLaneSameKeyRetryAnswersASweptRowFromTheTurnsOp(t *testing.T) {
	old := laneControlAnswerBound
	laneControlAnswerBound = 2 * time.Second
	t.Cleanup(func() { laneControlAnswerBound = old })
	e, st, info := laneStopEnv(t, nil)
	// The lane here closes nothing, so the row's close is the stop's only
	// op-row update.
	e.store.set(func(s *opsTestStore) { s.updateFails = 1 })
	first := e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`)
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), "op_journal_unavailable") {
		t.Fatalf("first stop: %d %s, want 503 op_journal_unavailable", first.Code, first.Body.String())
	}
	rowID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", "click-1")
	if err != nil {
		t.Fatal(err)
	}
	swept := opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonDeadlineInFlight}
	if _, err := st.ops.Close(rowID, swept, "sweep"); err != nil {
		t.Fatalf("sweeping the row: %v", err)
	}
	targetKey, err := opjournal.InterruptKey(generationOf(t, e.store, info.ID), "t-1")
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", targetKey)
	if err != nil {
		t.Fatal(err)
	}
	st.onAwait = func(id string) {
		_, _ = st.ops.Close(id, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:ctl")
	}
	for _, body := range []string{"", `{"turn":"t-1"}`} {
		e.restart(t)
		e.resolver.state = st
		retry := e.post(t, "/session/"+info.ID+"/stop", "click-1", body)
		wantStatus(t, "retry "+body, retry, http.StatusOK)
		if a := decodeCommandAnswer(t, retry); a.Op == nil || a.Op.ID != targetID || !a.Replayed {
			t.Fatalf("retry %q = %+v, want the turn's op %s replayed", body, a, targetID)
		}
	}
	if row, err := st.ops.Get(rowID); err != nil || !row.Outcome.Equal(swept) {
		t.Fatalf("row = %+v, %v; want it left as the sweep closed it", row, err)
	}
	if n := len(e.opRows(t)); n != 2 {
		t.Fatalf("op rows = %d, want the swept row and the turn's op", n)
	}
}

// proposeLegacyStop writes an API-executed interrupt op, left open, under key
// on session id, scoped to turn ("" for none) in generation gen: an OJ-4 stop
// whose API died, or that a retryable error left open, before the flip.
func proposeLegacyStop(t *testing.T, ops *opjournal.Store, id, key string, gen int64, turn string) opjournal.Op {
	t.Helper()
	op, _, err := ops.Propose(opjournal.Request{
		Conversation: id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLegacyAPI, ClientKey: key,
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
		Policy:   interruptOpPolicy,
		Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: gen, Turn: turn},
	})
	if err != nil {
		t.Fatalf("proposing the legacy stop: %v", err)
	}
	return op
}

// An API-executed stop still open under a request's key, with no turn's op
// to answer it, is a stop nothing runs while the lane runs stops: a same-key
// retry is 409 op_already_failed naming the op and asking for a new key, not
// in_flight, and a stop under a new key goes to the lane.
func TestStopUnderLaneOpenLegacyStopUnderTheKeyAsksForANewKey(t *testing.T) {
	e, st, info := laneStopEnv(t, outcome(opjournal.ArmSucceeded, ""))
	legacy := proposeLegacyStop(t, st.ops, info.ID, "stop-oj4", generationOf(t, e.store, info.ID), "")
	rec := e.post(t, "/session/"+info.ID+"/stop", "stop-oj4", "")
	if body := rec.Body.String(); rec.Code != http.StatusConflict || !strings.Contains(body, opAlreadyFailedCode) ||
		!strings.Contains(body, legacy.ID) || !strings.Contains(body, "new Idempotency-Key") {
		t.Fatalf("same-key stop: %d %s, want 409 %s naming op %s and a new key", rec.Code, body, opAlreadyFailedCode, legacy.ID)
	}
	fresh := e.post(t, "/session/"+info.ID+"/stop", "stop-new", "")
	wantStatus(t, "new key", fresh, http.StatusOK)
	a := decodeCommandAnswer(t, fresh)
	if a.Op == nil || a.IdempotencyKey != "stop-new" {
		t.Fatalf("new-key answer = %+v", a)
	}
	if op, err := st.ops.Get(a.Op.ID); err != nil || op.Executor != opjournal.ExecutorLane || op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("new-key op = %+v, %v; want a succeeded lane op", op, err)
	}
}

// A named stop whose turn's target key names an OJ-4 stop still open, which
// nothing runs while the lane runs stops, does not wait on it: it stops the
// turn under its own key, as after a failed stop of the turn.
func TestNamedStopUnderLaneDoesNotWaitOnAnOpenLegacyStopOfItsTurn(t *testing.T) {
	e, st, info := laneStopEnv(t, outcome(opjournal.ArmSucceeded, ""))
	gen := generationOf(t, e.store, info.ID)
	targetKey, err := opjournal.InterruptKey(gen, "t-1")
	if err != nil {
		t.Fatal(err)
	}
	legacy := proposeLegacyStop(t, st.ops, info.ID, targetKey, gen, "t-1")
	rec := e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`)
	wantStatus(t, "named stop", rec, http.StatusOK)
	a := decodeCommandAnswer(t, rec)
	if a.Op == nil || a.Op.ID == legacy.ID || a.IdempotencyKey != "click-1" || a.Replayed {
		t.Fatalf("answer = %+v, want a new op under the request's key, not the open legacy op %s", a, legacy.ID)
	}
	op, err := st.ops.Get(a.Op.ID)
	if err != nil || op.Executor != opjournal.ExecutorLane || op.Scope.Turn != "t-1" || op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v, %v; want a succeeded lane stop of turn t-1", op, err)
	}
}

// restartSession moves session id to its next generation, as a restart does
// once the lane has read its scope.
func restartSession(t *testing.T, store beads.Store, id string) {
	t.Helper()
	gen := generationOf(t, store, id)
	if err := store.SetMetadata(id, "generation", strconv.FormatInt(gen+1, 10)); err != nil {
		t.Fatal(err)
	}
}

// An unnamed /stop whose session restarts after the stop read its scope has
// its op fenced off by the lane, skipped{stale}, unrun, so the new
// incarnation was never interrupted: the stop carries on to it within the
// same bound, under the stale op's carry-on key, and answers from that op. A
// same-key retry follows the stale op to it.
func TestStopUnderLaneCarriesOnToARestartedSession(t *testing.T) {
	var restart func() int64
	e, st, info := laneStopEnv(t, func(opjournal.Op) *opjournal.Outcome {
		if restart != nil {
			gen := restart()
			restart = nil
			return fencedOff(gen)
		}
		return &opjournal.Outcome{Arm: opjournal.ArmSucceeded}
	})
	gen := generationOf(t, e.store, info.ID)
	restart = func() int64 {
		restartSession(t, e.store, info.ID)
		return gen + 1
	}
	rec := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	wantStatus(t, "stop", rec, http.StatusOK)
	staleID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	if stale, err := st.ops.Get(staleID); err != nil || stale.Outcome.Reason != opjournal.ReasonStale || stale.Scope.Generation != gen {
		t.Fatalf("the stop's first op = %+v, %v; want it closed stale in generation %d", stale, err, gen)
	}
	a := decodeCommandAnswer(t, rec)
	if a.Op == nil || a.Op.ID == staleID || a.IdempotencyKey != restartedStopKey(staleID) || a.Replayed {
		t.Fatalf("answer = %+v, want a new op under %s", a, restartedStopKey(staleID))
	}
	op, err := st.ops.Get(a.Op.ID)
	if err != nil || op.Executor != opjournal.ExecutorLane || op.Scope != (opjournal.Scope{Kind: opjournal.ScopeSession, Generation: gen + 1}) ||
		op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("carried-on op = %+v, %v; want a succeeded lane stop of generation %d", op, err, gen+1)
	}
	e.restart(t)
	e.resolver.state = st
	retry := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	wantStatus(t, "retry", retry, http.StatusOK)
	if b := decodeCommandAnswer(t, retry); b.Op == nil || b.Op.ID != op.ID || !b.Replayed {
		t.Fatalf("retry = %+v, want op %s replayed", b, op.ID)
	}
	if rows := e.opRows(t); len(rows) != 2 {
		t.Fatalf("op rows = %d, want the stale op and the one it carried on to", len(rows))
	}
}

// The lane fences an op against its effective incarnation, its runtime row's
// when that leads the session row: an unnamed stop's op fenced off so records
// the runtime row's generation while the session row still reads the old one.
// The stop carries on at that recorded generation, which the lane holds until
// its view catches up and then runs, within the same bound, and not at the
// lagging session row's, which the lane would fence off again until the bound.
func TestStopUnderLaneCarriesOnAtTheFencingGenerationWhileTheSessionRowLags(t *testing.T) {
	old := laneControlAnswerBound
	laneControlAnswerBound = 2 * time.Second
	t.Cleanup(func() { laneControlAnswerBound = old })
	var lane *staleViewLane
	e, st, info := laneStopEnv(t, func(op opjournal.Op) *opjournal.Outcome { return lane.outcome(op) })
	gen := generationOf(t, e.store, info.ID)
	lane = newStaleViewLane(t, e.store, info.ID, gen+1)
	st.onAwait = func(id string) {
		if lane.catchUp(id) {
			st.PokeConversationLane(info.ID)
		}
	}
	rec := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	wantStatus(t, "stop", rec, http.StatusOK)
	staleID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	if stale, err := st.ops.Get(staleID); err != nil || stale.Scope.Generation != gen || !opjournal.FencedOff(stale) || stale.Outcome.Result.Generation != gen+1 {
		t.Fatalf("the stop's first op = %+v, %v; want it scoped at generation %d and fenced off by %d", stale, err, gen, gen+1)
	}
	carryID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", restartedStopKey(staleID))
	if err != nil {
		t.Fatal(err)
	}
	if a := decodeCommandAnswer(t, rec); a.Op == nil || a.Op.ID != carryID || a.Replayed {
		t.Fatalf("answer = %+v, want the carry-on %s", a, carryID)
	}
	op, err := st.ops.Get(carryID)
	if err != nil || op.Scope != (opjournal.Scope{Kind: opjournal.ScopeSession, Generation: gen + 1}) ||
		op.Outcome.Arm != opjournal.ArmSucceeded || !lane.wasHeld(carryID) {
		t.Fatalf("carry-on = %+v, %v (held %t); want a lane stop of generation %d, held until the view caught up, then run", op, err, lane.wasHeld(carryID), gen+1)
	}
	if n := len(e.opRows(t)); n != 2 {
		t.Fatalf("op rows = %d, want the fenced op and its one carry-on", n)
	}
}

// An unnamed stop's op that closed skipped{stale} answers "nothing to stop"
// when the stop does not carry on: the executor ran it and found no turn,
// even when the session restarted since, or the lane fenced it off unrun
// after a restart into an incarnation that runs none, which the stop
// records under the stale op's carry-on key as an API-executed row closed
// skipped{not_needed}. A same-key retry after the session restarted again,
// with a turn, replays that answer: it never stops a turn that began after
// the stop answered.
func TestStopUnderLaneStaleWithNoRestartedTurnIsNothingToStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		// onPass is the lane's pass over the stop's op, which it closes with
		// the outcome onPass returns.
		onPass func(t *testing.T, e *opsEnv, info session.Info) *opjournal.Outcome
		// carried: the stop answers from the row under the stale op's
		// carry-on key.
		carried bool
	}{
		{"no turn found", func(*testing.T, *opsEnv, session.Info) *opjournal.Outcome {
			return &opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
		}, false},
		{"no turn found, then restarted with a turn", func(t *testing.T, e *opsEnv, info session.Info) *opjournal.Outcome {
			restartSession(t, e.store, info.ID)
			return &opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
		}, false},
		{"fenced off, restarted with no turn", func(t *testing.T, e *opsEnv, info session.Info) *opjournal.Outcome {
			restartSession(t, e.store, info.ID)
			if err := e.fs.sp.Stop(info.SessionName); err != nil {
				t.Fatal(err)
			}
			return fencedOff(generationOf(t, e.store, info.ID))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var onPass func() *opjournal.Outcome
			e, st, info := laneStopEnv(t, func(opjournal.Op) *opjournal.Outcome { return onPass() })
			onPass = func() *opjournal.Outcome { return tc.onPass(t, e, info) }
			rec := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
			wantStatus(t, "stop", rec, http.StatusOK)
			a := decodeCommandAnswer(t, rec)
			staleID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", "stop-1")
			if err != nil {
				t.Fatal(err)
			}
			answerID, rows := staleID, 1
			if tc.carried {
				if answerID, err = opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", restartedStopKey(staleID)); err != nil {
					t.Fatal(err)
				}
				rows = 2
				row, err := st.ops.Get(answerID)
				if err != nil || row.Executor == opjournal.ExecutorLane || row.Status != opjournal.StatusClosed ||
					row.Outcome.Arm != opjournal.ArmSkipped || row.Outcome.Reason != opjournal.ReasonNotNeeded {
					t.Fatalf("carry-on row = %+v, %v; want an API-executed row closed skipped{not_needed}", row, err)
				}
			}
			if a.Op == nil || a.Op.ID != answerID || a.Replayed {
				t.Fatalf("answer = %+v, want op %s", a, answerID)
			}
			// The session restarts, with a turn, after the stop answered.
			restartSession(t, e.store, info.ID)
			if !e.fs.sp.IsRunning(info.SessionName) {
				if err := e.fs.sp.Start(context.Background(), info.SessionName, runtime.Config{}); err != nil {
					t.Fatal(err)
				}
			}
			e.restart(t)
			e.resolver.state = st
			replay := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
			wantStatus(t, "replay", replay, http.StatusOK)
			if b := decodeCommandAnswer(t, replay); b.Op == nil || b.Op.ID != answerID || !b.Replayed {
				t.Fatalf("replay = %+v, want op %s replayed", b, answerID)
			}
			if got := len(e.opRows(t)); got != rows {
				t.Fatalf("op rows = %d, want %d: no stop of the turn that began after the answer", got, rows)
			}
		})
	}
}

// An unnamed stop that answered in_flight, its op still open at the bound,
// and whose op the lane then fenced off unrun after the session restarted,
// never ran: a same-key retry carries it on to the session's new
// incarnation, under the stale op's carry-on key, rather than replay
// "nothing to stop". When that incarnation runs no turn, the retry records
// so under the carry-on key. A later retry, after another restart into an
// incarnation with a turn, replays the carry-on either way.
func TestStopUnderLaneRetryCarriesOnAStopFencedOffAfterInFlight(t *testing.T) {
	old := laneControlAnswerBound
	t.Cleanup(func() { laneControlAnswerBound = old })
	for _, tc := range []struct {
		name     string
		withTurn bool
	}{
		{"restarted with a turn", true},
		{"restarted with no turn", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			laneControlAnswerBound = 50 * time.Millisecond
			e, st, info := laneStopEnv(t, nil)
			gen := generationOf(t, e.store, info.ID)
			first := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
			if first.Code != http.StatusConflict || !strings.Contains(first.Body.String(), "in_flight") {
				t.Fatalf("first stop: %d %s, want 409 in_flight", first.Code, first.Body.String())
			}
			staleID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", "stop-1")
			if err != nil {
				t.Fatal(err)
			}
			// The session restarts, and the lane fences the op off unrun.
			restartSession(t, e.store, info.ID)
			if !tc.withTurn {
				if err := e.fs.sp.Stop(info.SessionName); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.ops.Close(staleID, *fencedOff(gen + 1), "lane:ctl"); err != nil {
				t.Fatalf("fencing the op off: %v", err)
			}
			st.onPoke = func(conv string) { scriptLanePass(t, st.ops, conv, outcome(opjournal.ArmSucceeded, "")) }
			laneControlAnswerBound = 2 * time.Second
			e.restart(t)
			e.resolver.state = st
			retry := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
			wantStatus(t, "retry", retry, http.StatusOK)
			carryKey := restartedStopKey(staleID)
			carryID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", carryKey)
			if err != nil {
				t.Fatal(err)
			}
			if a := decodeCommandAnswer(t, retry); a.Op == nil || a.Op.ID != carryID || a.IdempotencyKey != carryKey || a.Replayed {
				t.Fatalf("retry = %+v, want a new op %s under %s", a, carryID, carryKey)
			}
			op, err := st.ops.Get(carryID)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.withTurn && (op.Executor != opjournal.ExecutorLane || op.Scope != (opjournal.Scope{Kind: opjournal.ScopeSession, Generation: gen + 1}) ||
				op.Outcome.Arm != opjournal.ArmSucceeded):
				t.Fatalf("carry-on = %+v; want a succeeded lane stop of generation %d", op, gen+1)
			case !tc.withTurn && (op.Executor == opjournal.ExecutorLane || op.Status != opjournal.StatusClosed ||
				op.Outcome.Arm != opjournal.ArmSkipped || op.Outcome.Reason != opjournal.ReasonNotNeeded):
				t.Fatalf("carry-on = %+v; want an API-executed row closed skipped{not_needed}", op)
			}
			// The session restarts again, with a turn: a later retry
			// replays the carry-on and stops nothing more.
			restartSession(t, e.store, info.ID)
			if !e.fs.sp.IsRunning(info.SessionName) {
				if err := e.fs.sp.Start(context.Background(), info.SessionName, runtime.Config{}); err != nil {
					t.Fatal(err)
				}
			}
			e.restart(t)
			e.resolver.state = st
			again := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
			wantStatus(t, "later retry", again, http.StatusOK)
			if b := decodeCommandAnswer(t, again); b.Op == nil || b.Op.ID != carryID || !b.Replayed {
				t.Fatalf("later retry = %+v, want op %s replayed", b, carryID)
			}
			if n := len(e.opRows(t)); n != 2 {
				t.Fatalf("op rows = %d, want the fenced op and its carry-on", n)
			}
		})
	}
}

// A stop that found no turn records so in two writes: an API-executed row
// whose causation says so (noTurnStopCausation), then its close
// skipped{not_needed}. When the close does not land (it failed, or the API
// died between the writes), the stop answers 503 and the row stays open,
// until the sweep closes it failed{deadline_in_flight} once its finish_by
// passes. A same-key retry finishes the row as the stop would have and
// answers 200, the stop's answer, rather than 409 op_already_failed, which
// asks for a new key that would stop a turn begun since. It reads no turn
// again, so the turn begun since is not stopped. The row a stop records under
// its fenced-off op's carry-on key is finished the same way.
func TestStopUnderLaneSameKeyRetryFinishesANoTurnRowItsCloseMissed(t *testing.T) {
	old := laneControlAnswerBound
	t.Cleanup(func() { laneControlAnswerBound = old })
	// leftOpen runs a stop under key stop-1 that finds no turn and whose
	// row's close fails, and returns the key the row is under and how many
	// op rows the stop wrote.
	type leftOpen func(t *testing.T, e *opsEnv, st *lanePokeState, info session.Info) (key string, rows int)
	closeFails := func(t *testing.T, e *opsEnv, info session.Info) {
		t.Helper()
		e.store.set(func(s *opsTestStore) { s.updateFails = 1 })
		rec := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "op_journal_unavailable") {
			t.Fatalf("stop: %d %s, want 503 op_journal_unavailable", rec.Code, rec.Body.String())
		}
	}
	var direct leftOpen = func(t *testing.T, e *opsEnv, _ *lanePokeState, info session.Info) (string, int) {
		if err := e.fs.sp.Stop(info.SessionName); err != nil {
			t.Fatal(err)
		}
		closeFails(t, e, info)
		return "stop-1", 1
	}
	// carriedOn: the stop answered in_flight, then the lane fenced its op off
	// after the session restarted into an incarnation with no turn, so the
	// retry records that under the op's carry-on key.
	var carriedOn leftOpen = func(t *testing.T, e *opsEnv, st *lanePokeState, info session.Info) (string, int) {
		laneControlAnswerBound = 50 * time.Millisecond
		gen := generationOf(t, e.store, info.ID)
		first := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
		if first.Code != http.StatusConflict || !strings.Contains(first.Body.String(), "in_flight") {
			t.Fatalf("first stop: %d %s, want 409 in_flight", first.Code, first.Body.String())
		}
		staleID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", "stop-1")
		if err != nil {
			t.Fatal(err)
		}
		restartSession(t, e.store, info.ID)
		if err := e.fs.sp.Stop(info.SessionName); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ops.Close(staleID, *fencedOff(gen + 1), "lane:ctl"); err != nil {
			t.Fatalf("fencing the op off: %v", err)
		}
		laneControlAnswerBound = 2 * time.Second
		closeFails(t, e, info)
		return restartedStopKey(staleID), 2
	}
	for _, tc := range []struct {
		name  string
		leave leftOpen
		swept bool
	}{
		{"direct, left open", direct, false},
		{"direct, swept", direct, true},
		{"carried on, left open", carriedOn, false},
		{"carried on, swept", carriedOn, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			laneControlAnswerBound = 2 * time.Second
			// The lane here closes nothing, so a retry that proposed a lane
			// op would answer in_flight at the bound.
			e, st, info := laneStopEnv(t, nil)
			key, rows := tc.leave(t, e, st, info)
			rowID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", key)
			if err != nil {
				t.Fatal(err)
			}
			if row, err := st.ops.Get(rowID); err != nil || row.Status == opjournal.StatusClosed || row.Executor == opjournal.ExecutorLane ||
				row.Causation != noTurnStopCausation {
				t.Fatalf("row under %s = %+v, %v; want an open API-executed row recording a stop that found no turn", key, row, err)
			}
			want := opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonNotNeeded}
			if tc.swept {
				want = opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonDeadlineInFlight}
				if _, err := st.ops.Close(rowID, want, "sweep"); err != nil {
					t.Fatalf("sweeping the row: %v", err)
				}
			}
			// A turn begins before the retry.
			if err := e.fs.sp.Start(context.Background(), info.SessionName, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			e.restart(t)
			e.resolver.state = st
			retry := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
			wantStatus(t, "retry", retry, http.StatusOK)
			if a := decodeCommandAnswer(t, retry); a.Op == nil || a.Op.ID != rowID || a.IdempotencyKey != key || !a.Replayed {
				t.Fatalf("retry = %+v, want row %s replayed under %s", a, rowID, key)
			}
			if row, err := st.ops.Get(rowID); err != nil || row.Status != opjournal.StatusClosed || !row.Outcome.Equal(want) {
				t.Fatalf("row = %+v, %v; want it closed %+v", row, err, want)
			}
			if n := len(e.opRows(t)); n != rows {
				t.Fatalf("op rows = %d, want %d: no stop of the turn that began after", n, rows)
			}
			if n := runtimeInterrupts(e.fs.sp, info.SessionName); n != 0 {
				t.Fatalf("the API sent %d interrupts itself", n)
			}
		})
	}
}

// An unnamed stop whose op the lane fenced off unrun carries on only until
// that op's finish_by, the latest the stop was to finish by. A same-key
// retry after it that finds no carry-on proposes nothing: it is 409
// op_already_failed naming the fenced op, so a stale key never stops the
// turn running when it comes back. One that finds a carry-on follows it,
// however late.
func TestStopUnderLaneRetryPastAFencedOpsFinishByCarriesNothingOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// carried: an earlier request carried the stop on, and its op
		// succeeded.
		carried bool
	}{
		{"never carried on", false},
		{"carried on before", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The lane here succeeds every lane op, so a carry-on would
			// answer 200.
			e, _, info := laneStopEnv(t, outcome(opjournal.ArmSucceeded, ""))
			gen := generationOf(t, e.store, info.ID)
			// The stop's op was proposed an hour ago, and the lane fenced it
			// off unrun once the session restarted.
			hourAgo, err := opjournal.NewStore(beads.SessionStore{Store: e.store.MemStore}, &clock.Fake{Time: time.Now().Add(-time.Hour)}, opjournal.WithIDPrefix("gc"))
			if err != nil {
				t.Fatal(err)
			}
			stop := opjournal.Request{
				Conversation: info.ID, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "stop-1",
				Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
				Policy:   interruptOpPolicy,
				Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: gen},
			}
			fenced, _, err := hourAgo.Propose(stop)
			if err != nil {
				t.Fatalf("proposing the stop's op: %v", err)
			}
			restartSession(t, e.store, info.ID)
			if _, err := hourAgo.Close(fenced.ID, *fencedOff(gen + 1), "lane:ctl"); err != nil {
				t.Fatalf("fencing the op off: %v", err)
			}
			rows := 1
			var carry opjournal.Op
			if tc.carried {
				req := stop
				req.ClientKey, req.Scope.Generation = restartedStopKey(fenced.ID), gen+1
				if carry, _, err = hourAgo.Propose(req); err != nil {
					t.Fatalf("proposing the carry-on: %v", err)
				}
				if carry, err = hourAgo.Close(carry.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:ctl"); err != nil {
					t.Fatalf("closing the carry-on: %v", err)
				}
				rows = 2
			}
			rec := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
			if tc.carried {
				wantStatus(t, "retry", rec, http.StatusOK)
				if a := decodeCommandAnswer(t, rec); a.Op == nil || a.Op.ID != carry.ID || !a.Replayed {
					t.Fatalf("retry = %+v, want the carry-on %s replayed", a, carry.ID)
				}
			} else if body := rec.Body.String(); rec.Code != http.StatusConflict || !strings.Contains(body, opAlreadyFailedCode) ||
				!strings.Contains(body, fenced.ID) || !strings.Contains(body, "new Idempotency-Key") {
				t.Fatalf("retry: %d %s, want 409 %s naming op %s and a new key", rec.Code, body, opAlreadyFailedCode, fenced.ID)
			}
			if n := len(e.opRows(t)); n != rows {
				t.Fatalf("op rows = %d, want %d: no stop carried on past the fenced op's finish_by", n, rows)
			}
			if n := runtimeInterrupts(e.fs.sp, info.SessionName); n != 0 {
				t.Fatalf("the API sent %d interrupts itself", n)
			}
		})
	}
}

// Every carry-on of a stop is due by the stop's own finish_by, so a chain of
// them ends there: a carry-on a retry made before that finish_by, which the
// lane then fenced off too after another restart, is followed by a retry
// after it to nothing more, 409 op_already_failed and no third op, though the
// carry-on was proposed after the stop and the session runs a turn.
func TestStopUnderLaneCarryOnsAreDueByTheStopsFinishBy(t *testing.T) {
	old := laneControlAnswerBound
	t.Cleanup(func() { laneControlAnswerBound = old })
	// The lane here closes nothing; the test fences the ops off itself.
	e, st, info := laneStopEnv(t, nil)
	e.server(t)
	synctest.Test(t, func(t *testing.T) {
		laneControlAnswerBound = 50 * time.Millisecond
		// fence restarts the session and fences the op under key off, unrun.
		fence := func(key string) opjournal.Op {
			t.Helper()
			id, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", key)
			if err != nil {
				t.Fatal(err)
			}
			restartSession(t, e.store, info.ID)
			op, err := st.ops.Close(id, *fencedOff(generationOf(t, e.store, info.ID)), "lane:ctl")
			if err != nil {
				t.Fatalf("fencing op %s off: %v", id, err)
			}
			return op
		}
		inFlight := func(what string) {
			t.Helper()
			rec := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in_flight") {
				t.Fatalf("%s: %d %s, want 409 in_flight", what, rec.Code, rec.Body.String())
			}
		}
		inFlight("stop")
		stop := fence("stop-1")
		<-time.After(2 * time.Minute)
		inFlight("retry before the stop's finish_by")
		carry := fence(restartedStopKey(stop.ID))
		if !carry.FinishBy.Equal(stop.FinishBy) {
			t.Fatalf("carry-on finish_by %s, want the stop's %s", carry.FinishBy, stop.FinishBy)
		}
		<-time.After(2 * time.Minute)
		late := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
		if body := late.Body.String(); late.Code != http.StatusConflict || !strings.Contains(body, opAlreadyFailedCode) || !strings.Contains(body, carry.ID) {
			t.Fatalf("retry after the stop's finish_by: %d %s, want 409 %s naming the carry-on %s", late.Code, body, opAlreadyFailedCode, carry.ID)
		}
		if n := len(e.opRows(t)); n != 2 {
			t.Fatalf("op rows = %d, want the stop's op and its one carry-on", n)
		}
		if n := runtimeInterrupts(e.fs.sp, info.SessionName); n != 0 {
			t.Fatalf("the API sent %d interrupts itself", n)
		}
	})
}

// slowLiveness is a FullFake whose liveness read takes wait, as a runtime slow
// to answer does; in a synctest bubble the wait passes at once.
type slowLiveness struct {
	runtime.FullFake
	wait time.Duration
}

func (s slowLiveness) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	<-time.After(s.wait)
	return s.FullFake.ObserveLivenessWithError(name, processNames)
}

// A stop is carried on only until the fenced op's finish_by, also when that
// passes while the stop reads the session's runtime, after it found the
// finish_by still ahead: the journal refuses a carry-on due by a finish_by
// already past, and the stop answers that as a retry after it would, 409
// op_already_failed naming the fenced op, nothing written.
func TestStopUnderLaneCarryOnWhoseFinishByPassesMeanwhileIsRefused(t *testing.T) {
	old := laneControlAnswerBound
	t.Cleanup(func() { laneControlAnswerBound = old })
	// The lane here closes nothing; the test fences the op off itself.
	e, st, info := laneStopEnv(t, nil)
	e.server(t)
	synctest.Test(t, func(t *testing.T) {
		laneControlAnswerBound = 50 * time.Millisecond
		gen := generationOf(t, e.store, info.ID)
		first := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
		if first.Code != http.StatusConflict || !strings.Contains(first.Body.String(), "in_flight") {
			t.Fatalf("stop: %d %s, want 409 in_flight", first.Code, first.Body.String())
		}
		staleID, err := opjournal.DeriveID(st.ops.Prefix(), info.ID, "operator", "stop-1")
		if err != nil {
			t.Fatal(err)
		}
		restartSession(t, e.store, info.ID)
		stale, err := st.ops.Close(staleID, *fencedOff(gen + 1), "lane:ctl")
		if err != nil {
			t.Fatalf("fencing the op off: %v", err)
		}
		// The retry comes a second before the op's finish_by, and the
		// session's runtime answers it two seconds later.
		<-time.After(time.Until(stale.FinishBy) - time.Second)
		e.fs.sessionProvider = slowLiveness{FullFake: runtime.FullFake{Fake: e.fs.sp}, wait: 2 * time.Second}
		retry := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
		if body := retry.Body.String(); retry.Code != http.StatusConflict || !strings.Contains(body, opAlreadyFailedCode) || !strings.Contains(body, staleID) {
			t.Fatalf("retry: %d %s, want 409 %s naming the fenced op %s", retry.Code, body, opAlreadyFailedCode, staleID)
		}
		if n := len(e.opRows(t)); n != 1 {
			t.Fatalf("op rows = %d, want the fenced op alone", n)
		}
		if n := runtimeInterrupts(e.fs.sp, info.SessionName); n != 0 {
			t.Fatalf("the API sent %d interrupts itself", n)
		}
	})
}

// runCancelLaneEnv is newRunCancelInterruptFixture's city with interrupt
// listed. Its lane host's passes close a poked holder's open interrupt ops
// through script's function for that holder, else with closeWith's outcome
// for it, succeeded when it has neither, and leave a held holder's open.
type runCancelLaneEnv struct {
	runCancelInterruptFixture
	ops       *opjournal.Store
	s         *Server
	lane      *lanePokeState
	closeWith map[string]opjournal.Outcome
	script    map[string]func(opjournal.Op) *opjournal.Outcome
	held      map[string]bool
}

func newRunCancelLaneEnv(t *testing.T) *runCancelLaneEnv {
	t.Helper()
	e := &runCancelLaneEnv{
		runCancelInterruptFixture: newRunCancelInterruptFixture(t),
		closeWith:                 map[string]opjournal.Outcome{},
		script:                    map[string]func(opjournal.Op) *opjournal.Outcome{},
		held:                      map[string]bool{},
	}
	mem, ok := e.fs.cityBeadStore.(*beads.MemStore)
	if !ok {
		t.Fatalf("city bead store is %T, want *beads.MemStore", e.fs.cityBeadStore)
	}
	mem.HonorExplicitIDs = true
	e.fs.sessionsBeadStore = prefixedStore{MemStore: mem}
	e.ops = newOpsTestStoreOver(t, e.fs)
	st := &lanePokeState{fakeState: e.fs, kinds: []string{"interrupt"}, ops: e.ops}
	var mu sync.Mutex
	st.onPoke = func(conv string) {
		// Holders' awaits run concurrently, and so may their proposals'
		// pokes.
		mu.Lock()
		defer mu.Unlock()
		if e.held[conv] {
			return
		}
		if script, ok := e.script[conv]; ok {
			scriptLanePass(t, e.ops, conv, script)
			return
		}
		o, ok := e.closeWith[conv]
		if !ok {
			o = opjournal.Outcome{Arm: opjournal.ArmSucceeded}
		}
		scriptLanePass(t, e.ops, conv, func(opjournal.Op) *opjournal.Outcome { return &o })
	}
	e.lane = st
	e.s = New(st)
	return e
}

// interruptHolders drives interruptRunHoldersLane over the run's two live
// holders directly: a later cancel of the run reaches its holders again only
// after an earlier one failed mid-wind-down (the root left open). It returns
// the holders interrupted, sorted, and the failures by holder.
func (e *runCancelLaneEnv) interruptHolders(t *testing.T) ([]string, map[string]error) {
	t.Helper()
	failed := map[string]error{}
	holders := []string{e.stepWorker.ID, e.itemWorker.ID}
	got := e.s.interruptRunHoldersLane(context.Background(), e.fs.cityBeadStore, e.runID, holders, func(id string, err error) { failed[id] = err })
	slices.Sort(got)
	return got, failed
}

// runCancelOp is the op under holder id's run-cancel key of its current
// generation.
func (e *runCancelLaneEnv) runCancelOp(t *testing.T, id string) opjournal.Op {
	t.Helper()
	return e.opUnder(t, id, runCancelLaneKey(e.runID, generationOf(t, e.fs.cityBeadStore, id)))
}

// opUnder is the op under key, an operator's client key, on holder id.
func (e *runCancelLaneEnv) opUnder(t *testing.T, id, key string) opjournal.Op {
	t.Helper()
	op, err := e.ops.Get(e.opID(t, id, key))
	if err != nil {
		t.Fatalf("session %s's op under key %q: %v", id, key, err)
	}
	return op
}

// opID is the id of the op under key, an operator's client key, on holder
// id.
func (e *runCancelLaneEnv) opID(t *testing.T, id, key string) string {
	t.Helper()
	opID, err := opjournal.DeriveID(e.ops.Prefix(), id, "operator", key)
	if err != nil {
		t.Fatal(err)
	}
	return opID
}

// holderOps is every op of holder id, closed ones included.
func (e *runCancelLaneEnv) holderOps(t *testing.T, id string) []opjournal.Op {
	t.Helper()
	rows, err := e.fs.cityBeadStore.List(beads.ListQuery{Label: opjournal.LabelOp, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list op rows: %v", err)
	}
	var ops []opjournal.Op
	for _, row := range rows {
		op, err := opjournal.DecodeOp(row)
		if err != nil {
			t.Fatalf("decode op row %s: %v", row.ID, err)
		}
		if op.Conversation == id {
			ops = append(ops, op)
		}
	}
	return ops
}

// A run cancel under lane authority proposes one interrupt op per live
// holder session, keyed run-cancel:<run id>:<generation> with the run as
// causation, and reports from their outcomes; it interrupts nothing itself.
func TestRunCancelUnderLaneProposesOneInterruptPerHolder(t *testing.T) {
	e := newRunCancelLaneEnv(t)
	e.closeWith[e.itemWorker.ID] = opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonNoBoundary}
	out, err := e.s.humaHandleRunCancel(context.Background(), &RunCancelInput{CityScope: CityScope{CityName: "test-city"}, RunID: e.runID})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !slices.Equal(out.Body.Interrupted, []string{e.stepWorker.ID}) {
		t.Fatalf("interrupted = %v, want [%s] (failures %+v)", out.Body.Interrupted, e.stepWorker.ID, out.Body.InterruptFailures)
	}
	if len(out.Body.InterruptFailures) != 1 || out.Body.InterruptFailures[0].Session != e.itemWorker.ID || !strings.Contains(out.Body.InterruptFailures[0].Error, opjournal.ReasonNoBoundary) {
		t.Fatalf("failures = %+v, want the item worker's no_boundary", out.Body.InterruptFailures)
	}
	for _, owner := range []session.Info{e.stepWorker, e.itemWorker} {
		if n := interruptCount(e.fs.sp, owner.SessionName); n != 0 {
			t.Fatalf("session %s interrupted %d times by the API itself", owner.ID, n)
		}
		if op := e.runCancelOp(t, owner.ID); op.Kind != opjournal.KindInterrupt || op.Executor != opjournal.ExecutorLane || op.Causation != e.runID {
			t.Fatalf("session %s op = %+v", owner.ID, op)
		}
		if n := len(e.holderOps(t, owner.ID)); n != 1 {
			t.Fatalf("session %s has %d ops, want its one interrupt", owner.ID, n)
		}
	}
	for _, bystander := range []session.Info{e.teardownWorker, e.otherRunWorker, e.goneWorker} {
		if open, _ := e.ops.ListOpen(bystander.ID); len(open) != 0 {
			t.Fatalf("bystander %s got ops %+v", bystander.ID, open)
		}
	}
}

// Every cancel of a run interrupts each live holder again, as legacy's
// StopTurn of every live holder on every cancel does: an op that closed
// before a cancel reached it, however it closed (succeeded, failed,
// canceled, or skipped: no turn to stop then), is followed by its retry, so
// a holder that started another turn since is interrupted too. A holder's
// ops form one chain, its first op then one retry per later cancel.
func TestRunCancelUnderLaneInterruptsEachLiveHolderOnEveryCancel(t *testing.T) {
	for _, first := range []opjournal.Outcome{
		{Arm: opjournal.ArmSucceeded},
		{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonNoBoundary},
		{Arm: opjournal.ArmCanceled},
		{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale},
		{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonNotNeeded},
	} {
		t.Run(string(first.Arm)+"/"+first.Reason, func(t *testing.T) {
			e := newRunCancelLaneEnv(t)
			item := e.itemWorker.ID
			e.closeWith[item] = first
			got, failed := e.interruptHolders(t)
			wantInterrupted := first.Arm == opjournal.ArmSucceeded
			wantFailure := first.Arm == opjournal.ArmFailed || first.Arm == opjournal.ArmCanceled
			if slices.Contains(got, item) != wantInterrupted || (failed[item] != nil) != wantFailure {
				t.Fatalf("first interrupt interrupted %v, failures %v; want the item worker interrupted %t, a failure %t", got, failed, wantInterrupted, wantFailure)
			}
			delete(e.closeWith, item)
			want := []string{e.stepWorker.ID, item}
			slices.Sort(want)
			for _, call := range []string{"second", "third"} {
				got, failed = e.interruptHolders(t)
				if !slices.Equal(got, want) || len(failed) != 0 {
					t.Fatalf("%s interrupt interrupted %v, failures %v; want %v and none", call, got, failed, want)
				}
			}
			if op := e.runCancelOp(t, item); op.Outcome.Arm != first.Arm {
				t.Fatalf("item worker's first op = %+v; want it left closed %s", op.Outcome, first.Arm)
			}
			for _, holder := range want {
				op := e.runCancelOp(t, holder)
				for range 2 {
					next := e.opUnder(t, holder, runCancelRetryKey(e.runID, op.Scope.Generation, op.ID))
					if next.Outcome.Arm != opjournal.ArmSucceeded || next.Causation != e.runID || next.Scope != op.Scope {
						t.Fatalf("session %s's retry of op %s = %+v; want a succeeded op of the run, scoped as the op it follows", holder, op.ID, next)
					}
					op = next
				}
				if n := len(e.holderOps(t, holder)); n != 3 {
					t.Fatalf("session %s has %d ops, want one per cancel", holder, n)
				}
			}
		})
	}
}

// Cancels that overlap while a holder's interrupt is still open share that
// op, a retry included: each awaits it rather than proposing another
// interrupt of the holder. A cancel walks the holder's ops past every one
// that closed, and the next cancel after one interrupts the holder again.
func TestRunCancelUnderLaneOverlappingCancelsShareTheOpenOp(t *testing.T) {
	old := laneControlAnswerBound
	laneControlAnswerBound = 50 * time.Millisecond
	t.Cleanup(func() { laneControlAnswerBound = old })
	e := newRunCancelLaneEnv(t)
	item := e.itemWorker.ID
	e.held[item] = true
	// overlap runs two cancels at once. Each must report the item worker's
	// op want still running at the bound, and the item worker must then
	// have total ops.
	overlap := func(want string, total int) {
		t.Helper()
		failures := make([]map[string]error, 2)
		var wg sync.WaitGroup
		for i := range failures {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, failures[i] = e.interruptHolders(t)
			}()
		}
		wg.Wait()
		for i, failed := range failures {
			if err := failed[item]; err == nil || !strings.Contains(err.Error(), "op "+want+" still running") {
				t.Fatalf("cancel %d reported the item worker as %v, want op %s still running", i, err, want)
			}
		}
		if n := len(e.holderOps(t, item)); n != total {
			t.Fatalf("item worker has %d ops after two overlapping cancels, want %d", n, total)
		}
	}
	closeOp := func(id string, o opjournal.Outcome) {
		t.Helper()
		if _, err := e.ops.Close(id, o, "lane:ctl"); err != nil {
			t.Fatalf("closing op %s: %v", id, err)
		}
	}
	gen := generationOf(t, e.fs.cityBeadStore, item)
	first := e.opID(t, item, runCancelLaneKey(e.runID, gen))
	overlap(first, 1)
	closeOp(first, opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonNoBoundary})
	retry := e.opID(t, item, runCancelRetryKey(e.runID, gen, first))
	overlap(retry, 2)
	closeOp(retry, opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale})
	delete(e.held, item)
	prev := retry
	for _, call := range []string{"third", "fourth"} {
		if got, failed := e.interruptHolders(t); !slices.Contains(got, item) || failed[item] != nil {
			t.Fatalf("%s cancel interrupted %v, failures %v; want the item worker interrupted", call, got, failed)
		}
		next := e.opUnder(t, item, runCancelRetryKey(e.runID, gen, prev))
		if next.Outcome.Arm != opjournal.ArmSucceeded {
			t.Fatalf("%s cancel's op = %+v, want it succeeded", call, next.Outcome)
		}
		prev = next.ID
	}
	if n := len(e.holderOps(t, item)); n != 4 {
		t.Fatalf("item worker has %d ops, want two that closed before a cancel reached them and one per later cancel", n)
	}
}

// A holder that restarted since an earlier cancel of the run (its generation
// moved) gets an interrupt of its new incarnation: the run-cancel key
// carries the generation, so the earlier op neither answers for it nor
// refuses its request as key reuse.
func TestRunCancelUnderLaneInterruptsARestartedHolder(t *testing.T) {
	e := newRunCancelLaneEnv(t)
	if got, failed := e.interruptHolders(t); len(got) != 2 || len(failed) != 0 {
		t.Fatalf("first interrupt interrupted %v, failures %v; want both holders", got, failed)
	}
	first := e.runCancelOp(t, e.itemWorker.ID)
	gen := generationOf(t, e.fs.cityBeadStore, e.itemWorker.ID)
	if err := e.fs.cityBeadStore.SetMetadata(e.itemWorker.ID, "generation", strconv.FormatInt(gen+1, 10)); err != nil {
		t.Fatal(err)
	}
	got, failed := e.interruptHolders(t)
	if !slices.Contains(got, e.itemWorker.ID) || len(failed) != 0 {
		t.Fatalf("after the restart interrupted %v, failures %v; want the restarted holder interrupted again", got, failed)
	}
	op := e.runCancelOp(t, e.itemWorker.ID)
	if op.ID == first.ID || op.Scope.Generation != gen+1 || op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("restarted holder's run-cancel op = %+v; want a new succeeded op of generation %d (first %s)", op, gen+1, first.ID)
	}
}

// A holder that restarts while a cancel's interrupt of it is open, after the
// cancel read its generation, has that op fenced off by the lane,
// skipped{stale}, unrun: the cancel then interrupts the new incarnation,
// under its generation's run-cancel key, within the same bound, and reports
// the holder interrupted rather than as one with nothing to stop.
func TestRunCancelUnderLaneInterruptsAHolderThatRestartedMidCancel(t *testing.T) {
	e := newRunCancelLaneEnv(t)
	item := e.itemWorker.ID
	gen := generationOf(t, e.fs.cityBeadStore, item)
	e.script[item] = func(op opjournal.Op) *opjournal.Outcome {
		if op.Scope.Generation == gen {
			restartSession(t, e.fs.cityBeadStore, item)
			return fencedOff(gen + 1)
		}
		return &opjournal.Outcome{Arm: opjournal.ArmSucceeded}
	}
	got, failed := e.interruptHolders(t)
	if !slices.Contains(got, item) || len(failed) != 0 {
		t.Fatalf("interrupted %v, failures %v; want the restarted holder interrupted", got, failed)
	}
	if stale := e.opUnder(t, item, runCancelLaneKey(e.runID, gen)); stale.Outcome.Reason != opjournal.ReasonStale {
		t.Fatalf("the holder's first op = %+v, want it closed stale", stale.Outcome)
	}
	op := e.opUnder(t, item, runCancelLaneKey(e.runID, gen+1))
	if op.Outcome.Arm != opjournal.ArmSucceeded || op.Scope.Generation != gen+1 || op.Causation != e.runID {
		t.Fatalf("the new incarnation's op = %+v; want a succeeded op of the run in generation %d", op, gen+1)
	}
	if n := len(e.holderOps(t, item)); n != 2 {
		t.Fatalf("item worker has %d ops, want the stale one and its new incarnation's", n)
	}
}

// A holder whose interrupt the lane fenced off against its runtime row, which
// leads the holder's session row, is interrupted again at the generation the
// lane recorded: the lane holds that op until its view catches up and then
// runs it, within the same bound, and the cancel reports the holder
// interrupted. An op at the lagging session row's generation would be fenced
// off again until the bound.
func TestRunCancelUnderLaneReinterruptsAtTheFencingGenerationWhileTheSessionRowLags(t *testing.T) {
	old := laneControlAnswerBound
	laneControlAnswerBound = 2 * time.Second
	t.Cleanup(func() { laneControlAnswerBound = old })
	e := newRunCancelLaneEnv(t)
	item := e.itemWorker.ID
	gen := generationOf(t, e.fs.cityBeadStore, item)
	lane := newStaleViewLane(t, e.fs.cityBeadStore, item, gen+1)
	e.script[item] = lane.outcome
	e.lane.onAwait = func(id string) {
		if lane.catchUp(id) {
			e.lane.PokeConversationLane(item)
		}
	}
	got, failed := e.interruptHolders(t)
	if !slices.Contains(got, item) || len(failed) != 0 {
		t.Fatalf("interrupted %v, failures %v; want the item worker interrupted", got, failed)
	}
	if stale := e.opUnder(t, item, runCancelLaneKey(e.runID, gen)); !opjournal.FencedOff(stale) || stale.Outcome.Result.Generation != gen+1 {
		t.Fatalf("the holder's first op = %+v, want it fenced off by generation %d", stale.Outcome, gen+1)
	}
	op := e.opUnder(t, item, runCancelLaneKey(e.runID, gen+1))
	if op.Outcome.Arm != opjournal.ArmSucceeded || op.Scope.Generation != gen+1 || op.Causation != e.runID || !lane.wasHeld(op.ID) {
		t.Fatalf("the re-interrupt = %+v (held %t); want a succeeded op of the run in generation %d, held until the view caught up", op, lane.wasHeld(op.ID), gen+1)
	}
	if n := len(e.holderOps(t, item)); n != 2 {
		t.Fatalf("item worker has %d ops, want the fenced one and its re-interrupt", n)
	}
}

// A run holder whose runtime runs while its agent is dead has no turn to
// interrupt, as the lane's probe reads it (Running && Alive, its liveness
// read with the session's agent process names): the cancel proposes it no
// op, which the lane would hold not_running for the whole bound, and reports
// it neither interrupted nor failed.
func TestRunCancelUnderLaneSkipsAHolderWhoseAgentIsDead(t *testing.T) {
	old := laneControlAnswerBound
	laneControlAnswerBound = 2 * time.Second
	t.Cleanup(func() { laneControlAnswerBound = old })
	e := newRunCancelLaneEnv(t)
	withAgentProcessNames(e.fs)
	item := e.itemWorker
	e.fs.sp.Zombies[item.SessionName] = true
	// An op proposed for it would stay open: a failure at the bound.
	e.held[item.ID] = true
	out, err := e.s.humaHandleRunCancel(context.Background(), &RunCancelInput{CityScope: CityScope{CityName: "test-city"}, RunID: e.runID})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !slices.Equal(out.Body.Interrupted, []string{e.stepWorker.ID}) || len(out.Body.InterruptFailures) != 0 {
		t.Fatalf("interrupted %v, failures %+v; want the step worker alone and no failure", out.Body.Interrupted, out.Body.InterruptFailures)
	}
	if ops := e.holderOps(t, item.ID); len(ops) != 0 {
		t.Fatalf("the dead agent's session got ops %+v, want none", ops)
	}
}

// newOpsTestStoreOver is the op store the API opens over fs's sessions store.
func newOpsTestStoreOver(t *testing.T, fs *fakeState) *opjournal.Store {
	t.Helper()
	st, err := opjournal.NewStore(fs.SessionsBeadStore(), clock.Real{})
	if err != nil {
		t.Fatalf("open op store: %v", err)
	}
	return st
}

// prefixedStore is a bead store whose ids carry the gc prefix, as a city's
// sessions store does.
type prefixedStore struct{ *beads.MemStore }

func (prefixedStore) IDPrefix() string { return "gc" }
