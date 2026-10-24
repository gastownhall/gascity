package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// The producer API (DES-O1 §11, OJ-20): Propose, Get, the open-op list and
// Cancel over a session's conversation lane.

// lanePokeState is the city's State with a conversation lane host that
// executes kinds, latched when it was made as the controller's host latches
// them at start, and records its doorbell.
type lanePokeState struct {
	*fakeState
	kinds []string
	mu    sync.Mutex
	pokes []string
}

func (l *lanePokeState) ConversationLaneKinds() []string {
	return append([]string(nil), l.kinds...)
}

func (l *lanePokeState) PokeConversationLane(conv string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pokes = append(l.pokes, conv)
}

func (l *lanePokeState) poked() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.pokes...)
}

// newLaneOpsEnv is an opsEnv whose [ops] authoritative lists kinds, served
// through a State whose running conversation lane host executes them and
// records its pokes. Every ops gate stays off: the producer API does not
// depend on them.
func newLaneOpsEnv(t *testing.T, kinds ...string) (*opsEnv, *lanePokeState) {
	t.Helper()
	e := newControlOpsEnv(t, "off", "off")
	e.fs.cfg.Ops.Authoritative = kinds
	st := &lanePokeState{fakeState: e.fs, kinds: kinds}
	e.resolver.state = st
	return e, st
}

func (e *opsEnv) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, cityURL(e.fs, path), nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func decodeInto[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return v
}

// problemCode is a problem+json answer's code.
func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decodeInto[struct {
		Code string `json:"code"`
	}](t, rec).Code
}

func TestProposeWritesALaneOpAndPokesTheLane(t *testing.T) {
	e, st := newLaneOpsEnv(t, "message")
	info := e.session(t, "Lane")
	body := `{"kind":"message","client_key":"msg-1","payload":"hello","principal":"t3:env-a","actor":"user","client_seq":7,"causation":"ui","correlation":"thread-9"}`
	rec := e.post(t, "/session/"+info.ID+"/ops", "", body)
	wantStatus(t, "propose", rec, http.StatusCreated)
	got := decodeInto[ConversationOpProposeResult](t, rec)
	if !got.Created || got.ClientKey != "msg-1" || got.Status != "submitted" || got.Cursor == "" {
		t.Fatalf("answer = %+v", got)
	}
	wantID, err := opjournal.DeriveID("gc", info.ID, "t3:env-a", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.OpID != wantID || got.Op.ID != wantID {
		t.Fatalf("op id = %s / %s, want %s", got.OpID, got.Op.ID, wantID)
	}
	op, err := e.opStore(t).Get(wantID)
	if err != nil {
		t.Fatal(err)
	}
	if op.Executor != opjournal.ExecutorLane || op.Payload != "hello" || op.ClientSeq != 7 ||
		op.Producer.Principal != "t3:env-a" || op.Producer.Actor != "user" ||
		op.Causation != "ui" || op.Correlation != "thread-9" || op.Kind != opjournal.KindMessage {
		t.Fatalf("stored op = %+v", op)
	}
	if op.Policy.Message.When != opjournal.WhenBoundary || op.Policy.Message.Role != opjournal.RolePrompt {
		t.Fatalf("default policy = %+v", op.Policy.Message)
	}
	if got := st.poked(); len(got) != 1 || got[0] != info.ID {
		t.Fatalf("pokes = %v, want [%s]", got, info.ID)
	}
	if strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("the answer carries the payload: %s", rec.Body.String())
	}

	// The same request again is the same op, 200, and rings nothing new.
	again := e.post(t, "/session/"+info.ID+"/ops", "", body)
	wantStatus(t, "repropose", again, http.StatusOK)
	if a := decodeInto[ConversationOpProposeResult](t, again); a.Created || a.OpID != wantID {
		t.Fatalf("repropose = %+v", a)
	}
	if got := st.poked(); len(got) != 1 {
		t.Fatalf("a replayed proposal poked the lane: %v", got)
	}
}

