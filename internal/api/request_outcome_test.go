package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// requestOutcomeWire is the GET /v0/city/{cityName}/request/{requestId} body
// as a client decodes it, kept independent of the server's Go types so these
// tests pin the wire contract.
type requestOutcomeWire struct {
	RequestID string `json:"request_id"`
	Operation string `json:"operation"`
	State     string `json:"state"`
	Event     *struct {
		Seq     uint64          `json:"seq"`
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	} `json:"event"`
}

// getRequestOutcome reads requestID's outcome from the events after afterSeq,
// the event_cursor of the request's 202 as a client passes it.
func getRequestOutcome(t *testing.T, h http.Handler, state State, requestID, afterSeq string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/request/")+requestID+"?after_seq="+url.QueryEscape(afterSeq), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeRequestOutcome(t *testing.T, rec *httptest.ResponseRecorder) requestOutcomeWire {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("request outcome status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out requestOutcomeWire
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode request outcome: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

// terminalEventFor returns the recorded terminal event carrying requestID, so
// a test can compare the lookup against exactly what the stream delivered.
func terminalEventFor(t *testing.T, prov events.Provider, eventType, requestID string) events.Event {
	t.Helper()
	evts, err := prov.List(events.Filter{Type: eventType})
	if err != nil {
		t.Fatalf("list %s: %v", eventType, err)
	}
	for _, e := range evts {
		var p struct {
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.RequestID == requestID {
			return e
		}
	}
	t.Fatalf("no %s event for request_id %s", eventType, requestID)
	return events.Event{}
}

func assertSameJSON(t *testing.T, what string, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: decode got: %v (%s)", what, err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: decode want: %v (%s)", what, err, want)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if !bytes.Equal(gb, wb) {
		t.Fatalf("%s = %s, want %s", what, gb, wb)
	}
}

func postSubmit(t *testing.T, h http.Handler, state State, sessionID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/session/")+sessionID+"/submit", strings.NewReader(body)))
	return rec
}

// A client that missed request.result.session.submit can still read the
// outcome by request_id, with the same typed payload the event carried.
func TestRequestOutcomeReportsSucceededSubmitWithEventPayload(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Outcome")

	rec := postSubmit(t, h, fs, info.ID, `{"message":"later please","intent":"follow_up"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)
	if success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID); success == nil {
		t.Fatalf("submit failed: %+v", failure)
	}
	want := terminalEventFor(t, fs.eventProv, events.RequestResultSessionSubmit, accepted.RequestID)

	out := decodeRequestOutcome(t, getRequestOutcome(t, h, fs, accepted.RequestID, accepted.EventCursor))
	if out.RequestID != accepted.RequestID || out.Operation != RequestOperationSessionSubmit || out.State != "succeeded" {
		t.Fatalf("outcome = %+v, want succeeded session.submit for %s", out, accepted.RequestID)
	}
	if out.Event == nil || out.Event.Type != events.RequestResultSessionSubmit || out.Event.Seq != want.Seq {
		t.Fatalf("outcome event = %+v, want %s at seq %d", out.Event, events.RequestResultSessionSubmit, want.Seq)
	}
	assertSameJSON(t, "outcome event payload", out.Event.Payload, want.Payload)
}

// A failed request reports the same error the request.failed event carried.
func TestRequestOutcomeReportsFailedSubmitWithFailurePayload(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.sp.TurnInterruptModeValue = runtime.TurnInterruptUnsupported
	h := newTestCityHandler(t, fs)
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "helper", Title: "Codex", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := postSubmit(t, h, fs, info.ID, `{"message":"now","intent":"interrupt_now"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)
	waitForRequestFailed(t, fs.eventProv, accepted.RequestID)
	want := terminalEventFor(t, fs.eventProv, events.RequestFailed, accepted.RequestID)

	out := decodeRequestOutcome(t, getRequestOutcome(t, h, fs, accepted.RequestID, accepted.EventCursor))
	if out.Operation != RequestOperationSessionSubmit || out.State != "failed" {
		t.Fatalf("outcome = %+v, want failed session.submit", out)
	}
	if out.Event == nil || out.Event.Type != events.RequestFailed || out.Event.Seq != want.Seq {
		t.Fatalf("outcome event = %+v, want request.failed at seq %d", out.Event, want.Seq)
	}
	assertSameJSON(t, "outcome failure payload", out.Event.Payload, want.Payload)
}

// While the operation is still running in this process the outcome is
// pending and carries no event; once it finishes the lookup turns terminal.
func TestRequestOutcomeReportsPendingWhileInFlight(t *testing.T) {
	fs := newSessionFakeState(t)
	blocker := &blockingNudgeProvider{Fake: fs.sp, started: make(chan struct{}), unblock: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(blocker.unblock) }) }
	t.Cleanup(unblock)
	srv := New(&stateWithSessionProvider{fakeState: fs, provider: blocker})
	h := newTestCityHandlerWith(t, fs, srv)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "pending-message")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/")+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("message status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)
	select {
	case <-blocker.started:
	case <-time.After(testEventTimeout):
		t.Fatal("provider nudge was not reached")
	}

	out := decodeRequestOutcome(t, getRequestOutcome(t, h, fs, accepted.RequestID, accepted.EventCursor))
	if out.Operation != RequestOperationSessionMessage || out.State != "pending" || out.Event != nil {
		t.Fatalf("in-flight outcome = %+v, want pending session.message with no event", out)
	}

	unblock()
	if success, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID); success == nil {
		t.Fatalf("message failed: %+v", failure)
	}
	out = decodeRequestOutcome(t, getRequestOutcome(t, h, fs, accepted.RequestID, accepted.EventCursor))
	if out.State != "succeeded" || out.Event == nil || out.Event.Type != events.RequestResultSessionMessage {
		t.Fatalf("finished outcome = %+v, want succeeded with request.result.session.message", out)
	}
}

