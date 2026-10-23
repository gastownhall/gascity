package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
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

// A kill of a configured named session whose runtime is already gone counts
// as done, as gc session kill's does, and closes skipped{not_needed}, as an
// unnamed session's kill does.
func TestSessionControlOpsKillOfAnInactiveNamedSessionSkips(t *testing.T) {
	e := newControlOpsEnv(t, "off", "auto")
	e.fs.sessionProvider = runtime.FullFake{Fake: e.fs.sp}
	info := e.session(t, "Gone named")
	if err := e.store.SetMetadata(info.ID, session.NamedSessionIdentityMetadata, "helper"); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.sp.Stop(info.SessionName); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetMetadata(info.ID, "state", "asleep"); err != nil {
		t.Fatal(err)
	}
	rec := e.post(t, "/session/"+info.ID+"/kill", "kill-1", "")
	wantStatus(t, "kill", rec, http.StatusOK)
	if a := decodeCommandAnswer(t, rec); a.Op == nil || a.Op.Outcome == nil || a.Op.Outcome.Arm != string(opjournal.ArmSkipped) || a.Op.Outcome.Reason != opjournal.ReasonNotNeeded {
		t.Fatalf("kill op = %+v, want it closed skipped{not_needed}", a.Op)
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
// records its turns' starts and ends.
func protocolStopEnv(t *testing.T) (*opsEnv, session.Info) {
	t.Helper()
	return protocolStopEnvMode(t, "auto")
}

// protocolStopEnvMode is protocolStopEnv under ops.control mode control.
func protocolStopEnvMode(t *testing.T, control string) (*opsEnv, session.Info) {
	t.Helper()
	e := newControlOpsEnv(t, control, "off")
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
		{opjournal.KindKill, opjournal.Scope{}, opjournal.ArmFailed, "refused:session_stopping", "session_stopping:"},
		{opjournal.KindKill, opjournal.Scope{}, opjournal.ArmFailed, "refused:kill_pending", "being killed"},
		{opjournal.KindSuspend, opjournal.Scope{}, opjournal.ArmFailed, "refused:illegal_transition", "illegal_transition:"},
		{opjournal.KindKill, opjournal.Scope{Generation: 3}, opjournal.ArmFailed, "refused:" + refusedTargetMoved, refusedTargetMoved + ":"},
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
		{opjournal.KindWake, &session.WakeConflictError{SessionID: "gc-1", State: "Archived"}, false, opjournal.ArmFailed, "refused:archived", true},
		{opjournal.KindWake, &session.WakeConflictError{SessionID: "gc-1", State: "archived"}, true, opjournal.ArmFailed, "refused:archived", false},
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

// seedSyncOp proposes an open op of kind on conv under key, as a producer
// (executor) that proposed it at now and has not closed it.
func (e *opsEnv) seedSyncOp(t *testing.T, conv string, kind opjournal.Kind, key string, scope opjournal.Scope, policy opjournal.Policy, payload string, executor opjournal.ExecutorKind, now time.Time) opjournal.Op {
	t.Helper()
	st, err := opjournal.NewStore(beads.SessionStore{Store: e.store.MemStore}, &clock.Fake{Time: now}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("open op store: %v", err)
	}
	op, created, err := st.Propose(opjournal.Request{
		Conversation: conv, Kind: kind, Executor: executor, ClientKey: key,
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  payload, Policy: policy, Scope: scope,
		StartBy: now.Add(legacyop.ExecutionBound), FinishBy: now.Add(legacyop.ExecutionBound),
	})
	if err != nil || !created {
		t.Fatalf("seed %s op: %v created %v", kind, err, created)
	}
	return op
}

// turnStopScope is the scope of a stop of turn in session id's generation.
func turnStopScope(t *testing.T, e *opsEnv, id, turn string) opjournal.Scope {
	t.Helper()
	return opjournal.Scope{Kind: opjournal.ScopeSession, Generation: generationOf(t, e.store, id), Turn: turn}
}

// interruptsSent counts the interrupts a stop sent to name: session-scoped
// (Interrupt) and turn-scoped ones the fence let through.
func interruptsSent(sp *runtime.Fake, name string) (unfenced, turnScoped int) {
	for _, c := range sp.SnapshotCalls() {
		switch {
		case c.Name != name:
		case c.Method == "Interrupt":
			unfenced++
		case c.Method == "InterruptTurn" && c.Value == "sent":
			turnScoped++
		}
	}
	return unfenced, turnScoped
}

// opOf decodes the stored op the answer names.
func opOf(t *testing.T, e *opsEnv, a commandAnswer) opjournal.Op {
	t.Helper()
	if a.Op == nil {
		t.Fatalf("answer %+v carries no op", a)
	}
	op, err := e.opStore(t).Get(a.Op.ID)
	if err != nil {
		t.Fatalf("op %s: %v", a.Op.ID, err)
	}
	return op
}

// A turn's target key whose op closed failed (here an op a sent-but-unsettled
// first stop left open past its finish_by) does not use up the turn: every
// later stop of it, key-less or naming it under a new Idempotency-Key, acts
// under its own key, and only while the turn is current.
func TestSessionControlOpsFailedTurnKeyOpDoesNotRefuseLaterStops(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		body string
	}{
		{name: "key-less, turn read live", keys: []string{"", ""}},
		{name: "named, new keys", keys: []string{"fresh-1", "fresh-2"}, body: `{"turn":"t-1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, info := protocolStopEnv(t)
			key, err := opjournal.InterruptKey(generationOf(t, e.store, info.ID), "t-1")
			if err != nil {
				t.Fatal(err)
			}
			expired := e.seedSyncOp(t, info.ID, opjournal.KindInterrupt, key, turnStopScope(t, e, info.ID, "t-1"), interruptOpPolicy, "", legacyop.ExecutorAPI, time.Now().Add(-30*time.Minute))
			// t-1 is still running, 30 minutes later; each stop's fence read
			// finds it, and the turn ends after the interrupt.
			e.fs.sp.TurnHistoryScripts[info.SessionName] = []runtime.TurnHistory{{Current: runtime.TurnState{TurnID: "t-1"}}}
			e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", "", "t-1", ""}
			seen := map[string]bool{}
			for i, k := range tc.keys {
				rec := e.post(t, "/session/"+info.ID+"/stop", k, tc.body)
				wantStatus(t, fmt.Sprintf("stop %d", i+1), rec, http.StatusOK)
				a := decodeCommandAnswer(t, rec)
				op := opOf(t, e, a)
				if a.Replayed || op.ID == expired.ID || seen[op.ID] || op.Outcome.Arm != opjournal.ArmSucceeded || op.Scope.Turn != "t-1" {
					t.Fatalf("stop %d answer = %+v, op %+v; want a new op of turn t-1 that succeeded", i+1, a, op)
				}
				if k != "" && a.IdempotencyKey != k {
					t.Fatalf("stop %d recorded under %q, want its own key %q", i+1, a.IdempotencyKey, k)
				}
				if k == "" && !mintedKeyRE.MatchString(a.IdempotencyKey) {
					t.Fatalf("key-less stop %d recorded under %q, want a minted key", i+1, a.IdempotencyKey)
				}
				seen[op.ID] = true
			}
			if _, sent := interruptsSent(e.fs.sp, info.SessionName); sent != 2 {
				t.Fatalf("turn-scoped interrupts sent = %d, want 2", sent)
			}
			if got, err := e.opStore(t).Get(expired.ID); err != nil || got.Status != opjournal.StatusClosed || got.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
				t.Fatalf("the expired turn-key op = %+v, %v; want closed failed{deadline_in_flight}", got, err)
			}
		})
	}
}

// A named stop's own key replays its op: the target key's failed op hands
// the request to its key, which finds the op it recorded under.
func TestSessionControlOpsNamedStopSuccessorKeyReplays(t *testing.T) {
	e, info := protocolStopEnv(t)
	key, _ := opjournal.InterruptKey(generationOf(t, e.store, info.ID), "t-1")
	e.seedSyncOp(t, info.ID, opjournal.KindInterrupt, key, turnStopScope(t, e, info.ID, "t-1"), interruptOpPolicy, "", legacyop.ExecutorAPI, time.Now().Add(-30*time.Minute))
	e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
	first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/stop", "fresh-1", `{"turn":"t-1"}`))
	e.restart(t)
	rec := e.post(t, "/session/"+info.ID+"/stop", "fresh-1", `{"turn":"t-1"}`)
	wantStatus(t, "replay", rec, http.StatusOK)
	if a := decodeCommandAnswer(t, rec); !a.Replayed || a.Op == nil || first.Op == nil || a.Op.ID != first.Op.ID {
		t.Fatalf("replay = %+v, want op %+v replayed", a, first.Op)
	}
	if _, sent := interruptsSent(e.fs.sp, info.SessionName); sent != 1 {
		t.Fatalf("turn-scoped interrupts sent = %d, want 1", sent)
	}
}

// A named stop answered from its turn's target-key op, the first click on the
// turn or a second one, leaves a row under its own Idempotency-Key that
// coalesced into that op, so the key finds the stop after a restart: a retry
// that names another turn is 422 idempotency_mismatch, and one with no turn
// replays the stop rather than stopping the turn running now. Neither sends
// anything.
func TestSessionControlOpsNamedStopKeyFindsItsStopAfterARestart(t *testing.T) {
	e, info := protocolStopEnv(t)
	e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
	first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`))
	second := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/stop", "click-2", `{"turn":"t-1"}`))
	if first.Op == nil || second.Op == nil || second.Op.ID != first.Op.ID || !second.Replayed {
		t.Fatalf("clicks = %+v, %+v; want the second answered from the first's op", first, second)
	}
	// t-2 runs now.
	e.fs.sp.TurnHistoryScripts[info.SessionName] = []runtime.TurnHistory{{Current: runtime.TurnState{TurnID: "t-2"}}}
	e.fs.sp.TurnScripts[info.SessionName] = []string{"t-2"}
	for _, key := range []string{"click-1", "click-2"} {
		e.restart(t)
		other := e.post(t, "/session/"+info.ID+"/stop", key, `{"turn":"t-2"}`)
		if other.Code != http.StatusUnprocessableEntity || !strings.Contains(other.Body.String(), "idempotency_mismatch") {
			t.Fatalf("%s naming t-2: %d %s, want 422 idempotency_mismatch", key, other.Code, other.Body.String())
		}
		rec := e.post(t, "/session/"+info.ID+"/stop", key, "")
		wantStatus(t, key+" with no turn", rec, http.StatusOK)
		if a := decodeCommandAnswer(t, rec); !a.Replayed || a.Op == nil || a.Op.ID != first.Op.ID || a.IdempotencyKey != first.IdempotencyKey {
			t.Fatalf("%s with no turn = %+v, want stop %+v replayed", key, a, first)
		}
	}
	for _, c := range e.fs.sp.SnapshotCalls() {
		if c.Method == "InterruptTurn" && c.Key == "t-2" {
			t.Fatalf("a retry interrupted the later turn t-2: %+v", c)
		}
	}
	if unfenced, sent := interruptsSent(e.fs.sp, info.SessionName); unfenced != 0 || sent != 1 {
		t.Fatalf("sent %d session-scoped and %d turn-scoped interrupts, want the first stop's only", unfenced, sent)
	}
}

// panickingTurnInterruptProvider panics, once armed, after a turn-scoped
// interrupt it sent: the API died before it closed the stop's op.
type panickingTurnInterruptProvider struct {
	runtime.FullFake
	armed atomic.Bool
}

func (p *panickingTurnInterruptProvider) InterruptTurn(ctx context.Context, name, turnID string) (runtime.TurnInterrupt, error) {
	ack, err := p.FullFake.InterruptTurn(ctx, name, turnID)
	if err == nil && p.armed.CompareAndSwap(true, false) {
		panic("injected panic after the turn interrupt")
	}
	return ack, err
}

// A named stop that does not succeed binds its Idempotency-Key to its turn's
// target-key op all the same, before it sends anything. After a retryable
// failure, a turn_not_current refusal, or an API that died after the
// interrupt, a same-key retry after a restart is that stop again, with no
// turn or naming the same one: it adopts the op left open, or replays the
// closed one, and never stops the turn running then. One naming another turn
// is 422 idempotency_mismatch.
func TestSessionControlOpsFailedNamedStopKeyFindsItsStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fail runs the first stop, of t-1 under click-1, so that it fails,
		// and returns its status.
		fail func(t *testing.T, e *opsEnv, info session.Info) int
		// adopted says the first stop left its op open for the retry to
		// adopt; otherwise it closed it.
		adopted bool
	}{
		{name: "retryable failure", adopted: true, fail: func(t *testing.T, e *opsEnv, info session.Info) int {
			e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1"}
			e.fs.sp.InterruptTurnErrors = map[string]error{info.SessionName: runtime.ErrRuntimeUnavailable}
			return e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`).Code
		}},
		{name: "turn not current", fail: func(t *testing.T, e *opsEnv, info session.Info) int {
			e.fs.sp.TurnScripts[info.SessionName] = []string{"t-0"}
			return e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`).Code
		}},
		{name: "API died after the interrupt", adopted: true, fail: func(t *testing.T, e *opsEnv, info session.Info) int {
			panicking := &panickingTurnInterruptProvider{FullFake: runtime.FullFake{Fake: e.fs.sp}}
			panicking.armed.Store(true)
			e.fs.sessionProvider = panicking
			e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1"}
			code := http.StatusInternalServerError
			func() {
				defer func() { _ = recover() }()
				code = e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`).Code
			}()
			if _, sent := interruptsSent(e.fs.sp, info.SessionName); sent != 1 {
				t.Fatalf("turn-scoped interrupts sent before the panic = %d, want 1", sent)
			}
			return code
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, info := protocolStopEnv(t)
			if code := tc.fail(t, e, info); code == http.StatusOK {
				t.Fatal("the first stop answered 200")
			}
			targetKey, err := opjournal.InterruptKey(generationOf(t, e.store, info.ID), "t-1")
			if err != nil {
				t.Fatal(err)
			}
			targetID, err := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, targetKey)
			if err != nil {
				t.Fatal(err)
			}
			if first, err := e.opStore(t).Get(targetID); err != nil || first.Attempt != 0 || (first.Status == opjournal.StatusClosed) == tc.adopted {
				t.Fatalf("the first stop's op = %+v, %v; want attempt 0, left open %v", first, err, tc.adopted)
			}

			// t-2 runs now.
			e.fs.sp.InterruptTurnErrors = nil
			e.fs.sp.TurnHistoryScripts[info.SessionName] = []runtime.TurnHistory{{Current: runtime.TurnState{TurnID: "t-2"}}}
			e.fs.sp.TurnScripts[info.SessionName] = []string{"t-2"}
			e.restart(t)
			rec := e.post(t, "/session/"+info.ID+"/stop", "click-1", "")
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), turnNotCurrentCode) {
				t.Fatalf("same-key retry with no turn: %d %s, want 409 turn_not_current", rec.Code, rec.Body.String())
			}
			if !tc.adopted && !strings.Contains(rec.Body.String(), "replayed op "+targetID) {
				t.Fatalf("same-key retry with no turn: %s, want op %s replayed", rec.Body.String(), targetID)
			}
			wantAttempt := 0
			if tc.adopted {
				wantAttempt = 1
			}
			if got, err := e.opStore(t).Get(targetID); err != nil || got.Status != opjournal.StatusClosed || got.Attempt != wantAttempt ||
				got.Outcome.Arm != opjournal.ArmSkipped || got.Outcome.Reason != opjournal.ReasonStale {
				t.Fatalf("the first stop's op after the retry = %+v, %v; want attempt %d closed skipped{stale}", got, err, wantAttempt)
			}

			same := e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`)
			if same.Code != http.StatusConflict || !strings.Contains(same.Body.String(), "replayed op "+targetID) {
				t.Fatalf("same-key retry naming t-1: %d %s, want op %s's turn_not_current replayed", same.Code, same.Body.String(), targetID)
			}
			other := e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-2"}`)
			if other.Code != http.StatusUnprocessableEntity || !strings.Contains(other.Body.String(), "idempotency_mismatch") {
				t.Fatalf("same-key retry naming t-2: %d %s, want 422 idempotency_mismatch", other.Code, other.Body.String())
			}

			for _, c := range e.fs.sp.SnapshotCalls() {
				if (c.Method == "InterruptTurn" && c.Key == "t-2") || c.Method == "Interrupt" {
					t.Fatalf("a retry stopped the turn running then: %+v", c)
				}
			}
			keyID, err := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "click-1")
			if err != nil {
				t.Fatal(err)
			}
			keyed, err := e.opStore(t).Get(keyID)
			if err != nil || keyed.Status != opjournal.StatusClosed || keyed.Outcome.Reason != opjournal.ReasonCoalescedInto || keyed.Outcome.Result.CoalescedInto != targetID {
				t.Fatalf("click-1's op = %+v, %v; want closed skipped{coalesced_into: %s}", keyed, err, targetID)
			}
			if n := len(e.opRows(t)); n != 2 {
				t.Fatalf("op rows = %d, want the target-key op and click-1's row", n)
			}
		})
	}
}