func TestProposeRefusesAKindNotListedAuthoritative(t *testing.T) {
	for _, kinds := range [][]string{nil, {"message"}} {
		e, st := newLaneOpsEnv(t, kinds...)
		info := e.session(t, "Off")
		kind := "message"
		if len(kinds) > 0 {
			kind = "interrupt"
		}
		rec := e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"`+kind+`","client_key":"k"}`)
		wantStatus(t, "propose "+kind, rec, http.StatusConflict)
		if code := problemCode(t, rec); code != "kind-not-enabled" {
			t.Fatalf("code = %q, want kind-not-enabled", code)
		}
		if rows := e.opRows(t); len(rows) != 0 {
			t.Fatalf("a refused kind wrote %d op rows", len(rows))
		}
		if p := st.poked(); len(p) != 0 {
			t.Fatalf("a refused kind poked the lane: %v", p)
		}
	}
}

func TestProposeKeyReuseNamesTheExistingOp(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Reuse")
	first := e.post(t, "/session/"+info.ID+"/ops", "k-1", `{"kind":"message","payload":"one"}`)
	wantStatus(t, "first", first, http.StatusCreated)
	id := decodeInto[ConversationOpProposeResult](t, first).OpID
	rec := e.post(t, "/session/"+info.ID+"/ops", "k-1", `{"kind":"message","payload":"two"}`)
	wantStatus(t, "reuse", rec, http.StatusConflict)
	p := decodeInto[struct {
		Code   string `json:"code"`
		Errors []struct {
			Location string `json:"location"`
			Value    string `json:"value"`
		} `json:"errors"`
	}](t, rec)
	if p.Code != "key-reuse" || len(p.Errors) != 1 || p.Errors[0].Location != "op_id" || p.Errors[0].Value != id {
		t.Fatalf("problem = %+v, want key-reuse naming %s", p, id)
	}
}

func TestProposeKeys(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Keys")

	// Neither header nor body: minted and returned.
	rec := e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"message","payload":"m"}`)
	wantStatus(t, "minted", rec, http.StatusCreated)
	got := decodeInto[ConversationOpProposeResult](t, rec)
	if !mintedKeyRE.MatchString(got.ClientKey) {
		t.Fatalf("minted key = %q", got.ClientKey)
	}
	if op, err := e.opStore(t).Get(got.OpID); err != nil || op.ClientKey != got.ClientKey || op.Producer.Principal != "operator" || op.Producer.Actor != "operator" {
		t.Fatalf("minted op = %+v, %v", op, err)
	}

	// Header and body agreeing is one key.
	rec = e.post(t, "/session/"+info.ID+"/ops", "same", `{"kind":"message","client_key":"same","payload":"m"}`)
	wantStatus(t, "agree", rec, http.StatusCreated)

	// Header and body disagreeing is refused, with nothing written.
	before := len(e.opRows(t))
	rec = e.post(t, "/session/"+info.ID+"/ops", "one", `{"kind":"message","client_key":"two","payload":"m"}`)
	wantStatus(t, "disagree", rec, http.StatusUnprocessableEntity)
	if after := len(e.opRows(t)); after != before {
		t.Fatalf("a refused proposal wrote %d rows", after-before)
	}
}

func TestProposeValidatesTheProducer(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Producer")
	for _, tc := range []struct{ name, body string }{
		{"t3 needs an actor", `{"kind":"message","client_key":"a","principal":"t3:env"}`},
		{"agent principal is reserved", `{"kind":"message","client_key":"b","principal":"conversation:` + info.ID + `","actor":"agent"}`},
		{"gc principal is reserved", `{"kind":"message","client_key":"c","principal":"gc:backstop","actor":"controller"}`},
		{"bad actor", `{"kind":"message","client_key":"d","principal":"t3:env","actor":"User Name"}`},
		{"policy of another kind", `{"kind":"message","client_key":"e","policy":{"interrupt":{"on_failure":"fail"}}}`},
		{"unknown when", `{"kind":"message","client_key":"f","policy":{"message":{"when":"later"}}}`},
	} {
		rec := e.post(t, "/session/"+info.ID+"/ops", "", tc.body)
		wantStatus(t, tc.name, rec, http.StatusUnprocessableEntity)
	}
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("refused proposals wrote %d op rows", len(rows))
	}
}

func TestProposeCarriesPolicyScopeAndTimes(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Policy")
	gen := generationOf(t, e.store, info.ID)
	notBefore := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	startBy := notBefore.Add(time.Hour)
	body := `{"kind":"message","client_key":"p","payload":"x","policy":{"guarantee":"at_most_once","message":{"when":"if_idle","wake":"live_only","role":"notice"}},` +
		`"scope":{"kind":"session","generation":` + jsonInt(gen) + `},"not_before":"` + notBefore.Format(time.RFC3339) + `","start_by":"` + startBy.Format(time.RFC3339) + `"}`
	rec := e.post(t, "/session/"+info.ID+"/ops", "", body)
	wantStatus(t, "propose", rec, http.StatusCreated)
	op, err := e.opStore(t).Get(decodeInto[ConversationOpProposeResult](t, rec).OpID)
	if err != nil {
		t.Fatal(err)
	}
	m := op.Policy.Message
	if op.Policy.Guarantee != opjournal.GuaranteeAtMostOnce || m.When != opjournal.WhenIfIdle || m.Wake != opjournal.WakeLiveOnly || m.Role != opjournal.RoleNotice {
		t.Fatalf("policy = %+v", op.Policy)
	}
	if op.Scope.Kind != opjournal.ScopeSession || op.Scope.Generation != gen {
		t.Fatalf("scope = %+v", op.Scope)
	}
	if !op.NotBefore.Equal(notBefore) || !op.StartBy.Equal(startBy) {
		t.Fatalf("times = %s %s", op.NotBefore, op.StartBy)
	}
}

