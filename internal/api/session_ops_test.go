package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	createErr, ambiguous, panicAfter := s.createErr, s.ambiguous, s.panicAfterCreate
	s.panicAfterCreate = false
	s.mu.Unlock()
	if createErr != nil {
		return beads.Bead{}, createErr
	}
	created, err := s.MemStore.Create(b)
	if err != nil {
		return created, err
	}
	s.creates.Add(1)
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

// opsEnv is a fake city whose sessions store records ops under mode.
type opsEnv struct {
	fs    *fakeState
	store *opsTestStore
	h     http.Handler
}

func newOpsEnv(t *testing.T, mode string) *opsEnv {
	t.Helper()
	t.Setenv("GC_OPS_CONTENT", "")
	fs := newSessionFakeState(t)
	store := newOpsTestStore()
	fs.cityBeadStore = store
	fs.cfg.Ops.ContentMode = mode
	return &opsEnv{fs: fs, store: store, h: newTestCityHandler(t, fs)}
}

// restart is a new API server (an empty LRU and in-flight registry) over
// the same city, as after an API restart.
func (e *opsEnv) restart(t *testing.T) { e.h = newTestCityHandler(t, e.fs) }

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

func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
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

// I5: in off no op row is written and the 202 body and terminal events keep
// their legacy fields, whether or not the request carries a key.
func TestSessionContentOpsOffKeepsTheLegacyWire(t *testing.T) {
	e := newOpsEnv(t, "")
	info := e.session(t, "Off")
	for _, tc := range []struct{ path, key, body, eventType string }{
		{"/submit", "", `{"message":"one"}`, events.RequestResultSessionSubmit},
		{"/submit", "k1", `{"message":"two"}`, events.RequestResultSessionSubmit},
		{"/messages", "", `{"message":"three"}`, events.RequestResultSessionMessage},
		{"/messages", "k2", `{"message":"four"}`, events.RequestResultSessionMessage},
	} {
		rec := e.post(t, "/session/"+info.ID+tc.path, tc.key, tc.body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: status %d: %s", tc.path, rec.Code, rec.Body.String())
		}
		if got := jsonKeys(t, rec.Body.Bytes()); !slices.Equal(got, []string{"event_cursor", "request_id", "status"}) {
			t.Fatalf("%s: 202 keys = %v", tc.path, got)
		}
		acc := decodeAsyncAccepted(t, rec.Body)
		if !strings.HasPrefix(acc.RequestID, "req-") {
			t.Fatalf("%s: request_id = %q, want req-", tc.path, acc.RequestID)
		}
		evs := waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
		if evs[0].Type != tc.eventType {
			t.Fatalf("%s: event %s: %s", tc.path, evs[0].Type, evs[0].Payload)
		}
		if strings.Contains(string(evs[0].Payload), `"op"`) {
			t.Fatalf("%s: off event carries an op: %s", tc.path, evs[0].Payload)
		}
	}
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("off wrote %d op rows", len(rows))
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

// §6.2 in-flight rows and §6.1: while the first request delivers, a same-key
// request on the other endpoint gets the cached 202 (same request_id and
// cursor), another body under the key is refused 422, and the op existed
// before the delivery ran.
func TestSessionContentOpsAutoInFlightReplay(t *testing.T) {
	e := newOpsEnv(t, "auto")
	info := e.session(t, "In flight")
	gp := &gatedProvider{Fake: e.fs.sp, release: make(chan struct{}), entered: make(chan struct{})}
	gp.opExisted = func() bool { return len(e.opRows(t)) == 1 }
	e.fs.sessionProvider = gp
	released := false
	defer func() {
		if !released {
			close(gp.release)
		}
	}()

	first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-f", `{"message":"hi"}`).Body)
	select {
	case <-gp.entered:
	case <-time.After(testEventTimeout):
		t.Fatal("delivery never ran")
	}
	rec := e.post(t, "/session/"+info.ID+"/messages", "key-f", `{"message":"hi"}`)
	second := decodeAsyncAccepted(t, rec.Body)
	if rec.Code != http.StatusAccepted || second.RequestID != first.RequestID || second.EventCursor != first.EventCursor || second.Replayed {
		t.Fatalf("in-flight replay = %d %+v, want the first answer %+v", rec.Code, second, first)
	}
	if mismatch := e.post(t, "/session/"+info.ID+"/messages", "key-f", `{"message":"other"}`); mismatch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("other body under the key = %d: %s", mismatch.Code, mismatch.Body.String())
	}
	close(gp.release)
	released = true
	waitTerminalEvents(t, e.fs.eventProv, first.RequestID, 1)
	if !gp.sawOp.Load() {
		t.Fatal("the delivery ran before the op row was written")
	}
	if got := len(deliveredCalls(e.fs.sp, info.SessionName)); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
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

// I11: the text the runtime receives is the same in off, auto and require,
// and carries no op id, key or request_id.
func TestSessionContentOpsDeliveredTextIsTheSameInEveryMode(t *testing.T) {
	const text = "exactly these bytes;\n  and these"
	var want []runtime.Call
	for _, mode := range []string{"off", "auto", "require"} {
		e := newOpsEnv(t, mode)
		info := e.session(t, "Text "+mode)
		body, _ := json.Marshal(map[string]string{"message": text})
		acc := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+"/submit", "key-t", string(body)).Body)
		waitTerminalEvents(t, e.fs.eventProv, acc.RequestID, 1)
		calls := deliveredCalls(e.fs.sp, info.SessionName)
		if len(calls) != 1 || calls[0].Message != text {
			t.Fatalf("%s: delivered %+v", mode, calls)
		}
		assertNoOpIDs(t, calls, acc.RequestID, acc.IdempotencyKey)
		got := []runtime.Call{{Method: calls[0].Method, Message: calls[0].Message, Content: calls[0].Content, DeliveryKey: calls[0].DeliveryKey}}
		if want == nil {
			want = got
			continue
		}
		if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
			t.Fatalf("%s delivered %+v, off delivered %+v", mode, got, want)
		}
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
		{"store unavailable", func(e *opsEnv) { e.store.set(func(s *opsTestStore) { s.createErr = errors.New("transport: down") }) },
			`{"message":"x"}`, http.StatusServiceUnavailable, "op_journal_unavailable"},
		{"payload too large", func(*opsEnv) {}, `{"message":"` + strings.Repeat("y", opjournal.MaxInlinePayload+1) + `"}`,
			http.StatusUnprocessableEntity, "payload_too_large"},
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
			op := opjournal.Op{ID: "gc-op-1", Conversation: "gc-1", Kind: opjournal.KindMessage, Status: opjournal.StatusClosed, Outcome: res.Outcome,
				Policy: messageOpPolicy(session.SubmitIntentFollowUp)}
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

