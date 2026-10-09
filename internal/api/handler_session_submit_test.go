package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestHandleSessionSubmitResumesOnlyWithResumeTrue is the owner ruling on
// D8 (CONTRACT v5.9): POST /submit resumes a suspended session only when the
// request carries resume: true. Without it the message queues and the
// session stays suspended, held for its operator. Kills an API submit that
// resumes by default, and one that ignores resume: true.
func TestHandleSessionSubmitResumesOnlyWithResumeTrue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		resume bool
	}{
		{name: "without resume queues", body: `{"message":"hello"}`},
		{name: "resume true resumes", body: `{"message":"hello","resume":true}`, resume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			h := newTestCityHandler(t, fs)

			info := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit Me")
			mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
			if err := mgr.Suspend(info.ID); err != nil {
				t.Fatalf("Suspend: %v", err)
			}

			req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/submit", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("submit status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
			}
			var accepted asyncAcceptedBody
			if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
				t.Fatalf("decode: %v", err)
			}

			success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID)
			if success == nil {
				t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
			}
			if success.Queued == tc.resume || fs.sp.IsRunning(info.SessionName) != tc.resume {
				t.Fatalf("queued = %v, running = %v; want resumed = %v", success.Queued, fs.sp.IsRunning(info.SessionName), tc.resume)
			}
			if success.Intent != string(session.SubmitIntentDefault) {
				t.Fatalf("intent = %q, want %q", success.Intent, session.SubmitIntentDefault)
			}
			b, err := fs.cityBeadStore.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if wantState := map[bool]string{false: "suspended", true: "active"}[tc.resume]; b.Metadata["state"] != wantState {
				t.Fatalf("state = %q, want %q", b.Metadata["state"], wantState)
			}
		})
	}
}

func TestHandleSessionSubmitUsesImmediateDefaultForCodex(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "helper", Title: "Codex Submit", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/submit", strings.NewReader(`{"message":"hello","resume":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var accepted asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if accepted.RequestID == "" {
		t.Fatal("missing request_id")
	}

	success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
}

func TestHandleSessionSubmitFollowUpQueuesMessage(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Queue Me")

	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/submit", strings.NewReader(`{"message":"later please","intent":"follow_up"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var accepted asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if accepted.RequestID == "" {
		t.Fatal("missing request_id")
	}

	success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
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
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Capabilities")
	if err := fs.cityBeadStore.Update(info.ID, beads.UpdateOpts{
		Metadata: map[string]string{
			"pool_managed": "true",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	h.ServeHTTP(rec, req)

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

func TestHandleSessionStopUsesSoftEscapeForCodex(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "helper", Title: "Codex", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := fs.cityBeadStore.Update(info.ID, beads.UpdateOpts{
		Metadata: map[string]string{"pool_managed": "true"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/stop", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stop status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var sawEscape, sawInterrupt bool
	for _, call := range fs.sp.Calls {
		if call.Method == "SendKeys" && call.Name == info.SessionName && call.Message == "Escape" {
			sawEscape = true
		}
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			sawInterrupt = true
		}
	}
	if !sawEscape {
		t.Fatalf("calls = %#v, want SendKeys(Escape)", fs.sp.Calls)
	}
	if sawInterrupt {
		t.Fatalf("calls = %#v, did not want Interrupt for codex stop", fs.sp.Calls)
	}
}

// TestHandleSessionMessageReportsQueued: POST /messages without resume: true
// to a held session queues the message, and its result says so (queued).
// Kills a result that reports a queued message as delivered.
func TestHandleSessionMessageReportsQueued(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Held")
	if err := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp).Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/")+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)
	success, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("message failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if !success.Queued {
		t.Fatal("queued = false for a message queued on a held session")
	}
}
