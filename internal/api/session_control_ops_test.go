package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The O0 shadow ops on the synchronous session commands (DES-O0API §7,
// §15 OJ-4).

// newControlOpsEnv is an opsEnv whose ops.control and ops.lifecycle gates
// are control and lifecycle (ops.content stays off).
func newControlOpsEnv(t *testing.T, control, lifecycle string) *opsEnv {
	t.Helper()
	t.Setenv("GC_OPS_CONTENT", "")
	t.Setenv("GC_OPS_CONTROL", "")
	t.Setenv("GC_OPS_LIFECYCLE", "")
	fs := newSessionFakeState(t)
	store := newOpsTestStore()
	fs.cityBeadStore = store
	fs.cfg.Ops.ControlMode = control
	fs.cfg.Ops.LifecycleMode = lifecycle
	e := &opsEnv{fs: fs, store: store}
	e.restart(t)
	return e
}

// commandAnswer is a session command's decoded 200 body.
type commandAnswer struct {
	Status         string          `json:"status"`
	ID             string          `json:"id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Replayed       bool            `json:"replayed"`
	Op             *ConversationOp `json:"op"`
}

func decodeCommandAnswer(t *testing.T, rec *httptest.ResponseRecorder) commandAnswer {
	t.Helper()
	var a commandAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return a
}

func wantStatus(t *testing.T, what string, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, rec.Code, code, rec.Body.String())
	}
}

// generationOf is session id's generation.
func generationOf(t *testing.T, store beads.Store, id string) int64 {
	t.Helper()
	scope, why, err := sessionOpScope(store, id)
	if why != "" {
		t.Fatalf("session %s has no generation: %s %v", id, why, err)
	}
	return scope.Generation
}

// I5 for the sync commands: with both gates off, no op row is written and
// every answer is the legacy body in full, keyed or not.
func TestSessionControlOpsOffKeepsTheLegacyWire(t *testing.T) {
	for _, key := range []string{"", "k-off"} {
		e := newControlOpsEnv(t, "off", "off")
		info := e.session(t, "Off")
		e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "ok?"})
		for _, tc := range []struct{ path, body, want string }{
			{"/stop", "", `{"status":"ok","id":"` + info.ID + `"}`},
			{"/respond", `{"request_id":"req-1","action":"approve"}`, `{"status":"accepted","id":"` + info.ID + `"}`},
			{"/suspend", "", `{"status":"ok"}`},
			{"/wake", "", `{"status":"ok","id":"` + info.ID + `"}`},
			{"/reset", "", `{"status":"ok","id":"` + info.ID + `"}`},
			{"/kill", "", `{"status":"ok","id":"` + info.ID + `"}`},
			{"/close", "", `{"status":"ok"}`},
		} {
			rec := e.post(t, "/session/"+info.ID+tc.path, key, tc.body)
			if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
				t.Fatalf("%s key %q: status %d body %s", tc.path, key, rec.Code, rec.Body.String())
			}
			assertJSONEqual(t, tc.path, rec.Body.Bytes(), tc.want)
		}
		if rows := e.opRows(t); len(rows) != 0 {
			t.Fatalf("off wrote %d op rows", len(rows))
		}
	}
}

// Each command records one op of its kind, closed succeeded{via: legacy}
// before the answer, which carries the op and a minted key.
func TestSessionControlOpsAutoRecordsEachCommand(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "auto")
	info := e.session(t, "Auto")
	e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "ok?"})
	for _, tc := range []struct {
		path, body string
		kind       opjournal.Kind
		code       int
	}{
		{"/stop", "", opjournal.KindInterrupt, http.StatusOK},
		{"/respond", `{"request_id":"req-1","action":"approve"}`, opjournal.KindRespond, http.StatusAccepted},
		{"/suspend", "", opjournal.KindSuspend, http.StatusOK},
		{"/wake", "", opjournal.KindWake, http.StatusOK},
		{"/reset", "", opjournal.KindRestart, http.StatusOK},
		{"/kill", "", opjournal.KindKill, http.StatusOK},
		{"/close", "", opjournal.KindClose, http.StatusOK},
	} {
		rec := e.post(t, "/session/"+info.ID+tc.path, "", tc.body)
		wantStatus(t, tc.path, rec, tc.code)
		a := decodeCommandAnswer(t, rec)
		if !mintedKeyRE.MatchString(a.IdempotencyKey) || a.Replayed || a.Op == nil {
			t.Fatalf("%s answer = %+v, want a minted key and the op", tc.path, a)
		}
		if a.Op.Kind != string(tc.kind) || a.Op.Status != string(opjournal.StatusClosed) || a.Op.Outcome == nil ||
			a.Op.Outcome.Arm != string(opjournal.ArmSucceeded) || a.Op.Outcome.Via != legacyop.ViaLegacy {
			t.Fatalf("%s op = %+v outcome %+v", tc.path, a.Op, a.Op.Outcome)
		}
		stored, err := e.opStore(t).Get(a.Op.ID)
		if err != nil || stored.Status != opjournal.StatusClosed || stored.Executor != legacyop.ExecutorAPI {
			t.Fatalf("%s stored op = %+v, %v", tc.path, stored, err)
		}
		if tc.kind == opjournal.KindInterrupt || tc.kind == opjournal.KindKill || tc.kind == opjournal.KindRespond {
			if stored.Scope.Kind != opjournal.ScopeSession || stored.Scope.Generation < 1 {
				t.Fatalf("%s scope = %+v, want session(gen)", tc.path, stored.Scope)
			}
		}
		if tc.kind == opjournal.KindRespond && stored.Scope.Interaction != "req-1" {
			t.Fatalf("respond scope = %+v, want interaction req-1", stored.Scope)
		}
		if tc.kind == opjournal.KindInterrupt && stored.Outcome.Degradation != session.DegradationUnfencedInterrupt {
			t.Fatalf("session-scoped stop outcome = %+v, want unfenced_interrupt", stored.Outcome)
		}
	}
	if rows := e.opRows(t); len(rows) != 7 {
		t.Fatalf("op rows = %d, want 7", len(rows))
	}
}

// A completed /stop replays across an API restart without interrupting
// again: a retry can never interrupt a later turn.
func TestSessionControlOpsCompletedStopReplaysAcrossARestart(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "off")
	info := e.session(t, "Stop")
	first := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	wantStatus(t, "first stop", first, http.StatusOK)
	a := decodeCommandAnswer(t, first)
	if a.IdempotencyKey != "stop-1" || a.Op == nil {
		t.Fatalf("first answer = %+v", a)
	}

	e.restart(t)
	replay := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	wantStatus(t, "replay", replay, http.StatusOK)
	r := decodeCommandAnswer(t, replay)
	if !r.Replayed || r.Op == nil || r.Op.ID != a.Op.ID || r.ID != info.ID {
		t.Fatalf("replay answer = %+v, want the first op replayed", r)
	}
	if got := e.fs.sp.CountCalls("Interrupt", info.SessionName); got != 1 {
		t.Fatalf("Interrupt calls = %d, want 1", got)
	}
}

// A same-key /stop while the first runs is 409 in_flight from the op's
// in-flight entry, also when the LRU does not see it (a rebuilt Server).
func TestSessionControlOpsConcurrentSameKeyStopIsBusy(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "off")
	blocking := &blockingInterruptProvider{Fake: e.fs.sp, entered: make(chan struct{}), release: make(chan struct{})}
	e.fs.sessionProvider = blocking
	info := e.session(t, "Busy")

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- e.post(t, "/session/"+info.ID+"/stop", "stop-1", "") }()
	<-blocking.entered

	e.rebuild(t)
	busy := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	if busy.Code != http.StatusConflict || !strings.Contains(busy.Body.String(), "idempotency-in-flight") {
		close(blocking.release)
		t.Fatalf("same-key stop while in flight: %d %s, want 409 in_flight", busy.Code, busy.Body.String())
	}
	close(blocking.release)
	wantStatus(t, "first stop", <-firstDone, http.StatusOK)
	if got := e.fs.sp.CountCalls("Interrupt", info.SessionName); got != 1 {
		t.Fatalf("Interrupt calls = %d, want 1", got)
	}
}

// A retryable error leaves a client-keyed op open, and the same-key retry
// adopts it and stops again, as the LRU's "an error releases the key" does;
// a minted-key op closes from the error.
func TestSessionControlOpsRetryableErrorLeavesAClientKeyedOpOpen(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "off")
	failing := &failingInterruptProvider{Fake: e.fs.sp}
	e.fs.sessionProvider = failing
	info := e.session(t, "Retry")

	failing.fail.Store(true)
	failed := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	if failed.Code == http.StatusOK {
		t.Fatalf("failing stop answered 200: %s", failed.Body.String())
	}
	id, err := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "stop-1")
	if err != nil {
		t.Fatal(err)
	}
	open, err := e.opStore(t).Get(id)
	if err != nil || open.Status == opjournal.StatusClosed {
		t.Fatalf("client-keyed op after a retryable error = %+v, %v; want open", open, err)
	}

	failing.fail.Store(false)
	e.restart(t)
	retry := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	wantStatus(t, "retry", retry, http.StatusOK)
	a := decodeCommandAnswer(t, retry)
	if a.Replayed || a.Op == nil || a.Op.ID != id || a.Op.Attempt != 1 || a.Op.Outcome == nil || a.Op.Outcome.Arm != string(opjournal.ArmSucceeded) {
		t.Fatalf("retry answer = %+v (op %+v), want the adopted op succeeded at attempt 1", a, a.Op)
	}
	if got := e.fs.sp.CountCalls("Interrupt", info.SessionName); got != 2 {
		t.Fatalf("Interrupt calls = %d, want 2", got)
	}

	failing.fail.Store(true)
	minted := e.post(t, "/session/"+info.ID+"/stop", "", "")
	if minted.Code == http.StatusOK {
		t.Fatalf("failing stop answered 200: %s", minted.Body.String())
	}
	var closedFailed int
	for _, row := range e.opRows(t) {
		op, err := opjournal.DecodeOp(row)
		if err == nil && op.ID != id && op.Status == opjournal.StatusClosed && op.Outcome.Arm == opjournal.ArmFailed && op.Outcome.Reason == opjournal.ReasonOther {
			closedFailed++
		}
	}
	if closedFailed != 1 {
		t.Fatalf("minted-key ops closed failed{other} = %d, want 1", closedFailed)
	}
}

// A minted-key wake the controller will not start closes
// failed{refused:will_not_start}; under a client key it stays open.
func TestSessionControlOpsMintedKeyRefusalCloses(t *testing.T) {
	e := newControlOpsEnv(t, "off", "auto")
	refusing := &wakeRefusingState{fakeState: e.fs, refusal: "the agent is suspended"}
	e.resolver.state = refusing
	info := e.session(t, "Refused")

	rec := e.post(t, "/session/"+info.ID+"/wake", "", "")
	wantStatus(t, "minted wake", rec, http.StatusConflict)
	rows := e.opRows(t)
	if len(rows) != 1 {
		t.Fatalf("op rows = %d, want 1", len(rows))
	}
	op, err := opjournal.DecodeOp(rows[0])
	if err != nil || op.Status != opjournal.StatusClosed || op.Outcome.Reason != opjournal.RefusedReasonPrefix+refusedWillNotStart {
		t.Fatalf("minted wake op = %+v, %v", op, err)
	}

	rec = e.post(t, "/session/"+info.ID+"/wake", "wake-1", "")
	wantStatus(t, "keyed wake", rec, http.StatusConflict)
	id, _ := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "wake-1")
	if keyed, err := e.opStore(t).Get(id); err != nil || keyed.Status == opjournal.StatusClosed {
		t.Fatalf("client-keyed wake op = %+v, %v; want open", keyed, err)
	}
}

// A kill of a session whose runtime is already gone answers 200 and closes
// skipped{not_needed}; its same-key retry replays 200 after a restart.
func TestSessionControlOpsKillOfAnInactiveSessionReplays(t *testing.T) {
	e := newControlOpsEnv(t, "off", "auto")
	e.fs.sessionProvider = runtime.FullFake{Fake: e.fs.sp}
	info := e.session(t, "Gone")
	if err := e.fs.sp.Stop(info.SessionName); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetMetadata(info.ID, "state", "asleep"); err != nil {
		t.Fatal(err)
	}
	first := e.post(t, "/session/"+info.ID+"/kill", "kill-1", "")
	wantStatus(t, "kill", first, http.StatusOK)
	a := decodeCommandAnswer(t, first)
	if a.Op == nil || a.Op.Outcome == nil || a.Op.Outcome.Arm != string(opjournal.ArmSkipped) || a.Op.Outcome.Reason != opjournal.ReasonNotNeeded {
		t.Fatalf("kill op = %+v outcome %+v", a.Op, a.Op.Outcome)
	}
	e.restart(t)
	replay := e.post(t, "/session/"+info.ID+"/kill", "kill-1", "")
	wantStatus(t, "replay", replay, http.StatusOK)
	if r := decodeCommandAnswer(t, replay); !r.Replayed || r.Op == nil || r.Op.ID != a.Op.ID || r.ID != info.ID {
		t.Fatalf("replay = %+v", r)
	}
}

// A same-key kill after the session moved to a later generation answers from
// the first kill instead of refusing the key as another request.
func TestSessionControlOpsKillRetryAfterARestartedSessionReplays(t *testing.T) {
	e := newControlOpsEnv(t, "off", "auto")
	info := e.session(t, "Restarted")
	first := e.post(t, "/session/"+info.ID+"/kill", "kill-1", "")
	wantStatus(t, "kill", first, http.StatusOK)
	gen := generationOf(t, e.store, info.ID)
	if err := e.store.SetMetadata(info.ID, "generation", "99"); err != nil {
		t.Fatal(err)
	}
	if generationOf(t, e.store, info.ID) == gen {
		t.Fatal("generation did not move")
	}
	e.restart(t)
	replay := e.post(t, "/session/"+info.ID+"/kill", "kill-1", "")
	wantStatus(t, "replay", replay, http.StatusOK)
	if r := decodeCommandAnswer(t, replay); !r.Replayed {
		t.Fatalf("replay = %+v", r)
	}
}

// /respond is keyed by the client key, never by the interaction: two
// answers to one interaction with two keys are two ops, and the second
// closes skipped{stale}; a completed respond replays across a restart.
func TestSessionControlOpsRespondIsKeyedByTheClientKey(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "off")
	info := e.session(t, "Respond")
	e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "ok?"})
	body := `{"request_id":"req-1","action":"approve"}`

	first := e.post(t, "/session/"+info.ID+"/respond", "r-1", body)
	wantStatus(t, "first", first, http.StatusAccepted)
	second := e.post(t, "/session/"+info.ID+"/respond", "r-2", body)
	wantStatus(t, "second", second, http.StatusConflict)
	id2, _ := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "r-2")
	op2, err := e.opStore(t).Get(id2)
	if err != nil || op2.Status != opjournal.StatusClosed || op2.Outcome.Arm != opjournal.ArmSkipped || op2.Outcome.Reason != opjournal.ReasonStale {
		t.Fatalf("second respond op = %+v, %v; want skipped{stale}", op2, err)
	}

	e.restart(t)
	replay := e.post(t, "/session/"+info.ID+"/respond", "r-1", body)
	wantStatus(t, "replay", replay, http.StatusAccepted)
	if r := decodeCommandAnswer(t, replay); !r.Replayed || r.Status != "accepted" {
		t.Fatalf("replay = %+v", r)
	}
	replayStale := e.post(t, "/session/"+info.ID+"/respond", "r-2", body)
	if replayStale.Code != http.StatusConflict || !strings.Contains(replayStale.Body.String(), "replayed op "+id2) {
		t.Fatalf("stale replay = %d %s, want 409 naming the op", replayStale.Code, replayStale.Body.String())
	}
	if got := countSessionResponds(e.fs.sp, info.SessionName); got != 1 {
		t.Fatalf("Respond calls = %d, want 1", got)
	}
}

// A respond with no interaction to name is refused at the front door in
// require, with nothing recorded and nothing sent.
func TestSessionControlOpsRequireRefusesARespondWithNoInteraction(t *testing.T) {
	e := newControlOpsEnv(t, "require", "off")
	info := e.session(t, "No pending")
	rec := e.post(t, "/session/"+info.ID+"/respond", "", `{"action":"approve"}`)
	wantStatus(t, "respond", rec, http.StatusConflict)
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("op rows = %d, want 0", len(rows))
	}
	if got := countSessionResponds(e.fs.sp, info.SessionName); got != 0 {
		t.Fatalf("Respond calls = %d, want 0", got)
	}
}

// require refuses a command it cannot record before any effect.
func TestSessionControlOpsRequireRefusesWhatItCannotRecord(t *testing.T) {
	e := newControlOpsEnv(t, "require", "require")
	info := e.session(t, "Require")
	e.store.set(func(s *opsTestStore) { s.createErr = errors.New("store down") })
	for _, path := range []string{"/stop", "/kill", "/suspend", "/wake", "/reset", "/close"} {
		rec := e.post(t, "/session/"+info.ID+path, "", "")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "op-journal-unavailable") {
			t.Fatalf("%s: %d %s, want 503 op_journal_unavailable", path, rec.Code, rec.Body.String())
		}
	}
	if got := e.fs.sp.CountCalls("Interrupt", info.SessionName); got != 0 {
		t.Fatalf("Interrupt calls = %d, want 0", got)
	}
	if got := e.fs.sp.CountCalls("Stop", info.SessionName); got != 0 {
		t.Fatalf("Stop calls = %d, want 0", got)
	}
	got, err := session.NewStore(beads.SessionStore{Store: e.store}).Get(info.ID)
	if err != nil || got.State == session.StateSuspended || got.Closed {
		t.Fatalf("session after refusals = %+v, %v; want untouched", got, err)
	}
	invalid := e.post(t, "/session/"+info.ID+"/kill", " bad", "")
	wantStatus(t, "invalid key", invalid, http.StatusUnprocessableEntity)
}

// A same-key retry of an op that closed failed for a reason it cannot be
// rebuilt from is 409 op_already_failed and runs nothing.
func TestSessionControlOpsReplayOfAFailedOpIsOpAlreadyFailed(t *testing.T) {
	e := newControlOpsEnv(t, "off", "auto")
	info := e.session(t, "Failed")
	now := time.Now()
	st, err := opjournal.NewStore(beads.SessionStore{Store: e.store.MemStore}, &clock.Fake{Time: now}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := st.Propose(opjournal.Request{
		Conversation: info.ID, Kind: opjournal.KindSuspend, Executor: legacyop.ExecutorAPI, ClientKey: "s-1",
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		StartBy:  now.Add(legacyop.ExecutionBound), FinishBy: now.Add(legacyop.ExecutionBound),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Close(op.ID, opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonDeadlineInFlight}, "test"); err != nil {
		t.Fatal(err)
	}
	rec := e.post(t, "/session/"+info.ID+"/suspend", "s-1", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "op-already-failed") || !strings.Contains(rec.Body.String(), op.ID) {
		t.Fatalf("replay: %d %s, want 409 op_already_failed", rec.Code, rec.Body.String())
	}
	if got, _ := session.NewStore(beads.SessionStore{Store: e.store}).Get(info.ID); got.State == session.StateSuspended {
		t.Fatal("the replay suspended the session")
	}
}

// protocolStopEnv is an opsEnv, ops.control auto, whose session runs on a protocol runtime that
// records turns, as t3bridge does.
func protocolStopEnv(t *testing.T) (*opsEnv, session.Info) {
	t.Helper()
	e := newControlOpsEnv(t, "auto", "off")
	e.fs.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	e.fs.sessionProvider = runtime.FullFake{Fake: e.fs.sp}
	info := e.session(t, "Protocol")
	e.fs.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{info.SessionName: {{
		Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceChecked, Link: runtime.TurnLinkCorrelated,
	}}}
	e.fs.sp.TurnScripts = map[string][]string{}
	e.fs.sp.TurnHistoryScripts = map[string][]runtime.TurnHistory{}
	e.fs.sp.TurnHistoryErrors = map[string][]error{}
	return e, info
}

// On a protocol runtime a recorded /stop interrupts the turn a live read
// finds, turn-scoped, keyed stop:<gen>:<turn> without a client key, and
// never sends the unfenced Interrupt (DES-T3I §14 item 3).
func TestSessionControlOpsProtocolStopIsTurnScoped(t *testing.T) {
	e, info := protocolStopEnv(t)
	e.fs.sp.TurnHistoryScripts[info.SessionName] = []runtime.TurnHistory{{Current: runtime.TurnState{TurnID: "t-1"}}}
	e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
	rec := e.post(t, "/session/"+info.ID+"/stop", "", "")
	wantStatus(t, "stop", rec, http.StatusOK)
	a := decodeCommandAnswer(t, rec)
	gen := generationOf(t, e.store, info.ID)
	wantKey, _ := opjournal.InterruptKey(gen, "t-1")
	if a.IdempotencyKey != wantKey || a.Op == nil || a.Op.Outcome == nil || a.Op.Outcome.Degradation != session.DegradationUnfencedInterrupt {
		t.Fatalf("answer = %+v op %+v", a, a.Op)
	}
	if got := e.fs.sp.CountCalls("InterruptTurn", info.SessionName); got != 1 {
		t.Fatalf("InterruptTurn calls = %d, want 1", got)
	}
	if got := e.fs.sp.CountCalls("Interrupt", info.SessionName); got != 0 {
		t.Fatalf("Interrupt calls = %d, want 0", got)
	}
}

// On a protocol runtime with no turn running a recorded /stop sends nothing
// and closes skipped{not_needed}; a turn read that fails sends nothing and
// answers 503.
func TestSessionControlOpsProtocolStopNeverSendsUnfenced(t *testing.T) {
	e, info := protocolStopEnv(t)
	rec := e.post(t, "/session/"+info.ID+"/stop", "", "")
	wantStatus(t, "idle stop", rec, http.StatusOK)
	if a := decodeCommandAnswer(t, rec); a.Op == nil || a.Op.Outcome == nil || a.Op.Outcome.Reason != opjournal.ReasonNotNeeded {
		t.Fatalf("idle stop op = %+v", a.Op)
	}
	e.fs.sp.TurnHistoryErrors[info.SessionName] = []error{runtime.ErrRuntimeUnavailable}
	rec = e.post(t, "/session/"+info.ID+"/stop", "", "")
	wantStatus(t, "unreadable stop", rec, http.StatusServiceUnavailable)
	if got := e.fs.sp.CountCalls("Interrupt", info.SessionName) + e.fs.sp.CountCalls("InterruptTurn", info.SessionName); got != 0 {
		t.Fatalf("interrupts sent = %d, want 0", got)
	}
}

// A turn the request names is stopped only while current: two stops of it
// with different keys are one op, and a turn no longer running is 409
// turn_not_current, nothing sent.
func TestSessionControlOpsNamedTurnStop(t *testing.T) {
	e, info := protocolStopEnv(t)
	e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
	first := e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`)
	wantStatus(t, "first", first, http.StatusOK)
	a := decodeCommandAnswer(t, first)
	second := e.post(t, "/session/"+info.ID+"/stop", "click-2", `{"turn":"t-1"}`)
	wantStatus(t, "second", second, http.StatusOK)
	if b := decodeCommandAnswer(t, second); !b.Replayed || b.Op == nil || a.Op == nil || b.Op.ID != a.Op.ID {
		t.Fatalf("second click = %+v, want the first op replayed", b)
	}
	stale := e.post(t, "/session/"+info.ID+"/stop", "", `{"turn":"t-9"}`)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), turnNotCurrentCode) {
		t.Fatalf("stale turn: %d %s, want 409 turn_not_current", stale.Code, stale.Body.String())
	}
	if got := e.fs.sp.CountCalls("Interrupt", info.SessionName); got != 0 {
		t.Fatalf("Interrupt calls = %d, want 0", got)
	}
}