// failOpCreatesAfterTheNext lets the next op-row create commit and fails
// every one after it: a named stop's turn op records, its key row does not.
func failOpCreatesAfterTheNext(e *opsEnv) {
	e.store.set(func(s *opsTestStore) {
		s.onOpCreate = func() {
			e.store.set(func(s *opsTestStore) { s.createErr = errors.New("transport: store down") })
		}
	})
}

// A named stop whose row under its own Idempotency-Key does not record never
// leaves that key unbound behind a stop in require: a stop that runs its
// turn's target-key op is 503 op_journal_unavailable before it sends
// anything, the op left open, and one that op answers from its outcome is
// 503 rather than a success. The client's retry then binds the key, so a
// same-key retry with no turn, after a restart, replays the stop and never
// stops the turn running then.
func TestSessionControlOpsRequireNamedStopRefusesAKeyRowThatDoesNotRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		// answered says another click's stop of t-1 succeeded first, so the
		// target-key op answers click-1's stop from its outcome.
		answered bool
	}{
		{name: "stop that runs the op"},
		{name: "stop the op answers", answered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, info := protocolStopEnvMode(t, "require")
			e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
			stop := "/session/" + info.ID + "/stop"
			targetKey, err := opjournal.InterruptKey(generationOf(t, e.store, info.ID), "t-1")
			if err != nil {
				t.Fatal(err)
			}
			targetID, err := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, targetKey)
			if err != nil {
				t.Fatal(err)
			}
			keyID, err := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "click-1")
			if err != nil {
				t.Fatal(err)
			}
			wantSent := 0
			if tc.answered {
				wantStatus(t, "the other click's stop", e.post(t, stop, "click-0", `{"turn":"t-1"}`), http.StatusOK)
				wantSent = 1
				e.store.set(func(s *opsTestStore) { s.createErr = errors.New("transport: store down") })
			} else {
				failOpCreatesAfterTheNext(e)
			}

			rec := e.post(t, stop, "click-1", `{"turn":"t-1"}`)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "op_journal_unavailable") {
				t.Fatalf("stop whose key row does not record: %d %s, want 503 op_journal_unavailable", rec.Code, rec.Body.String())
			}
			if unfenced, sent := interruptsSent(e.fs.sp, info.SessionName); unfenced != 0 || sent != wantSent {
				t.Fatalf("sent %d session-scoped and %d turn-scoped interrupts, want 0 and %d", unfenced, sent, wantSent)
			}
			target, err := e.opStore(t).Get(targetID)
			if err != nil || target.Attempt != 0 || (target.Status == opjournal.StatusClosed) != tc.answered {
				t.Fatalf("the target-key op = %+v, %v; want attempt 0, closed %v", target, err, tc.answered)
			}
			if keyed, err := e.opStore(t).Get(keyID); err == nil {
				t.Fatalf("click-1 names op %+v, want none", keyed)
			}

			e.store.set(func(s *opsTestStore) { s.createErr = nil })
			retry := e.post(t, stop, "click-1", `{"turn":"t-1"}`)
			wantStatus(t, "the client's retry", retry, http.StatusOK)
			if a := decodeCommandAnswer(t, retry); a.Op == nil || a.Op.ID != targetID || a.Replayed != tc.answered {
				t.Fatalf("the client's retry = %+v, want op %s, replayed %v", a, targetID, tc.answered)
			}
			keyed, err := e.opStore(t).Get(keyID)
			if err != nil || keyed.Status != opjournal.StatusClosed || keyed.Outcome.Reason != opjournal.ReasonCoalescedInto || keyed.Outcome.Result.CoalescedInto != targetID {
				t.Fatalf("click-1's op = %+v, %v; want closed skipped{coalesced_into: %s}", keyed, err, targetID)
			}

			// t-2 runs now.
			e.fs.sp.TurnHistoryScripts[info.SessionName] = []runtime.TurnHistory{{Current: runtime.TurnState{TurnID: "t-2"}}}
			e.fs.sp.TurnScripts[info.SessionName] = []string{"t-2"}
			e.restart(t)
			later := e.post(t, stop, "click-1", "")
			wantStatus(t, "a later retry with no turn", later, http.StatusOK)
			if a := decodeCommandAnswer(t, later); !a.Replayed || a.Op == nil || a.Op.ID != targetID {
				t.Fatalf("a later retry with no turn = %+v, want op %s replayed", a, targetID)
			}
			for _, c := range e.fs.sp.SnapshotCalls() {
				if (c.Method == "InterruptTurn" && c.Key == "t-2") || c.Method == "Interrupt" {
					t.Fatalf("a retry stopped the turn running then: %+v", c)
				}
			}
			if _, sent := interruptsSent(e.fs.sp, info.SessionName); sent != 1 {
				t.Fatalf("turn-scoped interrupts sent = %d, want the one stop of t-1", sent)
			}
		})
	}
}

// I8 for a named stop's key row: in auto, a key row the op store does not
// record is logged and the stop goes on, its key naming nothing, as an
// unrecorded request's.
func TestSessionControlOpsAutoNamedStopRunsWhenItsKeyRowDoesNotRecord(t *testing.T) {
	e, info := protocolStopEnv(t)
	e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
	var mu sync.Mutex
	var lines []string
	e.server(t).ops.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	failOpCreatesAfterTheNext(e)

	rec := e.post(t, "/session/"+info.ID+"/stop", "click-1", `{"turn":"t-1"}`)
	wantStatus(t, "stop", rec, http.StatusOK)
	op := opOf(t, e, decodeCommandAnswer(t, rec))
	if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmSucceeded || op.Scope.Turn != "t-1" {
		t.Fatalf("the stop's op = %+v, want its stop of t-1 closed succeeded", op)
	}
	if unfenced, sent := interruptsSent(e.fs.sp, info.SessionName); unfenced != 0 || sent != 1 {
		t.Fatalf("sent %d session-scoped and %d turn-scoped interrupts, want 0 and 1", unfenced, sent)
	}
	keyID, err := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "click-1")
	if err != nil {
		t.Fatal(err)
	}
	if keyed, err := e.opStore(t).Get(keyID); err == nil {
		t.Fatalf("click-1 names op %+v, want none", keyed)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, op.ID) && strings.Contains(l, "names nothing") }) {
		t.Fatalf("log = %q, want a line saying the key names nothing for op %s", lines, op.ID)
	}
}

