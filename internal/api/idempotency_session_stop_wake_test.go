package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests pin the Idempotency-Key wire contract on the body-less session
// control endpoints (/stop, /wake): a client whose request timed out can retry
// with the same key and get the original response back without interrupting a
// second turn or recording a second wake. Neither endpoint takes a request
// body, so the key's scope is the action on the addressed target.

// countSessionEnqueues counts reconcile pokes for one session.
func countSessionEnqueues(fs *fakeState, id string) int {
	n := 0
	want := reconcilekey.Session(id).Normalize()
	for _, k := range fs.enqueuedKeys() {
		if k == want {
			n++
		}
	}
	return n
}

func TestSessionStopIdempotentReplay(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Stop Replay")
	url := cityURL(fs, "/session/") + info.ID + "/stop"

	first := postSessionAction(t, h, url, "stop-1", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first stop: status = %d, want 200; body = %s", first.Code, first.Body.String())
	}
	replay := postSessionAction(t, h, url, "stop-1", "")
	if replay.Code != http.StatusOK {
		t.Fatalf("replay: status = %d, want 200; body = %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replay body = %s, want first-response body %s", replay.Body.String(), first.Body.String())
	}
	if got := fs.sp.CountCalls("Interrupt", info.SessionName); got != 1 {
		t.Fatalf("Interrupt calls = %d, want 1 (replay interrupted a second turn); calls = %#v", got, fs.sp.SnapshotCalls())
	}
}

func TestSessionStopWithoutKeyStopsEachRequest(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Stop No Key")
	url := cityURL(fs, "/session/") + info.ID + "/stop"

	for i := 0; i < 2; i++ {
		rec := postSessionAction(t, h, url, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("stop %d: status = %d, want 200; body = %s", i, rec.Code, rec.Body.String())
		}
	}
	if got := fs.sp.CountCalls("Interrupt", info.SessionName); got != 2 {
		t.Fatalf("Interrupt calls = %d, want 2 without Idempotency-Key", got)
	}
}

// failingInterruptProvider fails Interrupt while fail is set.
type failingInterruptProvider struct {
	*runtime.Fake
	fail atomic.Bool
}

func (p *failingInterruptProvider) Interrupt(name string) error {
	if err := p.Fake.Interrupt(name); err != nil {
		return err
	}
	if p.fail.Load() {
		return errors.New("interrupt failed")
	}
	return nil
}

func TestSessionStopFailureReleasesKey(t *testing.T) {
	fs := newSessionFakeState(t)
	failing := &failingInterruptProvider{Fake: fs.sp}
	fs.sessionProvider = failing
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Stop Retry")
	url := cityURL(fs, "/session/") + info.ID + "/stop"

	failing.fail.Store(true)
	failed := postSessionAction(t, h, url, "stop-1", "")
	if failed.Code == http.StatusOK {
		t.Fatalf("stop with failing interrupt: status = 200, want an error; body = %s", failed.Body.String())
	}
	failing.fail.Store(false)

	retry := postSessionAction(t, h, url, "stop-1", "")
	if retry.Code != http.StatusOK {
		t.Fatalf("retry after failure: status = %d, want 200 (failed stop pinned the key); body = %s", retry.Code, retry.Body.String())
	}
	if got := fs.sp.CountCalls("Interrupt", info.SessionName); got != 2 {
		t.Fatalf("Interrupt calls = %d, want 2 (failed attempt + retry)", got)
	}
}

// blockingInterruptProvider holds Interrupt open until release is closed so a
// concurrent same-key stop observes the in-flight claim.
type blockingInterruptProvider struct {
	*runtime.Fake
	enterOnce sync.Once
	entered   chan struct{}
	release   chan struct{}
}

func (p *blockingInterruptProvider) Interrupt(name string) error {
	p.enterOnce.Do(func() { close(p.entered) })
	<-p.release
	return p.Fake.Interrupt(name)
}

