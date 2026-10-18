package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	st := e.opStore(t)
	deadline := time.Now().Add(testEventTimeout)
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
	var out []events.Event
	for _, e := range all {
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
// does not gate.
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
				info, err := session.NewManagerWithOptions(e.fs.cityBeadStore, e.fs.sessionProvider).CreateSession(context.Background(), session.CreateOptions{
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
// deliver a second time. The old Server is shut down.
func TestSessionContentOpsServerRebuildKeepsTheInflightRequest(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "Rebuild")
	gp, release := e.gatedProvider(t)
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-r", `{"message":"once"}`).Body)
	gp.waitEntered(t)
	old := e.server(t)
	e.rebuild(t)
	select {
	case <-old.ops.done:
	default:
		t.Fatal("the replaced Server was not shut down")
	}
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

// A dropped Server's close retry ends: in require the terminal event waits
// for a close that keeps failing, and the replaced Server's shutdown ends
// that wait at once instead of at finish_by. The op stays open and leaves the
// registry, so a same-key retry adopts it.
func TestSessionContentOpsShutdownEndsTheCloseRetry(t *testing.T) {
	e := newOpsEnv(t, "require")
	info := e.session(t, "Shutdown")
	gp, release := e.gatedProvider(t)
	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-s", `{"message":"x"}`).Body)
	gp.waitEntered(t)
	e.store.set(func(s *opsTestStore) { s.updateFails = 1 << 30 })
	e.rebuild(t)
	release()
	evs := waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	var p SessionSubmitSucceededPayload
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil || p.Op == nil || p.Op.Status == "closed" {
		t.Fatalf("event = %s (%v), want the op still open", evs[0].Payload, err)
	}
	e.store.set(func(s *opsTestStore) { s.updateFails = 0 })
	deadline := time.Now().Add(testEventTimeout)
	for e.sm.cityOpsFor(e.fs.CityName(), e.fs).inflight.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the in-flight entry was never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec := e.post(t, "/session/"+info.ID+"/submit", "key-s", `{"message":"x"}`)
	acc := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || acc.RequestID != first.RequestID || acc.Op == nil || acc.Op.Attempt != 1 {
		t.Fatalf("retry = %d %+v op %+v, want an adoption", rec.Code, acc, acc.Op)
	}
	e.waitClosed(t, first.RequestID)
}

// seedAPIOp proposes the op a submit of text under key would record, under
// executor, as a producer that then died would leave it.
func (e *opsEnv) seedOp(t *testing.T, conv, key, text string, executor opjournal.ExecutorKind) opjournal.Op {
	t.Helper()
	now := time.Now()
	op, created, err := e.opStore(t).Propose(opjournal.Request{
		Conversation: conv, Kind: opjournal.KindMessage, Executor: executor, ClientKey: key,
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  text, Policy: messageOpPolicy(session.SubmitIntentDefault),
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

func assertNoOpIDs(t *testing.T, calls []runtime.Call, ids ...string) {
	t.Helper()
	for _, c := range calls {
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
	mgr := session.NewManagerWithOptions(e.fs.cityBeadStore, e.fs.sessionProvider)
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
		{"runtime unavailable", false, fmt.Errorf("x: %w", runtime.ErrRuntimeUnavailable), opjournal.ArmFailed, opjournal.ReasonProviderUnavailable, "", "submit_failed", "message_failed"},
		{"timeout", false, messageTimeoutError{}, opjournal.ArmFailed, opjournal.ReasonDeadlineInFlight, "", "timeout", "timeout"},
		{"other", false, errors.New("boom"), opjournal.ArmFailed, opjournal.ReasonOther, "", "submit_failed", "message_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := messageOpResult(tc.queued, tc.err, nil)
			if res.Outcome.Arm != tc.arm || res.Outcome.Reason != tc.reason || res.Outcome.Result.Via != tc.via || res.LeaveOpen {
				t.Fatalf("result = %+v", res)
			}
			op := opjournal.Op{
				ID: "gc-op-1", Conversation: "gc-1", Kind: opjournal.KindMessage, Status: opjournal.StatusClosed, Outcome: res.Outcome,
				Policy: messageOpPolicy(session.SubmitIntentFollowUp),
			}
			for _, ep := range []struct{ operation, code, success string }{
				{RequestOperationSessionSubmit, tc.submitCode, events.RequestResultSessionSubmit},
				{RequestOperationSessionMessage, tc.msgCode, events.RequestResultSessionMessage},
			} {
				fs := newSessionFakeState(t)
				s := New(fs)
				s.emitMessageOpProjection(ep.operation, op)
				evs, _ := fs.eventProv.List(events.Filter{})
				if len(evs) != 1 {
					t.Fatalf("%s: events = %d", ep.operation, len(evs))
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
					p.Operation != ep.operation || p.ErrorCode != ep.code || p.Op == nil || !strings.Contains(p.ErrorMessage, "replayed") {
					t.Fatalf("%s: %s %s", ep.operation, evs[0].Type, evs[0].Payload)
				}
			}
		})
	}
	unfenced := &session.TurnInterruptReport{Outcome: session.TurnInterruptSettled, Degradation: session.DegradationUnfencedInterrupt}
	if res := messageOpResult(false, nil, unfenced); res.Outcome.Degradation != session.DegradationUnfencedInterrupt {
		t.Fatalf("unfenced interrupt outcome = %+v", res.Outcome)
	}
	if res := messageOpResult(false, errors.New("x"), unfenced); res.Outcome.Degradation != "" {
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
// interval: a sustained outage raises one event however long it lasts, and a
// stage that reports success re-arms only its own classes. Off is ignored,
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
	r.Healthy(rollout.KeyOpsContent, legacyop.SurfaceAPI, legacyop.StagePropose)
	r.Report(rollout.KeyOpsContent, d)
	if n := len(rec.events()); n != 4 {
		t.Fatalf("flapping within the interval: events = %d, want 4", n)
	}
	clk.Time = clk.Time.Add(opsDegradeInterval)
	r.Report(rollout.KeyOpsContent, d)
	if n := len(rec.events()); n != 4 {
		t.Fatalf("still failing since the suppressed report: events = %d, want 4", n)
	}
	r.Healthy(rollout.KeyOpsContent, legacyop.SurfaceAPI, legacyop.StagePropose)
	r.Report(rollout.KeyOpsContent, d)
	if n := len(rec.events()); n != 5 {
		t.Fatalf("healthy, then failing an interval later: events = %d, want 5", n)
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
// closes before its terminal event; auto may emit first only when the close
// outlasts its foreground retries).
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
		if len(open) > 0 && (mode == "require" || fault != "close_error_background") {
			t.Fatalf("I10: terminal events emitted before their op closed: %v", open)
		}
		if mode == "auto" && fault == "close_error_background" && acc.Op != nil && len(open) == 0 {
			t.Fatal("auto waited for a background close before emitting")
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
	}{
		{name: "no key", answer: map[string]any{"status": "accepted", "request_id": "req-1", "event_cursor": "3"}, stream: true},
		{name: "keyed, live", key: "key-1", answer: map[string]any{
			"status": "accepted", "request_id": "gc-op-a", "event_cursor": "3",
			"idempotency_key": "key-1", "op": map[string]any{"id": "gc-op-a", "conversation": "sess-1", "kind": "message", "status": "submitted"},
		}, stream: true},
		{name: "keyed, replayed success", key: "key-2", queued: true, answer: map[string]any{
			"status": "accepted", "request_id": "gc-op-b", "event_cursor": "3",
			"replayed": true, "op": map[string]any{
				"id": "gc-op-b", "conversation": "sess-1", "kind": "message", "status": "closed",
				"outcome": map[string]any{"arm": "succeeded", "via": queued},
			},
		}},
		{
			name: "keyed, replayed failure", key: "key-3", wantErr: "submit failed: resolve_failed: replayed: operation gc-op-c already closed failed{session_gone}",
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
	if err != nil || !first.Queued || first.ID != info.ID {
		t.Fatalf("first = %+v, %v", first, err)
	}

	e.restart(t)
	srv2 := httptest.NewServer(e.h)
	defer srv2.Close()
	c2 := NewCityScopedClient(srv2.URL, e.fs.CityName())
	replayed, err := c2.SubmitSessionWithKey(info.ID, "via client", session.SubmitIntentFollowUp, "client-key")
	if err != nil || !replayed.Queued || replayed.ID != info.ID || replayed.Intent != session.SubmitIntentFollowUp {
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
func driftEnv(t *testing.T, mode string) (e *opsEnv, a, b session.Info, logs func() []string) {
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
	return e, a, b, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(lines)
	}
}

// §6.1 target drift: in auto the delivery re-resolves the target, as it
// always has, so it reaches the session the alias names now; the op, which
// names the session the 202 resolved, closes from that result and the drift
// is logged and counted. In require the delivery goes to the op's
// conversation: the op is the request.
func TestSessionContentOpsTargetDrift(t *testing.T) {
	for _, path := range []string{"/submit", "/messages"} {
		t.Run("auto "+path, func(t *testing.T) {
			e, a, b, logs := driftEnv(t, "auto")
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
		})
		t.Run("require "+path, func(t *testing.T) {
			e, a, b, logs := driftEnv(t, "require")
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
	mgr := session.NewManagerWithOptions(e.fs.cityBeadStore, e.fs.sessionProvider)
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
