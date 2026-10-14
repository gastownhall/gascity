package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// neverCommandableState defers session starts to a controller that never makes
// the session commandable, as when the start outlives the create wait.
type neverCommandableState struct {
	*fakeState
}

func (s *neverCommandableState) WaitForSessionCommandable(_ context.Context, _ string) (session.Info, error) {
	return session.Info{}, errors.New("session did not become commandable")
}

// TestSessionCreateFailureAfterCreateNamesTheSession pins the outcome of a
// create whose session bead was written but which then failed waiting to
// become commandable. The session exists, so request.failed must say which one:
// otherwise the client cannot find or clean it up, and a retry creates a second.
func TestSessionCreateFailureAfterCreateNamesTheSession(t *testing.T) {
	fs := newSessionFakeState(t)
	state := &neverCommandableState{fakeState: fs}
	h := newTestCityHandlerWith(t, state, New(state))

	body := `{"kind":"agent","name":"myrig/worker","alias":"late","async":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)

	failed := waitForRequestFailed(t, fs.eventProv, accepted.RequestID)

	created, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("session beads = %d, want the 1 the create wrote", len(created))
	}
	if failed.SessionID != created[0].ID {
		t.Fatalf("request.failed session_id = %q, want %q (the session the failed create left behind); payload: %+v", failed.SessionID, created[0].ID, failed)
	}
}

// stopRefusingProvider fails initial message delivery and then refuses the
// rollback's stop, so the session the create wrote outlives the failed create.
type stopRefusingProvider struct {
	*failNudgeProvider
}

func (p *stopRefusingProvider) Stop(string) error {
	return errors.New("stop refused")
}

// TestProviderSessionCreateRollbackFailureNamesTheSession pins the provider
// create path when initial message delivery fails and the rollback cannot
// remove the session it wrote. The session survives, so request.failed must
// name it, as the agent path does.
func TestProviderSessionCreateRollbackFailureNamesTheSession(t *testing.T) {
	fs := newSessionFakeState(t)
	provider := &stopRefusingProvider{&failNudgeProvider{Fake: runtime.NewFake(), err: errors.New("nudge failed")}}
	state := &stateWithSessionProvider{fakeState: fs, provider: provider}
	h := newTestCityHandlerWith(t, state, New(state))

	body := `{"kind":"provider","name":"test-agent","message":"hello"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)

	success, failed := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success != nil {
		t.Fatalf("session create succeeded unexpectedly: %+v", success)
	}
	if failed.ErrorCode != "message_delivery_failed" || !strings.Contains(failed.ErrorMessage, "rollback failed") {
		t.Fatalf("request.failed = %q: %q, want message_delivery_failed with a rollback failure", failed.ErrorCode, failed.ErrorMessage)
	}

	created, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("session beads = %d, want the 1 the failed rollback left behind", len(created))
	}
	if failed.SessionID != created[0].ID {
		t.Fatalf("request.failed session_id = %q, want %q (the session the failed rollback left behind); payload: %+v", failed.SessionID, created[0].ID, failed)
	}
}