func TestSessionStopInFlightReturnsConflict(t *testing.T) {
	fs := newSessionFakeState(t)
	blocking := &blockingInterruptProvider{Fake: fs.sp, entered: make(chan struct{}), release: make(chan struct{})}
	fs.sessionProvider = blocking
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Stop In Flight")
	url := cityURL(fs, "/session/") + info.ID + "/stop"

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- postSessionAction(t, h, url, "stop-1", "") }()
	<-blocking.entered

	inFlightDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { inFlightDone <- postSessionAction(t, h, url, "stop-1", "") }()
	var inFlight *httptest.ResponseRecorder
	select {
	case inFlight = <-inFlightDone:
	case <-time.After(30 * time.Second):
		close(blocking.release)
		t.Fatal("same-key stop blocked behind the in-flight stop instead of returning 409")
	}
	if inFlight.Code != http.StatusConflict || !strings.Contains(inFlight.Body.String(), "idempotency-in-flight") {
		t.Fatalf("in-flight repeat: status = %d body = %s, want 409 idempotency-in-flight", inFlight.Code, inFlight.Body.String())
	}

	close(blocking.release)
	first := <-firstDone
	if first.Code != http.StatusOK {
		t.Fatalf("first stop: status = %d, want 200; body = %s", first.Code, first.Body.String())
	}
	replay := postSessionAction(t, h, url, "stop-1", "")
	if replay.Code != http.StatusOK || replay.Body.String() != first.Body.String() {
		t.Fatalf("replay after completion: status = %d body = %s, want 200 %s", replay.Code, replay.Body.String(), first.Body.String())
	}
	if got := fs.sp.CountCalls("Interrupt", info.SessionName); got != 1 {
		t.Fatalf("Interrupt calls = %d, want 1", got)
	}
}

func suspendForWakeTest(t *testing.T, fs *fakeState, id string) {
	t.Helper()
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	if err := mgr.Suspend(context.Background(), testOperatorIn(t, fs.cityPath), id, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
}

func TestSessionWakeIdempotentReplay(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Wake Replay")
	suspendForWakeTest(t, fs, info.ID)
	url := cityURL(fs, "/session/") + info.ID + "/wake"

	first := postSessionAction(t, h, url, "wake-1", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first wake: status = %d, want 200; body = %s", first.Code, first.Body.String())
	}
	// Consume the recorded wake as the controller would, so a replay that
	// records a second wake is visible on the bead.
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, session.ClearWakeRequestPatch()); err != nil {
		t.Fatalf("clearing wake request: %v", err)
	}

	replay := postSessionAction(t, h, url, "wake-1", "")
	if replay.Code != http.StatusOK {
		t.Fatalf("replay: status = %d, want 200; body = %s", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replay body = %s, want first-response body %s", replay.Body.String(), first.Body.String())
	}
	if got := countSessionEnqueues(fs, info.ID); got != 1 {
		t.Fatalf("session enqueues = %d, want 1 (replay recorded a second wake)", got)
	}
	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.Metadata["wake_request"] != "" || b.Metadata["wake_requested_at"] != "" {
		t.Fatalf("wake_request = %q wake_requested_at = %q after replay, want both empty (replay recorded a second wake)", b.Metadata["wake_request"], b.Metadata["wake_requested_at"])
	}
}

func TestSessionWakeWithoutKeyWakesEachRequest(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Wake No Key")
	suspendForWakeTest(t, fs, info.ID)
	url := cityURL(fs, "/session/") + info.ID + "/wake"

	for i := 0; i < 2; i++ {
		rec := postSessionAction(t, h, url, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("wake %d: status = %d, want 200; body = %s", i, rec.Code, rec.Body.String())
		}
	}
	if got := countSessionEnqueues(fs, info.ID); got != 2 {
		t.Fatalf("session enqueues = %d, want 2 without Idempotency-Key", got)
	}
}

// TestSessionWakeRefusalReleasesKey pins that an error outcome (409
// wake-will-not-start) is not cached: a retry with the same key re-runs the
// wake once the controller would start the session.
func TestSessionWakeRefusalReleasesKey(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Wake Retry")
	suspendForWakeTest(t, fs, info.ID)
	state := &wakeRefusingState{fakeState: fs, refusal: "rig \"r\" is suspended"}
	h := newTestCityHandlerWith(t, state, New(state))
	url := cityURL(fs, "/session/") + info.ID + "/wake"

	refused := postSessionAction(t, h, url, "wake-1", "")
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), "will not start") {
		t.Fatalf("refused wake: status = %d body = %s, want 409 will not start", refused.Code, refused.Body.String())
	}
	state.refusal = ""
	retry := postSessionAction(t, h, url, "wake-1", "")
	if retry.Code != http.StatusOK {
		t.Fatalf("retry after refusal: status = %d, want 200 (refusal pinned the key); body = %s", retry.Code, retry.Body.String())
	}
	if got := countSessionEnqueues(fs, info.ID); got != 2 {
		t.Fatalf("session enqueues = %d, want 2 (refused attempt + retry)", got)
	}
}

