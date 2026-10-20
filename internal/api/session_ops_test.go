package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The O0 shadow ops on /submit and /messages (DES-O0API §6, §15 OJ-2b).

// opsTestStore is a MemStore that names its id prefix, as a real sessions
// store does, so the op store opens over it exactly as in production. Its
// writes to op rows can be made to fail.
type opsTestStore struct {
	*beads.MemStore

	mu sync.Mutex
	// createErr fails op-row creates, writing nothing.
	createErr error
	// ambiguous commits an op-row create, then answers an error.
	ambiguous bool
	// panicAfterCreate commits the next op-row create, then panics (a crash
	// after the commit).
	panicAfterCreate bool
	// updateFails fails this many op-row conditional updates.
	updateFails int
	// onOpCreate runs once after the next op-row create commits.
	onOpCreate func()
	// creates counts committed op rows.
	creates atomic.Int32
}

func newOpsTestStore() *opsTestStore {
	return &opsTestStore{MemStore: &beads.MemStore{HonorExplicitIDs: true}}
}

func (s *opsTestStore) IDPrefix() string { return "gc" }

func (s *opsTestStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.Type != opjournal.BeadType {
		return s.MemStore.Create(b)
	}
	s.mu.Lock()
	createErr, ambiguous, panicAfter, onCreate := s.createErr, s.ambiguous, s.panicAfterCreate, s.onOpCreate
	s.panicAfterCreate, s.onOpCreate = false, nil
	s.mu.Unlock()
	if createErr != nil {
		return beads.Bead{}, createErr
	}
	created, err := s.MemStore.Create(b)
	if err != nil {
		return created, err
	}
	s.creates.Add(1)
	if onCreate != nil {
		onCreate()
	}
	if panicAfter {
		panic("injected crash after the op row committed")
	}
	if ambiguous {
		return beads.Bead{}, errors.New("transport: connection reset after commit")
	}
	return created, nil
}