func TestProposeUnknownSessionIs404(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	rec := e.post(t, "/session/no-such-session/ops", "", `{"kind":"message","client_key":"k","payload":"x"}`)
	wantStatus(t, "unknown session", rec, http.StatusNotFound)
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("wrote %d op rows", len(rows))
	}
}

func TestOpsGetListAndCancel(t *testing.T) {
	e, st := newLaneOpsEnv(t, "message")
	info := e.session(t, "Ops")
	other := e.session(t, "Other")
	var ids []string
	for _, key := range []string{"a", "b"} {
		rec := e.post(t, "/session/"+info.ID+"/ops", key, `{"kind":"message","payload":"`+key+`","principal":"t3:env","actor":"user","client_seq":1}`)
		wantStatus(t, "propose "+key, rec, http.StatusCreated)
		ids = append(ids, decodeInto[ConversationOpProposeResult](t, rec).OpID)
	}

	rec := e.get(t, "/session/"+info.ID+"/ops/"+ids[0])
	wantStatus(t, "get", rec, http.StatusOK)
	op := decodeInto[ConversationOp](t, rec)
	if op.ID != ids[0] || op.Executor != "lane" || op.Principal != "t3:env" || op.Actor != "user" || op.ClientKey != "a" || op.ClientSeq != 1 || op.CreatedAt == nil || op.At == nil {
		t.Fatalf("get = %+v", op)
	}

	// An op of another session is not found there.
	wantStatus(t, "get elsewhere", e.get(t, "/session/"+other.ID+"/ops/"+ids[0]), http.StatusNotFound)
	wantStatus(t, "get missing", e.get(t, "/session/"+info.ID+"/ops/gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa"), http.StatusNotFound)

	rec = e.get(t, "/session/"+info.ID+"/ops")
	wantStatus(t, "list", rec, http.StatusOK)
	list := decodeInto[ListBody[ConversationOp]](t, rec)
	if list.Total != 2 || len(list.Items) != 2 || list.Items[0].ID != ids[0] || list.Items[1].ID != ids[1] {
		t.Fatalf("list = %+v", list)
	}
	if rec := e.get(t, "/session/"+other.ID+"/ops"); decodeInto[ListBody[ConversationOp]](t, rec).Total != 0 {
		t.Fatalf("another session lists %s", rec.Body.String())
	}

	pokes := len(st.poked())
	rec = e.post(t, "/session/"+info.ID+"/ops/"+ids[0]+"/cancel", "", `{"principal":"t3:env","reason":"edited"}`)
	wantStatus(t, "cancel", rec, http.StatusOK)
	c := decodeInto[ConversationOpCancelResult](t, rec)
	if c.Result != "closed" || c.Op.Status != "closed" || c.Op.Outcome == nil || c.Op.Outcome.Arm != "canceled" {
		t.Fatalf("cancel = %+v", c)
	}
	if got := st.poked(); len(got) != pokes+1 || got[len(got)-1] != info.ID {
		t.Fatalf("cancel pokes = %v", got)
	}
	stored, err := e.opStore(t).Get(ids[0])
	if err != nil || stored.Cancel.By != "t3:env" || stored.Cancel.Reason != "edited" {
		t.Fatalf("stored cancel = %+v, %v", stored.Cancel, err)
	}
	again := e.post(t, "/session/"+info.ID+"/ops/"+ids[0]+"/cancel", "", "")
	wantStatus(t, "cancel again", again, http.StatusOK)
	if c := decodeInto[ConversationOpCancelResult](t, again); c.Result != "already_terminal" {
		t.Fatalf("second cancel = %+v", c)
	}
	if rec := e.get(t, "/session/"+info.ID+"/ops"); decodeInto[ListBody[ConversationOp]](t, rec).Total != 1 {
		t.Fatalf("a canceled op is still listed open: %s", rec.Body.String())
	}
	wantStatus(t, "cancel elsewhere", e.post(t, "/session/"+other.ID+"/ops/"+ids[1]+"/cancel", "", ""), http.StatusNotFound)
}

