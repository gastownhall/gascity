package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
)

// The producer API (DES-O1 §11, OJ-20): Propose, Get, the open-op list and
// Cancel over a session's conversation lane.

// lanePokeState is the city's State with a conversation lane that records
// its doorbell.
type lanePokeState struct {
	*fakeState
	mu    sync.Mutex
	pokes []string
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
// through a State whose conversation lane records its pokes. Every ops gate
// stays off: the producer API does not depend on them.
func newLaneOpsEnv(t *testing.T, kinds ...string) (*opsEnv, *lanePokeState) {
	t.Helper()
	e := newControlOpsEnv(t, "off", "off")
	e.fs.cfg.Ops.Authoritative = kinds
	st := &lanePokeState{fakeState: e.fs}
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
			got.At == nil || !got.At.Equal(at) || len(got.Caps) != 2 || got.Caps[1] != (SessionTurnCapPair{Key: "fence", Value: "atomic"}) {
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