// A named turn on a runtime that records no turn starts is refused at the
// front door, in every mode, with no op.
func TestSessionControlOpsNamedTurnNeedsTurnIdentity(t *testing.T) {
	for _, mode := range []string{"off", "auto"} {
		e := newControlOpsEnv(t, mode, "off")
		info := e.session(t, "No identity")
		rec := e.post(t, "/session/"+info.ID+"/stop", "", `{"turn":"t-1"}`)
		if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), turnIdentityUnsupportedCode) {
			t.Fatalf("%s: %d %s, want 501 turn_identity_unsupported", mode, rec.Code, rec.Body.String())
		}
		if rows := e.opRows(t); len(rows) != 0 {
			t.Fatalf("%s: op rows = %d, want 0", mode, len(rows))
		}
		if got := e.fs.sp.CountCalls("Interrupt", info.SessionName); got != 0 {
			t.Fatalf("%s: Interrupt calls = %d, want 0", mode, got)
		}
	}
}

// A same-key retry of a stop whose turn a live read resolved reuses that
// stop's scope: it replays it, rather than resolving the turn running now.
func TestSessionControlOpsKeyedStopRetryReusesItsTurn(t *testing.T) {
	e, info := protocolStopEnv(t)
	e.fs.sp.TurnHistoryScripts[info.SessionName] = []runtime.TurnHistory{{Current: runtime.TurnState{TurnID: "t-1"}}, {Current: runtime.TurnState{TurnID: "t-2"}}}
	e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
	first := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	wantStatus(t, "first", first, http.StatusOK)
	e.restart(t)
	replay := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	wantStatus(t, "replay", replay, http.StatusOK)
	if r := decodeCommandAnswer(t, replay); !r.Replayed {
		t.Fatalf("replay = %+v", r)
	}
	if got := e.fs.sp.CountCalls("InterruptTurn", info.SessionName); got != 1 {
		t.Fatalf("InterruptTurn calls = %d, want 1", got)
	}
}