// blockingWakeState holds the controller's will-not-start check open until
// release is closed so a concurrent same-key wake observes the in-flight claim.
type blockingWakeState struct {
	*fakeState
	enterOnce sync.Once
	entered   chan struct{}
	release   chan struct{}
}

func (s *blockingWakeState) WakeStartRefusal(session.Info) (string, bool) {
	s.enterOnce.Do(func() { close(s.entered) })
	<-s.release
	return "", true
}

func TestSessionWakeInFlightReturnsConflict(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Wake In Flight")
	suspendForWakeTest(t, fs, info.ID)
	state := &blockingWakeState{fakeState: fs, entered: make(chan struct{}), release: make(chan struct{})}
	h := newTestCityHandlerWith(t, state, New(state))
	url := cityURL(fs, "/session/") + info.ID + "/wake"

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- postSessionAction(t, h, url, "wake-1", "") }()
	<-state.entered

	inFlightDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { inFlightDone <- postSessionAction(t, h, url, "wake-1", "") }()
	var inFlight *httptest.ResponseRecorder
	select {
	case inFlight = <-inFlightDone:
	case <-time.After(30 * time.Second):
		close(state.release)
		t.Fatal("same-key wake blocked behind the in-flight wake instead of returning 409")
	}
	if inFlight.Code != http.StatusConflict || !strings.Contains(inFlight.Body.String(), "idempotency-in-flight") {
		t.Fatalf("in-flight repeat: status = %d body = %s, want 409 idempotency-in-flight", inFlight.Code, inFlight.Body.String())
	}

	close(state.release)
	first := <-firstDone
	if first.Code != http.StatusOK {
		t.Fatalf("first wake: status = %d, want 200; body = %s", first.Code, first.Body.String())
	}
	replay := postSessionAction(t, h, url, "wake-1", "")
	if replay.Code != http.StatusOK || replay.Body.String() != first.Body.String() {
		t.Fatalf("replay after completion: status = %d body = %s, want 200 %s", replay.Code, replay.Body.String(), first.Body.String())
	}
	if got := countSessionEnqueues(fs, info.ID); got != 1 {
		t.Fatalf("session enqueues = %d, want 1", got)
	}
}

// TestSessionStopAndWakeKeepSeparateScopes pins that one Idempotency-Key used
// on /stop and /wake of the same session, or on one action of two sessions, never
// replays another action's or another target's response.
func TestSessionStopAndWakeKeepSeparateScopes(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	a := createTestSession(t, fs.cityBeadStore, fs.sp, "Scope A")
	b := createTestSession(t, fs.cityBeadStore, fs.sp, "Scope B")
	base := cityURL(fs, "/session/")

	for _, info := range []session.Info{a, b} {
		rec := postSessionAction(t, h, base+info.ID+"/stop", "shared-key", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("stop %s: status = %d, want 200; body = %s", info.ID, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), info.ID) {
			t.Fatalf("stop %s body = %s, want its own id (replayed another target)", info.ID, rec.Body.String())
		}
		if got := fs.sp.CountCalls("Interrupt", info.SessionName); got != 1 {
			t.Fatalf("Interrupt calls for %s = %d, want 1", info.ID, got)
		}
	}

	for _, info := range []session.Info{a, b} {
		suspendForWakeTest(t, fs, info.ID)
		rec := postSessionAction(t, h, base+info.ID+"/wake", "shared-key", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("wake %s: status = %d, want 200; body = %s", info.ID, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), info.ID) {
			t.Fatalf("wake %s body = %s, want its own id (replayed another action or target)", info.ID, rec.Body.String())
		}
		if got := countSessionEnqueues(fs, info.ID); got != 1 {
			t.Fatalf("session enqueues for %s = %d, want 1 (wake replayed another action's or target's response)", info.ID, got)
		}
	}
}
