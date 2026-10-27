package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
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

// runtimeInterrupts counts every interrupt the API sent the runtime itself.
func runtimeInterrupts(sp *runtime.Fake, name string) int {
	return sp.CountCalls("Interrupt", name) + sp.CountCalls("InterruptTurn", name) + sp.CountCalls("SendKeys", name)
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
	got, err := s.awaitLaneOp(context.Background(), st.ops, info.ID, op.ID, 50*time.Millisecond)
	if err != nil || got.Status == opjournal.StatusClosed {
		t.Fatalf("await of an open op = %+v, %v; want it open at the bound", got, err)
	}
	st.onAwait = func(id string) {
		_, _ = st.ops.Close(id, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:ctl")
	}
	got, err = s.awaitLaneOp(context.Background(), st.ops, info.ID, op.ID, 2*time.Second)
	if err != nil || got.Status != opjournal.StatusClosed {
		t.Fatalf("await = %+v, %v; want it closed", got, err)
	}
}

// A run cancel under lane authority proposes one interrupt op per live
// holder session, keyed run-cancel:<run id> with the run as causation, and
// reports from their outcomes; it interrupts nothing itself.
func TestRunCancelUnderLaneProposesOneInterruptPerHolder(t *testing.T) {
	f := newRunCancelInterruptFixture(t)
	mem, ok := f.fs.cityBeadStore.(*beads.MemStore)
	if !ok {
		t.Fatalf("city bead store is %T, want *beads.MemStore", f.fs.cityBeadStore)
	}
	mem.HonorExplicitIDs = true
	f.fs.sessionsBeadStore = prefixedStore{MemStore: mem}
	ops := newOpsTestStoreOver(t, f.fs)
	st := &lanePokeState{fakeState: f.fs, kinds: []string{"interrupt"}, ops: ops}
	st.onPoke = func(conv string) {
		reason := ""
		arm := opjournal.ArmSucceeded
		if conv == f.itemWorker.ID {
			arm, reason = opjournal.ArmFailed, opjournal.ReasonNoBoundary
		}
		scriptLanePass(t, ops, conv, outcome(arm, reason))
	}
	s := New(st)
	out, err := s.humaHandleRunCancel(context.Background(), &RunCancelInput{CityScope: CityScope{CityName: "test-city"}, RunID: f.runID})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !slices.Equal(out.Body.Interrupted, []string{f.stepWorker.ID}) {
		t.Fatalf("interrupted = %v, want [%s] (failures %+v)", out.Body.Interrupted, f.stepWorker.ID, out.Body.InterruptFailures)
	}
	if len(out.Body.InterruptFailures) != 1 || out.Body.InterruptFailures[0].Session != f.itemWorker.ID || !strings.Contains(out.Body.InterruptFailures[0].Error, opjournal.ReasonNoBoundary) {
		t.Fatalf("failures = %+v, want the item worker's no_boundary", out.Body.InterruptFailures)
	}
	for _, owner := range []session.Info{f.stepWorker, f.itemWorker} {
		if n := interruptCount(f.fs.sp, owner.SessionName); n != 0 {
			t.Fatalf("session %s interrupted %d times by the API itself", owner.ID, n)
		}
		id, _ := opjournal.DeriveID(ops.Prefix(), owner.ID, "operator", runCancelLaneKey(f.runID))
		op, err := ops.Get(id)
		if err != nil || op.Kind != opjournal.KindInterrupt || op.Executor != opjournal.ExecutorLane || op.Causation != f.runID {
			t.Fatalf("session %s op = %+v, %v", owner.ID, op, err)
		}
	}
	for _, bystander := range []session.Info{f.teardownWorker, f.otherRunWorker, f.goneWorker} {
		if open, _ := ops.ListOpen(bystander.ID); len(open) != 0 {
			t.Fatalf("bystander %s got ops %+v", bystander.ID, open)
		}
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