// A legacy op (an O0 shadow op) is closed only by its legacy path: Cancel
// refuses it.
func TestCancelRefusesALegacyOp(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Legacy")
	op, _, err := e.opStore(t).Propose(opjournal.Request{
		Conversation: info.ID, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLegacyAPI,
		ClientKey: "legacy", Producer: opjournal.Producer{Principal: "operator", Actor: "operator"}, Payload: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := e.post(t, "/session/"+info.ID+"/ops/"+op.ID+"/cancel", "", "")
	wantStatus(t, "cancel legacy", rec, http.StatusConflict)
}

// The op routes read, and with [ops] authoritative empty Propose refuses:
// a city that lists no kind writes no row through them.
func TestOpsRoutesWithNothingAuthoritative(t *testing.T) {
	e, _ := newLaneOpsEnv(t)
	info := e.session(t, "Idle")
	rec := e.get(t, "/session/"+info.ID+"/ops")
	wantStatus(t, "list", rec, http.StatusOK)
	if list := decodeInto[ListBody[ConversationOp]](t, rec); list.Total != 0 || list.Items == nil {
		t.Fatalf("list = %s", rec.Body.String())
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// putRuntimeRow writes conv's runtime row with turn cells.
func (e *opsEnv) putRuntimeRow(t *testing.T, conv string, cells opjournal.TurnCells) {
	t.Helper()
	b, err := opjournal.EncodeRuntimeRow("gc", opjournal.RuntimeRow{Conversation: conv, Generation: cells.Generation, Turn: cells})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Create(b); err != nil {
		t.Fatal(err)
	}
}

// X11 §5.3 on the wire (OJ-20): a session read carries the turn cells and
// the capabilities stamp the lane's observer recorded, only while [ops]
// authoritative lists a kind, the only time a lane host runs an observer.
func TestSessionGetExposesTheObservedTurn(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	caps := opjournal.TurnCapsStamp{{Key: "start", Value: "recorded"}, {Key: "fence", Value: "atomic"}}

	t.Run("listed", func(t *testing.T) {
		e, _ := newLaneOpsEnv(t, "message")
		info := e.session(t, "Turn")
		gen := generationOf(t, e.store, info.ID)
		e.putRuntimeRow(t, info.ID, opjournal.TurnCells{
			Generation: gen, Caps: caps, ID: "turn-7", State: opjournal.TurnStateRunning, At: at,
			LastID: "turn-6", LastEnd: opjournal.TurnEndCompleted, LastAt: at.Add(-time.Minute), ObservedAt: at,
		})
		rec := e.get(t, "/session/"+info.ID)
		wantStatus(t, "get", rec, http.StatusOK)
		got := decodeInto[struct {
			Turn *SessionTurn `json:"turn"`
		}](t, rec).Turn
		if got == nil || got.Generation != gen || got.State != "running" || got.ID != "turn-7" || got.LastID != "turn-6" || got.LastEnd != "completed" ||
			got.At == nil || !got.At.Equal(at) || len(got.Caps) != 2 || got.Caps[1] != (SessionTurnCapPair{Key: "fence", Value: "atomic"}) ||
			got.LastAt == nil || !got.LastAt.Equal(at.Add(-time.Minute)) || got.ObservedAt == nil || !got.ObservedAt.Equal(at) {
			t.Fatalf("turn = %+v", got)
		}
	})

	t.Run("another generation reads unknown", func(t *testing.T) {
		e, _ := newLaneOpsEnv(t, "message")
		info := e.session(t, "Stale")
		gen := generationOf(t, e.store, info.ID)
		e.putRuntimeRow(t, info.ID, opjournal.TurnCells{Generation: gen + 1, Caps: caps, ID: "turn-9", State: opjournal.TurnStateRunning, At: at})
		got := decodeInto[struct {
			Turn *SessionTurn `json:"turn"`
		}](t, e.get(t, "/session/"+info.ID)).Turn
		if got == nil || got.State != "unknown" || got.ID != "" || got.Generation != gen+1 || len(got.Caps) != 0 {
			t.Fatalf("turn = %+v", got)
		}
	})

	t.Run("no runtime row", func(t *testing.T) {
		e, _ := newLaneOpsEnv(t, "message")
		info := e.session(t, "None")
		if strings.Contains(e.get(t, "/session/"+info.ID).Body.String(), `"turn"`) {
			t.Fatal("a session without a runtime row carries a turn")
		}
	})

	t.Run("nothing listed", func(t *testing.T) {
		e, _ := newLaneOpsEnv(t)
		info := e.session(t, "Unlisted")
		e.putRuntimeRow(t, info.ID, opjournal.TurnCells{Generation: generationOf(t, e.store, info.ID), Caps: caps, ID: "t", State: opjournal.TurnStateRunning, At: at})
		if strings.Contains(e.get(t, "/session/"+info.ID).Body.String(), `"turn"`) {
			t.Fatal("a city that lists no kind carries a turn")
		}
	})
}

// A replayed proposal answers 200 with the same body as the first one's 201,
// and the generated client decodes both (the spec declares both).
func TestProposeReplayDecodesThroughTheGeneratedClient(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Typed")
	body := `{"kind":"message","client_key":"k-typed","payload":"hello"}`
	first, err := genclient.ParseProposeSessionOpResponse(e.post(t, "/session/"+info.ID+"/ops", "", body).Result())
	if err != nil {
		t.Fatalf("parse first: %v", err)
	}
	if first.StatusCode() != http.StatusCreated || first.JSON201 == nil || !first.JSON201.Created {
		t.Fatalf("first: status %d, JSON201 %+v, body %s", first.StatusCode(), first.JSON201, first.Body)
	}
	again, err := genclient.ParseProposeSessionOpResponse(e.post(t, "/session/"+info.ID+"/ops", "", body).Result())
	if err != nil {
		t.Fatalf("parse replay: %v", err)
	}
	if again.StatusCode() != http.StatusOK || again.JSON200 == nil {
		t.Fatalf("replay: status %d, JSON200 %+v, body %s", again.StatusCode(), again.JSON200, again.Body)
	}
	if again.JSON200.Created || again.JSON200.OpId != first.JSON201.OpId || again.JSON200.Op.Id != first.JSON201.OpId {
		t.Fatalf("replay = %+v, want op %s with created false", again.JSON200, first.JSON201.OpId)
	}
}

// Every message delivery mode is one the lane lowers, a deliver-now message
// (now, or the legacy interrupt) as a deliver-now unit (OJ-17), so each is
// written and pokes the lane.
func TestProposeAcceptsEveryWhen(t *testing.T) {
	e, st := newLaneOpsEnv(t, "message")
	info := e.session(t, "Now")
	whens := []string{"boundary", "after_turn", "if_idle", "steer", "now", "interrupt"}
	for _, when := range whens {
		rec := e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"message","client_key":"k-`+when+`","payload":"x","policy":{"message":{"when":"`+when+`"}}}`)
		wantStatus(t, "when "+when, rec, http.StatusCreated)
	}
	if rows := e.opRows(t); len(rows) != len(whens) {
		t.Fatalf("wrote %d op rows, want %d", len(rows), len(whens))
	}
	if p := st.poked(); len(p) != len(whens) {
		t.Fatalf("poked the lane %d times, want %d", len(p), len(whens))
	}
}

// The kind gate is the running lane host's latched kinds, not the live
// config: a reload that lists a kind does not switch Propose on, a host that
// is not running (no kinds) refuses a listed kind, and a State with no lane
// host refuses every kind. A session read carries the observed turn on the
// same condition.
func TestProposeGatesOnTheRunningLaneHost(t *testing.T) {
	t.Run("reload lists a kind", func(t *testing.T) {
		e, _ := newLaneOpsEnv(t)
		info := e.session(t, "Reload")
		e.fs.cfg.Ops.Authoritative = []string{"message"}
		rec := e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"message","client_key":"k","payload":"x"}`)
		wantStatus(t, "after reload", rec, http.StatusConflict)
		if code := problemCode(t, rec); code != "kind-not-enabled" {
			t.Fatalf("code = %q", code)
		}
	})
	t.Run("host not running", func(t *testing.T) {
		e, st := newLaneOpsEnv(t, "message")
		st.kinds = nil // a refused or stopped host executes nothing
		info := e.session(t, "Refused")
		e.putRuntimeRow(t, info.ID, opjournal.TurnCells{Generation: generationOf(t, e.store, info.ID), State: opjournal.TurnStateRunning, ID: "t", At: time.Now()})
		wantStatus(t, "propose", e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"message","client_key":"k","payload":"x"}`), http.StatusConflict)
		if rows := e.opRows(t); len(rows) != 0 {
			t.Fatalf("wrote %d op rows", len(rows))
		}
		if strings.Contains(e.get(t, "/session/"+info.ID).Body.String(), `"turn"`) {
			t.Fatal("a session read carries a turn while no lane host runs")
		}
	})
	t.Run("no lane host", func(t *testing.T) {
		e, _ := newLaneOpsEnv(t, "message")
		e.resolver.state = e.fs
		info := e.session(t, "Hostless")
		wantStatus(t, "propose", e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"message","client_key":"k","payload":"x"}`), http.StatusConflict)
	})
}