// Every replayed refusal rebuilds the original's wire error, its message
// naming the op; what it cannot rebuild is op_already_failed.
func TestReplayedSyncErrorRebuildsTheRefusal(t *testing.T) {
	for _, tc := range []struct {
		kind   opjournal.Kind
		scope  opjournal.Scope
		arm    opjournal.Arm
		reason string
		want   string
	}{
		{opjournal.KindRespond, opjournal.Scope{}, opjournal.ArmFailed, opjournal.ReasonUnsupported, "unsupported:"},
		{opjournal.KindInterrupt, opjournal.Scope{}, opjournal.ArmFailed, opjournal.ReasonUnsupported, turnInterruptUnsupportedCode},
		{opjournal.KindInterrupt, opjournal.Scope{Turn: "t"}, opjournal.ArmFailed, opjournal.ReasonUnsupported, turnIdentityUnsupportedCode},
		{opjournal.KindInterrupt, opjournal.Scope{Turn: "t"}, opjournal.ArmSkipped, opjournal.ReasonStale, turnNotCurrentCode},
		{opjournal.KindRespond, opjournal.Scope{}, opjournal.ArmSkipped, opjournal.ReasonStale, "no_pending:"},
		{opjournal.KindKill, opjournal.Scope{}, opjournal.ArmFailed, "refused:session_starting", "session_starting:"},
		{opjournal.KindWake, opjournal.Scope{}, opjournal.ArmFailed, "refused:not_a_session", "invalid:"},
		{opjournal.KindWake, opjournal.Scope{}, opjournal.ArmFailed, "refused:will_not_start", "will not start"},
		{opjournal.KindWake, opjournal.Scope{}, opjournal.ArmFailed, "refused:archived", "was archived"},
		{opjournal.KindClose, opjournal.Scope{}, opjournal.ArmFailed, opjournal.ReasonNoBoundary, opAlreadyFailedCode},
	} {
		op := opjournal.Op{
			ID: "gc-op-1", Conversation: "gc-1", Kind: tc.kind, Scope: tc.scope, Status: opjournal.StatusClosed,
			Outcome: opjournal.Outcome{Arm: tc.arm, Reason: tc.reason},
		}
		err := replayedSyncError(op, humaStopError)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "gc-op-1") {
			t.Fatalf("%s %s: %v, want %q naming the op", tc.kind, tc.reason, err, tc.want)
		}
	}
}