func (s *opsTestStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if b, err := s.Get(id); err == nil && b.Type == opjournal.BeadType {
		s.mu.Lock()
		fail := s.updateFails > 0
		if fail {
			s.updateFails--
		}
		s.mu.Unlock()
		if fail {
			return errors.New("transport: update failed")
		}
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func (s *opsTestStore) set(f func(*opsTestStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// opsEnv is a fake city whose sessions store records ops under mode, served
// by one supervisor (one API process).
type opsEnv struct {
	fs       *fakeState
	store    *opsTestStore
	resolver *stateCityResolver
	sm       *SupervisorMux
	h        http.Handler
}

func newOpsEnv(t *testing.T, mode string) *opsEnv {
	t.Helper()
	t.Setenv("GC_OPS_CONTENT", "")
	fs := newSessionFakeState(t)
	store := newOpsTestStore()
	fs.cityBeadStore = store
	fs.cfg.Ops.ContentMode = mode
	e := &opsEnv{fs: fs, store: store}
	e.restart(t)
	return e
}

// restart is a new API process over the same city: a new supervisor, so an
// empty LRU and in-flight registry.
func (e *opsEnv) restart(t *testing.T) {
	t.Helper()
	e.resolver = &stateCityResolver{state: e.fs}
	e.sm = NewSupervisorMux(e.resolver, nil, false, "test", "", time.Now())
	e.h = wrapTestSupervisorMiddleware(e.sm)
}

// rebuiltState is the same city under a new State value, as after the city
// restarts under a running supervisor.
type rebuiltState struct{ *fakeState }

// rebuild makes the supervisor build a new Server for the city, in the same
// process, as it does whenever the city's State changes.
func (e *opsEnv) rebuild(t *testing.T) {
	t.Helper()
	old := e.server(t)
	e.resolver.state = &rebuiltState{fakeState: e.fs}
	if e.server(t) == old {
		t.Fatal("the supervisor kept the old Server")
	}
}

// rebuiltOverState is the city after a restart that opened a new sessions
// store over the same data and, when ev is set, a new event provider.
type rebuiltOverState struct {
	*fakeState
	store beads.Store
	ev    events.Provider
}

func (r *rebuiltOverState) SessionsBeadStore() beads.SessionStore {
	return beads.SessionStore{Store: r.store}
}
func (r *rebuiltOverState) CityBeadStore() beads.Store { return r.store }
func (r *rebuiltOverState) EventProvider() events.Provider {
	if r.ev != nil {
		return r.ev
	}
	return r.fakeState.EventProvider()
}

// rebuildOver is rebuild for a restart that reopened the city's store as
// store and, when ev is not nil, its event provider as ev.
func (e *opsEnv) rebuildOver(t *testing.T, store beads.Store, ev events.Provider) {
	t.Helper()
	old := e.server(t)
	e.resolver.state = &rebuiltOverState{fakeState: e.fs, store: store, ev: ev}
	if e.server(t) == old {
		t.Fatal("the supervisor kept the old Server")
	}
}

// server is the Server the supervisor serves the city with now.
func (e *opsEnv) server(t *testing.T) *Server {
	t.Helper()
	return e.sm.getCityServer(e.fs.CityName(), e.resolver.state)
}

func (e *opsEnv) session(t *testing.T, title string) session.Info {
	t.Helper()
	return createTestSession(t, e.fs.cityBeadStore, e.fs.sp, title)
}

func (e *opsEnv) post(t *testing.T, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postSessionAction(t, e.h, cityURL(e.fs, path), key, body)
}

func (e *opsEnv) opStore(t *testing.T) *opjournal.Store {
	t.Helper()
	st, err := opjournal.NewStore(beads.SessionStore{Store: e.store.MemStore}, clock.Real{}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("open op store: %v", err)
	}
	return st
}

// opRows lists every op row, open or closed.
func (e *opsEnv) opRows(t *testing.T) []beads.Bead {
	t.Helper()
	rows, err := e.store.List(beads.ListQuery{Label: opjournal.LabelOp, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list op rows: %v", err)
	}
	return rows
}

// waitClosed polls until op id is closed.
func (e *opsEnv) waitClosed(t *testing.T, id string) opjournal.Op {
	t.Helper()
	return e.waitClosedWithin(t, id, testEventTimeout)
}

// waitClosedWithin is waitClosed with its own deadline.
func (e *opsEnv) waitClosedWithin(t *testing.T, id string, timeout time.Duration) opjournal.Op {
	t.Helper()
	st := e.opStore(t)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		op, err := st.Get(id)
		if err == nil && op.Status == opjournal.StatusClosed {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	op, err := st.Get(id)
	t.Fatalf("op %s not closed: %+v, %v", id, op, err)
	return opjournal.Op{}
}

// terminalEvents are the recorded terminal events for requestID.
func terminalEvents(t *testing.T, prov events.Provider, requestID string) []events.Event {
	t.Helper()
	all, err := prov.List(events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return terminalEventsOf(all, requestID)
}

// terminalEventsOf are the terminal events for requestID among evs.
func terminalEventsOf(evs []events.Event, requestID string) []events.Event {
	var out []events.Event
	for _, e := range evs {
		switch e.Type {
		case events.RequestResultSessionSubmit, events.RequestResultSessionMessage, events.RequestFailed:
		default:
			continue
		}
		var p struct {
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.RequestID == requestID {
			out = append(out, e)
		}
	}
	return out
}

func waitTerminalEvents(t *testing.T, prov events.Provider, requestID string, n int) []events.Event {
	t.Helper()
	deadline := time.Now().Add(testEventTimeout)
	for time.Now().Before(deadline) {
		if got := terminalEvents(t, prov, requestID); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("want %d terminal events for %s, got %d", n, requestID, len(terminalEvents(t, prov, requestID)))
	return nil
}

// deliveredCalls are the runtime calls that carry text to name.
func deliveredCalls(sp *runtime.Fake, name string) []runtime.Call {
	var out []runtime.Call
	for _, c := range sp.SnapshotCalls() {
		if c.Name == name && (strings.HasPrefix(c.Method, "Nudge") || c.Method == "SendKeys") {
			out = append(out, c)
		}
	}
	return out
}

var mintedKeyRE = regexp.MustCompile(`^k-[a-z2-7]{26}$`)

// I5: in off no op row is written, and the 202 body and each terminal
// event, envelope and payload, are the legacy ones in full, keyed or not and
// failed or not. The one field off shares with the other modes is an
// interrupt_now's "interrupt" report (DES-T3I §14 item 2), which ops.content
// does not gate. Nor does it gate the keeping of a terminal event the event
// log refuses: while it is kept, off answers a same-key retry from the LRU
// 409 in_flight, not the legacy 202
// (TestSessionContentOpsOffReplayIsBusyWhileTheLiveLogRefusesTheEvent).
func TestSessionContentOpsOffKeepsTheLegacyWire(t *testing.T) {
	for _, tc := range []struct {
		name, path, key, body string
		setup                 func(*testing.T, *opsEnv) session.Info
		eventType             string
		subject               bool
		payload               string
	}{
		{
			name: "submit", path: "/submit", body: `{"message":"one"}`, eventType: events.RequestResultSessionSubmit, subject: true,
			payload: `{"request_id":"$R","session_id":"$S","queued":false,"intent":"default"}`,
		},
		{
			name: "submit keyed", path: "/submit", key: "k1", body: `{"message":"two","intent":"follow_up"}`, eventType: events.RequestResultSessionSubmit, subject: true,
			payload: `{"request_id":"$R","session_id":"$S","queued":true,"intent":"follow_up"}`,
		},
		{
			name: "messages", path: "/messages", body: `{"message":"three"}`, eventType: events.RequestResultSessionMessage, subject: true,
			payload: `{"request_id":"$R","session_id":"$S","queued":false}`,
		},
		{
			name: "messages keyed", path: "/messages", key: "k2", body: `{"message":"four"}`, eventType: events.RequestResultSessionMessage, subject: true,
			payload: `{"request_id":"$R","session_id":"$S","queued":false}`,
		},
		{
			name: "submit failed", path: "/submit", key: "k3", body: `{"message":"five"}`, eventType: events.RequestFailed,
			setup: func(t *testing.T, e *opsEnv) session.Info {
				info := e.session(t, "Off failed")
				e.fs.sp.NudgeErrors = map[string]error{info.SessionName: errors.New("boom")}
				return info
			},
			payload: `{"request_id":"$R","operation":"session.submit","error_code":"submit_failed","error_message":"sending message to session: boom"}`,
		},
		{
			name: "messages failed", path: "/messages", body: `{"message":"six"}`, eventType: events.RequestFailed,
			setup: func(t *testing.T, e *opsEnv) session.Info {
				info := e.session(t, "Off failed")
				e.fs.sp.NudgeErrors = map[string]error{info.SessionName: errors.New("boom")}
				return info
			},
			payload: `{"request_id":"$R","operation":"session.message","error_code":"message_failed","error_message":"sending message to session: boom"}`,
		},
		{
			name: "submit interrupt_now", path: "/submit", body: `{"message":"now","intent":"interrupt_now"}`, eventType: events.RequestResultSessionSubmit, subject: true,
			setup: func(t *testing.T, e *opsEnv) session.Info {
				e.fs.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
				e.fs.sessionProvider = runtime.FullFake{Fake: e.fs.sp}
				info, err := session.NewManagerWithOptions(e.fs.cityBeadStore, e.fs.sessionProvider, session.WithCityPath(e.fs.cityPath)).CreateSession(context.Background(), session.CreateOptions{
					Template: "helper", Title: "Off interrupt", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"},
				})
				if err != nil {
					t.Fatal(err)
				}
				return info
			},
			payload: `{"request_id":"$R","session_id":"$S","queued":false,"intent":"interrupt_now","interrupt":{"outcome":"no_turn"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newOpsEnv(t, "")
			var info session.Info
			if tc.setup != nil {
				info = tc.setup(t, e)
			} else {
				info = e.session(t, "Off")
			}
			e.recordUnrelatedEvent()
			seq, err := e.fs.eventProv.LatestSeq()
			if err != nil {
				t.Fatal(err)
			}
			cursor := strconv.FormatUint(seq, 10)
			rec := e.post(t, "/session/"+info.ID+tc.path, tc.key, tc.body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			acc := decodeAsyncAccepted(t, bytes.NewReader(rec.Body.Bytes()))
			if !strings.HasPrefix(acc.RequestID, "req-") {
				t.Fatalf("request_id = %q, want req-", acc.RequestID)
			}
			wantBody := fmt.Sprintf(`{"status":"accepted","request_id":%q,"event_cursor":%q}`, acc.RequestID, cursor)
			assertJSONEqual(t, "202 body", rec.Body.Bytes(), wantBody)
			evs := waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
			wantSubject := ""
			if tc.subject {
				wantSubject = info.ID
			}
			if evs[0].Type != tc.eventType || evs[0].Subject != wantSubject {
				t.Fatalf("event %s subject %q, want %s subject %q", evs[0].Type, evs[0].Subject, tc.eventType, wantSubject)
			}
			want := strings.NewReplacer("$R", acc.RequestID, "$S", info.ID).Replace(tc.payload)
			assertJSONEqual(t, "event payload", evs[0].Payload, want)
			if rows := e.opRows(t); len(rows) != 0 {
				t.Fatalf("off wrote %d op rows", len(rows))
			}
		})
	}
}

// assertJSONEqual compares two JSON documents structurally.
func assertJSONEqual(t *testing.T, what string, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: decode %s: %v", what, got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: decode want %s: %v", what, want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s = %s, want %s", what, got, want)
	}
}

// I4 and §6.3: in auto a submit is recorded as a message op whose id is the
// request_id, the minted key is echoed, and the op closes from the legacy
// result with the intent's policy before its event carries it.
func TestSessionContentOpsAutoRecordsEachIntent(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		when             opjournal.When
		via              string
	}{
		{"submit default", "/submit", `{"message":"hello"}`, opjournal.WhenBoundary, legacyop.ViaLegacyDelivered},
		{"submit follow_up", "/submit", `{"message":"later","intent":"follow_up"}`, opjournal.WhenAfterTurn, legacyop.ViaLegacyQueued},
		{"messages", "/messages", `{"message":"hello"}`, opjournal.WhenBoundary, legacyop.ViaLegacyDelivered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newOpsEnv(t, "auto")
			info := e.session(t, "Auto")
			rec := e.post(t, "/session/"+info.ID+tc.path, "", tc.body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			acc := decodeAsyncAccepted(t, rec.Body)
			if acc.Op == nil || acc.Op.ID != acc.RequestID || acc.Op.Conversation != info.ID || acc.Op.Kind != "message" || acc.Op.Status != "submitted" {
				t.Fatalf("202 = %+v op %+v", acc, acc.Op)
			}
			if !mintedKeyRE.MatchString(acc.IdempotencyKey) || acc.Replayed {
				t.Fatalf("idempotency_key = %q replayed = %v", acc.IdempotencyKey, acc.Replayed)
			}
			evs := waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
			var p struct {
				Queued bool            `json:"queued"`
				Op     *ConversationOp `json:"op"`
			}
			if err := json.Unmarshal(evs[0].Payload, &p); err != nil || p.Op == nil {
				t.Fatalf("event %s: %s (%v)", evs[0].Type, evs[0].Payload, err)
			}
			if p.Op.Status != "closed" || p.Op.Outcome == nil || p.Op.Outcome.Arm != "succeeded" || p.Op.Outcome.Via != tc.via {
				t.Fatalf("event op = %+v outcome %+v", p.Op, p.Op.Outcome)
			}
			op := e.waitClosed(t, acc.RequestID)
			if op.Executor != legacyop.ExecutorAPI || op.Payload != strings.Split(strings.Split(tc.body, `"message":"`)[1], `"`)[0] ||
				op.Producer.Principal != legacyop.PrincipalOperator || op.Policy.Message.When != tc.when || op.ClientKey != acc.IdempotencyKey {
				t.Fatalf("op = %+v", op)
			}
		})
	}
}

// §6.2: a retry after the request finished and the LRU forgot it (a new
// server) replays the stored outcome: same request_id, replayed, the closed
// op, a re-emitted terminal event after the new cursor, and no second
// delivery.
func TestSessionContentOpsAutoReplaysAClosedOpAfterRestart(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Replay")
	body := `{"message":"later please","intent":"follow_up"}`
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-1", body).Body)
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	e.waitClosed(t, first.RequestID)

	e.restart(t)
	rec := e.post(t, "/session/"+info.ID+"/submit", "key-1", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay status %d: %s", rec.Code, rec.Body.String())
	}
	replay := decodeAsyncAccepted(t, rec.Body)
	if replay.RequestID != first.RequestID || !replay.Replayed || replay.Op == nil || replay.Op.Status != "closed" ||
		replay.Op.Outcome == nil || replay.Op.Outcome.Via != legacyop.ViaLegacyQueued || replay.IdempotencyKey != "key-1" {
		t.Fatalf("replay = %+v op %+v", replay, replay.Op)
	}
	evs := waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 2)
	cursor := mustSeq(t, replay.EventCursor)
	if evs[1].Seq <= cursor || evs[1].Type != events.RequestResultSessionSubmit {
		t.Fatalf("re-emitted event %+v not after cursor %d", evs[1], cursor)
	}
	var p SessionSubmitSucceededPayload
	if err := json.Unmarshal(evs[1].Payload, &p); err != nil || !p.Queued || p.Intent != "follow_up" || p.SessionID != info.ID || p.Op == nil {
		t.Fatalf("re-emitted payload = %s (%v)", evs[1].Payload, err)
	}
	if got := queuedSubmitCount(t, e.fs.cityPath, info.ID); got != 1 {
		t.Fatalf("queued submits = %d, want 1 (the replay re-delivered)", got)
	}
	if rows := e.opRows(t); len(rows) != 1 {
		t.Fatalf("op rows = %d, want 1", len(rows))
	}
}

func mustSeq(t *testing.T, cursor string) uint64 {
	t.Helper()
	var n uint64
	if _, err := fmt.Sscan(cursor, &n); err != nil {
		t.Fatalf("cursor %q: %v", cursor, err)
	}
	return n
}

// §10.1 rule 3: one key against one session is one op, whichever endpoint
// and whichever spelling of the target the retry uses.
func TestSessionContentOpsAutoOneKeyIsOneOpAcrossPathsAndSpellings(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Paths")
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-p", `{"message":"hi"}`).Body)
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	e.waitClosed(t, first.RequestID)
	for _, target := range []string{info.ID + "/messages", info.SessionName + "/submit", info.SessionName + "/messages"} {
		rec := e.post(t, "/session/"+target, "key-p", `{"message":"hi"}`)
		acc := decodeAsyncAccepted(t, rec.Body)
		if rec.Code != http.StatusAccepted || acc.RequestID != first.RequestID || !acc.Replayed {
			t.Fatalf("%s: %d %+v", target, rec.Code, acc)
		}
	}
	if rows := e.opRows(t); len(rows) != 1 {
		t.Fatalf("op rows = %d, want 1", len(rows))
	}
	if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
	// The re-emitted events match the replaying endpoint's operation.
	evs := waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 4)
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	want := []string{events.RequestResultSessionSubmit, events.RequestResultSessionMessage, events.RequestResultSessionSubmit, events.RequestResultSessionMessage}
	if !slices.Equal(types, want) {
		t.Fatalf("event types = %v, want %v", types, want)
	}
}

// DES-O0API §3.1, request_id rule 6: resume is outside the op and its digest,
// so a same-key retry that adds it to a message queued for a suspended session
// replays that outcome and starts nothing, and the replay's event says so. The
// cache refuses the flip on the first request's endpoint and spelling (it
// compares whole bodies); every retry it misses gets the replay.
func TestSessionContentOpsAutoReplayDoesNotApplyResume(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Resume")
	mgr := session.NewManagerWithOptions(e.fs.cityBeadStore, e.fs.sp, session.WithCityPath(e.fs.cityPath))
	if err := mgr.Suspend(context.Background(), testOperatorIn(t, e.fs.cityPath), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.SessionName+"/submit", "key-r", `{"message":"hi"}`).Body)
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	if op := e.waitClosed(t, first.RequestID); op.Outcome.Result.Via != legacyop.ViaLegacyQueued {
		t.Fatalf("first outcome = %+v, want queued", op.Outcome)
	}
	starts := e.fs.sp.CountCalls("Start", info.SessionName)

	resumed := `{"message":"hi","resume":true}`
	if rec := e.post(t, "/session/"+info.SessionName+"/submit", "key-r", resumed); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("same-spelling resume flip: %d %s, want 422", rec.Code, rec.Body.String())
	}
	for i, path := range []string{info.ID + "/submit", info.ID + "/messages"} {
		rec := e.post(t, "/session/"+path, "key-r", resumed)
		replay := decodeAsyncAccepted(t, rec.Body)
		if rec.Code != http.StatusAccepted || replay.RequestID != first.RequestID || !replay.Replayed ||
			replay.Op == nil || replay.Op.Outcome == nil || replay.Op.Outcome.Via != legacyop.ViaLegacyQueued {
			t.Fatalf("%s: %d %+v op %+v", path, rec.Code, replay, replay.Op)
		}
		evs := waitTerminalEvents(t, e.fs.eventProv, first.RequestID, i+2)
		if ev := evs[i+1]; ev.Seq <= mustSeq(t, replay.EventCursor) || !strings.Contains(ev.Message, replayOptionsNote) {
			t.Fatalf("%s: re-emitted event %+v, want one after the cursor whose message says resume took no effect", path, ev)
		}
	}
	if e.fs.sp.IsRunning(info.SessionName) || e.fs.sp.CountCalls("Start", info.SessionName) != starts {
		t.Fatal("a replay started the session")
	}
	b, err := e.fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["state"] != "suspended" {
		t.Fatalf("state = %q, want suspended", b.Metadata["state"])
	}
	if rows := e.opRows(t); len(rows) != 1 {
		t.Fatalf("op rows = %d, want 1", len(rows))
	}
	if got := queuedSubmitCount(t, e.fs.cityPath, info.ID); got != 1 {
		t.Fatalf("queued submits = %d, want 1", got)
	}
}

// request_id rule 8 (engdocs/design/async-request-result.md): the
// Idempotency-Key cache sits in front of the front door in every mode, so a
// retry it answers (the same endpoint and spelling) gets the first 202 though
// the session now waits on a pending interaction, while a retry it misses
// (another endpoint or spelling) is refused 409 before the stored op is
// consulted. Once the interaction clears, a recording mode replays the
// stored op, with no second delivery.
func TestSessionContentOpsFrontDoorRunsWhenTheCacheMisses(t *testing.T) {
	for _, mode := range []string{"off", "auto"} {
		t.Run(mode, func(t *testing.T) {
			e := newOpsEnv(t, mode)
			info := e.session(t, "Front door")
			path, body := "/session/"+info.ID+"/submit", `{"message":"hi"}`
			rec := e.post(t, path, "key-f", body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("first status %d: %s", rec.Code, rec.Body.String())
			}
			first := decodeAsyncAccepted(t, rec.Body)
			waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
			e.fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
				RequestID: "approval-1", Kind: "approval", Prompt: "Bash: date", Options: []string{"Yes", "No"},
			})

			rec = e.post(t, path, "key-f", body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("cache hit: status %d, want the first 202: %s", rec.Code, rec.Body.String())
			}
			if hit := decodeAsyncAccepted(t, rec.Body); hit.RequestID != first.RequestID || hit.EventCursor != first.EventCursor || hit.Replayed {
				t.Fatalf("cache hit = %+v, want the first 202 %+v", hit, first)
			}
			for _, miss := range []string{info.ID + "/messages", info.SessionName + "/submit"} {
				rec := e.post(t, "/session/"+miss, "key-f", body)
				var problem struct {
					Detail string `json:"detail"`
				}
				if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &problem) != nil || !strings.HasPrefix(problem.Detail, "pending_interaction: ") {
					t.Fatalf("cache miss %s: status %d, want 409 pending_interaction: %s", miss, rec.Code, rec.Body.String())
				}
			}
			if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
				t.Fatalf("deliveries = %d, want 1", got)
			}
			if mode == "off" {
				return // off records no op for a retry to replay
			}

			e.waitClosed(t, first.RequestID)
			e.fs.sp.SetPendingInteraction(info.SessionName, nil)
			rec = e.post(t, "/session/"+info.ID+"/messages", "key-f", body)
			replay := decodeAsyncAccepted(t, rec.Body)
			if rec.Code != http.StatusAccepted || replay.RequestID != first.RequestID || !replay.Replayed {
				t.Fatalf("after the interaction cleared: %d %+v, want the stored op's replay", rec.Code, replay)
			}
			if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
				t.Fatalf("deliveries = %d, want 1: the replay delivered again", got)
			}
		})
	}
}

// gatedProvider is a fake runtime whose deliveries wait for release and
// note whether an op row existed when they ran.
type gatedProvider struct {
	*runtime.Fake
	release   chan struct{}
	entered   chan struct{}
	opExisted func() bool
	sawOp     atomic.Bool
	once      sync.Once
}

func (p *gatedProvider) gate() {
	if p.opExisted != nil && p.opExisted() {
		p.sawOp.Store(true)
	}
	p.once.Do(func() { close(p.entered) })
	<-p.release
}

func (p *gatedProvider) Nudge(name string, content []runtime.ContentBlock) error {
	p.gate()
	return p.Fake.Nudge(name, content)
}

func (p *gatedProvider) NudgeNow(name string, content []runtime.ContentBlock) error {
	p.gate()
	return p.Fake.NudgeNow(name, content)
}

func (p *gatedProvider) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	p.gate()
	return p.Fake.NudgeFenced(ctx, name, content, n)
}

// newGatedProvider installs a provider whose deliveries wait for release.
// The returned release is idempotent and also runs at cleanup.
func (e *opsEnv) gatedProvider(t *testing.T) (*gatedProvider, func()) {
	t.Helper()
	gp := &gatedProvider{Fake: e.fs.sp, release: make(chan struct{}), entered: make(chan struct{})}
	e.fs.sessionProvider = gp
	var once sync.Once
	release := func() { once.Do(func() { close(gp.release) }) }
	t.Cleanup(release)
	return gp, release
}

func (p *gatedProvider) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(testEventTimeout):
		t.Fatal("delivery never ran")
	}
}

// recordUnrelatedEvent moves the city's event cursor.
func (e *opsEnv) recordUnrelatedEvent() {
	e.fs.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "test", Subject: "unrelated"})
}

// §6.2 in-flight rows and §6.1: while the first request delivers, a same-key
// request on the same endpoint under another spelling of the target (past
// the per-path LRU) gets the cached 202 (same request_id and the original
// cursor, though events landed since), another body under the key is refused
// 422, and the op existed before the delivery ran.
func TestSessionContentOpsAutoInFlightReplay(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "In flight")
	gp, release := e.gatedProvider(t)
	gp.opExisted = func() bool { return len(e.opRows(t)) == 1 }

	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-f", `{"message":"hi"}`).Body)
	gp.waitEntered(t)
	e.recordUnrelatedEvent()
	rec := e.post(t, "/session/"+info.SessionName+"/submit", "key-f", `{"message":"hi"}`)
	second := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || second.RequestID != first.RequestID || second.EventCursor != first.EventCursor || second.Replayed {
		t.Fatalf("in-flight replay = %d %+v, want the first answer %+v", rec.Code, second, first)
	}
	if mismatch := e.post(t, "/session/"+info.SessionName+"/submit", "key-f", `{"message":"other"}`); mismatch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("other body under the key = %d: %s", mismatch.Code, mismatch.Body.String())
	}
	release()
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	if !gp.sawOp.Load() {
		t.Fatal("the delivery ran before the op row was written")
	}
	if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
}

// §6.2 on /messages: a same-key retry while the first request runs, by alias
// past the per-path LRU, gets the cached 202, and a Go-client waiter on it
// matches the one terminal event.
func TestSessionContentOpsAutoMessagesInFlightReplay(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Messages in flight")
	gp, release := e.gatedProvider(t)
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/messages", "key-m", `{"message":"hi"}`).Body)
	gp.waitEntered(t)
	e.recordUnrelatedEvent()
	rec := e.post(t, "/session/"+info.SessionName+"/messages", "key-m", `{"message":"hi"}`)
	second := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || second.RequestID != first.RequestID || second.EventCursor != first.EventCursor || second.Replayed {
		t.Fatalf("in-flight replay = %d %+v, want the first answer %+v", rec.Code, second, first)
	}
	release()
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	c := NewCityScopedClient(srv.URL, e.fs.CityName())
	ctx, cancel := context.WithTimeout(context.Background(), testEventTimeout)
	defer cancel()
	if _, err := c.waitForEvent(ctx, second.RequestID, events.RequestResultSessionMessage, RequestOperationSessionMessage, second.EventCursor); err != nil {
		t.Fatalf("waiter on the replayed /messages 202: %v", err)
	}
	if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
}

// §6.2: a same-key request on the other endpoint while the first is in
// flight is busy (409 in_flight), not the cached 202: the running request
// emits its terminal event for its own operation only, so a waiter on the
// other endpoint would never match it. Once the first finishes, the retry
// replays the stored outcome with an event for its own endpoint.
func TestSessionContentOpsAutoInFlightOtherEndpointIsBusy(t *testing.T) {
	for _, tc := range []struct{ first, other, otherEvent string }{
		{"/submit", "/messages", events.RequestResultSessionMessage},
		{"/messages", "/submit", events.RequestResultSessionSubmit},
	} {
		t.Run(tc.first+" then "+tc.other, func(t *testing.T) {
			e := newOpsEnv(t, "auto")
			info := e.session(t, "Other endpoint")
			gp, release := e.gatedProvider(t)
			first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+tc.first, "key-e", `{"message":"hi"}`).Body)
			gp.waitEntered(t)
			if rec := e.post(t, "/session/"+info.ID+tc.other, "key-e", `{"message":"hi"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in_flight") {
				t.Fatalf("other endpoint in flight = %d: %s", rec.Code, rec.Body.String())
			}
			release()
			waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
			e.waitClosed(t, first.RequestID)
			// The LRU in front is per path, so the retry reaches the op row.
			rec := e.post(t, "/session/"+info.ID+tc.other, "key-e", `{"message":"hi"}`)
			acc := decodeAsyncAccepted(t, rec.Body)
			if rec.Code != http.StatusAccepted || acc.RequestID != first.RequestID || !acc.Replayed {
				t.Fatalf("retry after the first finished = %d %+v", rec.Code, acc)
			}
			evs := waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 2)
			if evs[1].Type != tc.otherEvent {
				t.Fatalf("replayed event %s, want %s", evs[1].Type, tc.otherEvent)
			}
			if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
				t.Fatalf("deliveries = %d, want 1", got)
			}
		})
	}
}

// I3 across a Server rebuild: the supervisor builds a new Server for a city
// whose State changed while the old Server still delivers. A same-key retry
// on the new Server finds the request in flight (the registry is the
// city's, per process) and gets the cached 202: it does not adopt and
// deliver a second time.
func TestSessionContentOpsServerRebuildKeepsTheInflightRequest(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Rebuild")
	gp, release := e.gatedProvider(t)
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-r", `{"message":"once"}`).Body)
	gp.waitEntered(t)
	e.rebuild(t)
	rec := e.post(t, "/session/"+info.ID+"/submit", "key-r", `{"message":"once"}`)
	retry := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || retry.RequestID != first.RequestID || retry.Op == nil || retry.Op.Attempt != 0 {
		t.Fatalf("retry on the rebuilt Server = %d %+v op %+v, want the cached 202", rec.Code, retry, retry.Op)
	}
	release()
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	if op := e.waitClosed(t, first.RequestID); op.Attempt != 0 {
		t.Fatalf("op = %+v, want the first attempt only", op)
	}
	time.Sleep(20 * time.Millisecond)
	if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
	if n := len(terminalEvents(t, e.fs.eventProv, first.RequestID)); n != 1 {
		t.Fatalf("terminal events = %d, want 1", n)
	}
}

// §6.1 step 8 in require: a close that outlasts its foreground retries does
// not hold the terminal event back. A client stops waiting after
// sessionMessageTimeout, long before finish_by, and its unkeyed retry would
// deliver again. The event goes out at once with the op still open; the close
// retries in the background, holding the in-flight entry, so a same-key retry
// gets the cached 202, and the op closes once the store recovers.
func TestSessionContentOpsRequireEmitsWhileTheCloseRetries(t *testing.T) {
	e := newOpsEnv(t, "require")
	info := e.session(t, "Require close")
	e.store.set(func(s *opsTestStore) { s.updateFails = 1 << 30 })
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-s", `{"message":"x"}`).Body)
	evs := waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	var p SessionSubmitSucceededPayload
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil || p.Op == nil || p.Op.Status == "closed" {
		t.Fatalf("event = %s (%v), want the op still open", evs[0].Payload, err)
	}
	rec := e.post(t, "/session/"+info.SessionName+"/submit", "key-s", `{"message":"x"}`)
	acc := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || acc.RequestID != first.RequestID || acc.Op == nil || acc.Op.Attempt != 0 || acc.Replayed {
		t.Fatalf("retry while the close retries = %d %+v op %+v, want the cached 202", rec.Code, acc, acc.Op)
	}
	e.store.set(func(s *opsTestStore) { s.updateFails = 0 })
	// The background retry backs off toward 5s between attempts.
	if op := e.waitClosedWithin(t, first.RequestID, 3*testEventTimeout); op.Attempt != 0 || op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v, want the first attempt closed succeeded", op)
	}
	city := e.server(t).ops.city
	waitFor(t, testEventTimeout, "the closed op's in-flight entry is released", func() bool { return city.inflight.Len() == 0 })
	if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
}

// closingStore is a State's sessions store that the city's restart closes:
// once closed, op-row conditional updates fail.
type closingStore struct {
	*opsTestStore
	closed atomic.Bool
}

func (s *closingStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if s.closed.Load() {
		return errors.New("store closed by the city restart")
	}
	return s.opsTestStore.UpdateIfMatch(id, rev, opts)
}

// closableEvents is a State's event provider that the city's restart closes:
// once closed, it refuses what is recorded, as a closed FileRecorder does:
// RecordAck reports the refusal and Record drops the event silently. It
// keeps what it refused for the test to inspect.
type closableEvents struct {
	*events.Fake
	closed atomic.Bool

	mu      sync.Mutex
	refused []events.Event
}

func (c *closableEvents) Record(e events.Event) {
	_ = c.RecordAck(e)
}

func (c *closableEvents) RecordAck(e events.Event) error {
	if c.closed.Load() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.refused = append(c.refused, e)
		return errors.New("event log closed by the city restart")
	}
	return c.Fake.RecordAck(e)
}

// refusedEvents are the events it refused while closed.
func (c *closableEvents) refusedEvents() []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.refused)
}

// I3 and §6.1 step 8 when the city restart closes the old State's store and
// event log while the old Server still delivers: a same-key retry on the new
// Server gets the cached 202 instead of adopting and delivering again, the
// old Server's terminal event for it lands in the new Server's event log,
// and the op's close, refused by the old store, is retried through the new
// one: the op closes with the delivery's outcome instead of expiring at
// finish_by.
func TestSessionContentOpsRebuildWithTheOldStoreClosedDeliversOnce(t *testing.T) {
	e := newOpsEnv(t, "auto")
	old := &closingStore{opsTestStore: e.store}
	e.fs.cityBeadStore = old
	oldEv := &closableEvents{Fake: events.NewFake()}
	e.fs.eventProv = oldEv
	info := e.session(t, "Old store")
	gp, release := e.gatedProvider(t)
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-c", `{"message":"once"}`).Body)
	gp.waitEntered(t)
	newEv := events.NewFake()
	e.rebuildOver(t, e.store, newEv)
	old.closed.Store(true)
	oldEv.closed.Store(true)
	rec := e.post(t, "/session/"+info.ID+"/submit", "key-c", `{"message":"once"}`)
	acc := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || acc.RequestID != first.RequestID || acc.Op == nil || acc.Op.Attempt != 0 || acc.Replayed {
		t.Fatalf("retry on the new Server = %d %+v op %+v, want the cached 202", rec.Code, acc, acc.Op)
	}
	release()
	if evs := waitTerminalEvents(t, newEv, acc.RequestID, 1); evs[0].Type != events.RequestResultSessionSubmit {
		t.Fatalf("event on the new provider = %s: %s", evs[0].Type, evs[0].Payload)
	}
	if op := e.waitClosed(t, first.RequestID); op.Attempt != 0 || op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v, want the first attempt closed succeeded", op)
	}
	city := e.server(t).ops.city
	waitFor(t, testEventTimeout, "the closed op's in-flight entry is released", func() bool { return city.inflight.Len() == 0 })
	if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
		t.Fatalf("deliveries for one key = %d, want 1", got)
	}
}

// restartGapEnv is a city whose restart closed the store and event log its
// Server delivers through while a submit was delivering: the delivery has
// finished, and the closed log refused its terminal event, but no request
// has brought the new Server yet. The op's close fails until the test clears
// updateFails.
type restartGapEnv struct {
	*opsEnv
	info  session.Info
	first asyncAcceptedBody
	// cityLog is the city's events.jsonl, reopened by the restart.
	cityLog *events.Fake
	city    *cityOps
}