// An id the city never accepted, or one whose events have aged out of the
// retained log, is a typed 404.
func TestRequestOutcomeUnknownIDIsRequestNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	rec := getRequestOutcome(t, h, fs, "req-000000000000000000000000", "0")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	var p struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, rec.Body.String())
	}
	if p.Type != "urn:gascity:error:request-not-found" {
		t.Fatalf("problem type = %q, want urn:gascity:error:request-not-found", p.Type)
	}
}

// The outcome is read from the persisted city event log, so it survives the
// API server and its event provider being rebuilt, as a supervisor restart
// rebuilds them.
func TestRequestOutcomeSurvivesEventProviderReopen(t *testing.T) {
	fs := newSessionFakeState(t)
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	first, err := events.NewFileRecorder(logPath, io.Discard)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	fs.eventProv = first
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Restart")

	rec := postSubmit(t, h, fs, info.ID, `{"message":"later please","intent":"follow_up"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)
	if success, failure := waitForSessionSubmitResult(t, first, accepted.RequestID); success == nil {
		t.Fatalf("submit failed: %+v", failure)
	}
	want := terminalEventFor(t, first, events.RequestResultSessionSubmit, accepted.RequestID)
	if err := first.Close(); err != nil {
		t.Fatalf("close event log: %v", err)
	}

	reopened, err := events.NewFileRecorder(logPath, io.Discard)
	if err != nil {
		t.Fatalf("reopen event log: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	fs.eventProv = reopened
	h2 := newTestCityHandlerWith(t, fs, New(fs))

	out := decodeRequestOutcome(t, getRequestOutcome(t, h2, fs, accepted.RequestID, accepted.EventCursor))
	if out.State != "succeeded" || out.Event == nil || out.Event.Seq != want.Seq {
		t.Fatalf("outcome after reopen = %+v, want succeeded at seq %d", out, want.Seq)
	}
	assertSameJSON(t, "outcome payload after reopen", out.Event.Payload, want.Payload)
}

// A request accepted by a process that exited before recording a terminal
// event has no running owner. The lookup must not report it pending forever,
// and must not invent a failure the client could retry into a duplicate.
func TestRequestOutcomeReportsOutcomeUnknownForOrphanedRequest(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	const orphan = "req-0123456789abcdef01234567"
	fs.eventProv.Record(events.Event{
		Type:    "request.accepted",
		Actor:   "api",
		Payload: json.RawMessage(`{"request_id":"` + orphan + `","operation":"session.submit"}`),
	})

	out := decodeRequestOutcome(t, getRequestOutcome(t, h, fs, orphan, "0"))
	if out.RequestID != orphan || out.Operation != RequestOperationSessionSubmit || out.State != "outcome_unknown" || out.Event != nil {
		t.Fatalf("orphaned outcome = %+v, want outcome_unknown session.submit with no event", out)
	}
}

// ackRefusingProvider reads normally but refuses every acknowledged append, as
// a FileRecorder does on a lock timeout or a full disk.
type ackRefusingProvider struct {
	*events.Fake
}

func (ackRefusingProvider) RecordAck(events.Event) error {
	return errors.New("event log append refused")
}

// Accepting work whose outcome cannot be recorded would hand the client a
// request_id nothing can ever resolve, so the submit is refused before it
// starts.
func TestSessionSubmitRefusedWhenAcceptanceCannotBeRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		prov events.Provider
	}{
		{name: "no event provider", prov: nil},
		{name: "event log refuses the append", prov: ackRefusingProvider{Fake: events.NewFake()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "Refused")
			fs.eventProv = tc.prov
			h := newTestCityHandler(t, fs)

			rec := postSubmit(t, h, fs, info.ID, `{"message":"hello"}`)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("submit status = %d, want 503; body: %s", rec.Code, rec.Body.String())
			}
			for _, call := range fs.sp.SnapshotCalls() {
				if call.Name == info.SessionName && strings.Contains(call.Message, "hello") {
					t.Fatalf("refused submit still delivered: %+v", call)
				}
			}
		})
	}
}

// The lookup reads the events after the cursor the request's 202 returned, so
// it is refused, 422, without one or with one that is not an event sequence
// number, rather than answered from a scan of the whole retained log.
func TestRequestOutcomeRequiresTheAcceptanceCursor(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	for name, query := range map[string]string{
		"no cursor":           "",
		"empty cursor":        "?after_seq=",
		"not a sequence":      "?after_seq=latest",
		"past the seq domain": "?after_seq=18446744073709551616",
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/request/")+"req-000000000000000000000000"+query, nil))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A request's events all follow its 202's cursor, so the lookup never opens an
// archive the log rotated out before it. The archive here is corrupt: the
// control read from the log's start opens it and fails.
func TestRequestOutcomeOpensNoArchiveBeforeTheCursor(t *testing.T) {
	fs := newSessionFakeState(t)
	dir := t.TempDir()
	log, err := events.NewFileRecorder(filepath.Join(dir, "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	for range 20 {
		log.Record(events.Event{Type: events.SessionWoke, Actor: "test", Subject: "history"})
	}
	rotated, err := log.ForceRotate()
	if err != nil || !rotated.Rotated {
		t.Fatalf("ForceRotate = %+v, %v; want a rotation", rotated, err)
	}
	<-rotated.Done
	fs.eventProv = log
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Rotated")

	rec := postSubmit(t, h, fs, info.ID, `{"message":"later please","intent":"follow_up"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)
	if success, failure := waitForSessionSubmitResult(t, log, accepted.RequestID); success == nil {
		t.Fatalf("submit failed: %+v", failure)
	}
	want := terminalEventFor(t, log, events.RequestResultSessionSubmit, accepted.RequestID)
	archives, err := filepath.Glob(filepath.Join(dir, "events.jsonl.archive-*.gz"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("archives = %v (%v), want the one rotated before the request", archives, err)
	}
	if err := os.WriteFile(archives[0], []byte("not a gzip stream"), 0o644); err != nil {
		t.Fatalf("corrupt the archive: %v", err)
	}

	out := decodeRequestOutcome(t, getRequestOutcome(t, h, fs, accepted.RequestID, accepted.EventCursor))
	if out.State != "succeeded" || out.Event == nil || out.Event.Seq != want.Seq {
		t.Fatalf("outcome = %+v, want succeeded at seq %d", out, want.Seq)
	}
	if control := getRequestOutcome(t, h, fs, accepted.RequestID, "0"); control.Code != http.StatusInternalServerError {
		t.Fatalf("read from the log's start = %d, want 500 from the corrupt archive; body: %s", control.Code, control.Body.String())
	}
}

// requestFinished waits until requestID's goroutine has left the in-flight
// registry, so the lookup can no longer answer pending from it.
func requestFinished(t *testing.T, state State, requestID string) {
	t.Helper()
	waitFor(t, testEventTimeout, "request "+requestID+" leaves the in-flight registry", func() bool {
		_, running := asyncRequestsInFlight.Load(asyncRequestKey{cityPath: state.CityPath(), requestID: requestID})
		return !running
	})
}

// A request whose terminal event the event log refused is kept by its city
// until a log takes it (session_ops.go), so its outcome is held, not lost:
// the lookup answers pending while the event is kept, never outcome_unknown,
// which tells a client to inspect and maybe resend a message that was
// delivered. Once the log takes events, the lookup records the kept event and
// answers it.
func TestRequestOutcomeIsPendingWhileItsTerminalEventIsKept(t *testing.T) {
	for _, tc := range []struct{ endpoint, event string }{
		{"/submit", events.RequestResultSessionSubmit},
		{"/messages", events.RequestResultSessionMessage},
	} {
		t.Run(strings.TrimPrefix(tc.endpoint, "/"), func(t *testing.T) {
			e := newOpsEnv(t, "")
			cityLog := events.NewFake()
			live := &closableEvents{Fake: cityLog}
			e.fs.eventProv = live
			info := e.session(t, "Kept outcome")
			gp, release := e.gatedProvider(t)
			first := decodeAsyncAccepted(t, e.post(t, "/session/"+info.ID+tc.endpoint, "", `{"message":"once"}`).Body)
			gp.waitEntered(t)
			live.closed.Store(true)
			release()
			waitFor(t, testEventTimeout, "the event log refuses the terminal event", func() bool {
				return len(terminalEventsOf(live.refusedEvents(), first.RequestID)) == 1
			})
			requestFinished(t, e.fs, first.RequestID)

			if out := decodeRequestOutcome(t, getRequestOutcome(t, e.h, e.fs, first.RequestID, first.EventCursor)); out.State != "pending" || out.Event != nil {
				t.Fatalf("outcome while the terminal event is kept = %+v, want pending with no event", out)
			}

			live.closed.Store(false)
			out := decodeRequestOutcome(t, getRequestOutcome(t, e.h, e.fs, first.RequestID, first.EventCursor))
			recorded := terminalEvents(t, cityLog, first.RequestID)
			if len(recorded) != 1 || recorded[0].Type != tc.event {
				t.Fatalf("recorded terminal events = %+v, want the kept %s", recorded, tc.event)
			}
			if out.State != "succeeded" || out.Event == nil || out.Event.Seq != recorded[0].Seq {
				t.Fatalf("outcome once the log takes events = %+v, want succeeded at seq %d", out, recorded[0].Seq)
			}
		})
	}
}

// A session create's terminal event is kept like a message's when the event
// log refuses it, so its lookup answers pending until a log takes the event,
// and then the create's outcome, instead of losing the outcome for good.
func TestRequestOutcomeKeepsARefusedSessionCreateResult(t *testing.T) {
	fs := newSessionFakeState(t)
	cityLog := events.NewFake()
	live := &closableEvents{Fake: cityLog}
	fs.eventProv = live
	state := &gatedCommandableState{fakeState: fs, release: make(chan struct{})}
	h := newTestCityHandlerWith(t, state, New(state))

	rec := postSessionAction(t, h, cityURL(fs, "/sessions"), "", `{"kind":"agent","name":"myrig/worker","async":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)
	createResults := func(evs []events.Event) int {
		n := 0
		for _, e := range evs {
			var p struct {
				RequestID string `json:"request_id"`
			}
			if e.Type == events.RequestResultSessionCreate && json.Unmarshal(e.Payload, &p) == nil && p.RequestID == accepted.RequestID {
				n++
			}
		}
		return n
	}
	live.closed.Store(true)
	close(state.release)
	waitFor(t, testEventTimeout, "the event log refuses the create's result", func() bool {
		return createResults(live.refusedEvents()) == 1
	})
	requestFinished(t, fs, accepted.RequestID)

	if out := decodeRequestOutcome(t, getRequestOutcome(t, h, fs, accepted.RequestID, accepted.EventCursor)); out.Operation != RequestOperationSessionCreate || out.State != "pending" {
		t.Fatalf("outcome while the create's result is kept = %+v, want pending session.create", out)
	}

	live.closed.Store(false)
	out := decodeRequestOutcome(t, getRequestOutcome(t, h, fs, accepted.RequestID, accepted.EventCursor))
	if out.State != "succeeded" || out.Event == nil || out.Event.Type != events.RequestResultSessionCreate {
		t.Fatalf("outcome once the log takes events = %+v, want succeeded with request.result.session.create", out)
	}
	recorded, err := cityLog.List(events.Filter{Type: events.RequestResultSessionCreate})
	if err != nil {
		t.Fatalf("list create results: %v", err)
	}
	if got := createResults(recorded); got != 1 {
		t.Fatalf("recorded create results = %d, want the kept one", got)
	}
}