// The sync commands' legacy errors map onto op outcomes (DES-O0API §7.2).
func TestSyncOpResultMapsTheLegacyErrors(t *testing.T) {
	for _, tc := range []struct {
		kind      opjournal.Kind
		err       error
		minted    bool
		arm       opjournal.Arm
		reason    string
		leaveOpen bool
	}{
		{opjournal.KindKill, session.ErrSessionInactive, false, opjournal.ArmSkipped, opjournal.ReasonNotNeeded, false},
		{opjournal.KindInterrupt, session.ErrTurnInterruptUnsupported, false, opjournal.ArmFailed, opjournal.ReasonUnsupported, false},
		{opjournal.KindInterrupt, &runtime.TurnNotCurrentError{TurnID: "t"}, false, opjournal.ArmSkipped, opjournal.ReasonStale, false},
		{opjournal.KindRespond, session.ErrNoPendingInteraction, false, opjournal.ArmSkipped, opjournal.ReasonStale, false},
		{opjournal.KindInterrupt, session.TurnNotEndedError("stopped", nil), true, opjournal.ArmFailed, opjournal.ReasonNoBoundary, false},
		{opjournal.KindInterrupt, session.TurnNotEndedError("stopped", nil), false, opjournal.ArmFailed, opjournal.ReasonNoBoundary, true},
		{opjournal.KindSuspend, session.ErrSessionStarting, true, opjournal.ArmFailed, "refused:session_starting", false},
		{opjournal.KindWake, &wakeWillNotStartError{message: "no"}, true, opjournal.ArmFailed, "refused:will_not_start", false},
		{opjournal.KindSuspend, runtime.ErrRuntimeUnavailable, true, opjournal.ArmFailed, opjournal.ReasonProviderUnavailable, false},
	} {
		res := wakeOpResult(tc.err, tc.minted)
		if tc.kind != opjournal.KindWake {
			res = syncOpResult(tc.kind, tc.err, tc.minted)
		}
		if res.Outcome.Arm != tc.arm || res.Outcome.Reason != tc.reason || res.LeaveOpen != tc.leaveOpen {
			t.Fatalf("%s %v minted %v: %+v, want %s{%s} leaveOpen %v", tc.kind, tc.err, tc.minted, res, tc.arm, tc.reason, tc.leaveOpen)
		}
	}
}