func newRestartGapEnv(t *testing.T, key string) *restartGapEnv {
	t.Helper()
	e := newOpsEnv(t, "auto")
	old := &closingStore{opsTestStore: e.store}
	e.fs.cityBeadStore = old
	cityLog := events.NewFake()
	oldEv := &closableEvents{Fake: cityLog}
	e.fs.eventProv = oldEv
	info := e.session(t, "Restart gap")
	gp, release := e.gatedProvider(t)
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", key, `{"message":"once"}`).Body)
	gp.waitEntered(t)
	city := e.server(t).ops.city
	// The restart closes the old store and event log, and the resolver
	// serves the reopened ones. The op's close keeps failing, so its
	// in-flight entry stays held.
	e.store.set(func(s *opsTestStore) { s.updateFails = 1 << 30 })
	old.closed.Store(true)
	oldEv.closed.Store(true)
	e.resolver.state = &rebuiltOverState{fakeState: e.fs, store: e.store, ev: cityLog}
	release()
	waitFor(t, testEventTimeout, "the closed event log refuses the terminal event", func() bool {
		return len(terminalEventsOf(oldEv.refusedEvents(), first.RequestID)) == 1
	})
	if n := len(terminalEvents(t, cityLog, first.RequestID)); n != 0 {
		t.Fatalf("terminal events before the city is served again = %d, want 0", n)
	}
	return &restartGapEnv{opsEnv: e, info: info, first: first, cityLog: cityLog, city: city}
}

// requireOneEventAfter fails unless the city's log holds exactly one
// terminal event for the submit, a submit result after cursor.
func (g *restartGapEnv) requireOneEventAfter(t *testing.T, what, cursor string) {
	t.Helper()
	evs := terminalEvents(t, g.cityLog, g.first.RequestID)
	if len(evs) != 1 || evs[0].Type != events.RequestResultSessionSubmit || evs[0].Seq <= mustSeq(t, cursor) {
		t.Fatalf("%s: terminal events = %+v, want one after cursor %s", what, evs, cursor)
	}
}

// finish lets the op close and checks it closed from its one delivery.
func (g *restartGapEnv) finish(t *testing.T) {
	t.Helper()
	g.store.set(func(s *opsTestStore) { s.updateFails = 0 })
	// The background retry backs off toward 5s between attempts.
	if op := g.waitClosedWithin(t, g.first.RequestID, 3*testEventTimeout); op.Attempt != 0 || op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v, want the first attempt closed succeeded", op)
	}
	waitFor(t, testEventTimeout, "the closed op's in-flight entry is released", func() bool { return g.city.inflight.Len() == 0 })
	if got := len(deliveredCalls(g.fs.sp, g.info.SessionName)); got != 1 {
		t.Fatalf("deliveries for one key = %d, want 1", got)
	}
}

// §6.1 step 8 across a restart gap: the supervisor serves a restarted city
// with a new Server only when a request for it arrives, so a delivery can
// finish after the restart closed the old State's event log while no new
// Server is live. Its terminal event, which the closed log refuses, is kept
// and recorded once the supervisor serves the city again, in the log the
// 202's cursor counts in (the city's events.jsonl, reopened): a same-key
// retry, the request that brings the new Server, gets the cached 202 and can
// read the event after its cursor, and so can a later retry the LRU answers.
func TestSessionContentOpsRestartGapKeepsTheTerminalEvent(t *testing.T) {
	g := newRestartGapEnv(t, "key-g")
	for _, retry := range []string{"the retry that brings the new Server", "the retry the LRU answers"} {
		rec := g.post(t, "/session/"+g.info.ID+"/submit", "key-g", `{"message":"once"}`)
		acc := decodeAsyncAccepted(t, rec.Body)
		if rec.Code != http.StatusAccepted || acc.RequestID != g.first.RequestID || acc.EventCursor != g.first.EventCursor || acc.Replayed {
			t.Fatalf("%s = %d %+v, want the cached 202 %+v", retry, rec.Code, acc, g.first)
		}
		g.requireOneEventAfter(t, retry, acc.EventCursor)
	}
	g.finish(t)
	g.requireOneEventAfter(t, "after the close", g.first.EventCursor)
}

// §6.1 step 8 across a restart gap, with no retry: whatever request brings
// the restarted city's new Server records the kept terminal event before it
// is served, so a client that waits on the event stream reads the event once
// anything serves the city.
func TestSessionContentOpsRestartGapRecordsTheKeptEventWhenTheCityIsServed(t *testing.T) {
	g := newRestartGapEnv(t, "key-s")
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(g.fs, "/sessions"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sessions = %d: %s", rec.Code, rec.Body.String())
	}
	g.requireOneEventAfter(t, "the request that brings the new Server", g.first.EventCursor)
	g.finish(t)
	g.requireOneEventAfter(t, "after the close", g.first.EventCursor)
}

// §6.2 in-flight rows across a restart gap whose reopened event log refuses
// too: the kept terminal event cannot be recorded yet, so a same-key retry
// is answered busy (409 in_flight), which the LRU does not keep, instead of a
// 202 whose cursor no event follows; once the log takes the event, the next
// retry records it and gets the cached 202.
func TestSessionContentOpsInFlightReplayIsBusyWhileTheEventIsKept(t *testing.T) {
	e := newOpsEnv(t, "auto")
	cityLog := events.NewFake()
	oldEv := &closableEvents{Fake: cityLog}
	e.fs.eventProv = oldEv
	info := e.session(t, "Kept")
	gp, release := e.gatedProvider(t)
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-k", `{"message":"once"}`).Body)
	gp.waitEntered(t)
	city := e.server(t).ops.city
	e.store.set(func(s *opsTestStore) { s.updateFails = 1 << 30 })
	oldEv.closed.Store(true)
	newEv := &closableEvents{Fake: cityLog}
	newEv.closed.Store(true)
	e.resolver.state = &rebuiltOverState{fakeState: e.fs, store: e.store, ev: newEv}
	release()
	waitFor(t, testEventTimeout, "the closed event log refuses the terminal event", func() bool {
		return len(terminalEventsOf(oldEv.refusedEvents(), first.RequestID)) == 1
	})
	if rec := e.post(t, "/session/"+info.ID+"/submit", "key-k", `{"message":"once"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in_flight") {
		t.Fatalf("retry while the event log refuses the kept event = %d: %s, want 409 in_flight", rec.Code, rec.Body.String())
	}
	if n := len(terminalEventsOf(newEv.refusedEvents(), first.RequestID)); n == 0 {
		t.Fatal("the reopened event log was never offered the kept event")
	}
	newEv.closed.Store(false)
	rec := e.post(t, "/session/"+info.ID+"/submit", "key-k", `{"message":"once"}`)
	acc := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || acc.RequestID != first.RequestID || acc.EventCursor != first.EventCursor || acc.Replayed {
		t.Fatalf("retry once the log takes the event = %d %+v, want the cached 202 %+v", rec.Code, acc, first)
	}
	if evs := terminalEvents(t, cityLog, first.RequestID); len(evs) != 1 || evs[0].Seq <= mustSeq(t, acc.EventCursor) {
		t.Fatalf("terminal events = %+v, want one after cursor %s", evs, acc.EventCursor)
	}
	e.store.set(func(s *opsTestStore) { s.updateFails = 0 })
	if op := e.waitClosedWithin(t, first.RequestID, 3*testEventTimeout); op.Attempt != 0 || op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v, want the first attempt closed succeeded", op)
	}
	waitFor(t, testEventTimeout, "the closed op's in-flight entry is released", func() bool { return city.inflight.Len() == 0 })
	if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
		t.Fatalf("deliveries for one key = %d, want 1", got)
	}
}

// §6.2 on the Server that accepted the request: its event log refuses the
// request's terminal event (a lock timeout, a full disk) while the city's
// State is unchanged, so no new Server records the kept event. Every same-key
// retry is busy (409 in_flight) while the event is kept, whether the LRU
// answers it (the same path) or the op row does (another spelling of the
// target), so none gets a 202 with no event after its cursor; once the log
// takes the event, the LRU's retry records it and gets the cached 202.
func TestSessionContentOpsReplayIsBusyWhileTheLiveLogRefusesTheEvent(t *testing.T) {
	for _, endpoint := range []string{"/submit", "/messages"} {
		t.Run(strings.TrimPrefix(endpoint, "/"), func(t *testing.T) {
			e := newOpsEnv(t, "auto")
			cityLog := events.NewFake()
			live := &closableEvents{Fake: cityLog}
			e.fs.eventProv = live
			info := e.session(t, "Live log refuses")
			gp, release := e.gatedProvider(t)
			path := "/session/" + info.ID + endpoint
			first := decodeAsyncAccepted(t, e.post(t, path, "key-l", `{"message":"once"}`).Body)
			gp.waitEntered(t)
			srv := e.server(t)
			live.closed.Store(true)
			release()
			waitFor(t, testEventTimeout, "the live event log refuses the terminal event", func() bool {
				return len(terminalEventsOf(live.refusedEvents(), first.RequestID)) == 1
			})
			e.waitClosed(t, first.RequestID)
			waitFor(t, testEventTimeout, "the closed op's in-flight entry is released", func() bool { return srv.ops.city.inflight.Len() == 0 })
			for _, retry := range []struct{ what, path string }{
				{"the retry the LRU answers", path},
				{"the retry the op row answers", "/session/" + info.SessionName + endpoint},
			} {
				if rec := e.post(t, retry.path, "key-l", `{"message":"once"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in_flight") {
					t.Fatalf("%s while the event is kept = %d: %s, want 409 in_flight", retry.what, rec.Code, rec.Body.String())
				}
			}
			if e.server(t) != srv {
				t.Fatal("the supervisor built a new Server for the unchanged State")
			}
			live.closed.Store(false)
			rec := e.post(t, path, "key-l", `{"message":"once"}`)
			acc := decodeAsyncAccepted(t, rec.Body)
			if rec.Code != http.StatusAccepted || acc.RequestID != first.RequestID || acc.EventCursor != first.EventCursor || acc.Replayed {
				t.Fatalf("retry once the log takes the event = %d %+v, want the cached 202 %+v", rec.Code, acc, first)
			}
			if evs := terminalEvents(t, cityLog, first.RequestID); len(evs) != 1 || evs[0].Seq <= mustSeq(t, acc.EventCursor) {
				t.Fatalf("terminal events = %+v, want the kept one after cursor %s", evs, acc.EventCursor)
			}
			if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
				t.Fatalf("deliveries for one key = %d, want 1", got)
			}
		})
	}
}

// The same in off mode, the default, which records no op: the
// Idempotency-Key LRU is the only path that replays a same-key retry, and it
// answers busy (409 in_flight) while the request's terminal event is kept;
// once the log takes the event, the retry records it and gets the cached 202.
func TestSessionContentOpsOffReplayIsBusyWhileTheLiveLogRefusesTheEvent(t *testing.T) {
	for _, endpoint := range []string{"/submit", "/messages"} {
		t.Run(strings.TrimPrefix(endpoint, "/"), func(t *testing.T) {
			e := newOpsEnv(t, "")
			cityLog := events.NewFake()
			live := &closableEvents{Fake: cityLog}
			e.fs.eventProv = live
			info := e.session(t, "Off live log refuses")
			gp, release := e.gatedProvider(t)
			path := "/session/" + info.ID + endpoint
			first := decodeAsyncAccepted(t, e.post(t, path, "key-o", `{"message":"once"}`).Body)
			if !strings.HasPrefix(first.RequestID, "req-") {
				t.Fatalf("request_id = %q, want an off-mode req- id", first.RequestID)
			}
			gp.waitEntered(t)
			srv := e.server(t)
			live.closed.Store(true)
			release()
			waitFor(t, testEventTimeout, "the live event log refuses the terminal event", func() bool {
				return len(terminalEventsOf(live.refusedEvents(), first.RequestID)) == 1
			})
			if rec := e.post(t, path, "key-o", `{"message":"once"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in_flight") {
				t.Fatalf("off-mode LRU retry while the event is kept = %d: %s, want 409 in_flight", rec.Code, rec.Body.String())
			}
			if e.server(t) != srv {
				t.Fatal("the supervisor built a new Server for the unchanged State")
			}
			live.closed.Store(false)
			rec := e.post(t, path, "key-o", `{"message":"once"}`)
			acc := decodeAsyncAccepted(t, rec.Body)
			if rec.Code != http.StatusAccepted || acc.RequestID != first.RequestID || acc.EventCursor != first.EventCursor {
				t.Fatalf("retry once the log takes the event = %d %+v, want the cached 202 %+v", rec.Code, acc, first)
			}
			if evs := terminalEvents(t, cityLog, first.RequestID); len(evs) != 1 || evs[0].Seq <= mustSeq(t, acc.EventCursor) {
				t.Fatalf("terminal events = %+v, want the kept one after cursor %s", evs, acc.EventCursor)
			}
			if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
				t.Fatalf("deliveries for one key = %d, want 1", got)
			}
			if rows := e.opRows(t); len(rows) != 0 {
				t.Fatalf("off wrote %d op rows", len(rows))
			}
		})
	}
}

// keptFor counts the terminal events city keeps for requestID.
func keptFor(city *cityOps, requestID string) int {
	city.emitMu.Lock()
	defer city.emitMu.Unlock()
	n := 0
	for _, k := range city.kept {
		if k.requestID == requestID {
			n++
		}
	}
	return n
}

// §6.3 for a replay of a finished request while the live event log refuses:
// the terminal event the replay re-emits is kept like any other, so the
// replay is busy (409 in_flight) rather than a 202 with no event after its
// cursor, and a further retry keeps no second copy. Once the log takes the
// kept event, the replay gets its 202 with one event for its endpoint after
// its cursor: the kept one, when the first request or a replay on this
// endpoint kept it, or a fresh one after it, when a replay on the other
// endpoint did, since a waiter on this endpoint would never match that one.
func TestSessionContentOpsStoredReplayIsBusyWhileItsEventIsKept(t *testing.T) {
	endpoints := []struct{ path, event string }{
		{"/submit", events.RequestResultSessionSubmit},
		{"/messages", events.RequestResultSessionMessage},
	}
	for i, ep := range endpoints {
		other := endpoints[1-i]
		for _, keptBy := range []string{"the first request", "a replay", "an other-endpoint replay"} {
			t.Run(strings.TrimPrefix(ep.path, "/")+"/kept by "+keptBy, func(t *testing.T) {
				e := newOpsEnv(t, "auto")
				cityLog := events.NewFake()
				live := &closableEvents{Fake: cityLog}
				e.fs.eventProv = live
				info := e.session(t, "Replay refused")
				var first asyncAcceptedBody
				if keptBy == "the first request" {
					gp, release := e.gatedProvider(t)
					first = decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+ep.path, "key-p", `{"message":"once"}`).Body)
					gp.waitEntered(t)
					live.closed.Store(true)
					release()
					waitFor(t, testEventTimeout, "the live event log refuses the first request's terminal event", func() bool {
						return len(terminalEventsOf(live.refusedEvents(), first.RequestID)) == 1
					})
				} else {
					first = decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+ep.path, "key-p", `{"message":"once"}`).Body)
					waitTerminalEvents(t, cityLog, first.RequestID, 1)
				}
				e.waitClosed(t, first.RequestID)
				city := e.server(t).ops.city
				waitFor(t, testEventTimeout, "the closed op's in-flight entry is released", func() bool { return city.inflight.Len() == 0 })
				live.closed.Store(true)
				// Another spelling of the target goes past the per-path LRU to
				// the op row, which replays the stored outcome.
				replay := "/session/" + info.SessionName + ep.path
				busy := replay
				if keptBy == "an other-endpoint replay" {
					busy = "/session/" + info.SessionName + other.path
				}
				for range 2 {
					if rec := e.post(t, busy, "key-p", `{"message":"once"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in_flight") {
						t.Fatalf("replay while the log refuses its event = %d: %s, want 409 in_flight", rec.Code, rec.Body.String())
					}
				}
				if n := keptFor(city, first.RequestID); n != 1 {
					t.Fatalf("kept events for the op = %d, want 1: a busy retry keeps no second copy", n)
				}
				live.closed.Store(false)
				rec := e.post(t, replay, "key-p", `{"message":"once"}`)
				acc := decodeAsyncAccepted(t, rec.Body)
				if rec.Code != http.StatusAccepted || acc.RequestID != first.RequestID || !acc.Replayed {
					t.Fatalf("replay once the log takes its event = %d %+v, want a replayed 202", rec.Code, acc)
				}
				var after []string
				for _, ev := range terminalEvents(t, cityLog, first.RequestID) {
					if ev.Seq > mustSeq(t, acc.EventCursor) {
						after = append(after, ev.Type)
					}
				}
				want := []string{ep.event}
				if keptBy == "an other-endpoint replay" {
					want = []string{other.event, ep.event}
				}
				if !slices.Equal(after, want) {
					t.Fatalf("terminal events after cursor %s = %v, want %v", acc.EventCursor, after, want)
				}
				if n := keptFor(city, first.RequestID); n != 0 {
					t.Fatalf("kept events for the op after its 202 = %d, want 0", n)
				}
				if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
					t.Fatalf("deliveries for one key = %d, want 1", got)
				}
			})
		}
	}
}

// The Server the supervisor serves a city with is the city's live Server,
// the one its deliveries report and close through. Dropping the city leaves
// it with none and keeps its background close retries running, so a city
// that comes back closes what they could not; the supervisor's shutdown
// ends them.
func TestSessionContentOpsLiveServerOnDropAndSupervisorShutdown(t *testing.T) {
	retriesEnded := func(srv *Server) bool {
		select {
		case <-srv.ops.content.Done:
			return true
		default:
			return false
		}
	}
	t.Run("drop", func(t *testing.T) {
		e := newOpsEnv(t, "auto")
		srv := e.server(t)
		if live := srv.liveServer(); live != srv {
			t.Fatalf("live Server = %p, want the served %p", live, srv)
		}
		e.sm.dropCityServer(e.fs.CityName())
		if live := srv.ops.city.liveServer(); live != nil {
			t.Fatalf("live Server after the drop = %p, want none", live)
		}
		if retriesEnded(srv) {
			t.Fatal("the drop ended the city's background close retries")
		}
		next := e.server(t)
		if next == srv || srv.liveServer() != next {
			t.Fatalf("the city came back on %p, live %p; want a new live Server", next, srv.liveServer())
		}
	})
	t.Run("supervisor shutdown", func(t *testing.T) {
		e := newOpsEnv(t, "auto")
		srv := e.server(t)
		if err := e.sm.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !retriesEnded(srv) {
			t.Fatal("the supervisor's shutdown did not end the city's background close retries")
		}
	})
}