// A named stop that succeeded replays to a same-key retry after a restart,
// with no turn or naming the same one, although its session's turns no
// longer read: the session closed, or a read of its turn source fails. The
// key names the stop, so the retry answers from its op, and nothing is sent.
func TestSessionControlOpsNamedStopReplaysWhenItsTurnsDoNotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		// unread makes the session's turns not read, after the stop.
		unread func(t *testing.T, e *opsEnv, info session.Info)
	}{
		{name: "session closed", unread: func(t *testing.T, e *opsEnv, info session.Info) {
			if err := e.store.Close(info.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "turn source unreadable", unread: func(_ *testing.T, e *opsEnv, info session.Info) {
			failing := &frameFailingStore{opsTestStore: e.store, id: info.ID, frame: "worker.(*SessionHandle).turnSession", err: fmt.Errorf("transport: %w", runtime.ErrRuntimeUnavailable)}
			failing.fails.Store(1)
			e.fs.cityBeadStore = failing
		}},
	} {
		for _, retry := range []struct{ name, body string }{
			{name: "no turn"},
			{name: "same turn", body: `{"turn":"t-1"}`},
		} {
			t.Run(tc.name+", retry with "+retry.name, func(t *testing.T) {
				e, info := protocolStopEnv(t)
				e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
				stop := "/session/" + info.ID + "/stop"
				first := e.post(t, stop, "click-1", `{"turn":"t-1"}`)
				wantStatus(t, "the stop", first, http.StatusOK)
				a := decodeCommandAnswer(t, first)
				if a.Op == nil {
					t.Fatalf("the stop = %+v, want its op", a)
				}

				tc.unread(t, e, info)
				e.restart(t)
				rec := e.post(t, stop, "click-1", retry.body)
				wantStatus(t, "the retry", rec, http.StatusOK)
				if r := decodeCommandAnswer(t, rec); !r.Replayed || r.Op == nil || r.Op.ID != a.Op.ID || r.IdempotencyKey != a.IdempotencyKey {
					t.Fatalf("the retry = %+v, want stop %+v replayed", r, a)
				}
				if unfenced, sent := interruptsSent(e.fs.sp, info.SessionName); unfenced != 0 || sent != 1 {
					t.Fatalf("sent %d session-scoped and %d turn-scoped interrupts, want the stop's one", unfenced, sent)
				}
			})
		}
	}
}

// I8: in auto, a /stop the op store failed to record runs exactly as in off:
// the session-scoped legacy stop, whatever its runtime's turn source reads.
func TestSessionControlOpsAutoUnrecordedStopRunsAsOff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *opsEnv, name string)
	}{
		{name: "idle", setup: func(*opsEnv, string) {}},
		{name: "turn running", setup: func(e *opsEnv, name string) {
			e.fs.sp.TurnHistoryScripts[name] = []runtime.TurnHistory{{Current: runtime.TurnState{TurnID: "t-1"}}}
			e.fs.sp.TurnScripts[name] = []string{"t-1", ""}
		}},
		{name: "turn unreadable", setup: func(e *opsEnv, name string) {
			e.fs.sp.TurnHistoryErrors[name] = []error{runtime.ErrRuntimeUnavailable}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			off, offInfo := protocolStopEnvMode(t, "off")
			tc.setup(off, offInfo.SessionName)
			offRec := off.post(t, "/session/"+offInfo.ID+"/stop", "", "")
			offUnfenced, offTurn := interruptsSent(off.fs.sp, offInfo.SessionName)

			auto, autoInfo := protocolStopEnvMode(t, "auto")
			tc.setup(auto, autoInfo.SessionName)
			auto.store.set(func(s *opsTestStore) { s.createErr = errors.New("store down") })
			autoRec := auto.post(t, "/session/"+autoInfo.ID+"/stop", "", "")
			autoUnfenced, autoTurn := interruptsSent(auto.fs.sp, autoInfo.SessionName)

			if autoRec.Code != offRec.Code || autoUnfenced != offUnfenced || autoTurn != offTurn {
				t.Fatalf("auto unrecorded: %d, sent %d unfenced and %d turn-scoped; off: %d, sent %d and %d",
					autoRec.Code, autoUnfenced, autoTurn, offRec.Code, offUnfenced, offTurn)
			}
			if offUnfenced != 1 {
				t.Fatalf("off sent %d session-scoped interrupts, want 1", offUnfenced)
			}
			if n := len(auto.opRows(t)); n != 0 {
				t.Fatalf("op rows = %d, want 0", n)
			}
		})
	}
}

// Shadow ops do not change how a runtime whose turns do not come from its
// protocol is stopped (BUILD-PLAN §10.4): a recorded /stop reads no turn and
// sends the session-scoped legacy interrupt, as off does.
func TestSessionControlOpsNonProtocolStopKeepsTheLegacyStop(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "off")
	e.fs.sessionProvider = runtime.FullFake{Fake: e.fs.sp}
	info := e.session(t, "Keystroke")
	e.fs.sp.TurnHistoryScripts = map[string][]runtime.TurnHistory{info.SessionName: {{Current: runtime.TurnState{TurnID: "t-1"}}}}
	e.fs.sp.TurnScripts = map[string][]string{info.SessionName: {"t-1"}}
	rec := e.post(t, "/session/"+info.ID+"/stop", "", "")
	wantStatus(t, "stop", rec, http.StatusOK)
	a := decodeCommandAnswer(t, rec)
	if op := opOf(t, e, a); op.Scope.Turn != "" || op.Outcome.Degradation != session.DegradationUnfencedInterrupt || !mintedKeyRE.MatchString(a.IdempotencyKey) {
		t.Fatalf("op = %+v key %q, want the session-scoped stop under a minted key", op, a.IdempotencyKey)
	}
	unfenced, turnScoped := interruptsSent(e.fs.sp, info.SessionName)
	if unfenced != 1 || turnScoped != 0 || e.fs.sp.CountCalls("TurnHistory", info.SessionName) != 0 {
		t.Fatalf("sent %d session-scoped and %d turn-scoped interrupts after %d turn reads; want the legacy stop alone",
			unfenced, turnScoped, e.fs.sp.CountCalls("TurnHistory", info.SessionName))
	}
}

// A tmux session whose turns come from its transcript keeps the legacy stop
// too: a recorded /stop does not read its turn (a read that cannot find the
// transcript would otherwise refuse it) and sends the session-scoped
// interrupt, as off does.
func TestSessionControlOpsTranscriptStopKeepsTheLegacyStop(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "off")
	e.fs.sessionProvider = runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: e.fs.sp})
	info := e.session(t, "Transcript")
	if err := e.store.SetMetadata(info.ID, "provider", "claude"); err != nil {
		t.Fatal(err)
	}
	handle, err := e.server(t).workerHandleForSession(e.store, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if caps, err := handle.TurnCapabilities(context.Background()); err != nil || caps.Source != worker.TurnSourceTranscript {
		t.Fatalf("turn capabilities = %+v, %v; the test needs a transcript turn source", caps, err)
	}
	rec := e.post(t, "/session/"+info.ID+"/stop", "", "")
	wantStatus(t, "stop", rec, http.StatusOK)
	if op := opOf(t, e, decodeCommandAnswer(t, rec)); op.Scope.Turn != "" || op.Outcome.Degradation != session.DegradationUnfencedInterrupt {
		t.Fatalf("op = %+v, want the session-scoped stop", op)
	}
	if unfenced, turnScoped := interruptsSent(e.fs.sp, info.SessionName); unfenced != 1 || turnScoped != 0 {
		t.Fatalf("sent %d session-scoped and %d turn-scoped interrupts; want the legacy stop alone", unfenced, turnScoped)
	}
}

// A turn the stop read but the request did not name, which ends before the
// interrupt lands, was nothing to stop: 200 and skipped{not_needed}, as an
// idle session answers, never 409 turn_not_current.
func TestSessionControlOpsReadTurnThatEndsIsNotNeeded(t *testing.T) {
	e, info := protocolStopEnv(t)
	e.fs.sp.TurnHistoryScripts[info.SessionName] = []runtime.TurnHistory{{Current: runtime.TurnState{TurnID: "t-1"}}}
	e.fs.sp.TurnScripts[info.SessionName] = []string{""}
	rec := e.post(t, "/session/"+info.ID+"/stop", "", "")
	wantStatus(t, "stop", rec, http.StatusOK)
	if op := opOf(t, e, decodeCommandAnswer(t, rec)); op.Outcome.Arm != opjournal.ArmSkipped || op.Outcome.Reason != opjournal.ReasonNotNeeded {
		t.Fatalf("op outcome = %+v, want skipped{not_needed}", op.Outcome)
	}
	if unfenced, sent := interruptsSent(e.fs.sp, info.SessionName); unfenced+sent != 0 {
		t.Fatalf("interrupts sent = %d", unfenced+sent)
	}
}

// X12 K3 for /stop: the first attempt interrupted the turn it read and died
// before closing its op. The same-key retry adopts the op; the turn has
// ended, so the stop worked: 200 skipped{not_needed}, nothing sent again.
func TestSessionControlOpsAdoptedStopOfAnEndedTurnIsNotNeeded(t *testing.T) {
	e, info := protocolStopEnv(t)
	op := e.seedSyncOp(t, info.ID, opjournal.KindInterrupt, "stop-1", turnStopScope(t, e, info.ID, "t-1"), interruptOpPolicy, "", legacyop.ExecutorAPI, time.Now())
	rec := e.post(t, "/session/"+info.ID+"/stop", "stop-1", "")
	wantStatus(t, "retry", rec, http.StatusOK)
	got, err := e.opStore(t).Get(op.ID)
	if err != nil || got.Attempt != 1 || got.Outcome.Arm != opjournal.ArmSkipped || got.Outcome.Reason != opjournal.ReasonNotNeeded {
		t.Fatalf("adopted op = %+v, %v; want attempt 1 skipped{not_needed}", got, err)
	}
	if unfenced, sent := interruptsSent(e.fs.sp, info.SessionName); unfenced+sent != 0 {
		t.Fatalf("interrupts sent = %d", unfenced+sent)
	}
}

// deleteFailingStore fails Delete of session beads while failDelete is set.
type deleteFailingStore struct {
	*opsTestStore
	failDelete atomic.Bool
}

func (s *deleteFailingStore) Delete(id string) error {
	if s.failDelete.Load() {
		return errors.New("transport: delete failed")
	}
	return s.opsTestStore.Delete(id)
}