// D13: the reporter latches one event per (gate, surface, stage, reason) per
// interval, ignores off, and bounds the detail.
func TestOpsDegradeReporterLatchesPerClass(t *testing.T) {
	rec := &fakeRecorder{}
	clk := &clock.Fake{Time: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	r := NewOpsDegradeReporter(func() events.Recorder { return rec }, clk)
	d := legacyop.Degrade{Mode: gate.Auto, Surface: legacyop.SurfaceAPI, Stage: legacyop.StagePropose, Reason: legacyop.DegradeStoreError, Err: errors.New(strings.Repeat("é", 400))}
	r.Report(rollout.KeyOpsContent, d)
	r.Report(rollout.KeyOpsContent, d)
	off := d
	off.Mode = gate.Off
	r.Report(rollout.KeyOpsContent, off)
	other := d
	other.Stage = legacyop.StageClose
	r.Report(rollout.KeyOpsContent, other)
	r.Report(rollout.KeyOpsControl, d)
	if n := len(rec.events()); n != 3 {
		t.Fatalf("events = %d, want 3 (one per class)", n)
	}
	clk.Time = clk.Time.Add(opsDegradeInterval)
	r.Report(rollout.KeyOpsContent, d)
	evs := rec.events()
	if len(evs) != 4 {
		t.Fatalf("events after the interval = %d, want 4", len(evs))
	}
	var p events.OpsRecordingDegradedPayload
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Detail) > opsDegradeDetailMax || !strings.HasPrefix(p.Detail, "é") || evs[0].Type != events.OpsRecordingDegraded || evs[0].Actor != "api" || evs[0].Subject != rollout.KeyOpsContent {
		t.Fatalf("event = %+v payload %+v", evs[0], p)
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

// The durable-202 property (DES-O0API §15): under randomized faults —
// Propose errors, a create that commits then errors, failing closes, a crash
// after the commit, a delivery panic — every answer keeps I1 (require: a 2xx
// is preceded by a committed op whose id is the request_id), I2 (each op
// ends with exactly one outcome), I4 (request_id is op.id, and a same-key
// retry gets it again) and I10 (the op closes before its terminal event).
func TestSessionContentOpsDurable202Property(t *testing.T) {
	const runs = 24
	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewPCG(uint64(seed), 0))
	faults := []string{"none", "propose_error", "ambiguous", "close_error", "crash_after_commit", "delivery_panic"}
	for i := range runs {
		mode := []string{"auto", "require"}[rng.IntN(2)]
		fault := faults[rng.IntN(len(faults))]
		keyed := rng.IntN(2) == 0
		t.Run(fmt.Sprintf("%02d_%s_%s_keyed=%v", i, mode, fault, keyed), func(t *testing.T) {
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
				n := 1 + rng.IntN(5)
				e.store.set(func(s *opsTestStore) { s.updateFails = n })
			case "crash_after_commit":
				e.store.set(func(s *opsTestStore) { s.panicAfterCreate = true })
			case "delivery_panic":
				pp.armed.Store(true)
			}
			path := "/session/" + info.ID + []string{"/submit", "/messages"}[rng.IntN(2)]
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
			if len(open) > 0 && (mode == "require" || fault != "close_error") {
				t.Fatalf("I10: terminal events emitted before their op closed: %v", open)
			}
		})
	}
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
		{name: "keyed, live", key: "key-1", answer: map[string]any{"status": "accepted", "request_id": "gc-op-a", "event_cursor": "3",
			"idempotency_key": "key-1", "op": map[string]any{"id": "gc-op-a", "conversation": "sess-1", "kind": "message", "status": "submitted"}}, stream: true},
		{name: "keyed, replayed success", key: "key-2", queued: true, answer: map[string]any{"status": "accepted", "request_id": "gc-op-b", "event_cursor": "3",
			"replayed": true, "op": map[string]any{"id": "gc-op-b", "conversation": "sess-1", "kind": "message", "status": "closed",
				"outcome": map[string]any{"arm": "succeeded", "via": queued}}}},
		{name: "keyed, replayed failure", key: "key-3", wantErr: "submit failed: resolve_failed: replayed: operation gc-op-c already closed failed{session_gone}",
			answer: map[string]any{"status": "accepted", "request_id": "gc-op-c", "event_cursor": "3", "replayed": true,
				"op": map[string]any{"id": "gc-op-c", "conversation": "sess-1", "kind": "message", "status": "closed",
					"outcome": map[string]any{"arm": "failed", "reason": gone}}}},
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