// seedOp proposes the op a submit of text under key would record, under
// executor, as a producer that then died would leave it.
func (e *opsEnv) seedOp(t *testing.T, conv, key, text string, executor opjournal.ExecutorKind) opjournal.Op {
	t.Helper()
	return e.seedOpAt(t, conv, key, text, executor, time.Now())
}

// seedOpAt is seedOp for a producer that proposed the op at now.
func (e *opsEnv) seedOpAt(t *testing.T, conv, key, text string, executor opjournal.ExecutorKind, now time.Time) opjournal.Op {
	t.Helper()
	st, err := opjournal.NewStore(beads.SessionStore{Store: e.store.MemStore}, &clock.Fake{Time: now}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("open op store: %v", err)
	}
	op, created, err := st.Propose(opjournal.Request{
		Conversation: conv, Kind: opjournal.KindMessage, Executor: executor, ClientKey: key,
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  text, Policy: MessageOpPolicy(session.SubmitIntentDefault),
		StartBy: now.Add(legacyop.ExecutionBound), FinishBy: now.Add(legacyop.ExecutionBound),
	})
	if err != nil || !created {
		t.Fatalf("seed: %v created %v", err, created)
	}
	return op
}

// D9: a same-key retry adopts the open legacy:api op a dead API left: it
// answers with that op's id, re-executes once (attempt 1), delivers the
// original text with no op id, key or marker (I11), and emits one terminal
// event.
func TestSessionContentOpsAutoAdoptsAnOrphan(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Orphan")
	orphan := e.seedOp(t, info.ID, "key-o", "deliver me", legacyop.ExecutorAPI)
	rec := e.post(t, "/session/"+info.ID+"/submit", "key-o", `{"message":"deliver me"}`)
	acc := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || acc.RequestID != orphan.ID || acc.Replayed || acc.Op == nil || acc.Op.Attempt != 1 {
		t.Fatalf("adoption = %d %+v op %+v", rec.Code, acc, acc.Op)
	}
	evs := waitTerminalEvents(t, e.fs.eventProv, orphan.ID, 1)
	if evs[0].Type != events.RequestResultSessionSubmit {
		t.Fatalf("event %s: %s", evs[0].Type, evs[0].Payload)
	}
	op := e.waitClosed(t, orphan.ID)
	if op.Attempt != 1 || op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("adopted op = %+v", op)
	}
	calls := deliveredCalls(e.fs.sp, info.SessionName)
	if len(calls) != 1 || calls[0].Message != "deliver me" {
		t.Fatalf("delivered = %+v", calls)
	}
	assertNoOpIDs(t, calls, orphan.ID, "key-o")
	time.Sleep(20 * time.Millisecond)
	if n := len(terminalEvents(t, e.fs.eventProv, orphan.ID)); n != 1 {
		t.Fatalf("terminal events = %d, want 1", n)
	}
}

// assertNoOpIDs asserts no runtime call carries ids, outside DeliveryKey:
// after OJ-7 only the delivery key carries the op-derived key, which a
// deduplicating runtime digests into its own ids (DES-O0API §15).
func assertNoOpIDs(t *testing.T, calls []runtime.Call, ids ...string) {
	t.Helper()
	for _, c := range calls {
		c.DeliveryKey = ""
		raw := fmt.Sprintf("%+v", c)
		for _, id := range ids {
			if id != "" && strings.Contains(raw, id) {
				t.Fatalf("runtime call carries %q: %s", id, raw)
			}
		}
	}
}

// §6.2: an open op another surface executes (the CLI) is busy: 409, no
// delivery.
func TestSessionContentOpsAutoAnswersBusyForAnOpenCLIOp(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "CLI")
	e.seedOp(t, info.ID, "key-c", "cli text", legacyop.ExecutorCLI)
	rec := e.post(t, "/session/"+info.ID+"/submit", "key-c", `{"message":"cli text"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in_flight") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 0 {
		t.Fatalf("deliveries = %d, want 0", got)
	}
}

// I3, D9: an open legacy op past its finish_by, as a producer that died
// leaves it until the sweeper runs, is not adopted, whichever surface
// executes it: its message may have landed. A same-key retry, on either
// endpoint, closes it failed{deadline_in_flight} at its attempt and replays
// that, delivering nothing.
func TestSessionContentOpsClosesAnExpiredOp(t *testing.T) {
	operations := map[string]string{"/submit": RequestOperationSessionSubmit, "/messages": RequestOperationSessionMessage}
	for _, mode := range []string{"auto", "require"} {
		for _, executor := range []opjournal.ExecutorKind{legacyop.ExecutorAPI, legacyop.ExecutorCLI} {
			for _, endpoint := range []string{"/submit", "/messages"} {
				t.Run(mode+"_"+string(executor)+"_"+strings.TrimPrefix(endpoint, "/"), func(t *testing.T) {
					e := newOpsEnv(t, mode)
					info := e.session(t, "Expired")
					expired := e.seedOpAt(t, info.ID, "key-x", "landed or not", executor, time.Now().Add(-legacyop.ExecutionBound-time.Minute))
					rec := e.post(t, "/session/"+info.ID+endpoint, "key-x", `{"message":"landed or not"}`)
					if rec.Code != http.StatusAccepted {
						t.Fatalf("retry of the expired op = %d %s, want its replay", rec.Code, rec.Body)
					}
					if acc := decodeAsyncAccepted(t, rec.Body); acc.RequestID != expired.ID || !acc.Replayed || acc.Op == nil || acc.Op.Attempt != 0 {
						t.Fatalf("retry of the expired op = %+v op %+v, want its replay", acc, acc.Op)
					}
					failure := waitForRequestFailed(t, e.fs.eventProv, expired.ID)
					if failure.Operation != operations[endpoint] || failure.ErrorCode != "timeout" {
						t.Fatalf("replayed failure = %+v, want %s timeout", failure, operations[endpoint])
					}
					op := e.waitClosed(t, expired.ID)
					if op.Attempt != 0 || op.Outcome.Arm != opjournal.ArmFailed || op.Outcome.Reason != opjournal.ReasonDeadlineInFlight || closes(op) != 1 {
						t.Fatalf("op = %+v at attempt %d closed %d times, want failed{deadline_in_flight} at attempt 0", op.Outcome, op.Attempt, closes(op))
					}
					if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 0 {
						t.Fatalf("deliveries = %d, want 0", got)
					}
				})
			}
		}
	}
}

// skewedClock is the real clock moved ahead by skew. Unlike clock.Fake it is
// safe for concurrent use, so a test can move a running recorder's clock.
type skewedClock struct{ skew atomic.Int64 }

func (c *skewedClock) Now() time.Time { return time.Now().Add(time.Duration(c.skew.Load())) }

func (c *skewedClock) advance(d time.Duration) { c.skew.Add(int64(d)) }

// §12, I3: a request whose close never lands holds its in-flight entry until
// its op's finish_by has passed, then releases it with the op still open and
// its message delivered. A same-key retry after that, before the sweeper
// runs, closes the op failed{deadline_in_flight} and replays that: it does
// not adopt the op and deliver the message again.
func TestSessionContentOpsRetryAfterTheHeldEntryIsReleasedDeliversNothing(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Released")
	srv := e.server(t)
	clk := &skewedClock{}
	srv.ops.content.Clock = clk
	e.store.set(func(s *opsTestStore) { s.updateFails = 1 << 30 })
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-r", `{"message":"once"}`).Body)
	if evs := waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1); evs[0].Type != events.RequestResultSessionSubmit {
		t.Fatalf("event %s: %s", evs[0].Type, evs[0].Payload)
	}
	clk.advance(legacyop.ExecutionBound + time.Second)
	waitFor(t, 3*testEventTimeout, "the background close gives up and releases the entry", func() bool { return srv.ops.city.inflight.Len() == 0 })
	e.store.set(func(s *opsTestStore) { s.updateFails = 0 })

	// Past the per-path LRU, as a retry by another spelling or after an
	// eviction gets.
	rec := e.post(t, "/session/"+info.SessionName+"/submit", "key-r", `{"message":"once"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry after the release = %d %s, want the op's replay", rec.Code, rec.Body)
	}
	if acc := decodeAsyncAccepted(t, rec.Body); acc.RequestID != first.RequestID || !acc.Replayed || acc.Op == nil || acc.Op.Attempt != 0 {
		t.Fatalf("retry after the release = %+v op %+v, want the op's replay", acc, acc.Op)
	}
	evs := waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 2)
	var replayed RequestFailedPayload
	if err := json.Unmarshal(evs[1].Payload, &replayed); err != nil || evs[1].Type != events.RequestFailed || replayed.ErrorCode != "timeout" {
		t.Fatalf("replayed event %s: %s (%v), want request.failed timeout", evs[1].Type, evs[1].Payload, err)
	}
	op := e.waitClosed(t, first.RequestID)
	if op.Attempt != 0 || op.Outcome.Arm != opjournal.ArmFailed || op.Outcome.Reason != opjournal.ReasonDeadlineInFlight || closes(op) != 1 {
		t.Fatalf("op = %+v at attempt %d closed %d times, want failed{deadline_in_flight} at attempt 0", op.Outcome, op.Attempt, closes(op))
	}
	if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
}

// I11: on each endpoint the text the runtime receives is the same in off,
// auto and require, and on an adopted re-execution, and carries no op id,
// key or request_id.
func TestSessionContentOpsDeliveredTextIsTheSameInEveryMode(t *testing.T) {
	const text = "exactly these bytes;\n  and these"
	body, _ := json.Marshal(map[string]string{"message": text})
	delivered := func(t *testing.T, e *opsEnv, info session.Info, acc asyncAcceptedBody) []runtime.Call {
		t.Helper()
		waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
		calls := deliveredCalls(e.fs.sp, info.SessionName)
		if len(calls) != 1 || calls[0].Message != text {
			t.Fatalf("delivered %+v", calls)
		}
		assertNoOpIDs(t, calls, acc.RequestID, acc.IdempotencyKey)
		return []runtime.Call{{Method: calls[0].Method, Message: calls[0].Message, Content: calls[0].Content, DeliveryKey: calls[0].DeliveryKey}}
	}
	for _, path := range []string{"/submit", "/messages"} {
		t.Run(path, func(t *testing.T) {
			var want []runtime.Call
			for _, mode := range []string{"off", "auto", "require", "adopted"} {
				gateMode := mode
				if mode == "adopted" {
					gateMode = "auto"
				}
				e := newOpsEnv(t, gateMode)
				info := e.session(t, "Text "+mode)
				if mode == "adopted" {
					e.seedOp(t, info.ID, "key-t", text, legacyop.ExecutorAPI)
				}
				acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+path, "key-t", string(body)).Body)
				if mode == "adopted" && (acc.Op == nil || acc.Op.Attempt != 1) {
					t.Fatalf("adopted: 202 %+v op %+v, want attempt 1", acc, acc.Op)
				}
				got := delivered(t, e, info, acc)
				if want == nil {
					want = got
					continue
				}
				if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
					t.Fatalf("%s delivered %+v, off delivered %+v", mode, got, want)
				}
			}
		})
	}
}

// D12: require refuses before any effect what it cannot record; auto runs
// the same request unrecorded and raises one latched degrade event.
func TestSessionContentOpsRequireRefusesWhatItCannotRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*opsEnv)
		body   string
		status int
		code   string
	}{
		{
			"store unavailable", func(e *opsEnv) { e.store.set(func(s *opsTestStore) { s.createErr = errors.New("transport: down") }) },
			`{"message":"x"}`, http.StatusServiceUnavailable, "op_journal_unavailable",
		},
		{
			"payload too large", func(*opsEnv) {}, `{"message":"` + strings.Repeat("y", opjournal.MaxInlinePayload+1) + `"}`,
			http.StatusUnprocessableEntity, "payload_too_large",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newOpsEnv(t, "require")
			info := e.session(t, "Require")
			tc.setup(e)
			rec := e.post(t, "/session/"+info.ID+"/submit", "", tc.body)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			time.Sleep(20 * time.Millisecond)
			if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 0 {
				t.Fatalf("require delivered %d times after refusing", got)
			}

			auto := newOpsEnv(t, "auto")
			info = auto.session(t, "Auto")
			tc.setup(auto)
			for range 2 {
				rec = auto.post(t, "/session/"+info.ID+"/submit", "", tc.body)
				acc := decodeAsyncAccepted(t, rec.Body)
				if rec.Code != http.StatusAccepted || acc.Op != nil || !strings.HasPrefix(acc.RequestID, "req-") || acc.IdempotencyKey != "" {
					t.Fatalf("auto = %d %+v", rec.Code, acc)
				}
				waitTerminalEvents(t, auto.fs.eventProv, acc.RequestID, 1)
			}
			if got := len(deliveredCalls(auto.fs.sp, info.SessionName)); got != 2 {
				t.Fatalf("auto deliveries = %d, want 2", got)
			}
		})
	}
}

// D13: a store that cannot record under auto raises ops.recording_degraded
// once per latch interval, naming the gate, mode, surface and stage.
func TestSessionContentOpsDegradeRaisesALatchedEvent(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Degrade")
	e.store.set(func(s *opsTestStore) { s.createErr = errors.New("transport: down") })
	for range 3 {
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"x"}`).Body)
		waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
	}
	evs, _ := e.fs.eventProv.List(events.Filter{Type: events.OpsRecordingDegraded})
	if len(evs) != 1 {
		t.Fatalf("degrade events = %d, want 1", len(evs))
	}
	var p events.OpsRecordingDegradedPayload
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	want := events.OpsRecordingDegradedPayload{Gate: rollout.KeyOpsContent, Mode: "auto", Surface: "api", Stage: "propose", Reason: legacyop.DegradeStoreError, Detail: p.Detail}
	if p != want || !strings.Contains(p.Detail, "transport: down") {
		t.Fatalf("payload = %+v", p)
	}
}

// §6.4: a configured named session with no row runs unrecorded in auto and
// is materialized, then recorded, in require.
func TestSessionContentOpsUnmaterializedNamedTarget(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		t.Run(mode, func(t *testing.T) {
			e := newOpsEnv(t, mode)
			rec := e.post(t, "/session/"+url.PathEscape("myrig/worker")+"/submit", "", `{"message":"wake up"}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			acc := decodeAsyncAccepted(t, rec.Body)
			if mode == "auto" {
				if acc.Op != nil || len(e.opRows(t)) != 0 {
					t.Fatalf("auto recorded an unmaterialized target: %+v", acc)
				}
				return
			}
			if acc.Op == nil || acc.Op.Conversation == "" {
				t.Fatalf("require did not record: %+v", acc)
			}
			if _, err := e.fs.cityBeadStore.Get(acc.Op.Conversation); err != nil {
				t.Fatalf("op names %q, which is not a materialized session: %v", acc.Op.Conversation, err)
			}
			waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
			e.waitClosed(t, acc.RequestID)
		})
	}
}

// Crash after commit (DES-O0API §15): the op row commits and the handler
// dies before the 202; the same-key retry adopts that op and answers with
// its id.
func TestSessionContentOpsCrashAfterCommitIsAdopted(t *testing.T) {
	e := newOpsEnv(t, "require")
	info := e.session(t, "Crash")
	e.store.set(func(s *opsTestStore) { s.panicAfterCreate = true })
	func() {
		defer func() { _ = recover() }()
		rec := e.post(t, "/session/"+info.ID+"/submit", "key-x", `{"message":"after crash"}`)
		if rec.Code < 500 {
			t.Fatalf("the crashed request answered %d: %s", rec.Code, rec.Body.String())
		}
	}()
	rows := e.opRows(t)
	if len(rows) != 1 {
		t.Fatalf("op rows after the crash = %d, want 1", len(rows))
	}
	rec := e.post(t, "/session/"+info.ID+"/submit", "key-x", `{"message":"after crash"}`)
	acc := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || acc.RequestID != rows[0].ID || acc.Op == nil || acc.Op.Attempt != 1 {
		t.Fatalf("retry = %d %+v op %+v; committed op %s", rec.Code, acc, acc.Op, rows[0].ID)
	}
	waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
	if op := e.waitClosed(t, acc.RequestID); op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v", op)
	}
}

// §12: a close that keeps failing still emits the event in auto, and the
// background retry closes the op.
func TestSessionContentOpsCloseFailureRetriesInTheBackground(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Close")
	e.store.set(func(s *opsTestStore) { s.updateFails = 4 })
	acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"x"}`).Body)
	waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
	if op := e.waitClosed(t, acc.RequestID); op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != legacyop.ViaLegacyDelivered {
		t.Fatalf("op = %+v", op)
	}
}

// T3-1b threading (DES-T3I §14 item 2): an interrupt_now on a runtime that
// reports turns carries the turn fence's report on its submit result.
func TestSessionSubmitInterruptNowReportsTheTurnFence(t *testing.T) {
	e := newOpsEnv(t, "auto")
	e.fs.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	e.fs.sessionProvider = runtime.FullFake{Fake: e.fs.sp}
	mgr := session.NewManagerWithOptions(e.fs.cityBeadStore, e.fs.sessionProvider, session.WithCityPath(e.fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "helper", Title: "Fenced", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"now","intent":"interrupt_now"}`).Body)
	success, failure := waitForSessionSubmitResult(t, e.fs.eventProv, acc.RequestID)
	if success == nil {
		t.Fatalf("submit failed: %+v", failure)
	}
	if success.Interrupt == nil || success.Interrupt.Outcome != string(session.TurnInterruptNoTurn) {
		t.Fatalf("interrupt = %+v, want no_turn", success.Interrupt)
	}
	if op := e.waitClosed(t, acc.RequestID); op.Policy.Message.When != opjournal.WhenInterrupt {
		t.Fatalf("op policy = %+v", op.Policy)
	}
}