// close?delete=true closes its op only after the delete: a delete that fails
// answers 500 and leaves a client-keyed op open, so the same-key retry
// adopts it and deletes the bead, as off's retry does; a recorded plain close
// under that key meanwhile is another request, 422, and leaves the op to its
// retry. A minted-key op closes failed.
func TestSessionControlOpsCloseDeleteFailureIsRetried(t *testing.T) {
	for _, mode := range []string{"off", "auto"} {
		t.Run(mode, func(t *testing.T) {
			e := newControlOpsEnv(t, "off", mode)
			info := e.session(t, "Close")
			failing := &deleteFailingStore{opsTestStore: e.store}
			e.fs.cityBeadStore = failing
			e.restart(t)
			failing.failDelete.Store(true)
			first := e.post(t, "/session/"+info.ID+"/close?delete=true", "c-1", "")
			wantStatus(t, "failed delete", first, http.StatusInternalServerError)
			if mode == "auto" {
				id, _ := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "c-1")
				if op, err := e.opStore(t).Get(id); err != nil || op.Status == opjournal.StatusClosed {
					t.Fatalf("op after the failed delete = %+v, %v; want open", op, err)
				}
			}
			failing.failDelete.Store(false)
			if mode == "auto" {
				flipped := e.post(t, "/session/"+info.ID+"/close", "c-1", "")
				if flipped.Code != http.StatusUnprocessableEntity || !strings.Contains(flipped.Body.String(), "idempotency_mismatch") {
					t.Fatalf("plain close under the open close?delete=true op's key: %d %s, want 422 idempotency_mismatch", flipped.Code, flipped.Body.String())
				}
			}
			retry := e.post(t, "/session/"+info.ID+"/close?delete=true", "c-1", "")
			wantStatus(t, "retry", retry, http.StatusOK)
			if a := decodeCommandAnswer(t, retry); a.Replayed {
				t.Fatalf("retry replayed %+v instead of deleting", a)
			}
			if _, err := e.store.Get(info.ID); !errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("session bead after the retry: %v, want deleted", err)
			}
		})
	}
	e := newControlOpsEnv(t, "off", "auto")
	info := e.session(t, "Close minted")
	failing := &deleteFailingStore{opsTestStore: e.store}
	e.fs.cityBeadStore = failing
	e.restart(t)
	failing.failDelete.Store(true)
	wantStatus(t, "failed delete", e.post(t, "/session/"+info.ID+"/close?delete=true", "", ""), http.StatusInternalServerError)
	rows := e.opRows(t)
	if len(rows) != 1 {
		t.Fatalf("op rows = %d, want 1", len(rows))
	}
	if op, err := opjournal.DecodeOp(rows[0]); err != nil || op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmFailed {
		t.Fatalf("minted-key op = %+v, %v; want closed failed", op, err)
	}
}

// metadataOf is a copy of bead id's metadata.
func metadataOf(t *testing.T, store beads.Store, id string) map[string]string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return maps.Clone(b.Metadata)
}

// require refuses every command it cannot record before its effect: no
// interrupt, stop, respond, suspend, wake, reset or close lands, and the
// session row is unchanged.
func TestSessionControlOpsRequireRefusalsHaveNoEffect(t *testing.T) {
	e := newControlOpsEnv(t, "require", "require")
	info := e.session(t, "Require effects")
	e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "ok?"})
	before := metadataOf(t, e.store, info.ID)
	startsBefore := e.fs.sp.CountCalls("Start", info.SessionName)
	e.store.set(func(s *opsTestStore) { s.createErr = errors.New("store down") })
	for _, tc := range []struct{ path, body string }{
		{"/stop", ""},
		{"/respond", `{"request_id":"req-1","action":"approve"}`},
		{"/kill", ""},
		{"/suspend", ""},
		{"/wake", ""},
		{"/reset", ""},
		{"/close", ""},
	} {
		rec := e.post(t, "/session/"+info.ID+tc.path, "", tc.body)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "op-journal-unavailable") {
			t.Fatalf("%s: %d %s, want 503 op_journal_unavailable", tc.path, rec.Code, rec.Body.String())
		}
		if after := metadataOf(t, e.store, info.ID); !maps.Equal(before, after) {
			t.Fatalf("%s changed the session row: %v -> %v", tc.path, before, after)
		}
	}
	if got := countSessionResponds(e.fs.sp, info.SessionName); got != 0 {
		t.Fatalf("Respond calls = %d, want 0", got)
	}
	for _, m := range []string{"Interrupt", "Stop"} {
		if got := e.fs.sp.CountCalls(m, info.SessionName); got != 0 {
			t.Fatalf("%s calls = %d, want 0", m, got)
		}
	}
	if got := e.fs.sp.CountCalls("Start", info.SessionName); got != startsBefore {
		t.Fatalf("Start calls = %d, want %d", got, startsBefore)
	}
	if b, err := e.store.Get(info.ID); err != nil || b.Status == "closed" {
		t.Fatalf("session bead = %+v, %v; want open", b, err)
	}
}

// require refuses a wake or suspend of a configured named session with no
// row for a reason that needs no conversation before it materializes the
// session.
func TestSessionControlOpsRequireRefusesBeforeMaterializing(t *testing.T) {
	for _, path := range []string{"/wake", "/suspend"} {
		t.Run(path, func(t *testing.T) {
			e := newControlOpsEnv(t, "off", "require")
			rowsBefore, startsBefore := e.sessionRowsAndStarts(t)
			rec := e.post(t, "/session/"+url.PathEscape("myrig/worker")+path, strings.Repeat("k", 513), "")
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid_idempotency_key") {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if rows, starts := e.sessionRowsAndStarts(t); rows != rowsBefore || starts != startsBefore {
				t.Fatalf("refused after materializing: session rows %d -> %d, starts %d -> %d", rowsBefore, rows, startsBefore, starts)
			}
		})
	}
}

// require refuses a session-scoped command on a session row that names no
// generation, nothing done; auto runs it unrecorded.
func TestSessionControlOpsSessionWithNoGeneration(t *testing.T) {
	for _, mode := range []string{"require", "auto"} {
		t.Run(mode, func(t *testing.T) {
			e := newControlOpsEnv(t, mode, mode)
			info := e.session(t, "No generation")
			if err := e.store.SetMetadata(info.ID, "generation", ""); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct{ path, method string }{{"/stop", "Interrupt"}, {"/kill", "Stop"}} {
				rec := e.post(t, "/session/"+info.ID+tc.path, "", "")
				got := e.fs.sp.CountCalls(tc.method, info.SessionName)
				if mode == "require" {
					if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), legacyop.ReasonNoGeneration) || got != 0 {
						t.Fatalf("%s: %d %s, %s calls %d; want 503 naming %s, nothing done", tc.path, rec.Code, rec.Body.String(), tc.method, got, legacyop.ReasonNoGeneration)
					}
					continue
				}
				if rec.Code != http.StatusOK || got != 1 || decodeCommandAnswer(t, rec).Op != nil {
					t.Fatalf("%s: %d %s, %s calls %d; want it run unrecorded", tc.path, rec.Code, rec.Body.String(), tc.method, got)
				}
			}
			if n := len(e.opRows(t)); n != 0 {
				t.Fatalf("op rows = %d, want 0", n)
			}
		})
	}
}

// panickingInterruptProvider panics in Interrupt while armed.
type panickingInterruptProvider struct {
	*runtime.Fake
	armed atomic.Bool
}

func (p *panickingInterruptProvider) Interrupt(name string) error {
	if p.armed.CompareAndSwap(true, false) {
		panic("injected interrupt panic")
	}
	return p.Fake.Interrupt(name)
}

// A sync command that panics after its op is admitted leaves the op open and
// its in-flight entry released, so the same-key retry adopts it.
func TestSessionControlOpsPanicLeavesTheOpForAdoption(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "off")
	panicking := &panickingInterruptProvider{Fake: e.fs.sp}
	panicking.armed.Store(true)
	e.fs.sessionProvider = panicking
	info := e.session(t, "Panic")
	func() {
		defer func() { _ = recover() }()
		if rec := e.post(t, "/session/"+info.ID+"/stop", "stop-p", ""); rec.Code < 500 {
			t.Fatalf("the panicked stop answered %d: %s", rec.Code, rec.Body.String())
		}
	}()
	id, _ := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "stop-p")
	if op, err := e.opStore(t).Get(id); err != nil || op.Status == opjournal.StatusClosed || op.Attempt != 0 {
		t.Fatalf("op after the panic = %+v, %v; want open at attempt 0", op, err)
	}
	if n := e.sm.cityOpsFor(e.fs.CityName(), e.fs).inflight.Len(); n != 0 {
		t.Fatalf("in-flight entries after the panic = %d", n)
	}
	rec := e.post(t, "/session/"+info.ID+"/stop", "stop-p", "")
	wantStatus(t, "retry", rec, http.StatusOK)
	if a := decodeCommandAnswer(t, rec); a.Op == nil || a.Op.ID != id || a.Op.Attempt != 1 || a.Op.Outcome == nil || a.Op.Outcome.Arm != string(opjournal.ArmSucceeded) {
		t.Fatalf("retry = %+v op %+v, want the op adopted and succeeded", a, a.Op)
	}
}

// A turn-scoped stop on a runtime that fences turns atomically carries no
// degradation.
func TestSessionControlOpsAtomicFenceIsNotDegraded(t *testing.T) {
	e, info := protocolStopEnv(t)
	e.fs.sp.TurnCapabilityScripts[info.SessionName] = []runtime.TurnCapabilities{{
		Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkCorrelated,
	}}
	e.fs.sp.FencedTurnInterrupts = true
	e.fs.sp.TurnHistoryScripts[info.SessionName] = []runtime.TurnHistory{{Current: runtime.TurnState{TurnID: "t-1"}}}
	e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
	rec := e.post(t, "/session/"+info.ID+"/stop", "", "")
	wantStatus(t, "stop", rec, http.StatusOK)
	if op := opOf(t, e, decodeCommandAnswer(t, rec)); op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Degradation != "" {
		t.Fatalf("op outcome = %+v, want succeeded with no degradation", op.Outcome)
	}
}

// On a protocol turn source a recorded /stop never falls back to the
// unfenced session-scoped interrupt: a turn read that finds no runtime
// session bound is nothing to stop, a turn-scoped interrupt the runtime
// refuses is the answer, and a turn source that cannot be read is refused.
func TestSessionControlOpsProtocolStopNeverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(e *opsEnv, name string)
		status int
		want   string
	}{
		{name: "no runtime session bound", status: http.StatusOK, setup: func(e *opsEnv, name string) {
			e.fs.sp.TurnHistoryErrors[name] = []error{runtime.ErrSessionNotFound}
		}},
		{name: "turn identity unsupported", status: http.StatusNotImplemented, want: turnIdentityUnsupportedCode, setup: func(e *opsEnv, name string) {
			e.fs.sp.TurnHistoryErrors[name] = []error{runtime.ErrTurnIdentityUnsupported}
		}},
		{name: "turn interrupt refused", status: http.StatusNotImplemented, want: turnIdentityUnsupportedCode, setup: func(e *opsEnv, name string) {
			e.fs.sp.TurnHistoryScripts[name] = []runtime.TurnHistory{{Current: runtime.TurnState{TurnID: "t-1"}}}
			e.fs.sp.TurnScripts[name] = []string{"t-1"}
			e.fs.sp.InterruptTurnErrors = map[string]error{name: runtime.ErrTurnIdentityUnsupported}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, info := protocolStopEnv(t)
			tc.setup(e, info.SessionName)
			rec := e.post(t, "/session/"+info.ID+"/stop", "", "")
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status %d: %s; want %d %s", rec.Code, rec.Body.String(), tc.status, tc.want)
			}
			if got := e.fs.sp.CountCalls("Interrupt", info.SessionName); got != 0 {
				t.Fatalf("unfenced Interrupt calls = %d, want 0", got)
			}
		})
	}
}

