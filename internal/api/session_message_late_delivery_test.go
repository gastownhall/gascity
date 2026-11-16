package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// lockedLogBuffer is a log destination a test can read, and wait on, while
// the handler's goroutine writes to it.
type lockedLogBuffer struct {
	mu      sync.Mutex
	buf     strings.Builder
	written chan struct{} // closed and replaced on every write
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	close(b.written)
	b.written = make(chan struct{})
	return n, err
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor blocks until the log contains substr, failing the test if it does
// not appear within testEventTimeout. It wakes on each write instead of
// polling.
func (b *lockedLogBuffer) waitFor(t *testing.T, substr string) {
	t.Helper()
	deadline := time.NewTimer(testEventTimeout)
	defer deadline.Stop()
	for {
		b.mu.Lock()
		found := strings.Contains(b.buf.String(), substr)
		written := b.written
		b.mu.Unlock()
		if found {
			return
		}
		select {
		case <-written:
		case <-deadline.C:
			t.Fatalf("logs = %q, want %q", b.String(), substr)
		}
	}
}

// captureLockedLog redirects the standard logger to a buffer for the test.
func captureLockedLog(t *testing.T) *lockedLogBuffer {
	t.Helper()
	logs := &lockedLogBuffer{written: make(chan struct{})}
	oldOutput, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
	})
	return logs
}

// sessionMessageTerminalEvents returns every terminal event recorded for one
// session.message request: its success events and its request.failed events.
func sessionMessageTerminalEvents(t *testing.T, prov events.Provider, requestID string) (successes []SessionMessageSucceededPayload, failures []RequestFailedPayload) {
	t.Helper()
	successEvents, err := prov.List(events.Filter{Type: events.RequestResultSessionMessage})
	if err != nil {
		t.Fatalf("list %s: %v", events.RequestResultSessionMessage, err)
	}
	for _, e := range successEvents {
		var p SessionMessageSucceededPayload
		if json.Unmarshal(e.Payload, &p) == nil && requestIDMatches(p.RequestID, requestID) {
			successes = append(successes, p)
		}
	}
	failedEvents, err := prov.List(events.Filter{Type: events.RequestFailed})
	if err != nil {
		t.Fatalf("list %s: %v", events.RequestFailed, err)
	}
	for _, e := range failedEvents {
		var p RequestFailedPayload
		if json.Unmarshal(e.Payload, &p) == nil && p.Operation == RequestOperationSessionMessage && requestIDMatches(p.RequestID, requestID) {
			failures = append(failures, p)
		}
	}
	return successes, failures
}

// A delivery that outlasts the API's wait must not be reported as failed when
// it later succeeds. The request's one terminal event is its real outcome, and
// a keyed retry (before or after the delivery lands) replays the same
// request_id, whose outcome is that real result rather than a false failure.
func TestHandleSessionMessageSlowDeliveryReportsRealOutcome(t *testing.T) {
	fs := newSessionFakeState(t)
	blocker := &blockingNudgeProvider{
		Fake:    fs.sp,
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(blocker.unblock) }) }
	t.Cleanup(unblock)
	prevWait := sessionMessageSlowDeliveryLogAfter
	sessionMessageSlowDeliveryLogAfter = time.Nanosecond
	t.Cleanup(func() { sessionMessageSlowDeliveryLogAfter = prevWait })
	// The bound of a recorded /messages: an unrecorded one (ops.content off)
	// ignores it.
	prevBound := sessionMessageAsyncTimeout
	sessionMessageAsyncTimeout = time.Nanosecond
	t.Cleanup(func() { sessionMessageAsyncTimeout = prevBound })
	logs := captureLockedLog(t)

	srv := New(&stateWithSessionProvider{fakeState: fs, provider: blocker})
	h := newTestCityHandlerWith(t, fs, srv)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "slow-message")
	url := cityURL(fs, "/session/") + info.ID + "/messages"
	const body = `{"message":"hello"}`

	first := postSessionAction(t, h, url, "slow-msg-1", body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("message status = %d, want %d; body: %s", first.Code, http.StatusAccepted, first.Body.String())
	}
	accepted := decodeAsyncAccepted(t, first.Body)

	select {
	case <-blocker.started:
	case <-time.After(testEventTimeout):
		t.Fatal("provider nudge was not reached")
	}

	// The API's wait boundary has passed for this request once it logs that
	// the delivery is still in flight.
	logs.waitFor(t, "session.message still in flight")
	if successes, failures := sessionMessageTerminalEvents(t, fs.eventProv, accepted.RequestID); len(successes)+len(failures) != 0 {
		t.Fatalf("terminal events while delivery is still in flight: successes=%+v failures=%+v; want none", successes, failures)
	}

	retry := postSessionAction(t, h, url, "slow-msg-1", body)
	if retry.Code != http.StatusAccepted {
		t.Fatalf("keyed retry status = %d, want %d; body: %s", retry.Code, http.StatusAccepted, retry.Body.String())
	}
	if got := decodeAsyncAccepted(t, retry.Body); got.RequestID != accepted.RequestID {
		t.Fatalf("keyed retry request_id = %q, want replay of %q", got.RequestID, accepted.RequestID)
	}

	unblock()
	success, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("delivered message reported as failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if success.SessionID != info.ID {
		t.Fatalf("success session_id = %q, want %q", success.SessionID, info.ID)
	}

	again := postSessionAction(t, h, url, "slow-msg-1", body)
	if got := decodeAsyncAccepted(t, again.Body); got.RequestID != accepted.RequestID {
		t.Fatalf("keyed retry after delivery request_id = %q, want replay of %q", got.RequestID, accepted.RequestID)
	}
	successes, failures := sessionMessageTerminalEvents(t, fs.eventProv, accepted.RequestID)
	if len(successes) != 1 || len(failures) != 0 {
		t.Fatalf("terminal events = successes %+v, failures %+v; want exactly one success", successes, failures)
	}
	if n := countSessionNudges(fs.sp, info.SessionName, "hello"); n != 1 {
		t.Fatalf("nudges = %d, want 1 (keyed retries must not redeliver)", n)
	}
}