// §6.3, both ways: each legacy result closes the op with its outcome, and a
// replay projects that outcome onto the replaying endpoint's terminal event.
func TestMessageOpOutcomesMapBothWays(t *testing.T) {
	for _, tc := range []struct {
		name       string
		queued     bool
		err        error
		arm        opjournal.Arm
		reason     string
		via        string
		submitCode string
		msgCode    string
	}{
		{"delivered", false, nil, opjournal.ArmSucceeded, "", legacyop.ViaLegacyDelivered, "", ""},
		{"queued", true, nil, opjournal.ArmSucceeded, "", legacyop.ViaLegacyQueued, "", ""},
		{"interrupt unsupported", false, fmt.Errorf("x: %w", session.ErrTurnInterruptUnsupported), opjournal.ArmFailed, opjournal.ReasonUnsupported, "", turnInterruptUnsupportedCode, turnInterruptUnsupportedCode},
		{"closed", false, fmt.Errorf("x: %w", session.ErrSessionClosed), opjournal.ArmSkipped, opjournal.ReasonConversationClosed, "", "resolve_failed", "resolve_failed"},
		{"gone", false, fmt.Errorf("x: %w", session.ErrSessionNotFound), opjournal.ArmFailed, opjournal.ReasonSessionGone, "", "resolve_failed", "resolve_failed"},
		{"runtime gone", false, fmt.Errorf("x: %w", runtime.ErrSessionNotFound), opjournal.ArmFailed, opjournal.ReasonSessionGone, "", "resolve_failed", "resolve_failed"},
		{"runtime unavailable", false, fmt.Errorf("x: %w", runtime.ErrRuntimeUnavailable), opjournal.ArmFailed, opjournal.ReasonProviderUnavailable, "", "submit_failed", "message_failed"},
		{"timeout", false, messageTimeoutError{}, opjournal.ArmFailed, opjournal.ReasonDeadlineInFlight, "", "timeout", "timeout"},
		{"other", false, errors.New("boom"), opjournal.ArmFailed, opjournal.ReasonOther, "", "submit_failed", "message_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := MessageOpResult(tc.queued, tc.err, nil)
			if res.Outcome.Arm != tc.arm || res.Outcome.Reason != tc.reason || res.Outcome.Result.Via != tc.via || res.LeaveOpen {
				t.Fatalf("result = %+v", res)
			}
			op := opjournal.Op{
				ID: "gc-op-1", Conversation: "gc-1", Kind: opjournal.KindMessage, Status: opjournal.StatusClosed, Outcome: res.Outcome,
				Policy: MessageOpPolicy(session.SubmitIntentFollowUp),
			}
			for _, ep := range []struct{ operation, code, success string }{
				{RequestOperationSessionSubmit, tc.submitCode, events.RequestResultSessionSubmit},
				{RequestOperationSessionMessage, tc.msgCode, events.RequestResultSessionMessage},
			} {
				fs := newSessionFakeState(t)
				s := New(fs)
				if s.emitMessageOpProjection(ep.operation, op) {
					t.Fatalf("%s: the projection reports a kept event", ep.operation)
				}
				evs, _ := fs.eventProv.List(events.Filter{})
				if len(evs) != 1 {
					t.Fatalf("%s: events = %d", ep.operation, len(evs))
				}
				if !strings.Contains(evs[0].Message, replayOptionsNote) {
					t.Fatalf("%s: event message %q lacks the replay options note", ep.operation, evs[0].Message)
				}
				if tc.arm == opjournal.ArmSucceeded {
					var p struct {
						RequestID string          `json:"request_id"`
						Queued    bool            `json:"queued"`
						Intent    string          `json:"intent"`
						Op        *ConversationOp `json:"op"`
					}
					if evs[0].Type != ep.success || json.Unmarshal(evs[0].Payload, &p) != nil || p.RequestID != op.ID || p.Queued != tc.queued || p.Op == nil {
						t.Fatalf("%s: %s %s", ep.operation, evs[0].Type, evs[0].Payload)
					}
					if ep.operation == RequestOperationSessionSubmit && p.Intent != "follow_up" {
						t.Fatalf("intent = %q", p.Intent)
					}
					continue
				}
				var p RequestFailedPayload
				if evs[0].Type != events.RequestFailed || json.Unmarshal(evs[0].Payload, &p) != nil ||
					p.Operation != ep.operation || p.ErrorCode != ep.code || p.Op == nil || p.ErrorMessage != evs[0].Message ||
					!strings.HasPrefix(p.ErrorMessage, "replayed: operation gc-op-1 already closed "+string(tc.arm)) {
					t.Fatalf("%s: %s %s", ep.operation, evs[0].Type, evs[0].Payload)
				}
			}
		})
	}
	unfenced := &session.TurnInterruptReport{Outcome: session.TurnInterruptSettled, Degradation: session.DegradationUnfencedInterrupt}
	if res := MessageOpResult(false, nil, unfenced); res.Outcome.Degradation != session.DegradationUnfencedInterrupt {
		t.Fatalf("unfenced interrupt outcome = %+v", res.Outcome)
	}
	if res := MessageOpResult(false, errors.New("x"), unfenced); res.Outcome.Degradation != "" {
		t.Fatalf("a failed delivery carries a degradation: %+v", res.Outcome)
	}
}

// fakeRecorder captures recorded events.
type fakeRecorder struct {
	mu  sync.Mutex
	evs []events.Event
}

func (r *fakeRecorder) Record(e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, e)
}

func (r *fakeRecorder) events() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.evs)
}

// D13 and §5.5: the reporter raises one event per (gate, surface, stage,
// reason) on its first failure after a healthy period, at most once per
// interval, a failure the interval held back raising it once the interval
// has passed: a sustained outage raises one event however long it lasts, and
// a stage that reports success re-arms only its own classes. Off is ignored,
// and the detail is cut to a valid UTF-8 prefix.
func TestOpsDegradeReporterLatchesPerClass(t *testing.T) {
	rec := &fakeRecorder{}
	clk := &clock.Fake{Time: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	r := NewOpsDegradeReporter(func() events.Recorder { return rec }, clk)
	// One byte ahead of 400 two-byte runes: the 512-byte cut falls inside a
	// rune and must back off to 511 bytes.
	d := legacyop.Degrade{Mode: gate.Auto, Surface: legacyop.SurfaceAPI, Stage: legacyop.StagePropose, Reason: legacyop.DegradeStoreError, Err: errors.New("x" + strings.Repeat("é", 400))}
	r.Report(rollout.KeyOpsContent, d)
	r.Report(rollout.KeyOpsContent, d)
	other := d
	other.Stage = legacyop.StageClose
	r.Report(rollout.KeyOpsContent, other)
	r.Report(rollout.KeyOpsControl, d)
	off := d
	off.Mode, off.Reason = gate.Off, legacyop.DegradeContention // a class no report has latched
	r.Report(rollout.KeyOpsContent, off)
	if n := len(rec.events()); n != 3 {
		t.Fatalf("events = %d, want 3 (one per class, none for off)", n)
	}
	evs := rec.events()
	var p events.OpsRecordingDegradedPayload
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Detail) != opsDegradeDetailMax-1 || !utf8.ValidString(p.Detail) || evs[0].Type != events.OpsRecordingDegraded || evs[0].Actor != "api" || evs[0].Subject != rollout.KeyOpsContent {
		t.Fatalf("event = %+v payload detail %d bytes valid=%v", evs[0], len(p.Detail), utf8.ValidString(p.Detail))
	}

	// A sustained outage: no new event however many intervals pass.
	clk.Time = clk.Time.Add(3 * opsDegradeInterval)
	r.Report(rollout.KeyOpsContent, d)
	if n := len(rec.events()); n != 3 {
		t.Fatalf("a sustained outage re-emitted: events = %d, want 3", n)
	}
	// Healthy at another stage or gate re-arms nothing here.
	r.Healthy(rollout.KeyOpsContent, legacyop.SurfaceAPI, legacyop.StageClose)
	r.Healthy(rollout.KeyOpsControl, legacyop.SurfaceAPI, legacyop.StagePropose)
	r.Report(rollout.KeyOpsContent, d)
	if n := len(rec.events()); n != 3 {
		t.Fatalf("healthy elsewhere re-armed propose: events = %d, want 3", n)
	}
	// Healthy at propose re-arms it: the next failure is news.
	r.Healthy(rollout.KeyOpsContent, legacyop.SurfaceAPI, legacyop.StagePropose)
	r.Report(rollout.KeyOpsContent, d)
	if n := len(rec.events()); n != 4 {
		t.Fatalf("after a healthy period: events = %d, want 4", n)
	}
	// Flapping inside the interval stays at one event per interval.
	for range 2 {
		r.Healthy(rollout.KeyOpsContent, legacyop.SurfaceAPI, legacyop.StagePropose)
		r.Report(rollout.KeyOpsContent, d)
	}
	if n := len(rec.events()); n != 4 {
		t.Fatalf("flapping within the interval: events = %d, want 4", n)
	}
	// The interval defers the event of a failure it held back, no more: the
	// class's first failure once the interval has passed raises it, so an
	// outage that began inside the interval is reported, once.
	clk.Time = clk.Time.Add(opsDegradeInterval)
	r.Report(rollout.KeyOpsContent, d)
	if n := len(rec.events()); n != 5 {
		t.Fatalf("an outage the interval held back: events = %d, want 5", n)
	}
	clk.Time = clk.Time.Add(3 * opsDegradeInterval)
	r.Report(rollout.KeyOpsContent, d)
	if n := len(rec.events()); n != 5 {
		t.Fatalf("the held-back event raised again: events = %d, want 5", n)
	}
	r.Healthy(rollout.KeyOpsContent, legacyop.SurfaceAPI, legacyop.StagePropose)
	r.Report(rollout.KeyOpsContent, d)
	if n := len(rec.events()); n != 6 {
		t.Fatalf("healthy, then failing an interval later: events = %d, want 6", n)
	}
}

// §5.5: an event the interval held back stays owed when the event log
// refuses it once due, so the class's next failure raises it, once.
func TestOpsDegradeReporterKeepsARefusedHeldBackEventOwed(t *testing.T) {
	ev := &closableEvents{Fake: events.NewFake()}
	clk := &clock.Fake{Time: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	r := NewOpsDegradeReporter(func() events.Recorder { return ev }, clk)
	d := legacyop.Degrade{Mode: gate.Auto, Surface: legacyop.SurfaceAPI, Stage: legacyop.StageClose, Reason: legacyop.DegradeStoreError}
	r.Report(rollout.KeyOpsContent, d)
	r.Healthy(rollout.KeyOpsContent, legacyop.SurfaceAPI, legacyop.StageClose)
	r.Report(rollout.KeyOpsContent, d) // held back by the interval
	clk.Time = clk.Time.Add(opsDegradeInterval)
	ev.closed.Store(true)
	r.Report(rollout.KeyOpsContent, d) // due, refused
	ev.closed.Store(false)
	r.Report(rollout.KeyOpsContent, d)
	r.Report(rollout.KeyOpsContent, d)
	recorded, err := ev.List(events.Filter{Type: events.OpsRecordingDegraded})
	if err != nil {
		t.Fatal(err)
	}
	if refused := len(ev.refusedEvents()); len(recorded) != 2 || refused != 1 {
		t.Fatalf("recorded events = %d, refused = %d; want 2 and 1: the refused held-back event was dropped or raised twice", len(recorded), refused)
	}
}

// §5.5: a report with nowhere to record (the city has no live Server) does
// not latch its class, so the first report that can record raises the event.
func TestOpsDegradeReporterDoesNotLatchWithoutARecorder(t *testing.T) {
	rec := &fakeRecorder{}
	var live atomic.Bool
	clk := &clock.Fake{Time: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	r := NewOpsDegradeReporter(func() events.Recorder {
		if !live.Load() {
			return nil
		}
		return rec
	}, clk)
	d := legacyop.Degrade{Mode: gate.Auto, Surface: legacyop.SurfaceAPI, Stage: legacyop.StagePropose, Reason: legacyop.DegradeStoreError}
	r.Report(rollout.KeyOpsContent, d)
	live.Store(true)
	r.Report(rollout.KeyOpsContent, d)
	if n := len(rec.events()); n != 1 {
		t.Fatalf("events = %d, want 1: the report with no recorder latched its class", n)
	}
}

// §5.5: a degrade event the event log refuses (a log the city's restart
// closed) does not latch its class either: the class's next failure raises
// the event again, and only an event the log acknowledged latches it.
func TestOpsDegradeReporterDoesNotLatchARefusedEvent(t *testing.T) {
	ev := &closableEvents{Fake: events.NewFake()}
	ev.closed.Store(true)
	clk := &clock.Fake{Time: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	r := NewOpsDegradeReporter(func() events.Recorder { return ev }, clk)
	d := legacyop.Degrade{Mode: gate.Auto, Surface: legacyop.SurfaceAPI, Stage: legacyop.StageClose, Reason: legacyop.DegradeStoreError}
	r.Report(rollout.KeyOpsContent, d)
	if n := len(ev.refusedEvents()); n != 1 {
		t.Fatalf("refused events = %d, want the first report's", n)
	}
	ev.closed.Store(false)
	r.Report(rollout.KeyOpsContent, d)
	r.Report(rollout.KeyOpsContent, d)
	recorded, err := ev.List(events.Filter{Type: events.OpsRecordingDegraded})
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 {
		t.Fatalf("recorded events = %d, want 1: the refused event latched its class, or the acknowledged one did not", len(recorded))
	}
}

// newKeepingCityOps is a city whose live Server's event log refuses events
// until reopened, with its log lines captured.
func newKeepingCityOps(t *testing.T) (*cityOps, *Server, *closableEvents, func() []string) {
	t.Helper()
	c := newCityOps()
	var mu sync.Mutex
	var lines []string
	c.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	ev := &closableEvents{Fake: events.NewFake()}
	ev.closed.Store(true)
	fs := newFakeState(t)
	fs.eventProv = ev
	srv := &Server{state: fs}
	c.publish(srv)
	return c, srv, ev, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(lines)
	}
}

// keptLines are the log lines that contain marker.
func keptLines(lines []string, marker string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, marker) {
			out = append(out, l)
		}
	}
	return out
}