// Off honors the Idempotency-Key on /kill and /suspend from the LRU: a
// same-key retry replays the first answer and does not act again.
func TestSessionControlOpsOffKillAndSuspendHonorTheKey(t *testing.T) {
	e := newControlOpsEnv(t, "off", "off")
	info := e.session(t, "Off keys")
	wantStatus(t, "kill", e.post(t, "/session/"+info.ID+"/kill", "kill-1", ""), http.StatusOK)
	e.fs.sp.StopErrors[info.SessionName] = errors.New("transport: reset")
	wantStatus(t, "kill retry", e.post(t, "/session/"+info.ID+"/kill", "kill-1", ""), http.StatusOK)
	if got := e.fs.sp.CountCalls("Stop", info.SessionName); got != 1 {
		t.Fatalf("Stop calls after a same-key kill = %d, want 1", got)
	}

	other := e.session(t, "Off suspend")
	wantStatus(t, "suspend", e.post(t, "/session/"+other.ID+"/suspend", "suspend-1", ""), http.StatusOK)
	if err := e.store.Close(other.ID); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "suspend retry", e.post(t, "/session/"+other.ID+"/suspend", "suspend-1", ""), http.StatusOK)
	if rec := e.post(t, "/session/"+other.ID+"/suspend", "suspend-2", ""); rec.Code == http.StatusOK {
		t.Fatalf("a new-key suspend of the closed session answered 200, so the LRU replay proves nothing: %s", rec.Body.String())
	}
}

// I9: the always-on named session refusal is a front-door refusal: it
// writes no op.
func TestSessionControlOpsCloseFrontDoorRefusalWritesNoOp(t *testing.T) {
	e := newControlOpsEnv(t, "off", "auto")
	info := e.session(t, "Always on")
	for k, v := range map[string]string{apiNamedSessionMetadataKey: "true", apiNamedSessionModeKey: "always", apiNamedSessionIdentityKey: "myrig/worker"} {
		if err := e.store.SetMetadata(info.ID, k, v); err != nil {
			t.Fatal(err)
		}
	}
	rec := e.post(t, "/session/"+info.ID+"/close", "close-1", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "always-on") {
		t.Fatalf("close: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if n := len(e.opRows(t)); n != 0 {
		t.Fatalf("op rows = %d, want 0", n)
	}
}

// The Idempotency-Key LRU compares a /stop's named turn: the same key with
// another turn is 422 idempotency_mismatch, not a replay of the first stop.
func TestSessionControlOpsStopKeyComparesTheTurn(t *testing.T) {
	e, info := protocolStopEnvMode(t, "off")
	e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
	wantStatus(t, "first", e.post(t, "/session/"+info.ID+"/stop", "k-1", `{"turn":"t-1"}`), http.StatusOK)
	rec := e.post(t, "/session/"+info.ID+"/stop", "k-1", `{"turn":"t-2"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "idempotency_mismatch") {
		t.Fatalf("same key, other turn: %d %s, want 422 idempotency_mismatch", rec.Code, rec.Body.String())
	}
}

// A keyed stop that found nothing to do replays that success after a
// restart, nothing sent.
func TestSessionControlOpsNotNeededStopReplaysAsSuccess(t *testing.T) {
	e, info := protocolStopEnv(t)
	first := decodeCommandAnswer(t, e.post(t, "/session/"+info.ID+"/stop", "idle-1", ""))
	if op := opOf(t, e, first); op.Outcome.Reason != opjournal.ReasonNotNeeded {
		t.Fatalf("idle stop op = %+v", op.Outcome)
	}
	e.restart(t)
	rec := e.post(t, "/session/"+info.ID+"/stop", "idle-1", "")
	wantStatus(t, "replay", rec, http.StatusOK)
	if a := decodeCommandAnswer(t, rec); !a.Replayed || a.Op == nil || a.Op.ID != first.Op.ID {
		t.Fatalf("replay = %+v", a)
	}
}

// A same-key /respond retry that names no request_id replays the first
// respond, scoped to the interaction it answered, after a new interaction
// became pending.
func TestSessionControlOpsRespondRetryWithoutRequestIDReplays(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "off")
	info := e.session(t, "Respond retry")
	e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "ok?"})
	first := e.post(t, "/session/"+info.ID+"/respond", "r-1", `{"action":"approve"}`)
	wantStatus(t, "first", first, http.StatusAccepted)
	if op := opOf(t, e, decodeCommandAnswer(t, first)); op.Scope.Interaction != "req-1" {
		t.Fatalf("respond scope = %+v, want interaction req-1", op.Scope)
	}
	e.restart(t)
	e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-2", Kind: "approval", Prompt: "again?"})
	rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", `{"action":"approve"}`)
	wantStatus(t, "retry", rec, http.StatusAccepted)
	if a := decodeCommandAnswer(t, rec); !a.Replayed {
		t.Fatalf("retry = %+v, want the first respond replayed", a)
	}
	if got := countSessionResponds(e.fs.sp, info.SessionName); got != 1 {
		t.Fatalf("Respond calls = %d, want 1", got)
	}
}

// respondedTo is the request ids of the Respond calls session name got.
func respondedTo(sp *runtime.Fake, name string) []string {
	var ids []string
	for _, c := range sp.SnapshotCalls() {
		if c.Name == name && c.Method == "Respond" {
			ids = append(ids, c.RequestID)
		}
	}
	return ids
}

// failingRespondProvider fails Respond, answering nothing, while fail is set,
// as a tmux approval whose prompt did not clear in time does.
type failingRespondProvider struct {
	*runtime.Fake
	fail atomic.Bool
}

func (p *failingRespondProvider) Respond(name string, response runtime.InteractionResponse) error {
	if p.fail.Load() {
		return errors.New("transport: the approval prompt did not clear")
	}
	return p.Fake.Respond(name, response)
}

// A recorded /respond that names no request_id answers only the interaction
// its op names: the same-key retry that adopts a respond left open at req-1,
// after the agent raised req-2, answers nothing and closes skipped{stale},
// 409, rather than approving req-2.
func TestSessionControlOpsAdoptedRespondAnswersOnlyItsInteraction(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "off")
	failing := &failingRespondProvider{Fake: e.fs.sp}
	e.fs.sessionProvider = failing
	info := e.session(t, "Respond adopted")
	e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "ok?"})
	failing.fail.Store(true)
	if rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", `{"action":"approve"}`); rec.Code == http.StatusAccepted {
		t.Fatalf("failing respond answered 202: %s", rec.Body.String())
	}
	id, _ := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "r-1")
	if op, err := e.opStore(t).Get(id); err != nil || op.Status == opjournal.StatusClosed || op.Scope.Interaction != "req-1" {
		t.Fatalf("respond op after the failed attempt = %+v, %v; want open, scoped to req-1", op, err)
	}

	failing.fail.Store(false)
	e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-2", Kind: "approval", Prompt: "again?"})
	e.restart(t)
	rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", `{"action":"approve"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "invalid_interaction") {
		t.Fatalf("adopted retry: %d %s, want 409 invalid_interaction", rec.Code, rec.Body.String())
	}
	op, err := e.opStore(t).Get(id)
	if err != nil || op.Attempt != 1 || op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmSkipped || op.Outcome.Reason != opjournal.ReasonStale {
		t.Fatalf("adopted respond op = %+v, %v; want attempt 1 closed skipped{stale}", op, err)
	}
	if got := respondedTo(e.fs.sp, info.SessionName); len(got) != 0 {
		t.Fatalf("Respond answered %v, want nothing", got)
	}
}

// landedRespondProvider answers the interaction and then fails Respond while
// fail is set, as a tmux approval whose keystroke landed but whose prompt
// did not clear in time does.
type landedRespondProvider struct {
	*runtime.Fake
	fail atomic.Bool
}

func (p *landedRespondProvider) Respond(name string, response runtime.InteractionResponse) error {
	if err := p.Fake.Respond(name, response); err != nil || !p.fail.Load() {
		return err
	}
	return errors.New("transport: the approval prompt did not clear")
}

// The limit of answering only the op's interaction: a runtime that derives
// request IDs from the prompt's content (tmux hashes the tool and its input)
// gives an identical prompt raised again the first one's ID, so the op
// cannot tell the two apart. A same-key retry adopted after the first answer
// landed and the agent raised the same prompt again answers that one too,
// until OJ-16 keys respond ops by the interaction occurrence.
func TestSessionControlOpsAdoptedRespondAnswersAnIdenticalPromptRaisedAgain(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "off")
	landing := &landedRespondProvider{Fake: e.fs.sp}
	e.fs.sessionProvider = landing
	info := e.session(t, "Respond identical")
	prompt := &runtime.PendingInteraction{RequestID: "tmux-5f0c2a9e41b7d3c8", Kind: "approval", Prompt: "Run npm test?"}
	e.fs.sp.SetPendingInteraction(info.SessionName, prompt)
	landing.fail.Store(true)
	if rec := e.post(t, "/session/"+info.ID+"/respond", "r-1", `{"action":"approve"}`); rec.Code == http.StatusAccepted {
		t.Fatalf("failing respond answered 202: %s", rec.Body.String())
	}

	landing.fail.Store(false)
	e.fs.sp.SetPendingInteraction(info.SessionName, prompt)
	e.restart(t)
	wantStatus(t, "adopted retry", e.post(t, "/session/"+info.ID+"/respond", "r-1", `{"action":"approve"}`), http.StatusAccepted)
	if got := respondedTo(e.fs.sp, info.SessionName); !slices.Equal(got, []string{prompt.RequestID, prompt.RequestID}) {
		t.Fatalf("Respond answered %v, want the prompt and the identical one raised again", got)
	}
}

// raisingRespondProvider holds the first Respond until release; once it
// lands, the agent raises next.
type raisingRespondProvider struct {
	*runtime.Fake
	enterOnce sync.Once
	entered   chan struct{}
	release   chan struct{}
	next      atomic.Pointer[runtime.PendingInteraction]
}

func (p *raisingRespondProvider) Respond(name string, response runtime.InteractionResponse) error {
	p.enterOnce.Do(func() { close(p.entered) })
	<-p.release
	err := p.Fake.Respond(name, response)
	if next := p.next.Swap(nil); err == nil && next != nil {
		p.SetPendingInteraction(name, next)
	}
	return err
}

// Two clicks on one prompt under two keys: the second reads req-1 pending
// while the first holds the session, so its op names req-1. When it runs, the
// first has answered req-1 and the agent raised req-2: the second answers
// nothing and closes skipped{stale}, 409, rather than approving req-2.
func TestSessionControlOpsConcurrentRespondsAnswerOnlyTheirInteraction(t *testing.T) {
	e := newControlOpsEnv(t, "auto", "off")
	blocking := &raisingRespondProvider{Fake: e.fs.sp, entered: make(chan struct{}), release: make(chan struct{})}
	blocking.next.Store(&runtime.PendingInteraction{RequestID: "req-2", Kind: "approval", Prompt: "again?"})
	e.fs.sessionProvider = blocking
	info := e.session(t, "Double click")
	e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "ok?"})

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- e.post(t, "/session/"+info.ID+"/respond", "click-1", `{"action":"approve"}`) }()
	<-blocking.entered
	recorded := make(chan struct{})
	e.store.set(func(s *opsTestStore) { s.onOpCreate = func() { close(recorded) } })
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- e.post(t, "/session/"+info.ID+"/respond", "click-2", `{"action":"approve"}`) }()
	<-recorded
	close(blocking.release)

	wantStatus(t, "click-1", <-firstDone, http.StatusAccepted)
	second := <-secondDone
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "invalid_interaction") {
		t.Fatalf("click-2: %d %s, want 409 invalid_interaction", second.Code, second.Body.String())
	}
	id2, _ := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "click-2")
	if op, err := e.opStore(t).Get(id2); err != nil || op.Scope.Interaction != "req-1" || op.Outcome.Arm != opjournal.ArmSkipped || op.Outcome.Reason != opjournal.ReasonStale {
		t.Fatalf("click-2 op = %+v, %v; want skipped{stale}, scoped to req-1", op, err)
	}
	if got := respondedTo(e.fs.sp, info.SessionName); !slices.Equal(got, []string{"req-1"}) {
		t.Fatalf("Respond answered %v, want [req-1] only", got)
	}
}

