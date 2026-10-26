package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// These tests pin the Idempotency-Key wire contract on POST /sessions: a
// client that lost the 202 can retry with the same key and get the original
// request_id back, whether the first create is still running or has finished,
// without a second session being created.

// gatedCommandableState defers session starts to a controller that holds each
// create in WaitForSessionCommandable until release is closed, so a test can
// retry while the first create is still in flight.
type gatedCommandableState struct {
	*fakeState
	release chan struct{}
}

func (s *gatedCommandableState) WaitForSessionCommandable(ctx context.Context, id string) (session.Info, error) {
	select {
	case <-s.release:
	case <-ctx.Done():
		return session.Info{}, ctx.Err()
	}
	return session.Info{ID: id, State: session.StateActive}, nil
}

func sessionBeadIDs(t *testing.T, fs *fakeState) []string {
	t.Helper()
	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", session.LabelSession, err)
	}
	ids := make([]string, 0, len(all))
	for _, b := range all {
		ids = append(ids, b.ID)
	}
	return ids
}

func TestSessionCreateIdempotentReplay(t *testing.T) {
	fs := newSessionFakeState(t)
	state := &gatedCommandableState{fakeState: fs, release: make(chan struct{})}
	h := newTestCityHandlerWith(t, state, New(state))
	url := cityURL(fs, "/sessions")
	body := `{"kind":"agent","name":"myrig/worker","async":true}`

	first := postSessionAction(t, h, url, "create-1", body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first create: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}
	firstBody := first.Body.String()
	accepted := decodeAsyncAccepted(t, strings.NewReader(firstBody))

	// Retry while the first create is still waiting to become commandable.
	pending := postSessionAction(t, h, url, "create-1", body)
	if pending.Code != http.StatusAccepted {
		t.Fatalf("retry while pending: status = %d, want 202; body = %s", pending.Code, pending.Body.String())
	}
	if pending.Body.String() != firstBody {
		t.Fatalf("retry while pending: body = %s, want first-response body %s", pending.Body.String(), firstBody)
	}

	close(state.release)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}

	// Retry after the first create has completed.
	done := postSessionAction(t, h, url, "create-1", body)
	if done.Code != http.StatusAccepted {
		t.Fatalf("retry after completion: status = %d, want 202; body = %s", done.Code, done.Body.String())
	}
	if done.Body.String() != firstBody {
		t.Fatalf("retry after completion: body = %s, want first-response body %s", done.Body.String(), firstBody)
	}

	ids := sessionBeadIDs(t, fs)
	if len(ids) != 1 {
		t.Fatalf("session beads = %v, want exactly 1 (a keyed retry created another session)", ids)
	}
	if success.Session.ID != ids[0] {
		t.Fatalf("request.result session id = %q, want the one created session %q", success.Session.ID, ids[0])
	}
}

func TestProviderSessionCreateIdempotentReplay(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	url := cityURL(fs, "/sessions")
	body := `{"kind":"provider","name":"test-agent"}`

	first := postSessionAction(t, h, url, "create-1", body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first create: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}
	firstBody := first.Body.String()
	accepted := decodeAsyncAccepted(t, strings.NewReader(firstBody))
	if success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID); success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}

	replay := postSessionAction(t, h, url, "create-1", body)
	if replay.Code != http.StatusAccepted || replay.Body.String() != firstBody {
		t.Fatalf("replay: status = %d body = %s, want 202 with first-response body %s", replay.Code, replay.Body.String(), firstBody)
	}
	if ids := sessionBeadIDs(t, fs); len(ids) != 1 {
		t.Fatalf("session beads = %v, want exactly 1 (a keyed retry created another session)", ids)
	}
}

func TestSessionCreateIdempotencyMismatch(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	url := cityURL(fs, "/sessions")

	first := postSessionAction(t, h, url, "create-1", `{"kind":"agent","name":"myrig/worker","title":"first"}`)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first create: status = %d, want 202; body = %s", first.Code, first.Body.String())
	}
	accepted := decodeAsyncAccepted(t, first.Body)

	mismatch := postSessionAction(t, h, url, "create-1", `{"kind":"agent","name":"myrig/worker","title":"second"}`)
	if mismatch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch: status = %d, want 422; body = %s", mismatch.Code, mismatch.Body.String())
	}
	if !strings.Contains(mismatch.Body.String(), "idempotency") {
		t.Fatalf("mismatch body = %s, want idempotency problem", mismatch.Body.String())
	}

	if success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID); success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if ids := sessionBeadIDs(t, fs); len(ids) != 1 {
		t.Fatalf("session beads = %v, want exactly 1 (the mismatched body created a session)", ids)
	}
}

func TestSessionCreateWithoutKeyCreatesEachRequest(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	url := cityURL(fs, "/sessions")
	body := `{"kind":"agent","name":"myrig/worker"}`

	var requestIDs []string
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newPostRequest(url, strings.NewReader(body)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("create %d: status = %d, want 202; body = %s", i, rec.Code, rec.Body.String())
		}
		requestIDs = append(requestIDs, decodeAsyncAccepted(t, rec.Body).RequestID)
	}
	if requestIDs[0] == requestIDs[1] {
		t.Fatalf("keyless creates share request_id %q, want distinct requests", requestIDs[0])
	}
	for _, id := range requestIDs {
		if success, failure := waitForSessionCreateResult(t, fs.eventProv, id); success == nil {
			t.Fatalf("session create %s failed: %s: %s", id, failure.ErrorCode, failure.ErrorMessage)
		}
	}
	if ids := sessionBeadIDs(t, fs); len(ids) != 2 {
		t.Fatalf("session beads = %v, want 2 without Idempotency-Key", ids)
	}
}