// A proposal to a configured named session with no row that is refused (a
// payload over the limit, an invalid policy or key), or whose event cursor
// cannot be read, materializes no session row; an accepted one does.
func TestProposeRefusalMaterializesNoNamedSession(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		// eventsDown breaks the city's event log, so the cursor read fails.
		eventsDown bool
	}{
		{name: "payload too large", body: `{"kind":"message","client_key":"k","payload":"` + strings.Repeat("x", opjournal.MaxInlinePayload+1) + `"}`, code: "payload-too-large"},
		{name: "policy of another kind", body: `{"kind":"message","client_key":"k","payload":"x","policy":{"interrupt":{"on_failure":"fail"}}}`, code: "validation"},
		{name: "invalid key", body: `{"kind":"message","client_key":"` + strings.Repeat("k", 513) + `","payload":"x"}`, code: "invalid-idempotency-key"},
		{name: "cursor unreadable", body: `{"kind":"message","client_key":"k","payload":"x"}`, code: "service-unavailable", eventsDown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newLaneOpsEnv(t, "message")
			rowsBefore, _ := e.sessionRowsAndStarts(t)
			if tc.eventsDown {
				e.fs.eventProv = events.NewFailFake()
			}
			rec := e.post(t, "/session/"+url.PathEscape("myrig/worker")+"/ops", "", tc.body)
			if rec.Code < 400 || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if rows, _ := e.sessionRowsAndStarts(t); rows != rowsBefore {
				t.Fatalf("a refused proposal materialized the named session: rows %d -> %d", rowsBefore, rows)
			}
			if n := len(e.opRows(t)); n != 0 {
				t.Fatalf("op rows = %d", n)
			}
		})
	}
	e, _ := newLaneOpsEnv(t, "message")
	rowsBefore, _ := e.sessionRowsAndStarts(t)
	wantStatus(t, "accepted", e.post(t, "/session/"+url.PathEscape("myrig/worker")+"/ops", "", `{"kind":"message","client_key":"k","payload":"x"}`), http.StatusCreated)
	if rows, _ := e.sessionRowsAndStarts(t); rows != rowsBefore+1 {
		t.Fatalf("an accepted proposal did not materialize the named session: rows %d -> %d", rowsBefore, rows)
	}
}