// syncEndpoint is one recorded session command, as the per-endpoint tests
// drive it.
type syncEndpoint struct {
	path, body string
	kind       opjournal.Kind
	code       int
	// sessionScoped says the op is scoped to the session generation.
	sessionScoped bool
	policy        opjournal.Policy
	payload       string
	interaction   string
}

var syncEndpoints = []syncEndpoint{
	{path: "/stop", kind: opjournal.KindInterrupt, code: http.StatusOK, sessionScoped: true, policy: interruptOpPolicy},
	{
		path: "/respond", body: `{"request_id":"req-1","action":"approve"}`, kind: opjournal.KindRespond, code: http.StatusAccepted, sessionScoped: true,
		payload: `{"action":"approve","text":""}`, interaction: "req-1", policy: respondOpPolicy,
	},
	{path: "/kill", kind: opjournal.KindKill, code: http.StatusOK, sessionScoped: true},
	{path: "/suspend", kind: opjournal.KindSuspend, code: http.StatusOK},
	{path: "/wake", kind: opjournal.KindWake, code: http.StatusOK},
	{path: "/reset", kind: opjournal.KindRestart, code: http.StatusOK, policy: resetSessionPolicy},
	{path: "/close", kind: opjournal.KindClose, code: http.StatusOK},
}

// newSyncEndpointEnv is an opsEnv recording every session command, with a
// session that has an interaction pending.
func newSyncEndpointEnv(t *testing.T) (*opsEnv, session.Info) {
	t.Helper()
	e := newControlOpsEnv(t, "auto", "auto")
	info := e.session(t, "Endpoint")
	e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "ok?"})
	return e, info
}

// §7.1 as OJ-2b for every session command: an open op another surface
// executes (the CLI) is busy, 409 in_flight, nothing done.
func TestSessionControlOpsOpenCLIOpIsBusy(t *testing.T) {
	for _, ep := range syncEndpoints {
		t.Run(ep.path, func(t *testing.T) {
			e, info := newSyncEndpointEnv(t)
			before := metadataOf(t, e.store, info.ID)
			scope := opjournal.Scope{}
			if ep.sessionScoped {
				scope = opjournal.Scope{Kind: opjournal.ScopeSession, Generation: generationOf(t, e.store, info.ID), Interaction: ep.interaction}
			}
			e.seedSyncOp(t, info.ID, ep.kind, "cli-1", scope, ep.policy, ep.payload, legacyop.ExecutorCLI, time.Now())
			rec := e.post(t, "/session/"+info.ID+ep.path, "cli-1", ep.body)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "idempotency-in-flight") {
				t.Fatalf("status %d: %s, want 409 in_flight", rec.Code, rec.Body.String())
			}
			if after := metadataOf(t, e.store, info.ID); !maps.Equal(before, after) {
				t.Fatalf("the busy request changed the session row: %v -> %v", before, after)
			}
			for _, m := range []string{"Interrupt", "Stop"} {
				if got := e.fs.sp.CountCalls(m, info.SessionName); got != 0 {
					t.Fatalf("%s calls = %d, want 0", m, got)
				}
			}
			if got := countSessionResponds(e.fs.sp, info.SessionName); got != 0 {
				t.Fatalf("Respond calls = %d, want 0", got)
			}
		})
	}
}

// One key is one op whether the request names the session by its session
// name or by its id, on every session command. (A close leaves the name free,
// so its retry finds the op by the id only.)
func TestSessionControlOpsIDAndNameAreOneOp(t *testing.T) {
	for _, ep := range syncEndpoints {
		t.Run(ep.path, func(t *testing.T) {
			e, info := newSyncEndpointEnv(t)
			first := e.post(t, "/session/"+info.SessionName+ep.path, "one-1", ep.body)
			wantStatus(t, "by name", first, ep.code)
			a := decodeCommandAnswer(t, first)
			e.restart(t)
			rec := e.post(t, "/session/"+info.ID+ep.path, "one-1", ep.body)
			wantStatus(t, "by id", rec, ep.code)
			if b := decodeCommandAnswer(t, rec); !b.Replayed || b.Op == nil || a.Op == nil || b.Op.ID != a.Op.ID || b.Op.Conversation != info.ID || b.ID != a.ID {
				t.Fatalf("by id = %+v, want op %+v replayed", b, a.Op)
			}
			if n := len(e.opRows(t)); n != 1 {
				t.Fatalf("op rows = %d, want 1", n)
			}
		})
	}
}

// A close that keeps failing still answers the command's result, and the
// background retry closes the op, on every session command.
func TestSessionControlOpsCloseFailureRetriesInTheBackground(t *testing.T) {
	for _, ep := range syncEndpoints {
		t.Run(ep.path, func(t *testing.T) {
			e, info := newSyncEndpointEnv(t)
			e.store.set(func(s *opsTestStore) { s.updateFails = 4 })
			rec := e.post(t, "/session/"+info.ID+ep.path, "", ep.body)
			wantStatus(t, ep.path, rec, ep.code)
			a := decodeCommandAnswer(t, rec)
			if a.Op == nil {
				t.Fatalf("answer = %+v, want the op", a)
			}
			if op := e.waitClosed(t, a.Op.ID); op.Outcome.Arm != opjournal.ArmSucceeded {
				t.Fatalf("op = %+v", op)
			}
		})
	}
}

// A recorded kill stops only the generation its op names: a same-key retry
// that adopts an op left open after the session restarted refuses
// refused:target_moved and kills nothing, as gc session kill does, and the
// next retry replays that refusal.
func TestSessionControlOpsAdoptedKillOfAMovedSessionIsRefused(t *testing.T) {
	e := newControlOpsEnv(t, "off", "auto")
	info := e.session(t, "Kill moved")
	gen := generationOf(t, e.store, info.ID)
	e.fs.sp.StopErrors[info.SessionName] = errors.New("transport: reset")
	if rec := e.post(t, "/session/"+info.ID+"/kill", "kill-1", ""); rec.Code == http.StatusOK {
		t.Fatalf("failing kill answered 200: %s", rec.Body.String())
	}
	delete(e.fs.sp.StopErrors, info.SessionName)
	if err := e.store.SetMetadata(info.ID, "generation", strconv.FormatInt(gen+41, 10)); err != nil {
		t.Fatal(err)
	}
	e.restart(t)
	for i, want := range []string{refusedTargetMoved, "replayed op"} {
		rec := e.post(t, "/session/"+info.ID+"/kill", "kill-1", "")
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), refusedTargetMoved) || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("retry %d: %d %s, want 409 %s", i+1, rec.Code, rec.Body.String(), want)
		}
	}
	id, _ := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "kill-1")
	op, err := e.opStore(t).Get(id)
	if err != nil || op.Status != opjournal.StatusClosed || op.Outcome.Reason != opjournal.RefusedReasonPrefix+refusedTargetMoved || op.Scope.Generation != gen {
		t.Fatalf("op = %+v, %v; want closed failed{refused:target_moved} at generation %d", op, err, gen)
	}
	if got := e.fs.sp.CountCalls("Stop", info.SessionName); got != 1 {
		t.Fatalf("Stop calls = %d, want only the first, failed kill's", got)
	}
	if !e.fs.sp.IsRunning(info.SessionName) {
		t.Fatal("the refused kill stopped the new run")
	}
}

// reopenAtKillStore reopens closed session id, and starts a new run of it, at
// the kill's first read of the row (session.Manager.Kill: the operator kill
// of a closed row runs no fence): a reopen and a start that land between a
// recorded kill's generation check and its kill. The start takes the
// session's runtime lease, as every start does; startErr is the lease's
// answer, and a start the lease refuses does nothing.
type reopenAtKillStore struct {
	*opsTestStore
	id, city, name string
	sp             *runtime.Fake
	fired          atomic.Bool
	startErr       error
}

func (s *reopenAtKillStore) Get(id string) (beads.Bead, error) {
	if id == s.id && callStackHasFunc("session.(*Manager).Kill") && s.fired.CompareAndSwap(false, true) {
		s.startErr = s.reopenAndStart()
	}
	return s.opsTestStore.Get(id)
}

func (s *reopenAtKillStore) reopenAndStart() error {
	if err := s.Reopen(s.id); err != nil {
		return err
	}
	front := session.NewStore(beads.SessionStore{Store: s.opsTestStore})
	lease, err := session.TryRuntimeLease(front, session.RuntimeLeaseRequest{City: s.city, Name: s.name, ID: s.id, TTL: session.RuntimeLeaseTTLFor(nil)})
	if err != nil {
		return err
	}
	defer lease.Release()
	scope, why, err := sessionOpScope(s.opsTestStore, s.id)
	if why != "" {
		return fmt.Errorf("reading the generation: %s %w", why, err)
	}
	return errors.Join(
		s.SetMetadata(s.id, "generation", strconv.FormatInt(scope.Generation+1, 10)),
		s.sp.Start(context.Background(), s.name, runtime.Config{}))
}

// A recorded kill of a closed row holds the session's runtime lease from its
// generation check through the kill, as on an open row: a reopen and a start
// that land in between find the lease held, so the start waits out the kill
// instead of making a run the kill then stops.
func TestSessionControlOpsKillOfAClosedRowHoldsTheLease(t *testing.T) {
	e := newControlOpsEnv(t, "off", "auto")
	e.fs.sessionProvider = runtime.FullFake{Fake: e.fs.sp}
	info := e.session(t, "Kill closed")
	if err := e.fs.sp.Stop(info.SessionName); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Close(info.ID); err != nil {
		t.Fatal(err)
	}
	reopening := &reopenAtKillStore{opsTestStore: e.store, id: info.ID, city: e.fs.cityPath, name: info.SessionName, sp: e.fs.sp}
	e.fs.cityBeadStore = reopening
	e.restart(t)
	starts := e.fs.sp.CountCalls("Start", info.SessionName)
	rec := e.post(t, "/session/"+info.ID+"/kill", "kill-1", "")
	wantStatus(t, "kill", rec, http.StatusOK)
	if !reopening.fired.Load() {
		t.Fatal("the kill never read the row")
	}
	if !errors.Is(reopening.startErr, session.ErrRuntimeLeaseBusy) {
		t.Fatalf("the start between the generation check and the kill: %v; want the runtime lease busy, held by the kill", reopening.startErr)
	}
	if got := e.fs.sp.CountCalls("Start", info.SessionName); got != starts || e.fs.sp.IsRunning(info.SessionName) {
		t.Fatalf("a run started under the kill: %d Start calls, running %v", got-starts, e.fs.sp.IsRunning(info.SessionName))
	}
}