// A city keeps at most maxKeptEvents terminal events its event log refuses,
// dropping the oldest and logging each drop; once the log takes events
// again, the next terminal event follows the kept ones, recorded oldest
// first.
func TestCityOpsKeepsBoundedEventsInOrder(t *testing.T) {
	c, srv, ev, lines := newKeepingCityOps(t)
	for i := range maxKeptEvents + 2 {
		id := fmt.Sprintf("req-%d", i)
		c.emit(srv, id, RequestOperationSessionSubmit, events.Event{Type: events.RequestFailed, Subject: id})
	}
	dropped := keptLines(lines(), "dropping the kept")
	if len(dropped) != 2 || !strings.Contains(dropped[0], "result req-0:") || !strings.Contains(dropped[1], "result req-1:") {
		t.Fatalf("drop log lines = %q, want req-0's then req-1's", dropped)
	}
	if !c.flushKeeps(srv, "req-2") || c.flushKeeps(srv, "req-1") {
		t.Fatal("kept events: want req-2 kept and req-1 dropped")
	}
	ev.closed.Store(false)
	c.emit(srv, "req-next", RequestOperationSessionSubmit, events.Event{Type: events.RequestFailed, Subject: "req-next"})
	recorded, err := ev.List(events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range recorded {
		got = append(got, e.Subject)
	}
	var want []string
	for i := 2; i < maxKeptEvents+2; i++ {
		want = append(want, fmt.Sprintf("req-%d", i))
	}
	want = append(want, "req-next")
	if !slices.Equal(got, want) {
		t.Fatalf("recorded %d events %q..., want %d in order", len(got), got[:min(len(got), 3)], len(want))
	}
	if c.flushKeeps(srv, "req-2") {
		t.Fatal("req-2 is still kept after the log took it")
	}
}

// A stored replay is busy while one of its request's terminal events is
// kept, keeping no second copy of its own, and once the event log takes
// them records its event after the kept ones, unless one of them is of its
// request, endpoint and type: that one stands for it. One for another
// request, for the other endpoint, whose waiter would never match it, or of
// another type does not.
func TestCityOpsReplayRecordsOneEventForItsRequestAndEndpoint(t *testing.T) {
	submitted := events.RequestResultSessionSubmit
	for _, tc := range []struct {
		name                 string
		keptID, keptOp       string // keptID "" keeps no event first
		keptType, replayType string
		want                 []string // the subjects recorded once the log takes events
	}{
		{"none: the replay keeps its own", "", "", "", submitted, []string{"replay"}},
		{"the replay's request, endpoint and type", "req-1", RequestOperationSessionSubmit, submitted, submitted, []string{"kept"}},
		{"the other endpoint", "req-1", RequestOperationSessionMessage, events.RequestResultSessionMessage, submitted, []string{"kept", "replay"}},
		{"the other endpoint's failure", "req-1", RequestOperationSessionMessage, events.RequestFailed, events.RequestFailed, []string{"kept", "replay"}},
		{"another type", "req-1", RequestOperationSessionSubmit, submitted, events.RequestFailed, []string{"kept", "replay"}},
		{"another request", "req-2", RequestOperationSessionSubmit, submitted, submitted, []string{"kept", "replay"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, srv, ev, _ := newKeepingCityOps(t)
			if tc.keptID != "" {
				c.emit(srv, tc.keptID, tc.keptOp, events.Event{Type: tc.keptType, Subject: "kept"})
			}
			replay := events.Event{Type: tc.replayType, Subject: "replay"}
			for i := range 2 {
				if !c.emitReplay(srv, "req-1", RequestOperationSessionSubmit, replay) {
					t.Fatalf("replay %d while the log refuses: not busy", i+1)
				}
			}
			if n := keptFor(c, "req-1"); n != 1 {
				t.Fatalf("req-1 keeps %d events, want 1", n)
			}
			ev.closed.Store(false)
			if c.emitReplay(srv, "req-1", RequestOperationSessionSubmit, replay) {
				t.Fatal("replay once the log takes events: busy")
			}
			recorded, err := ev.List(events.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range recorded {
				got = append(got, e.Subject)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("recorded %q, want %q", got, tc.want)
			}
			if n := keptFor(c, "req-1") + keptFor(c, "req-2"); n != 0 {
				t.Fatalf("%d events still kept, want 0", n)
			}
		})
	}
}

// At shutdown a city records the terminal events it kept when its event log
// takes them, and logs each one it cannot record as lost.
func TestCityOpsShutdownRecordsOrLogsTheKeptEvents(t *testing.T) {
	t.Run("the log refuses", func(t *testing.T) {
		c, srv, ev, lines := newKeepingCityOps(t)
		c.emit(srv, "req-a", RequestOperationSessionSubmit, events.Event{Type: events.RequestFailed, Subject: "req-a"})
		c.emit(srv, "req-b", RequestOperationSessionSubmit, events.Event{Type: events.RequestFailed, Subject: "req-b"})
		c.shutdown()
		lost := keptLines(lines(), "is lost")
		if len(lost) != 2 || !strings.Contains(lost[0], "result req-a ") || !strings.Contains(lost[1], "result req-b ") {
			t.Fatalf("lost log lines = %q, want req-a's then req-b's", lost)
		}
		if recorded, _ := ev.List(events.Filter{}); len(recorded) != 0 {
			t.Fatalf("recorded events = %d, want 0", len(recorded))
		}
		select {
		case <-c.done:
		default:
			t.Fatal("shutdown left the close retries running")
		}
	})
	t.Run("the log takes them", func(t *testing.T) {
		c, srv, ev, lines := newKeepingCityOps(t)
		c.emit(srv, "req-a", RequestOperationSessionSubmit, events.Event{Type: events.RequestFailed, Subject: "req-a"})
		ev.closed.Store(false)
		c.shutdown()
		if lost := keptLines(lines(), "is lost"); len(lost) != 0 {
			t.Fatalf("lost log lines = %q, want none", lost)
		}
		if recorded, _ := ev.List(events.Filter{}); len(recorded) != 1 || recorded[0].Subject != "req-a" {
			t.Fatalf("recorded events = %+v, want req-a's", recorded)
		}
	})
}

// blockingEvents is a State's event provider whose first record waits for
// the test's release, as a slow exec script holds the Record that runs it.
type blockingEvents struct {
	*events.Fake
	entered, release chan struct{}
	once             sync.Once
}

func (b *blockingEvents) Record(e events.Event) {
	_ = b.RecordAck(e)
}

func (b *blockingEvents) RecordAck(e events.Event) error {
	first := false
	b.once.Do(func() {
		first = true
		close(b.entered)
	})
	if first {
		<-b.release
	}
	return b.Fake.RecordAck(e)
}

// A city's terminal events serialize on its event log's Record, in every
// ops.content mode: while one Record blocks, the city's next terminal event,
// and the answer to a same-key retry, which records the kept events first,
// wait for it; once it returns, both go through, the retry's event recorded,
// not kept, and the log holds the events in order.
func TestCityOpsSerializesTerminalEventsAcrossABlockedWrite(t *testing.T) {
	c := newCityOps()
	ev := &blockingEvents{Fake: events.NewFake(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(ev.release) }) }
	t.Cleanup(release)
	fs := newFakeState(t)
	fs.eventProv = ev
	srv := &Server{state: fs}
	c.publish(srv)

	first := make(chan struct{})
	go func() {
		defer close(first)
		c.emit(srv, "req-a", RequestOperationSessionSubmit, events.Event{Type: events.RequestFailed, Subject: "req-a"})
	}()
	select {
	case <-ev.entered:
	case <-time.After(testEventTimeout):
		t.Fatal("req-a's Record never ran")
	}
	next := make(chan struct{})
	go func() {
		defer close(next)
		c.emit(srv, "req-b", RequestOperationSessionSubmit, events.Event{Type: events.RequestFailed, Subject: "req-b"})
	}()
	answered := make(chan bool, 1)
	go func() { answered <- c.flushKeeps(srv, "req-a") }()
	select {
	case <-next:
		t.Fatal("req-b's terminal event went through while req-a's Record was blocked")
	case <-answered:
		t.Fatal("a same-key retry of req-a was answered while its Record was blocked")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	for _, w := range []struct {
		what string
		done <-chan struct{}
	}{{"req-a's terminal event", first}, {"req-b's terminal event", next}} {
		select {
		case <-w.done:
		case <-time.After(testEventTimeout):
			t.Fatalf("%s never went through once req-a's Record returned", w.what)
		}
	}
	select {
	case kept := <-answered:
		if kept {
			t.Fatal("the same-key retry found req-a's terminal event kept, want it recorded")
		}
	case <-time.After(testEventTimeout):
		t.Fatal("the same-key retry of req-a was never answered once its Record returned")
	}
	recorded, err := ev.List(events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range recorded {
		got = append(got, e.Subject)
	}
	if want := []string{"req-a", "req-b"}; !slices.Equal(got, want) {
		t.Fatalf("recorded %q, want %q", got, want)
	}
}

// §5.5 end to end: an outage of the content gate's store raises one event;
// a request that records again re-arms it, so the next outage, an interval
// later, raises another. The latch is per city per process: a rebuilt Server
// does not re-emit for the same outage.
func TestSessionContentOpsDegradeRearmsAfterARecordedRequest(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Rearm")
	clk := &clock.Fake{Time: time.Now()}
	city := e.sm.cityOpsFor(e.fs.CityName(), e.fs)
	city.degraded.clk = clk
	submit := func() {
		t.Helper()
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"x"}`).Body)
		waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
	}
	count := func() int {
		evs, _ := e.fs.eventProv.List(events.Filter{Type: events.OpsRecordingDegraded})
		return len(evs)
	}
	e.store.set(func(s *opsTestStore) { s.createErr = errors.New("transport: down") })
	submit()
	e.rebuild(t)
	clk.Time = clk.Time.Add(opsDegradeInterval)
	submit()
	if n := count(); n != 1 {
		t.Fatalf("one outage across a rebuild and an interval raised %d events, want 1", n)
	}
	e.store.set(func(s *opsTestStore) { s.createErr = nil })
	submit()
	e.store.set(func(s *opsTestStore) { s.createErr = errors.New("transport: down") })
	submit()
	if n := count(); n != 2 {
		t.Fatalf("an outage after a recorded request raised %d events in total, want 2", n)
	}
}

// §5.5: a close-stage degrade stays latched until a close succeeds, and
// re-arms after one, so the next close outage, an interval later, raises a
// fresh event.
func TestSessionContentOpsDegradeRearmsAfterAClose(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Close rearm")
	e.server(t) // publishes the city's live Server, whose provider takes its degrade events
	clk := &clock.Fake{Time: time.Now()}
	city := e.sm.cityOpsFor(e.fs.CityName(), e.fs)
	city.degraded.clk = clk
	closeFailure := legacyop.Degrade{Mode: gate.Auto, Surface: legacyop.SurfaceAPI, Stage: legacyop.StageClose, Reason: legacyop.DegradeStoreError}
	outage := func() {
		clk.Time = clk.Time.Add(opsDegradeInterval)
		city.degraded.Report(rollout.KeyOpsContent, closeFailure)
	}
	count := func() int {
		evs, _ := e.fs.eventProv.List(events.Filter{Type: events.OpsRecordingDegraded})
		return len(evs)
	}
	outage()
	outage()
	if n := count(); n != 1 {
		t.Fatalf("a close outage with no close in between raised %d events, want 1", n)
	}
	acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"x"}`).Body)
	waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1) // the close lands before the event (I10)
	outage()
	if n := count(); n != 2 {
		t.Fatalf("a close outage after a healthy close raised %d events in total, want 2", n)
	}
}

// §5.5: a close the background retry lands, after the request's event went
// out with its op open, re-arms the close stage too, so the next close
// outage, an interval later, raises a fresh event.
func TestSessionContentOpsDegradeRearmsAfterABackgroundClose(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Background close rearm")
	clk := &clock.Fake{Time: time.Now()}
	city := e.sm.cityOpsFor(e.fs.CityName(), e.fs)
	city.degraded.clk = clk
	outage := func() {
		t.Helper()
		e.store.set(func(s *opsTestStore) { s.updateFails = 4 }) // outlasts the foreground retries
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"x"}`).Body)
		waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
		e.waitClosed(t, acc.RequestID)
		// The retry reports the close before it releases the entry.
		waitFor(t, testEventTimeout, "the background close releases its entry", func() bool { return city.inflight.Len() == 0 })
	}
	count := func() int {
		evs, _ := e.fs.eventProv.List(events.Filter{Type: events.OpsRecordingDegraded})
		return len(evs)
	}
	outage()
	if n := count(); n != 1 {
		t.Fatalf("a close outage raised %d events, want 1", n)
	}
	clk.Time = clk.Time.Add(opsDegradeInterval)
	outage()
	if n := count(); n != 2 {
		t.Fatalf("a close outage after a background close raised %d events in total, want 2", n)
	}
}

// §5.5: an adopt-stage degrade re-arms after an adoption that succeeds.
func TestSessionContentOpsDegradeRearmsAfterAnAdoption(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Adopt rearm")
	e.server(t) // publishes the city's live Server, whose provider takes its degrade events
	clk := &clock.Fake{Time: time.Now()}
	city := e.sm.cityOpsFor(e.fs.CityName(), e.fs)
	city.degraded.clk = clk
	adoptFailure := legacyop.Degrade{Mode: gate.Auto, Surface: legacyop.SurfaceAPI, Stage: legacyop.StageAdopt, Reason: legacyop.ReasonStoreUnavailable}
	count := func() int {
		evs, _ := e.fs.eventProv.List(events.Filter{Type: events.OpsRecordingDegraded})
		return len(evs)
	}
	city.degraded.Report(rollout.KeyOpsContent, adoptFailure)
	orphan := e.seedOp(t, info.ID, "key-ad", "adopt me", legacyop.ExecutorAPI)
	acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-ad", `{"message":"adopt me"}`).Body)
	if acc.Op == nil || acc.Op.Attempt != 1 {
		t.Fatalf("setup: %+v op %+v, want an adoption", acc, acc.Op)
	}
	waitTerminalEvents(t, e.fs.eventProv, orphan.ID, 1)
	clk.Time = clk.Time.Add(opsDegradeInterval)
	city.degraded.Report(rollout.KeyOpsContent, adoptFailure)
	if n := count(); n != 2 {
		t.Fatalf("an adopt failure after a healthy adoption raised %d events in total, want 2", n)
	}
}

// After a city restart that brought a new event provider, the city's degrade
// events go to the newest State's provider: the latch is the city's, the
// destination the current Server's.
func TestSessionContentOpsDegradeEventsFollowTheNewState(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "New provider")
	clk := &clock.Fake{Time: time.Now()}
	e.sm.cityOpsFor(e.fs.CityName(), e.fs).degraded.clk = clk
	submit := func(ev events.Provider) {
		t.Helper()
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"x"}`).Body)
		waitTerminalEvents(t, ev, acc.RequestID, 1)
	}
	oldEv := e.fs.eventProv
	e.store.set(func(s *opsTestStore) { s.createErr = errors.New("transport: down") })
	submit(oldEv)
	newEv := events.NewFake()
	e.rebuildOver(t, e.store, newEv)
	e.store.set(func(s *opsTestStore) { s.createErr = nil })
	submit(newEv)
	clk.Time = clk.Time.Add(opsDegradeInterval)
	e.store.set(func(s *opsTestStore) { s.createErr = errors.New("transport: down") })
	submit(newEv)
	o, _ := oldEv.List(events.Filter{Type: events.OpsRecordingDegraded})
	n, _ := newEv.List(events.Filter{Type: events.OpsRecordingDegraded})
	if len(o) != 1 || len(n) != 1 {
		t.Fatalf("degrade events: old provider %d, new provider %d; want 1 and 1", len(o), len(n))
	}
}

// §5.5 with overlapping States: the supervisor builds a Server for a new
// State before it serves it, and a concurrent miss can build one it never
// serves. Building a Server takes none of the city's degrade events: they
// go to the served Server's provider until the supervisor serves the new
// State.
func TestSessionContentOpsDegradeEventsGoToTheServedServer(t *testing.T) {
	e := newOpsEnv(t, "auto")
	e.server(t)
	clk := &clock.Fake{Time: time.Now()}
	city := e.sm.cityOpsFor(e.fs.CityName(), e.fs)
	city.degraded.clk = clk
	count := func(ev events.Provider) int {
		evs, _ := ev.List(events.Filter{Type: events.OpsRecordingDegraded})
		return len(evs)
	}
	newEv := events.NewFake()
	next := &rebuiltOverState{fakeState: e.fs, store: e.store, ev: newEv}
	newCityServer(next, e.sm.readOnly, e.sm.cityOpsFor(e.fs.CityName(), next))
	d := legacyop.Degrade{Mode: gate.Auto, Surface: legacyop.SurfaceAPI, Stage: legacyop.StagePropose, Reason: legacyop.DegradeStoreError}
	city.degraded.Report(rollout.KeyOpsContent, d)
	if o, n := count(e.fs.eventProv), count(newEv); o != 1 || n != 0 {
		t.Fatalf("with an unserved Server built: served provider %d, unserved %d; want 1 and 0", o, n)
	}
	e.resolver.state = next
	e.server(t)
	city.degraded.Healthy(rollout.KeyOpsContent, legacyop.SurfaceAPI, legacyop.StagePropose)
	clk.Time = clk.Time.Add(opsDegradeInterval)
	city.degraded.Report(rollout.KeyOpsContent, d)
	if o, n := count(e.fs.eventProv), count(newEv); o != 1 || n != 1 {
		t.Fatalf("once the new State is served: old provider %d, new %d; want 1 and 1", o, n)
	}
}

// closeCheckingEvents is an event provider that checks, as each terminal
// event is recorded, that the op it names is already closed (I10).
type closeCheckingEvents struct {
	events.Provider
	st      func() *opjournal.Store
	mu      sync.Mutex
	open    []string
	checked atomic.Int32
}

func (c *closeCheckingEvents) Record(e events.Event) {
	switch e.Type {
	case events.RequestResultSessionSubmit, events.RequestResultSessionMessage, events.RequestFailed:
		var p struct {
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && strings.HasPrefix(p.RequestID, "gc-op-") {
			c.checked.Add(1)
			if op, err := c.st().Get(p.RequestID); err != nil || op.Status != opjournal.StatusClosed {
				c.mu.Lock()
				c.open = append(c.open, p.RequestID)
				c.mu.Unlock()
			}
		}
	}
	c.Provider.Record(e)
}

// panickingProvider panics on delivery while armed.
type panickingProvider struct {
	*runtime.Fake
	armed atomic.Bool
}

func (p *panickingProvider) Nudge(name string, content []runtime.ContentBlock) error {
	if p.armed.Load() {
		panic("injected delivery panic")
	}
	return p.Fake.Nudge(name, content)
}

func (p *panickingProvider) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	if p.armed.Load() {
		panic("injected delivery panic")
	}
	return p.Fake.NudgeFenced(ctx, name, content, n)
}

// The durable-202 property (DES-O0API §15): over every combination of mode,
// fault, keyed or minted key and endpoint — Propose errors, a create that
// commits then errors, a close that fails within the foreground retries and
// one that outlasts them, a crash after the commit, a delivery panic — every
// answer keeps I1 (require: a 2xx is preceded by a committed op whose id is
// the request_id), I2 (each op ends with exactly one outcome), I4
// (request_id is op.id, and a same-key retry gets it again) and I10 (the op
// closes before its terminal event, unless the close outlasts its foreground
// retries: then the event goes out at once with the op still open, in every
// mode).
func TestSessionContentOpsDurable202Property(t *testing.T) {
	faults := []string{"none", "propose_error", "ambiguous", "close_error", "close_error_background", "crash_after_commit", "delivery_panic"}
	i := 0
	for _, mode := range []string{"auto", "require"} {
		for _, fault := range faults {
			for _, keyed := range []bool{false, true} {
				for _, endpoint := range []string{"/submit", "/messages"} {
					i++
					runDurable202Case(t, i, mode, fault, keyed, endpoint)
				}
			}
		}
	}
}

