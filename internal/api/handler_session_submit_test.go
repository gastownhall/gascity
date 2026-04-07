package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestHandleSessionSubmitDefaultsToProviderDefaultBehavior(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit Me")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	req := newPostRequest("/v0/session/"+info.ID+"/submit", strings.NewReader(`{"message":"hello"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp["queued"]; got != false {
		t.Fatalf("queued = %#v, want false", got)
	}
	if got := resp["intent"]; got != string(session.SubmitIntentDefault) {
		t.Fatalf("intent = %#v, want %q", got, session.SubmitIntentDefault)
	}
	if !fs.sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after POST /submit")
	}
	found := false
	for _, call := range fs.sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want Nudge(hello)", fs.sp.Calls)
	}
}

func TestHandleSessionSubmitUsesImmediateDefaultForCodex(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "helper", "Codex Submit", "codex", t.TempDir(), "codex", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	req := newPostRequest("/v0/session/"+info.ID+"/submit", strings.NewReader(`{"message":"hello"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	found := false
	for _, call := range fs.sp.Calls {
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want NudgeNow(hello)", fs.sp.Calls)
	}
}

func TestHandleSessionSubmitFollowUpQueuesMessage(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Queue Me")

	req := newPostRequest("/v0/session/"+info.ID+"/submit", strings.NewReader(`{"message":"later please","intent":"follow_up"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp["queued"]; got != true {
		t.Fatalf("queued = %#v, want true", got)
	}
	state, err := nudgequeue.LoadState(fs.cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending queued submits = %d, want 1", len(state.Pending))
	}
	item := state.Pending[0]
	if item.SessionID != info.ID {
		t.Fatalf("SessionID = %q, want %q", item.SessionID, info.ID)
	}
	if item.Message != "later please" {
		t.Fatalf("Message = %q, want %q", item.Message, "later please")
	}
}

func TestHandleSessionGetIncludesSubmissionCapabilities(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Capabilities")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v0/session/"+info.ID, nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.SubmissionCapabilities.SupportsFollowUp {
		t.Fatal("SupportsFollowUp = false, want true")
	}
	if !resp.SubmissionCapabilities.SupportsInterruptNow {
		t.Fatal("SupportsInterruptNow = false, want true")
	}
}

func TestHandleSessionStopUsesInterruptForCodex(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "helper", "Codex", "codex", t.TempDir(), "codex", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest("/v0/session/"+info.ID+"/stop", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stop status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	// StopTurn (backing /stop) always uses SIGINT regardless of provider.
	// Soft Escape is reserved for the submit interrupt_now path.
	var sawInterrupt bool
	for _, call := range fs.sp.Calls {
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
		}
	}
	if !sawInterrupt {
		t.Fatalf("calls = %#v, want Interrupt for StopTurn", fs.sp.Calls)
	}
}