// A recorded kill whose row does not read, for its runtime lease or under it,
// kills nothing: the read error is retryable, so the client-keyed op stays
// open, and the same-key retry adopts it and kills.
func TestSessionControlOpsKillOfAnUnreadableRowIsRetried(t *testing.T) {
	for _, read := range []struct{ name, frame string }{
		{name: "lease read", frame: "api.(*Server).recordedKillLease"},
		{name: "scope read", frame: "api.killScopeMoved"},
	} {
		t.Run(read.name, func(t *testing.T) {
			e := newControlOpsEnv(t, "off", "auto")
			info := e.session(t, "Kill unreadable")
			failing := &frameFailingStore{opsTestStore: e.store, id: info.ID, frame: read.frame, err: errors.New("transport: read failed")}
			failing.fails.Store(1)
			e.fs.cityBeadStore = failing
			e.restart(t)
			if rec := e.post(t, "/session/"+info.ID+"/kill", "kill-1", ""); rec.Code == http.StatusOK {
				t.Fatalf("kill over an unreadable row answered 200: %s", rec.Body.String())
			}
			if failing.fails.Load() != 0 {
				t.Fatalf("the %s never ran", read.name)
			}
			if got := e.fs.sp.CountCalls("Stop", info.SessionName); got != 0 {
				t.Fatalf("Stop calls = %d, want 0", got)
			}
			id, _ := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "kill-1")
			if op, err := e.opStore(t).Get(id); err != nil || op.Status == opjournal.StatusClosed {
				t.Fatalf("kill op after the failed read = %+v, %v; want open", op, err)
			}
			retry := e.post(t, "/session/"+info.ID+"/kill", "kill-1", "")
			wantStatus(t, "retry", retry, http.StatusOK)
			if a := decodeCommandAnswer(t, retry); a.Op == nil || a.Op.ID != id || a.Op.Attempt != 1 || a.Op.Outcome == nil || a.Op.Outcome.Arm != string(opjournal.ArmSucceeded) {
				t.Fatalf("retry = %+v (op %+v), want the op adopted and succeeded", a, a.Op)
			}
			if got := e.fs.sp.CountCalls("Stop", info.SessionName); got != 1 {
				t.Fatalf("Stop calls = %d, want 1", got)
			}
		})
	}
}

// A /stop body may carry fields the endpoint does not know, in every mode:
// they are ignored, as before the endpoint declared a body.
func TestSessionControlOpsStopIgnoresUnknownBodyFields(t *testing.T) {
	for _, mode := range []string{"off", "auto"} {
		e := newControlOpsEnv(t, mode, "off")
		info := e.session(t, "Body")
		rec := e.post(t, "/session/"+info.ID+"/stop", "", `{"reason":"user"}`)
		wantStatus(t, mode+" stop", rec, http.StatusOK)
		if got := e.fs.sp.CountCalls("Interrupt", info.SessionName); got != 1 {
			t.Fatalf("%s: Interrupt calls = %d, want 1", mode, got)
		}
	}
}