// runDurable202Case is one cell of the durable-202 property; i keys it.
func runDurable202Case(t *testing.T, i int, mode, fault string, keyed bool, endpoint string) {
	t.Helper()
	t.Run(fmt.Sprintf("%s_%s_keyed=%v_%s", mode, fault, keyed, strings.TrimPrefix(endpoint, "/")), func(t *testing.T) {
		e := newOpsEnv(t, mode)
		checker := &closeCheckingEvents{Provider: e.fs.eventProv, st: func() *opjournal.Store { return e.opStore(t) }}
		e.fs.eventProv = checker
		pp := &panickingProvider{Fake: e.fs.sp}
		e.fs.sessionProvider = pp
		info := e.session(t, "Property")
		key := ""
		if keyed {
			key = fmt.Sprintf("prop-%d", i)
		}
		switch fault {
		case "propose_error":
			e.store.set(func(s *opsTestStore) { s.createErr = errors.New("transport: down") })
		case "ambiguous":
			e.store.set(func(s *opsTestStore) { s.ambiguous = true })
		case "close_error":
			e.store.set(func(s *opsTestStore) { s.updateFails = 2 }) // within the 3 foreground tries
		case "close_error_background":
			e.store.set(func(s *opsTestStore) { s.updateFails = 5 }) // outlasts them
		case "crash_after_commit":
			e.store.set(func(s *opsTestStore) { s.panicAfterCreate = true })
		case "delivery_panic":
			pp.armed.Store(true)
		}
		path := "/session/" + info.ID + endpoint
		post := func() (rec *httptest.ResponseRecorder, crashed bool) {
			defer func() {
				if recover() != nil {
					crashed = true
				}
			}()
			return e.post(t, path, key, `{"message":"property"}`), false
		}
		rec, crashed := post()
		if crashed || rec.Code >= 500 && fault == "crash_after_commit" {
			if !keyed {
				return // a minted key is lost with the crash; the sweeper owns the op
			}
			rec, crashed = post()
			if crashed {
				t.Fatal("the retry crashed too")
			}
		}
		var acc asyncAcceptedBody
		if rec.Code == http.StatusAccepted {
			acc = decodeAsyncAccepted(t, rec.Body)
			if acc.Op != nil && acc.Op.ID != acc.RequestID {
				t.Fatalf("I4: request_id %s, op %s", acc.RequestID, acc.Op.ID)
			}
			if mode == "require" {
				if acc.Op == nil {
					t.Fatalf("I1: require answered 202 without an op: %+v", acc)
				}
				if _, err := e.opStore(t).Get(acc.RequestID); err != nil {
					t.Fatalf("I1: the 202's op %s is not committed: %v", acc.RequestID, err)
				}
			}
			waitTerminalEvents(t, checker, acc.RequestID, 1)
		} else if mode != "require" || fault != "propose_error" {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		pp.armed.Store(false)
		for _, row := range e.opRows(t) {
			op := e.waitClosed(t, row.ID)
			closes := 0
			for _, tr := range op.Transitions {
				if tr.Status == opjournal.StatusClosed {
					closes++
				}
			}
			if closes != 1 || !op.Outcome.Arm.Valid() {
				t.Fatalf("I2: op %s closed %d times: %+v", op.ID, closes, op.Outcome)
			}
		}
		if acc.Op != nil && keyed {
			again := e.post(t, path, key, `{"message":"property"}`)
			if got := decodeAsyncAccepted(t, again.Body); again.Code != http.StatusAccepted || got.RequestID != acc.RequestID {
				t.Fatalf("I4: the same-key retry got %d %+v, want request_id %s", again.Code, got, acc.RequestID)
			}
		}
		if acc.Op != nil && checker.checked.Load() == 0 {
			t.Fatal("I10 was never checked: no terminal event named the op")
		}
		if fault == "delivery_panic" && acc.Op != nil {
			if op := e.waitClosed(t, acc.RequestID); op.Outcome.Arm != opjournal.ArmFailed || op.Outcome.Reason != opjournal.ReasonOther {
				t.Fatalf("a panicked delivery closed %+v, want failed{other}", op.Outcome)
			}
			if f := waitForRequestFailed(t, checker, acc.RequestID); f.ErrorCode != "internal_error" && f.ErrorCode != "message_failed" {
				t.Fatalf("a panicked delivery reported %+v", f)
			}
		}
		checker.mu.Lock()
		open := slices.Clone(checker.open)
		checker.mu.Unlock()
		if len(open) > 0 && fault != "close_error_background" {
			t.Fatalf("I10: terminal events emitted before their op closed: %v", open)
		}
		if fault == "close_error_background" && acc.Op != nil && len(open) == 0 {
			t.Fatal("the terminal event waited for a background close")
		}
	})
}

// §6.5: SubmitSessionWithKey sends the key; SubmitSession sends none; a
// replay of a finished request is answered from the 202 without opening the
// event stream.
func TestClientSubmitSessionWithKeyAndReplays(t *testing.T) {
	queued := legacyop.ViaLegacyQueued
	gone := opjournal.ReasonSessionGone
	for _, tc := range []struct {
		name    string
		key     string
		answer  map[string]any
		stream  bool
		wantErr string
		queued  bool
		replay  string
		opID    string
	}{
		{name: "no key", answer: map[string]any{"status": "accepted", "request_id": "req-1", "event_cursor": "3"}, stream: true},
		{name: "keyed, live", key: "key-1", opID: "gc-op-a", answer: map[string]any{
			"status": "accepted", "request_id": "gc-op-a", "event_cursor": "3",
			"idempotency_key": "key-1", "op": map[string]any{"id": "gc-op-a", "conversation": "sess-1", "kind": "message", "status": "submitted"},
		}, stream: true},
		{name: "keyed, replayed success", key: "key-2", queued: true, opID: "gc-op-b", replay: "replayed: operation gc-op-b already closed succeeded; " + replayOptionsNote, answer: map[string]any{
			"status": "accepted", "request_id": "gc-op-b", "event_cursor": "3",
			"replayed": true, "op": map[string]any{
				"id": "gc-op-b", "conversation": "sess-1", "kind": "message", "status": "closed",
				"outcome": map[string]any{"arm": "succeeded", "via": queued},
			},
		}},
		{
			name: "keyed, replayed failure", key: "key-3",
			wantErr: "submit failed: resolve_failed: replayed: operation gc-op-c already closed failed{session_gone}; " + replayOptionsNote,
			answer: map[string]any{
				"status": "accepted", "request_id": "gc-op-c", "event_cursor": "3", "replayed": true,
				"op": map[string]any{
					"id": "gc-op-c", "conversation": "sess-1", "kind": "message", "status": "closed",
					"outcome": map[string]any{"arm": "failed", "reason": gone},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotKey atomic.Value
			gotKey.Store("")
			var streamed atomic.Bool
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v0/city/alpha/session/sess-1/submit":
					gotKey.Store(r.Header.Get("Idempotency-Key"))
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					json.NewEncoder(w).Encode(tc.answer) //nolint:errcheck
				case r.Method == http.MethodGet && r.URL.Path == "/v0/city/alpha/events/stream":
					streamed.Store(true)
					writeSSEEnvelope(t, w, events.RequestResultSessionSubmit, SessionSubmitSucceededPayload{
						RequestID: tc.answer["request_id"].(string), SessionID: "sess-1", Intent: "default",
					})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
				}
			}))
			defer ts.Close()
			c := NewCityScopedClient(ts.URL, "alpha")
			var (
				resp SessionSubmitResponse
				err  error
			)
			if tc.key == "" {
				resp, err = c.SubmitSession("sess-1", "hi", "")
			} else {
				resp, err = c.SubmitSessionWithKey("sess-1", "hi", "", tc.key)
			}
			if gotKey.Load().(string) != tc.key {
				t.Fatalf("Idempotency-Key = %q, want %q", gotKey.Load(), tc.key)
			}
			if streamed.Load() != tc.stream {
				t.Fatalf("opened the stream = %v, want %v", streamed.Load(), tc.stream)
			}
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || resp.ID != "sess-1" || resp.Queued != tc.queued || resp.Intent != session.SubmitIntentDefault {
				t.Fatalf("resp = %+v, %v", resp, err)
			}
			if resp.Replayed != (tc.replay != "") || resp.Message != tc.replay {
				t.Fatalf("resp replayed %v with message %q, want %q", resp.Replayed, resp.Message, tc.replay)
			}
			if resp.OpID != tc.opID {
				t.Fatalf("resp op id = %q, want %q", resp.OpID, tc.opID)
			}
		})
	}
}

// End to end over a real server in auto: the first keyed submit waits for
// its event; after a restart the same key replays without a second delivery,
// and the re-emitted event is the one a Go client waiting from the replay's
// cursor matches.
func TestClientSubmitSessionWithKeyReplaysAcrossARestart(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Client")
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	c := NewCityScopedClient(srv.URL, e.fs.CityName())
	first, err := c.SubmitSessionWithKey(info.ID, "via client", session.SubmitIntentFollowUp, "client-key")
	if err != nil || !first.Queued || first.ID != info.ID || first.Replayed {
		t.Fatalf("first = %+v, %v", first, err)
	}

	e.restart(t)
	srv2 := httptest.NewServer(e.h)
	defer srv2.Close()
	c2 := NewCityScopedClient(srv2.URL, e.fs.CityName())
	replayed, err := c2.SubmitSessionWithKey(info.ID, "via client", session.SubmitIntentFollowUp, "client-key")
	if err != nil || !replayed.Queued || replayed.ID != info.ID || replayed.Intent != session.SubmitIntentFollowUp ||
		!replayed.Replayed || !strings.HasSuffix(replayed.Message, replayOptionsNote) {
		t.Fatalf("replay = %+v, %v", replayed, err)
	}
	if got := queuedSubmitCount(t, e.fs.cityPath, info.ID); got != 1 {
		t.Fatalf("queued submits = %d, want 1", got)
	}

	rec := e.post(t, "/session/"+info.ID+"/submit", "client-key", `{"message":"via client","intent":"follow_up","resume":true}`)
	acc := decodeAsyncAccepted(t, rec.Body)
	ctx, cancel := context.WithTimeout(context.Background(), testEventTimeout)
	defer cancel()
	env, err := c2.waitForEvent(ctx, acc.RequestID, events.RequestResultSessionSubmit, RequestOperationSessionSubmit, acc.EventCursor)
	if err != nil || env.Type != events.RequestResultSessionSubmit {
		t.Fatalf("waiting from the replay's cursor: %+v, %v", env, err)
	}
}

// sessionRowsAndStarts counts the city's session rows and runtime starts.
func (e *opsEnv) sessionRowsAndStarts(t *testing.T) (rows, starts int) {
	t.Helper()
	all, err := e.store.List(beads.ListQuery{Type: session.BeadType, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range e.fs.sp.SnapshotCalls() {
		if c.Method == "Start" {
			starts++
		}
	}
	return len(all), starts
}

// D12 and §6.4: in require, a request to a configured named session with no
// row is refused before the session is materialized when it cannot be
// recorded whatever its conversation: a payload over the limit, an invalid
// key, an op store that does not open. No session row is written and no
// runtime starts.
func TestSessionContentOpsRequireRefusesBeforeMaterializing(t *testing.T) {
	for _, tc := range []struct {
		name, key, body string
		setup           func(*testing.T, *opsEnv)
		status          int
		code            string
	}{
		{
			name: "payload too large", body: `{"message":"` + strings.Repeat("y", opjournal.MaxInlinePayload+1) + `"}`,
			status: http.StatusUnprocessableEntity, code: "payload_too_large",
		},
		{
			name: "invalid key", key: strings.Repeat("k", 513), body: `{"message":"wake up"}`,
			status: http.StatusUnprocessableEntity, code: "invalid_idempotency_key",
		},
		{name: "store does not open", body: `{"message":"wake up"}`, setup: func(t *testing.T, e *opsEnv) {
			e.server(t).ops.openStore = func(beads.SessionStore, clock.Clock) (*opjournal.Store, error) {
				return nil, errors.New("transport: down")
			}
		}, status: http.StatusServiceUnavailable, code: "op_journal_unavailable"},
	} {
		for _, path := range []string{"/submit", "/messages"} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				e := newOpsEnv(t, "require")
				if tc.setup != nil {
					tc.setup(t, e)
				}
				rowsBefore, startsBefore := e.sessionRowsAndStarts(t)
				rec := e.post(t, "/session/"+url.PathEscape("myrig/worker")+path, tc.key, tc.body)
				if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
					t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
				rows, starts := e.sessionRowsAndStarts(t)
				if rows != rowsBefore || starts != startsBefore {
					t.Fatalf("refused after materializing: session rows %d -> %d, starts %d -> %d", rowsBefore, rows, startsBefore, starts)
				}
				if n := len(e.opRows(t)); n != 0 {
					t.Fatalf("op rows = %d", n)
				}
			})
		}
	}
}

// D12: require refuses an Idempotency-Key the journal cannot record under
// with 422 invalid_idempotency_key and delivers nothing; auto runs the same
// request unrecorded.
func TestSessionContentOpsRequireRefusesAnInvalidKey(t *testing.T) {
	key := strings.Repeat("k", 513)
	for _, path := range []string{"/submit", "/messages"} {
		t.Run(path, func(t *testing.T) {
			e := newOpsEnv(t, "require")
			info := e.session(t, "Bad key")
			rec := e.post(t, "/session/"+info.ID+path, key, `{"message":"x"}`)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid_idempotency_key") {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			time.Sleep(20 * time.Millisecond)
			if n := len(deliveredCalls(e.fs.sp, info.SessionName)); n != 0 || len(e.opRows(t)) != 0 {
				t.Fatalf("require delivered %d times, wrote %d op rows", n, len(e.opRows(t)))
			}

			auto := newOpsEnv(t, "auto")
			info = auto.session(t, "Bad key")
			acc := decodeAsyncAccepted(t, auto.post(t, "/session/"+info.ID+path, key, `{"message":"x"}`).Body)
			if acc.Op != nil || !strings.HasPrefix(acc.RequestID, "req-") {
				t.Fatalf("auto = %+v, want an unrecorded request", acc)
			}
			waitTerminalEvents(t, auto.fs.eventProv, acc.RequestID, 1)
		})
	}
}

// driftEnv is a city where the alias "drifty" names session a until the
// request's op row commits, then names session b: the target moves between
// the 202 and the delivery.
func driftEnv(t *testing.T, mode string) (e *opsEnv, a, b session.Info, logs func() []string, moved func() int) {
	t.Helper()
	e = newOpsEnv(t, mode)
	a, b = e.session(t, "Drift A"), e.session(t, "Drift B")
	if err := e.store.SetMetadata(a.ID, "alias", "drifty"); err != nil {
		t.Fatal(err)
	}
	e.store.set(func(s *opsTestStore) {
		s.onOpCreate = func() {
			if err := e.store.SetMetadata(a.ID, "alias", ""); err != nil {
				t.Error(err)
			}
			if err := e.store.SetMetadata(b.ID, "alias", "drifty"); err != nil {
				t.Error(err)
			}
		}
	})
	var mu sync.Mutex
	var lines []string
	e.server(t).ops.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	var counted atomic.Int32
	e.server(t).ops.recordOp = func(_ context.Context, kind, surface, stage, result, _ string) {
		if kind == string(opjournal.KindMessage) && surface == legacyop.SurfaceAPI && stage == legacyop.StageClose && result == opTargetMovedResult {
			counted.Add(1)
		}
	}
	return e, a, b, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(lines)
		}, func() int {
			return int(counted.Load())
		}
}

// §6.1 target drift: in auto the delivery re-resolves the target, as it
// always has, so it reaches the session the alias names now; the op, which
// names the session the 202 resolved, closes from that result and the drift
// is logged and counted; a replay's event names the op's session. In require
// the delivery goes to the op's conversation: the op is the request.
func TestSessionContentOpsTargetDrift(t *testing.T) {
	for _, path := range []string{"/submit", "/messages"} {
		t.Run("auto "+path, func(t *testing.T) {
			e, a, b, logs, moved := driftEnv(t, "auto")
			acc := decodeAsyncAccepted(t, e.post(t, "/session/drifty"+path, "key-d", `{"message":"moved"}`).Body)
			if acc.Op == nil || acc.Op.Conversation != a.ID {
				t.Fatalf("202 = %+v op %+v, want an op for %s", acc, acc.Op, a.ID)
			}
			waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
			if op := e.waitClosed(t, acc.RequestID); op.Outcome.Arm != opjournal.ArmSucceeded {
				t.Fatalf("op = %+v", op)
			}
			if na, nb := len(deliveredCalls(e.fs.sp, a.SessionName)), len(deliveredCalls(e.fs.sp, b.SessionName)); na != 0 || nb != 1 {
				t.Fatalf("delivered to a %d, b %d; want b once", na, nb)
			}
			got := logs()
			if len(got) != 1 || !strings.Contains(got[0], acc.RequestID) || !strings.Contains(got[0], b.ID) {
				t.Fatalf("drift log = %q", got)
			}
			if n := moved(); n != 1 {
				t.Fatalf("%s counted %d times, want 1", opTargetMovedResult, n)
			}
			// request_id rule 4: the executor's event names the session that
			// got the message, a replay's the op's.
			replay := decodeAsyncAccepted(t, e.post(t, "/session/"+a.ID+path, "key-d", `{"message":"moved"}`).Body)
			if replay.RequestID != acc.RequestID || !replay.Replayed {
				t.Fatalf("replay = %+v", replay)
			}
			evs := waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 2)
			for i, want := range []string{b.ID, a.ID} {
				var p struct {
					SessionID string `json:"session_id"`
				}
				if err := json.Unmarshal(evs[i].Payload, &p); err != nil || p.SessionID != want || evs[i].Subject != want {
					t.Fatalf("event %d names %q (subject %q), want %s (%v)", i, p.SessionID, evs[i].Subject, want, err)
				}
			}
		})
		t.Run("require "+path, func(t *testing.T) {
			e, a, b, logs, moved := driftEnv(t, "require")
			acc := decodeAsyncAccepted(t, e.post(t, "/session/drifty"+path, "key-d", `{"message":"moved"}`).Body)
			if acc.Op == nil || acc.Op.Conversation != a.ID {
				t.Fatalf("202 = %+v op %+v, want an op for %s", acc, acc.Op, a.ID)
			}
			waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
			e.waitClosed(t, acc.RequestID)
			if na, nb := len(deliveredCalls(e.fs.sp, a.SessionName)), len(deliveredCalls(e.fs.sp, b.SessionName)); na != 1 || nb != 0 {
				t.Fatalf("delivered to a %d, b %d; want a once", na, nb)
			}
			if got := logs(); len(got) != 0 {
				t.Fatalf("require logged drift: %q", got)
			}
			if n := moved(); n != 0 {
				t.Fatalf("require counted %s %d times", opTargetMovedResult, n)
			}
		})
	}
}

// §15 handler crash after the op commits: a panic after admission and
// before the delivery goroutine owns the request leaves the op open and out
// of the in-flight registry, so the same-key retry adopts it (attempt 1) and
// delivers once.
func TestSessionContentOpsHandlerPanicAfterAdmitLeavesTheOpForAdoption(t *testing.T) {
	for _, path := range []string{"/submit", "/messages"} {
		t.Run(path, func(t *testing.T) {
			e := newOpsEnv(t, "auto")
			info := e.session(t, "Handler panic")
			var armed atomic.Bool
			armed.Store(true)
			e.server(t).ops.afterAdmit = func() {
				if armed.CompareAndSwap(true, false) {
					panic("injected handler panic")
				}
			}
			func() {
				defer func() { _ = recover() }()
				if rec := e.post(t, "/session/"+info.ID+path, "key-h", `{"message":"after panic"}`); rec.Code < 500 {
					t.Fatalf("the panicked request answered %d: %s", rec.Code, rec.Body.String())
				}
			}()
			rows := e.opRows(t)
			if len(rows) != 1 {
				t.Fatalf("op rows = %d, want 1", len(rows))
			}
			op, err := e.opStore(t).Get(rows[0].ID)
			if err != nil || op.Status == opjournal.StatusClosed || op.Attempt != 0 {
				t.Fatalf("op after the panic = %+v (%v), want open at attempt 0", op, err)
			}
			if n := e.sm.cityOpsFor(e.fs.CityName(), e.fs).inflight.Len(); n != 0 {
				t.Fatalf("in-flight entries after the panic = %d", n)
			}
			if n := len(deliveredCalls(e.fs.sp, info.SessionName)); n != 0 {
				t.Fatalf("the panicked request delivered %d times", n)
			}
			rec := e.post(t, "/session/"+info.ID+path, "key-h", `{"message":"after panic"}`)
			acc := decodeAsyncAccepted(t, rec.Body)
			if rec.Code != http.StatusAccepted || acc.RequestID != op.ID || acc.Op == nil || acc.Op.Attempt != 1 {
				t.Fatalf("retry = %d %+v op %+v", rec.Code, acc, acc.Op)
			}
			waitTerminalEvents(t, e.fs.eventProv, op.ID, 1)
			if closed := e.waitClosed(t, op.ID); closed.Outcome.Arm != opjournal.ArmSucceeded {
				t.Fatalf("op = %+v", closed)
			}
			if n := len(deliveredCalls(e.fs.sp, info.SessionName)); n != 1 {
				t.Fatalf("deliveries = %d, want 1", n)
			}
		})
	}
}

