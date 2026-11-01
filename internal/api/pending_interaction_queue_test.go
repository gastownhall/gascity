package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// pendingEverywhereProvider reports a permission prompt pending in every
// session, as a fresh session that comes up showing a dialog does.
type pendingEverywhereProvider struct{ *runtime.Fake }

func (p *pendingEverywhereProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	if _, err := p.Fake.Pending(name); err != nil {
		return nil, err
	}
	return &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "Trust this folder?"}, nil
}

// queuedForSession lists the deliveries queued under cityPath for session id.
func queuedForSession(t *testing.T, cityPath, id string) []nudgequeue.Item {
	t.Helper()
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	var out []nudgequeue.Item
	for _, item := range state.Pending {
		if item.SessionID == id {
			out = append(out, item)
		}
	}
	return out
}

// TestExtmsgNotifyQueuesBehindPending (row A14): an extmsg notice to a
// session waiting on a permission prompt is queued for after the prompt is
// answered, not refused, logged and dropped.
func TestExtmsgNotifyQueuesBehindPending(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Blocked")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "Allow Bash?"})

	if err := srv.sendBackgroundMessageToSession(context.Background(), fs.cityBeadStore, info.ID, "new message in #ops"); err != nil {
		t.Fatalf("sendBackgroundMessageToSession = %v, want the notice queued", err)
	}
	if got := fs.sp.CountCalls("Nudge", info.SessionName) + fs.sp.CountCalls("NudgeNow", info.SessionName); got != 0 {
		t.Fatalf("typed %d notices into the pending prompt, want none", got)
	}
	items := queuedForSession(t, fs.cityPath, info.ID)
	if len(items) != 1 || items[0].Message != "new message in #ops" {
		t.Fatalf("queued %+v, want the notice queued for the session", items)
	}
}

// TestCreateWithInitialMessageQueuesBehindPending (row A17): a provider
// session that comes up waiting on a pending interaction keeps the session;
// its initial message is queued as the user's first turn for after the
// interaction is answered, and the create succeeds rather than rolling back.
func TestCreateWithInitialMessageQueuesBehindPending(t *testing.T) {
	fs := newSessionFakeState(t)
	provider := &pendingEverywhereProvider{Fake: runtime.NewFake()}
	wrappedState := &stateWithSessionProvider{fakeState: fs, provider: provider}
	srv := New(wrappedState)
	h := newTestCityHandlerWith(t, wrappedState, srv)

	body := `{"kind":"provider","name":"test-agent","message":"hello","title":"Blocked at start"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	accepted := decodeAsyncAccepted(t, rec.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if failure != nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if success == nil {
		t.Fatal("expected a session create success event")
	}
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, provider, session.WithCityPath(fs.cityPath))
	sessions, err := mgr.List("", "")
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want the created session kept: %+v", len(sessions), sessions)
	}
	if got := provider.CountCalls("Nudge", sessions[0].SessionName) + provider.CountCalls("NudgeNow", sessions[0].SessionName); got != 0 {
		t.Fatalf("typed %d messages into the pending prompt, want none", got)
	}
	items := queuedForSession(t, fs.cityPath, sessions[0].ID)
	if len(items) != 1 || items[0].Message != "hello" || items[0].Source != nudgequeue.SourceSubmit {
		t.Fatalf("queued %+v, want the initial message queued as the user's first turn", items)
	}
}
