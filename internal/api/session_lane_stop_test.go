package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	got, err := s.awaitLaneOp(context.Background(), info.ID, op.ID, 50*time.Millisecond)
	if err != nil || got.Status == opjournal.StatusClosed {
		t.Fatalf("await of an open op = %+v, %v; want it open at the bound", got, err)
	}
	st.onAwait = func(id string) {
		_, _ = st.ops.Close(id, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:ctl")
	}
	got, err = s.awaitLaneOp(context.Background(), info.ID, op.ID, 2*time.Second)
	if err != nil || got.Status != opjournal.StatusClosed {
		t.Fatalf("await = %+v, %v; want it closed", got, err)
	}
}

// runCancelLaneEnv is newRunCancelInterruptFixture's city with interrupt
// listed. Its lane host's passes close a poked holder's open interrupt ops
// with closeWith's outcome for that holder, succeeded when it has none, and
// leave a held holder's open.
type runCancelLaneEnv struct {
	runCancelInterruptFixture
	ops       *opjournal.Store
	s         *Server
	closeWith map[string]opjournal.Outcome
	held      map[string]bool
}

func newRunCancelLaneEnv(t *testing.T) *runCancelLaneEnv {
	t.Helper()
	e := &runCancelLaneEnv{
		runCancelInterruptFixture: newRunCancelInterruptFixture(t),
		closeWith:                 map[string]opjournal.Outcome{},
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
	st.onPoke = func(conv string) {
		if e.held[conv] {
			return
		}
		o, ok := e.closeWith[conv]
		if !ok {
			o = opjournal.Outcome{Arm: opjournal.ArmSucceeded}
		}
		scriptLanePass(t, e.ops, conv, func(opjournal.Op) *opjournal.Outcome { return &o })
	}
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

// A run cancel that finds a live holder's run-cancel op closed without
// interrupting it (failed, canceled, or skipped: no turn to stop then) does
// not replay that outcome: it interrupts the holder again under the missed
// op's retry key, as a /stop retries a failed stop of its turn. One that
// finds the op succeeded answers from it again, proposing nothing, and so
// does a later cancel once the retry succeeded.
func TestRunCancelUnderLaneRetriesAnInterruptThatMissed(t *testing.T) {
	for _, missed := range []opjournal.Outcome{
		{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonNoBoundary},
		{Arm: opjournal.ArmCanceled},
		{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale},
		{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonNotNeeded},
	} {
		t.Run(string(missed.Arm)+"/"+missed.Reason, func(t *testing.T) {
			e := newRunCancelLaneEnv(t)
			e.closeWith[e.itemWorker.ID] = missed
			got, failed := e.interruptHolders(t)
			wantFailure := missed.Arm != opjournal.ArmSkipped
			if slices.Contains(got, e.itemWorker.ID) || (failed[e.itemWorker.ID] != nil) != wantFailure {
				t.Fatalf("first interrupt interrupted %v, failures %v; want the item worker not interrupted (a failure: %t)", got, failed, wantFailure)
			}
			delete(e.closeWith, e.itemWorker.ID)
			want := []string{e.stepWorker.ID, e.itemWorker.ID}
			slices.Sort(want)
			for _, call := range []string{"second", "third"} {
				got, failed = e.interruptHolders(t)
				if !slices.Equal(got, want) || len(failed) != 0 {
					t.Fatalf("%s interrupt interrupted %v, failures %v; want %v and none", call, got, failed, want)
				}
			}
			first := e.runCancelOp(t, e.itemWorker.ID)
			if first.Outcome.Arm != missed.Arm {
				t.Fatalf("item worker's run-cancel op = %+v; want it left closed %s, the retry a new op", first.Outcome, missed.Arm)
			}
			retry := e.opUnder(t, e.itemWorker.ID, runCancelRetryKey(e.runID, first.Scope.Generation, first.ID))
			if retry.Outcome.Arm != opjournal.ArmSucceeded || retry.Causation != e.runID || retry.Scope != first.Scope {
				t.Fatalf("item worker's retry = %+v; want a succeeded op of the run, scoped as the op it retries", retry)
			}
			if n := len(e.holderOps(t, e.stepWorker.ID)); n != 1 {
				t.Fatalf("step worker has %d ops, want its first, replayed by each later cancel", n)
			}
			if n := len(e.holderOps(t, e.itemWorker.ID)); n != 2 {
				t.Fatalf("item worker has %d ops, want the missed op and its one retry", n)
			}
		})
	}
}

// Cancels that overlap while a holder's interrupt is still open share that
// op, a retry included: each awaits it rather than proposing another
// interrupt of the holder. A cancel walks the holder's ops past every one
// that missed: the next retries the last miss, and a later cancel answers
// from that retry once it succeeded.
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
	for _, call := range []string{"retrying", "answering"} {
		if got, failed := e.interruptHolders(t); !slices.Contains(got, item) || failed[item] != nil {
			t.Fatalf("%s cancel interrupted %v, failures %v; want the item worker interrupted", call, got, failed)
		}
	}
	if op := e.opUnder(t, item, runCancelRetryKey(e.runID, gen, retry)); op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("the retry of the missed retry = %+v, want it succeeded", op.Outcome)
	}
	if n := len(e.holderOps(t, item)); n != 3 {
		t.Fatalf("item worker has %d ops, want two misses and the retry that succeeded", n)
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