// The payload limit and the key shape are refused with their own codes.
func TestProposeRefusalCodes(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Codes")
	rec := e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"message","client_key":"k","payload":"`+strings.Repeat("x", opjournal.MaxInlinePayload+1)+`"}`)
	wantStatus(t, "too large", rec, http.StatusUnprocessableEntity)
	if code := problemCode(t, rec); code != "payload-too-large" {
		t.Fatalf("too large: code = %q", code)
	}
	rec = e.post(t, "/session/"+info.ID+"/ops", strings.Repeat("k", 513), `{"kind":"message","payload":"x"}`)
	wantStatus(t, "invalid key", rec, http.StatusUnprocessableEntity)
	if code := problemCode(t, rec); code != "invalid-idempotency-key" {
		t.Fatalf("invalid key: code = %q", code)
	}
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("refused proposals wrote %d op rows", len(rows))
	}
}

// The cursor a proposal returns is captured before its write: an event the
// write itself records lands after it, so a stream from the cursor sees it.
func TestProposeCursorPrecedesTheWrite(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Cursor")
	e.recordUnrelatedEvent()
	var writeSeq uint64
	e.store.set(func(s *opsTestStore) {
		s.onOpCreate = func() {
			e.recordUnrelatedEvent()
			seq, err := e.fs.eventProv.LatestSeq()
			if err != nil {
				t.Errorf("LatestSeq: %v", err)
			}
			writeSeq = seq
		}
	})
	rec := e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"message","client_key":"k","payload":"x"}`)
	wantStatus(t, "propose", rec, http.StatusCreated)
	cursor, err := strconv.ParseUint(decodeInto[ConversationOpProposeResult](t, rec).Cursor, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if writeSeq == 0 || cursor >= writeSeq {
		t.Fatalf("cursor %d, the write's event %d: the cursor must precede the write", cursor, writeSeq)
	}
}