// D12: in require a named stop refuses an invalid Idempotency-Key, although
// the turn's target key stands in for it, nothing sent.
func TestSessionControlOpsRequireNamedStopRefusesAnInvalidKey(t *testing.T) {
	e, info := protocolStopEnvMode(t, "require")
	e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
	rec := e.post(t, "/session/"+info.ID+"/stop", strings.Repeat("k", 513), `{"turn":"t-1"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid_idempotency_key") {
		t.Fatalf("status %d: %s, want 422 invalid_idempotency_key", rec.Code, rec.Body.String())
	}
	if unfenced, sent := interruptsSent(e.fs.sp, info.SessionName); unfenced+sent != 0 {
		t.Fatalf("interrupts sent = %d", unfenced+sent)
	}
	if n := len(e.opRows(t)); n != 0 {
		t.Fatalf("op rows = %d, want 0", n)
	}
}

// A named stop is turn-scoped in every mode: in off, and in an auto that
// could not record it (I8 keeps the request's own turn), it sends the one
// turn-scoped interrupt and never the session-scoped one.
func TestSessionControlOpsNamedStopIsTurnScopedUnrecorded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		control string
		setup   func(e *opsEnv)
	}{
		{name: "off", control: "off", setup: func(*opsEnv) {}},
		{name: "auto unrecorded", control: "auto", setup: func(e *opsEnv) {
			e.store.set(func(s *opsTestStore) { s.createErr = errors.New("store down") })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, info := protocolStopEnvMode(t, tc.control)
			e.fs.sp.TurnScripts[info.SessionName] = []string{"t-1", ""}
			tc.setup(e)
			rec := e.post(t, "/session/"+info.ID+"/stop", "", `{"turn":"t-1"}`)
			wantStatus(t, "named stop", rec, http.StatusOK)
			unfenced, turnScoped := interruptsSent(e.fs.sp, info.SessionName)
			if unfenced != 0 || turnScoped != 1 {
				t.Fatalf("sent %d session-scoped and %d turn-scoped interrupts, want 0 and 1", unfenced, turnScoped)
			}
			if n := len(e.opRows(t)); n != 0 {
				t.Fatalf("op rows = %d, want 0", n)
			}
		})
	}
}

// frameFailingStore fails one Get of bead id while fails is above zero and
// the call stack holds a frame whose function name contains frame: a read
// made on one path, the others served as usual.
type frameFailingStore struct {
	*opsTestStore
	id    string
	frame string
	err   error
	fails atomic.Int32
}

func (s *frameFailingStore) Get(id string) (beads.Bead, error) {
	if id == s.id && s.fails.Load() > 0 && callStackHas(s.frame) {
		s.fails.Add(-1)
		return beads.Bead{}, s.err
	}
	return s.opsTestStore.Get(id)
}

// callStackHasFunc reports whether a caller is the function whose name ends
// in fn: unlike callStackHas, "session.(*Manager).Kill" names Kill alone, not
// KillFencedReport or KillRuntimeGone.
func callStackHasFunc(fn string) bool {
	pcs := make([]uintptr, 64)
	frames := goruntime.CallersFrames(pcs[:goruntime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, fn) {
			return true
		}
		if !more {
			return false
		}
	}
}

// callStackHas reports whether a caller's function name contains frame.
func callStackHas(frame string) bool {
	pcs := make([]uintptr, 64)
	frames := goruntime.CallersFrames(pcs[:goruntime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.Contains(f.Function, frame) {
			return true
		}
		if !more {
			return false
		}
	}
}

// A protocol session whose worker handle or turn capabilities do not read is
// refused before anything is sent, on a fresh stop and on a same-key retry
// that adopts a stop of a turn: the unreadable read never falls back to the
// unfenced session-scoped stop (DES-T3I §14 item 3).
func TestSessionControlOpsUnreadableTurnSourceRefusesTheStop(t *testing.T) {
	for _, read := range []struct {
		name  string
		frame string
	}{
		{name: "worker handle", frame: "worker.(*Factory).SessionByHandle"},
		{name: "turn capabilities", frame: "worker.(*SessionHandle).turnSession"},
	} {
		for _, retry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s retry=%v", read.name, retry), func(t *testing.T) {
				e, info := protocolStopEnv(t)
				e.fs.sp.TurnHistoryScripts[info.SessionName] = []runtime.TurnHistory{{Current: runtime.TurnState{TurnID: "t-2"}}}
				e.fs.sp.TurnScripts[info.SessionName] = []string{"t-2", ""}
				key := ""
				if retry {
					key = "stop-1"
					e.seedSyncOp(t, info.ID, opjournal.KindInterrupt, key, turnStopScope(t, e, info.ID, "t-2"), interruptOpPolicy, "", legacyop.ExecutorAPI, time.Now())
				}
				failing := &frameFailingStore{opsTestStore: e.store, id: info.ID, frame: read.frame, err: fmt.Errorf("transport: %w", runtime.ErrRuntimeUnavailable)}
				failing.fails.Store(1)
				e.fs.cityBeadStore = failing
				e.restart(t)
				rec := e.post(t, "/session/"+info.ID+"/stop", key, "")
				wantStatus(t, "stop", rec, http.StatusServiceUnavailable)
				if failing.fails.Load() != 0 {
					t.Fatalf("the %s read never ran", read.name)
				}
				if unfenced, turnScoped := interruptsSent(e.fs.sp, info.SessionName); unfenced != 0 || turnScoped != 0 || e.fs.sp.CountCalls("Interrupt", info.SessionName) != 0 {
					t.Fatalf("sent %d session-scoped and %d turn-scoped interrupts, want none", unfenced, turnScoped)
				}
			})
		}
	}
}

// A plain close and a close?delete=true are different requests: reusing one's
// Idempotency-Key for the other is 422 idempotency_mismatch, nothing deleted,
// in the process that answered the first (the in-memory cache) and after a
// restart (the close op: a close that deleted its bead closes
// succeeded{via: legacy.deleted}). A replay never deletes, so the bead a
// later close left closed after a reopen stays.
func TestSessionControlOpsCloseAndCloseDeleteUnderOneKey(t *testing.T) {
	for _, mode := range []string{"off", "auto"} {
		t.Run(mode+" one process", func(t *testing.T) {
			e := newControlOpsEnv(t, "off", mode)
			info := e.session(t, "Close twice")
			wantStatus(t, "plain close", e.post(t, "/session/"+info.ID+"/close", "c-1", ""), http.StatusOK)
			rec := e.post(t, "/session/"+info.ID+"/close?delete=true", "c-1", "")
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "idempotency_mismatch") {
				t.Fatalf("close?delete=true under the plain close's key: %d %s, want 422 idempotency_mismatch", rec.Code, rec.Body.String())
			}
			if _, err := e.store.Get(info.ID); err != nil {
				t.Fatalf("session bead after the mismatch: %v, want kept", err)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		between func(t *testing.T, e *opsEnv, id string)
		status  string
	}{
		{name: "closed since", between: func(*testing.T, *opsEnv, string) {}, status: "closed"},
		{name: "reopened", between: func(t *testing.T, e *opsEnv, id string) {
			if err := e.store.Reopen(id); err != nil {
				t.Fatal(err)
			}
		}, status: "open"},
		{name: "reopened and closed again", between: func(t *testing.T, e *opsEnv, id string) {
			if err := e.store.Reopen(id); err != nil {
				t.Fatal(err)
			}
			wantStatus(t, "the later close", e.post(t, "/session/"+id+"/close", "c-2", ""), http.StatusOK)
		}, status: "closed"},
	} {
		t.Run("auto restart "+tc.name, func(t *testing.T) {
			e := newControlOpsEnv(t, "off", "auto")
			info := e.session(t, "Close twice")
			wantStatus(t, "plain close", e.post(t, "/session/"+info.ID+"/close", "c-1", ""), http.StatusOK)
			tc.between(t, e, info.ID)
			e.restart(t)
			rec := e.post(t, "/session/"+info.ID+"/close?delete=true", "c-1", "")
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "idempotency_mismatch") {
				t.Fatalf("close?delete=true under the plain close's key after a restart: %d %s, want 422 idempotency_mismatch", rec.Code, rec.Body.String())
			}
			if b, err := e.store.Get(info.ID); err != nil || b.Status != tc.status {
				t.Fatalf("session bead after the mismatch = %+v, %v; want it kept, %s", b, err, tc.status)
			}
		})
	}
	t.Run("auto restart deleted", func(t *testing.T) {
		e := newControlOpsEnv(t, "off", "auto")
		info := e.session(t, "Close deleted")
		first := e.post(t, "/session/"+info.ID+"/close?delete=true", "c-1", "")
		wantStatus(t, "close?delete=true", first, http.StatusOK)
		if op := opOf(t, e, decodeCommandAnswer(t, first)); op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != legacyop.ViaLegacyDeleted {
			t.Fatalf("close?delete=true op outcome = %+v, want succeeded{via: %s}", op.Outcome, legacyop.ViaLegacyDeleted)
		}
		e.restart(t)
		// The bead is gone, so its id resolves to no session: the retry is
		// 404 before any op is read, and nothing runs.
		rec := e.post(t, "/session/"+info.ID+"/close?delete=true", "c-1", "")
		wantStatus(t, "retry of the completed delete", rec, http.StatusNotFound)
	})
	t.Run("auto op recorded without the delete payload", func(t *testing.T) {
		// A close?delete=true op recorded before close ops carried their
		// delete has no payload, as a plain close's: a plain close under its
		// key matches its digest and replays it, and its via refuses that.
		e := newControlOpsEnv(t, "off", "auto")
		info := e.session(t, "Close old row")
		op := e.seedSyncOp(t, info.ID, opjournal.KindClose, "c-1", opjournal.Scope{}, opjournal.Policy{}, "", legacyop.ExecutorAPI, time.Now())
		deleted := opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: legacyop.ViaLegacyDeleted}}
		if _, err := e.opStore(t).CloseKnown(op, deleted, string(legacyop.ExecutorAPI)); err != nil {
			t.Fatal(err)
		}
		rec := e.post(t, "/session/"+info.ID+"/close", "c-1", "")
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "recorded a close with delete=true") {
			t.Fatalf("plain close under the old close?delete=true op's key: %d %s, want 422 naming the delete", rec.Code, rec.Body.String())
		}
		if b, err := e.store.Get(info.ID); err != nil || b.Status != "open" {
			t.Fatalf("session bead after the mismatch = %+v, %v; want it kept open", b, err)
		}
	})
}

// A close op carries its delete, so a same-key retry that changes it is 422
// idempotency_mismatch at admission even while the key's op is open, left by
// a retryable failure for a same-key retry to adopt: nothing closes, nothing
// is deleted, and the op stays as the failure left it. In the process that
// answered the failure (its in-memory cache released the key) and after a
// restart, in both directions.
func TestSessionControlOpsCloseRetryThatChangesDeleteIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, first, retry string }{
		{name: "close then close?delete=true", first: "/close", retry: "/close?delete=true"},
		{name: "close?delete=true then close", first: "/close?delete=true", retry: "/close"},
	} {
		for _, restart := range []bool{false, true} {
			name := tc.name + ", one process"
			if restart {
				name = tc.name + ", after a restart"
			}
			t.Run(name, func(t *testing.T) {
				e := newControlOpsEnv(t, "off", "auto")
				info := e.session(t, "Close changed")
				e.fs.sp.StopErrors[info.SessionName] = errors.New("transport: reset")
				if rec := e.post(t, "/session/"+info.ID+tc.first, "c-1", ""); rec.Code == http.StatusOK {
					t.Fatalf("failing close answered 200: %s", rec.Body.String())
				}
				delete(e.fs.sp.StopErrors, info.SessionName)
				id, _ := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "c-1")
				open, err := e.opStore(t).Get(id)
				if err != nil || open.Status == opjournal.StatusClosed {
					t.Fatalf("close op after the failed close = %+v, %v; want open", open, err)
				}
				if restart {
					e.restart(t)
				}
				rec := e.post(t, "/session/"+info.ID+tc.retry, "c-1", "")
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "idempotency_mismatch") {
					t.Fatalf("%s under the open op's key: %d %s, want 422 idempotency_mismatch", tc.retry, rec.Code, rec.Body.String())
				}
				if got := e.fs.sp.CountCalls("Stop", info.SessionName); got != 1 {
					t.Fatalf("Stop calls = %d, want only the failed close's", got)
				}
				if b, err := e.store.Get(info.ID); err != nil || b.Status != "open" {
					t.Fatalf("session bead after the mismatch = %+v, %v; want it kept open", b, err)
				}
				if op, err := e.opStore(t).Get(id); err != nil || op.Status != open.Status || op.Attempt != open.Attempt {
					t.Fatalf("close op after the mismatch = %+v, %v; want it as the failed close left it", op, err)
				}
			})
		}
	}
}

// leaseFreeMetadata is meta without the runtime lease record, which taking
// the kill's lease writes on every path, gc session kill's too.
func leaseFreeMetadata(meta map[string]string) map[string]string {
	out := maps.Clone(meta)
	for k := range out {
		if strings.HasPrefix(k, "runtime_lease_") {
			delete(out, k)
		}
	}
	return out
}

// openKillAtMovedGeneration leaves a client-keyed kill op open at session
// id's generation (a retryable Stop failure), then moves the session to a
// later generation, and returns the op's ID and the generation it names.
func openKillAtMovedGeneration(t *testing.T, e *opsEnv, info session.Info) (string, int64) {
	t.Helper()
	gen := generationOf(t, e.store, info.ID)
	e.fs.sp.StopErrors[info.SessionName] = errors.New("transport: reset")
	if rec := e.post(t, "/session/"+info.ID+"/kill", "kill-1", ""); rec.Code == http.StatusOK {
		t.Fatalf("failing kill answered 200: %s", rec.Body.String())
	}
	delete(e.fs.sp.StopErrors, info.SessionName)
	opID, _ := opjournal.DeriveID("gc", info.ID, legacyop.PrincipalOperator, "kill-1")
	if op, err := e.opStore(t).Get(opID); err != nil || op.Status == opjournal.StatusClosed {
		t.Fatalf("kill op after the failed kill = %+v, %v; want open", op, err)
	}
	if err := e.store.SetMetadata(info.ID, "generation", strconv.FormatInt(gen+41, 10)); err != nil {
		t.Fatal(err)
	}
	return opID, gen
}

// The generation check of an adopted kill runs before the kill does
// anything, as gc session kill's does: a later run that died, or a
// configured named session asleep with its runtime gone and a wake pending,
// is refused refused:target_moved and its row left as it was (no asleep
// sync, the pending wake kept).
func TestSessionControlOpsAdoptedKillOfAMovedSessionTouchesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, e *opsEnv, info session.Info)
	}{
		{name: "later run died", setup: func(*testing.T, *opsEnv, session.Info) {}},
		{name: "named session, runtime gone, wake pending", setup: func(t *testing.T, e *opsEnv, info session.Info) {
			for k, v := range map[string]string{
				"configured_named_identity": "chat", "state": "asleep", "sleep_reason": "idle",
				"wake_request": "true", "wake_requested_at": time.Now().UTC().Format(time.RFC3339),
			} {
				if err := e.store.SetMetadata(info.ID, k, v); err != nil {
					t.Fatal(err)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newControlOpsEnv(t, "off", "auto")
			e.fs.sessionProvider = runtime.FullFake{Fake: e.fs.sp}
			info := e.session(t, "Kill moved")
			opID, gen := openKillAtMovedGeneration(t, e, info)
			if err := e.fs.sp.Stop(info.SessionName); err != nil { // the later run died
				t.Fatal(err)
			}
			tc.setup(t, e, info)
			before := leaseFreeMetadata(metadataOf(t, e.store, info.ID))
			stops := e.fs.sp.CountCalls("Stop", info.SessionName)
			e.restart(t)
			rec := e.post(t, "/session/"+info.ID+"/kill", "kill-1", "")
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), refusedTargetMoved) {
				t.Fatalf("adopted kill: %d %s, want 409 %s", rec.Code, rec.Body.String(), refusedTargetMoved)
			}
			op, err := e.opStore(t).Get(opID)
			if err != nil || op.Status != opjournal.StatusClosed || op.Outcome.Reason != opjournal.RefusedReasonPrefix+refusedTargetMoved || op.Scope.Generation != gen {
				t.Fatalf("op = %+v, %v; want closed failed{refused:target_moved} at generation %d", op, err, gen)
			}
			if after := leaseFreeMetadata(metadataOf(t, e.store, info.ID)); !maps.Equal(before, after) {
				t.Fatalf("the refused kill changed the row:\nbefore %v\nafter  %v", before, after)
			}
			if got := e.fs.sp.CountCalls("Stop", info.SessionName); got != stops {
				t.Fatalf("Stop calls = %d, want %d: the refused kill stopped the runtime", got, stops)
			}
		})
	}
}

// An op a crash left open before its legacy effect ran (X12's K1: the row is
// durable, nothing answered, nothing acted) is adopted by a same-key retry in
// a new API process, and the adopted retry runs the legacy command: the
// session is suspended, woken or reset, not only the op closed succeeded at
// attempt 1.
func TestSessionControlOpsAdoptedLifecycleCommandReExecutes(t *testing.T) {
	for _, tc := range []struct {
		path      string
		kind      opjournal.Kind
		policy    opjournal.Policy
		suspended bool
		acted     func(t *testing.T, stops int, md map[string]string)
	}{
		{"/suspend", opjournal.KindSuspend, opjournal.Policy{}, false, func(t *testing.T, stops int, md map[string]string) {
			if md["state"] != "suspended" || stops != 1 {
				t.Fatalf("state %q, %d Stop calls; want suspended by one Stop", md["state"], stops)
			}
		}},
		{"/wake", opjournal.KindWake, opjournal.Policy{}, true, func(t *testing.T, _ int, md map[string]string) {
			if md["state"] == "suspended" || md["wake_request"] != "explicit" || md["wake_requested_at"] == "" {
				t.Fatalf("state %q wake_request %q at %q; want the explicit wake recorded", md["state"], md["wake_request"], md["wake_requested_at"])
			}
		}},
		{"/reset", opjournal.KindRestart, resetSessionPolicy, false, func(t *testing.T, _ int, md map[string]string) {
			if md["restart_requested"] != "true" || md["continuation_reset_pending"] != "true" {
				t.Fatalf("restart_requested %q continuation_reset_pending %q; want the fresh restart requested", md["restart_requested"], md["continuation_reset_pending"])
			}
		}},
	} {
		t.Run(strings.TrimPrefix(tc.path, "/"), func(t *testing.T) {
			e := newControlOpsEnv(t, "off", "auto")
			info := e.session(t, "Adopted "+tc.path)
			if tc.suspended {
				wantStatus(t, "fixture suspend", e.post(t, "/session/"+info.ID+"/suspend", "", ""), http.StatusOK)
			}
			stops := e.fs.sp.CountCalls("Stop", info.SessionName)
			op := e.seedSyncOp(t, info.ID, tc.kind, "k-1", opjournal.Scope{}, tc.policy, "", legacyop.ExecutorAPI, time.Now())
			e.restart(t)
			rec := e.post(t, "/session/"+info.ID+tc.path, "k-1", "")
			wantStatus(t, "adopting retry", rec, http.StatusOK)
			if a := decodeCommandAnswer(t, rec); a.Replayed {
				t.Fatalf("adopting retry answered a replay: %+v", a)
			}
			got, err := e.opStore(t).Get(op.ID)
			if err != nil || got.Status != opjournal.StatusClosed || got.Attempt != 1 || got.Outcome.Arm != opjournal.ArmSucceeded {
				t.Fatalf("adopted op = %+v, %v; want closed succeeded at attempt 1", got, err)
			}
			tc.acted(t, e.fs.sp.CountCalls("Stop", info.SessionName)-stops, metadataOf(t, e.store, info.ID))
		})
	}
}