// An interrupt_now the runtime accepted without fencing it to the turn
// closes its op with the unfenced_interrupt degradation (DES-T3I §14 item 2).
func TestSessionContentOpsUnfencedInterruptReachesTheOp(t *testing.T) {
	e := newOpsEnv(t, "auto")
	e.fs.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	e.fs.sessionProvider = runtime.FullFake{Fake: e.fs.sp}
	mgr := session.NewManagerWithOptions(e.fs.cityBeadStore, e.fs.sessionProvider, session.WithCityPath(e.fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "helper", Title: "Unfenced", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	e.fs.sp.TurnScripts = map[string][]string{info.SessionName: {"t1", "t1", ""}}
	acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"now","intent":"interrupt_now"}`).Body)
	success, failure := waitForSessionSubmitResult(t, e.fs.eventProv, acc.RequestID)
	if success == nil {
		t.Fatalf("submit failed: %+v", failure)
	}
	if success.Interrupt == nil || success.Interrupt.Degradation != session.DegradationUnfencedInterrupt {
		t.Fatalf("interrupt = %+v, want unfenced", success.Interrupt)
	}
	if op := e.waitClosed(t, acc.RequestID); op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Degradation != session.DegradationUnfencedInterrupt {
		t.Fatalf("op outcome = %+v, want succeeded with unfenced_interrupt", op.Outcome)
	}
}

// closes counts an op's transitions to closed.
func closes(op opjournal.Op) int {
	n := 0
	for _, tr := range op.Transitions {
		if tr.Status == opjournal.StatusClosed {
			n++
		}
	}
	return n
}

// §6.3 for /messages under auto: a message queued on a held session closes
// succeeded{via: legacy.queued}, never legacy.delivered (D2); the 4-minute
// timeout and a delivery canceled by it close failed{deadline_in_flight}
// with error_code timeout; each op closes exactly once and a late result
// after the timeout changes nothing.
func TestSessionContentOpsMessagesOutcomes(t *testing.T) {
	t.Run("queued", func(t *testing.T) {
		e := newOpsEnv(t, "auto")
		info := e.session(t, "Held")
		if err := session.NewManagerWithOptions(e.fs.cityBeadStore, e.fs.sp, session.WithCityPath(e.fs.cityPath)).Suspend(context.Background(), testOperatorIn(t, e.fs.cityPath), info.ID, false); err != nil {
			t.Fatal(err)
		}
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/messages", "", `{"message":"later"}`).Body)
		success, failure := waitForSessionMessageResult(t, e.fs.eventProv, acc.RequestID)
		if success == nil || !success.Queued || success.Op == nil || success.Op.Outcome == nil || success.Op.Outcome.Via != legacyop.ViaLegacyQueued {
			t.Fatalf("result = %+v (failure %+v), want queued via legacy.queued", success, failure)
		}
		if op := e.waitClosed(t, acc.RequestID); op.Outcome.Result.Via != legacyop.ViaLegacyQueued || closes(op) != 1 {
			t.Fatalf("op = %+v closed %d times", op.Outcome, closes(op))
		}
	})
	t.Run("timeout", func(t *testing.T) {
		prev := sessionMessageAsyncTimeout
		sessionMessageAsyncTimeout = 50 * time.Millisecond
		t.Cleanup(func() { sessionMessageAsyncTimeout = prev })
		e := newOpsEnv(t, "auto")
		info := e.session(t, "Slow")
		gp, release := e.gatedProvider(t)
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/messages", "", `{"message":"slow"}`).Body)
		gp.waitEntered(t)
		failure := waitForRequestFailed(t, e.fs.eventProv, acc.RequestID)
		if failure.ErrorCode != "timeout" || failure.Op == nil || failure.Op.Outcome == nil || failure.Op.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
			t.Fatalf("failure = %+v op %+v", failure, failure.Op)
		}
		release()
		time.Sleep(50 * time.Millisecond)
		op := e.waitClosed(t, acc.RequestID)
		if op.Outcome.Arm != opjournal.ArmFailed || op.Outcome.Reason != opjournal.ReasonDeadlineInFlight || closes(op) != 1 {
			t.Fatalf("op = %+v closed %d times", op.Outcome, closes(op))
		}
		if n := len(terminalEvents(t, e.fs.eventProv, acc.RequestID)); n != 1 {
			t.Fatalf("terminal events = %d, want 1", n)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		e := newOpsEnv(t, "auto")
		info := e.session(t, "Canceled")
		e.fs.sp.NudgeErrors = map[string]error{info.SessionName: fmt.Errorf("nudge: %w", context.Canceled)}
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/messages", "", `{"message":"x"}`).Body)
		failure := waitForRequestFailed(t, e.fs.eventProv, acc.RequestID)
		if failure.ErrorCode != "timeout" {
			t.Fatalf("failure = %+v, want timeout", failure)
		}
		if op := e.waitClosed(t, acc.RequestID); op.Outcome.Reason != opjournal.ReasonDeadlineInFlight || closes(op) != 1 {
			t.Fatalf("op = %+v closed %d times", op.Outcome, closes(op))
		}
	})
}

// lockedBuffer is a buffer the standard logger can write to while a test
// reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureStandardLog redirects the standard logger to a buffer until the test
// ends.
func captureStandardLog(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return logs
}

// I3, §6.3 for /submit: a recorded delivery still running at
// sessionSubmitAsyncTimeout, inside its op's finish_by, closes the op
// failed{deadline_in_flight} and emits request.failed{timeout} once, and its
// late result is only logged. An unrecorded /submit, as in off, has no op to
// close and is not bounded.
func TestSessionContentOpsSubmitTimeout(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		prev := sessionSubmitAsyncTimeout
		sessionSubmitAsyncTimeout = 50 * time.Millisecond
		t.Cleanup(func() { sessionSubmitAsyncTimeout = prev })
		logs := captureStandardLog(t)
		e := newOpsEnv(t, "auto")
		info := e.session(t, "Slow")
		gp, release := e.gatedProvider(t)
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"slow"}`).Body)
		gp.waitEntered(t)
		failure := waitForRequestFailed(t, e.fs.eventProv, acc.RequestID)
		if failure.Operation != RequestOperationSessionSubmit || failure.ErrorCode != "timeout" || failure.Op == nil || failure.Op.Outcome == nil || failure.Op.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
			t.Fatalf("failure = %+v op %+v, want a session.submit timeout carrying the op failed{deadline_in_flight}", failure, failure.Op)
		}
		release()
		late := "late session.submit result after timeout request_id=" + acc.RequestID
		waitFor(t, testEventTimeout, "the late result is logged", func() bool { return strings.Contains(logs.String(), late) })
		op := e.waitClosed(t, acc.RequestID)
		if op.Outcome.Arm != opjournal.ArmFailed || op.Outcome.Reason != opjournal.ReasonDeadlineInFlight || closes(op) != 1 {
			t.Fatalf("op = %+v closed %d times", op.Outcome, closes(op))
		}
		if n := len(terminalEvents(t, e.fs.eventProv, acc.RequestID)); n != 1 {
			t.Fatalf("terminal events = %d, want 1", n)
		}
	})
	t.Run("unrecorded", func(t *testing.T) {
		prev := sessionSubmitAsyncTimeout
		sessionSubmitAsyncTimeout = time.Nanosecond
		t.Cleanup(func() { sessionSubmitAsyncTimeout = prev })
		e := newOpsEnv(t, "off")
		info := e.session(t, "Unbounded")
		gp, release := e.gatedProvider(t)
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "", `{"message":"slow"}`).Body)
		gp.waitEntered(t)
		release()
		if success, failure := waitForSessionSubmitResult(t, e.fs.eventProv, acc.RequestID); success == nil {
			t.Fatalf("unrecorded submit = %+v, want it delivered however long it takes", failure)
		}
		if n := len(terminalEvents(t, e.fs.eventProv, acc.RequestID)); n != 1 {
			t.Fatalf("terminal events = %d, want 1", n)
		}
	})
}

// A recorded delivery's own timeout ends at least a minute inside its op's
// finish_by, ExecutionBound after admission or adoption, so the op closes
// from the delivery's result before the sweeper, or a same-key retry, could
// close it expired.
func TestMessageDeliveryTimeoutsEndInsideFinishBy(t *testing.T) {
	for endpoint, timeout := range map[string]time.Duration{
		"/messages": sessionMessageAsyncTimeout,
		"/submit":   sessionSubmitAsyncTimeout,
	} {
		if timeout <= 0 || legacyop.ExecutionBound-timeout < time.Minute {
			t.Errorf("%s delivery timeout = %s, want it positive and at least a minute inside ExecutionBound %s", endpoint, timeout, legacyop.ExecutionBound)
		}
	}
}

// newT3OpsEnv is an ops env whose sessions run on a t3bridge-profiled fake,
// with one session created on it.
func newT3OpsEnv(t *testing.T, mode string) (*opsEnv, session.Info) {
	t.Helper()
	e := newOpsEnv(t, mode)
	e.fs.sessionProvider = runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: e.fs.sp})
	mgr := session.NewManagerWithOptions(e.fs.cityBeadStore, e.fs.sessionProvider, session.WithCityPath(e.fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "helper", Title: "T3", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	return e, info
}

// callsOf are sp's calls of any of methods to name.
func callsOf(sp *runtime.Fake, name string, methods ...string) []runtime.Call {
	var out []runtime.Call
	for _, c := range sp.SnapshotCalls() {
		if c.Name == name && slices.Contains(methods, c.Method) {
			out = append(out, c)
		}
	}
	return out
}

// opDeliveryKey is the runtime delivery key of legacy op id.
func opDeliveryKey(id string) string { return "gc:" + id + ":1" }

// assertOneKeyedDelivery asserts the one runtime delivery to info carries
// key exactly and text with neither the op id nor the client key (I11).
func assertOneKeyedDelivery(t *testing.T, e *opsEnv, info session.Info, key, text string, ids ...string) {
	t.Helper()
	calls := deliveredCalls(e.fs.sp, info.SessionName)
	if len(calls) != 1 || calls[0].DeliveryKey != key || calls[0].Message != text {
		t.Fatalf("deliveries = %+v, want one of %q under key %q", calls, text, key)
	}
	assertNoOpIDs(t, calls, ids...)
}

// Kills a first execution sent unkeyed (M35): every recorded /submit and
// /messages delivery goes to T3 under gc:<op id>:1, and off or an
// unrecorded request goes unkeyed as before.
func TestSessionContentOpsKeyTheirT3Delivery(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		for _, path := range []string{"/submit", "/messages"} {
			t.Run(mode+path, func(t *testing.T) {
				e, info := newT3OpsEnv(t, mode)
				acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+path, "k1", `{"message":"hello"}`).Body)
				if acc.Op == nil {
					t.Fatalf("accepted = %+v, want a recorded op", acc)
				}
				waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
				e.waitClosed(t, acc.RequestID)
				assertOneKeyedDelivery(t, e, info, opDeliveryKey(acc.RequestID), "hello", acc.RequestID, "k1")
			})
		}
	}
	t.Run("off", func(t *testing.T) {
		e, info := newT3OpsEnv(t, "off")
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k1", `{"message":"hello"}`).Body)
		waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
		assertOneKeyedDelivery(t, e, info, "", "hello")
	})
	t.Run("unrecorded auto", func(t *testing.T) {
		e, info := newT3OpsEnv(t, "auto")
		e.store.set(func(s *opsTestStore) { s.createErr = errors.New("transport: down") })
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k1", `{"message":"hello"}`).Body)
		if acc.Op != nil {
			t.Fatalf("accepted = %+v, want unrecorded", acc)
		}
		waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
		assertOneKeyedDelivery(t, e, info, "", "hello")
	})
}

// seedOpWith is seedOp for a request of intent.
func (e *opsEnv) seedOpWith(t *testing.T, conv, key, text string, intent session.SubmitIntent) opjournal.Op {
	t.Helper()
	st := e.opStore(t)
	now := time.Now()
	op, created, err := st.Propose(opjournal.Request{
		Conversation: conv, Kind: opjournal.KindMessage, Executor: legacyop.ExecutorAPI, ClientKey: key,
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  text, Policy: MessageOpPolicy(intent),
		StartBy: now.Add(legacyop.ExecutionBound), FinishBy: now.Add(legacyop.ExecutionBound),
	})
	if err != nil || !created {
		t.Fatalf("seed: %v created %v", err, created)
	}
	return op
}

// Kills an adoption without Redelivery (M36) or under another key (M19 at
// the API): the re-execution looks the first execution's delivery up under
// gc:<op id>:1 and, finding none, delivers once under the same key, on
// either content path.
func TestSessionContentOpsAdoptionReusesTheDeliveryKey(t *testing.T) {
	for _, path := range []string{"/submit", "/messages"} {
		t.Run(path, func(t *testing.T) {
			e, info := newT3OpsEnv(t, "auto")
			orphan := e.seedOp(t, info.ID, "key-o", "deliver me", legacyop.ExecutorAPI)
			acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+path, "key-o", `{"message":"deliver me"}`).Body)
			if acc.RequestID != orphan.ID || acc.Op == nil || acc.Op.Attempt != 1 {
				t.Fatalf("adoption = %+v", acc)
			}
			if op := e.waitClosed(t, orphan.ID); op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != legacyop.ViaLegacyDelivered {
				t.Fatalf("adopted op = %+v", op.Outcome)
			}
			looks := callsOf(e.fs.sp, info.SessionName, "LookupDelivery")
			if len(looks) != 1 || looks[0].DeliveryKey != opDeliveryKey(orphan.ID) {
				t.Fatalf("lookups = %+v, want one under %s", looks, opDeliveryKey(orphan.ID))
			}
			assertOneKeyedDelivery(t, e, info, opDeliveryKey(orphan.ID), "deliver me", orphan.ID, "key-o")
		})
	}
}

// K3 on /messages: an adopted message whose first execution T3 already
// holds closes succeeded{legacy.delivered} after one lookup, with nothing
// sent.
func TestSessionContentOpsAdoptedMessageThatLandedSendsNothing(t *testing.T) {
	e, info := newT3OpsEnv(t, "auto")
	orphan := e.seedOp(t, info.ID, "key-m", "deliver me", legacyop.ExecutorAPI)
	e.fs.sp.KeyedDeliveries = map[string]map[string]runtime.DeliveryReceipt{info.SessionName: {opDeliveryKey(orphan.ID): {MessageID: "t3-message-1"}}}
	acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/messages", "key-m", `{"message":"deliver me"}`).Body)
	if acc.RequestID != orphan.ID || acc.Op == nil || acc.Op.Attempt != 1 {
		t.Fatalf("adoption = %+v", acc)
	}
	if op := e.waitClosed(t, orphan.ID); op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != legacyop.ViaLegacyDelivered {
		t.Fatalf("op = %+v, want succeeded via legacy.delivered", op.Outcome)
	}
	if looks := callsOf(e.fs.sp, info.SessionName, "LookupDelivery"); len(looks) != 1 {
		t.Fatalf("lookups = %+v, want exactly one", looks)
	}
	if got := callsOf(e.fs.sp, info.SessionName, "ObserveTurn", "InterruptTurn", "Interrupt", "Nudge", "NudgeNow"); len(got) != 0 {
		t.Fatalf("calls = %+v, want none", got)
	}
}

// K3, DES-T3I §14 item 8 (M27, M36 at the API): an adopted interrupt_now
// whose first execution T3 already holds, while that execution's turn still
// runs, sends nothing: no sighting, interrupt or send. It closes
// succeeded{legacy.delivered} with an unqueued result and no interrupt.
func TestSessionContentOpsAdoptedInterruptNowThatLandedSendsNothing(t *testing.T) {
	e, info := newT3OpsEnv(t, "auto")
	orphan := e.seedOpWith(t, info.ID, "key-k3", "now", session.SubmitIntentInterruptNow)
	e.fs.sp.KeyedDeliveries = map[string]map[string]runtime.DeliveryReceipt{info.SessionName: {opDeliveryKey(orphan.ID): {MessageID: "t3-message-1"}}}
	e.fs.sp.TurnScripts = map[string][]string{info.SessionName: {"thread/t1"}}
	rec := e.post(t, "/session/"+info.ID+"/submit", "key-k3", `{"message":"now","intent":"interrupt_now"}`)
	acc := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || acc.RequestID != orphan.ID || acc.Op == nil || acc.Op.Attempt != 1 {
		t.Fatalf("adoption = %d %+v", rec.Code, acc)
	}
	success, failure := waitForSessionSubmitResult(t, e.fs.eventProv, orphan.ID)
	if success == nil || success.Queued || success.Intent != "interrupt_now" || success.Interrupt != nil {
		t.Fatalf("terminal = %+v, %+v; want unqueued interrupt_now with no interrupt report", success, failure)
	}
	if op := e.waitClosed(t, orphan.ID); op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != legacyop.ViaLegacyDelivered {
		t.Fatalf("op = %+v, want succeeded via legacy.delivered", op.Outcome)
	}
	if looks := callsOf(e.fs.sp, info.SessionName, "LookupDelivery"); len(looks) != 1 {
		t.Fatalf("lookups = %+v, want exactly one", looks)
	}
	if got := callsOf(e.fs.sp, info.SessionName, "ObserveTurn", "InterruptTurn", "WaitTurnEnded", "Interrupt", "Nudge", "NudgeNow"); len(got) != 0 {
		t.Fatalf("calls = %+v, want none", got)
	}
}

// Kills a send after a failed lookup (M25 at the API): an adoption whose
// lookup cannot tell closes failed{provider_unavailable} and emits
// request.failed, with nothing sent or interrupted.
func TestSessionContentOpsAdoptionLookupFailureSendsNothing(t *testing.T) {
	e, info := newT3OpsEnv(t, "auto")
	orphan := e.seedOpWith(t, info.ID, "key-f", "now", session.SubmitIntentInterruptNow)
	e.fs.sp.LookupDeliveryErrors = map[string]error{info.SessionName: runtime.ErrRuntimeUnavailable}
	acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-f", `{"message":"now","intent":"interrupt_now"}`).Body)
	if acc.RequestID != orphan.ID {
		t.Fatalf("adoption = %+v", acc)
	}
	evs := waitTerminalEvents(t, e.fs.eventProv, orphan.ID, 1)
	if evs[0].Type != events.RequestFailed {
		t.Fatalf("terminal = %s %s, want request.failed", evs[0].Type, evs[0].Payload)
	}
	if op := e.waitClosed(t, orphan.ID); op.Outcome.Arm != opjournal.ArmFailed || op.Outcome.Reason != opjournal.ReasonProviderUnavailable {
		t.Fatalf("op = %+v, want failed{provider_unavailable}", op.Outcome)
	}
	if got := callsOf(e.fs.sp, info.SessionName, "ObserveTurn", "InterruptTurn", "Interrupt", "Nudge", "NudgeNow"); len(got) != 0 {
		t.Fatalf("calls = %+v, want none after a failed lookup", got)
	}
}

// Kills a lookup on every delivery (M24 at the API): a first execution
// looks nothing up.
func TestSessionContentOpsFirstExecutionLooksNothingUp(t *testing.T) {
	e, info := newT3OpsEnv(t, "auto")
	acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "k1", `{"message":"hello"}`).Body)
	waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
	if looks := callsOf(e.fs.sp, info.SessionName, "LookupDelivery"); len(looks) != 0 {
		t.Fatalf("lookups = %+v, want none on a first execution", looks)
	}
}