// seedLaneOp writes a lane op of conv's under key as its row would read
// after the lane worked on it: mutate moves the previewed op on.
func (e *opsEnv) seedLaneOp(t *testing.T, conv, key string, mutate func(*opjournal.Op)) opjournal.Op {
	t.Helper()
	st := e.opStore(t)
	op, err := st.Preview(opjournal.Request{
		Conversation: conv, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane,
		ClientKey: key, Producer: opjournal.Producer{Principal: "t3:env", Actor: "user"}, Payload: "secret text",
	})
	if err != nil {
		t.Fatal(err)
	}
	mutate(&op)
	b, err := opjournal.EncodeOp(op)
	if err != nil {
		t.Fatalf("EncodeOp: %v", err)
	}
	if _, err := e.store.Create(b); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// executingLaneOp moves op to executing under a lane lease, taken at at.
func executingLaneOp(op *opjournal.Op, at time.Time) {
	op.Status = opjournal.StatusExecuting
	op.Attempt = 1
	op.LaneSeq = 1
	op.DeliveryKey = op.ID + ":1"
	op.Lease = opjournal.Lease{Lane: string(opjournal.LaneConversation), Conversation: op.Conversation, Generation: 1, Nonce: "n1", Deadline: at.Add(time.Hour)}
	op.Transitions = append(op.Transitions, opjournal.Transition{Status: opjournal.StatusExecuting, At: at, By: "lane:n1"})
}

// DES-O1 §11's op record on the wire: a closed lane delivery carries its
// executor, producer, sequence numbers, the time of its last transition and
// its outcome's result, delivery record and turn link included, and never
// its text.
func TestOpGetCarriesTheClosedLaneDeliveryRecord(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Delivered")
	taken := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	closedAt := taken.Add(2 * time.Second)
	op := e.seedLaneOp(t, info.ID, "k", func(op *opjournal.Op) {
		executingLaneOp(op, taken)
		op.Lease = opjournal.Lease{}
		op.Status = opjournal.StatusClosed
		op.Outcome = opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{
			Generation: 3, Turn: "turn-4", TurnLink: opjournal.LinkExact, TurnBasis: "runtime_turn",
			Delivery: opjournal.RecordedDelivery{Seq: 1, At: taken.Add(time.Second), Ack: opjournal.AckDelivered, MessageID: "msg-9", MessageAt: taken.Add(time.Second)},
		}}
		op.Transitions = append(op.Transitions, opjournal.Transition{Status: opjournal.StatusClosed, At: closedAt, By: "lane:n1"})
	})
	rec := e.get(t, "/session/"+info.ID+"/ops/"+op.ID)
	wantStatus(t, "get", rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "secret text") {
		t.Fatalf("the op record carries its text: %s", rec.Body.String())
	}
	got := decodeInto[ConversationOp](t, rec)
	if got.Status != "closed" || got.Executor != "lane" || got.Principal != "t3:env" || got.Actor != "user" || got.ClientKey != "k" ||
		got.Attempt != 1 || got.LaneSeq != 1 || got.CancelRequested || got.At == nil || !got.At.Equal(closedAt) {
		t.Fatalf("op = %+v", got)
	}
	if got.Outcome == nil || got.Outcome.Arm != "succeeded" || got.Outcome.Result == nil {
		t.Fatalf("outcome = %+v", got.Outcome)
	}
	r := got.Outcome.Result
	if r.Generation != 3 || r.Turn != "turn-4" || r.TurnLink != "exact" || r.TurnBasis != "runtime_turn" || r.Delivery == nil {
		t.Fatalf("result = %+v", r)
	}
	if d := r.Delivery; d.Seq != 1 || d.Ack != "delivered" || d.MessageID != "msg-9" || d.At == nil || !d.At.Equal(taken.Add(time.Second)) || d.MessageAt == nil {
		t.Fatalf("delivery = %+v", d)
	}
}

// A cancel of an executing lane op records the request, says so on the op
// (cancel_requested), and rings the lane so the worker sees it.
func TestCancelOfAnExecutingOpRequestsAndPokes(t *testing.T) {
	e, st := newLaneOpsEnv(t, "message")
	info := e.session(t, "Executing")
	op := e.seedLaneOp(t, info.ID, "k", func(op *opjournal.Op) { executingLaneOp(op, time.Now().UTC()) })
	rec := e.post(t, "/session/"+info.ID+"/ops/"+op.ID+"/cancel", "", `{"principal":"t3:env","reason":"edited"}`)
	wantStatus(t, "cancel", rec, http.StatusOK)
	c := decodeInto[ConversationOpCancelResult](t, rec)
	if c.Result != "requested" || c.Op.Status != "executing" || !c.Op.CancelRequested || c.Op.Outcome != nil {
		t.Fatalf("cancel = %+v", c)
	}
	if got := st.poked(); len(got) != 1 || got[0] != info.ID {
		t.Fatalf("pokes = %v, want [%s]", got, info.ID)
	}
	rec = e.get(t, "/session/"+info.ID+"/ops/"+op.ID)
	if got := decodeInto[ConversationOp](t, rec); !got.CancelRequested {
		t.Fatalf("get after the cancel = %+v", got)
	}
}

// A closed session's ops stay readable: its producer reads the outcome of
// what it proposed after the session ended.
func TestOpsOfAClosedSessionStayReadable(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Ended")
	rec := e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"message","client_key":"k","payload":"x"}`)
	wantStatus(t, "propose", rec, http.StatusCreated)
	id := decodeInto[ConversationOpProposeResult](t, rec).OpID
	if err := e.store.Close(info.ID); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "get", e.get(t, "/session/"+info.ID+"/ops/"+id), http.StatusOK)
	rec = e.get(t, "/session/"+info.ID+"/ops")
	wantStatus(t, "list", rec, http.StatusOK)
	if list := decodeInto[ListBody[ConversationOp]](t, rec); list.Total != 1 || list.Items[0].ID != id {
		t.Fatalf("list = %s", rec.Body.String())
	}
}

// A runtime row whose observer has recorded no turn cells yet reads as no
// turn, not as generation 0 unknown.
func TestSessionGetOmitsAnUnobservedTurn(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Unobserved")
	e.putRuntimeRow(t, info.ID, opjournal.TurnCells{})
	if body := e.get(t, "/session/"+info.ID).Body.String(); strings.Contains(body, `"turn"`) {
		t.Fatalf("an unobserved runtime row carries a turn: %s", body)
	}
}

// op.changed and fact.recorded are the controller's, derived from journal
// rows: POST /v0/events refuses them and records nothing, so a caller cannot
// show a producer a terminal status its op never reached.
func TestEventEmitRefusesJournalDerivedTypes(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	for _, typ := range []string{"op.changed", "fact.recorded"} {
		before, err := e.fs.eventProv.LatestSeq()
		if err != nil {
			t.Fatal(err)
		}
		rec := e.post(t, "/events", "", `{"type":"`+typ+`","actor":"ci","subject":"gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
		wantStatus(t, typ, rec, http.StatusUnprocessableEntity)
		if after, _ := e.fs.eventProv.LatestSeq(); after != before {
			t.Fatalf("%s: recorded an event (seq %d -> %d)", typ, before, after)
		}
	}
	wantStatus(t, "another type", e.post(t, "/events", "", `{"type":"deploy.completed","actor":"ci"}`), http.StatusCreated)
}

// The op reads and the cancel resolve their session without materializing a
// configured named session: listing, getting or canceling ops of a named
// session that has no row is 404 and creates no row. Once a proposal has
// materialized it, its ops read by its name.
func TestOpReadsMaterializeNoNamedSession(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	name := url.PathEscape("myrig/worker")
	const opID = "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"list", func() *httptest.ResponseRecorder { return e.get(t, "/session/"+name+"/ops") }},
		{"get", func() *httptest.ResponseRecorder { return e.get(t, "/session/"+name+"/ops/"+opID) }},
		{"cancel", func() *httptest.ResponseRecorder {
			return e.post(t, "/session/"+name+"/ops/"+opID+"/cancel", "", `{"principal":"t3:env","reason":"edited"}`)
		}},
	} {
		rowsBefore, _ := e.sessionRowsAndStarts(t)
		rec := tc.do()
		if rows, _ := e.sessionRowsAndStarts(t); rows != rowsBefore {
			t.Fatalf("%s materialized the named session: rows %d -> %d", tc.name, rowsBefore, rows)
		}
		wantStatus(t, tc.name, rec, http.StatusNotFound)
	}
	prop := e.post(t, "/session/"+name+"/ops", "", `{"kind":"message","client_key":"k","payload":"x"}`)
	wantStatus(t, "propose", prop, http.StatusCreated)
	res := decodeInto[ConversationOpProposeResult](t, prop)
	wantStatus(t, "get by name", e.get(t, "/session/"+name+"/ops/"+res.OpID), http.StatusOK)
	rec := e.get(t, "/session/"+name+"/ops")
	wantStatus(t, "list by name", rec, http.StatusOK)
	if list := decodeInto[ListBody[ConversationOp]](t, rec); list.Total != 1 || list.Items[0].ID != res.OpID {
		t.Fatalf("list = %s", rec.Body.String())
	}
}

// client_seq is at most math.MaxInt64, what the generated client's int64
// decodes: 2^63 is refused 422 and writes nothing, and the maximum is
// accepted and reads back through the generated client.
func TestProposeClientSeqCap(t *testing.T) {
	e, _ := newLaneOpsEnv(t, "message")
	info := e.session(t, "Seq")
	rec := e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"message","client_key":"k-over","payload":"x","client_seq":9223372036854775808}`)
	wantStatus(t, "2^63", rec, http.StatusUnprocessableEntity)
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("a refused client_seq wrote %d op rows", len(rows))
	}
	ok, err := genclient.ParseProposeSessionOpResponse(e.post(t, "/session/"+info.ID+"/ops", "", `{"kind":"message","client_key":"k-max","payload":"x","client_seq":9223372036854775807}`).Result())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ok.StatusCode() != http.StatusCreated || ok.JSON201 == nil || ok.JSON201.Op.ClientSeq == nil || *ok.JSON201.Op.ClientSeq != math.MaxInt64 {
		t.Fatalf("max client_seq: status %d, JSON201 %+v, body %s", ok.StatusCode(), ok.JSON201, ok.Body)
	}
}
