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

func TestHandleSessionSubmitDefaultsToProviderDefaultBehavior(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Submit Me")
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/submit", strings.NewReader(`{"message":"hello"}`))
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
	// Default intent on a suspended session resumes immediately (not queued).
	if success.Queued {
		t.Fatalf("queued = true, want false (default intent resumes)")
	}
	if success.Intent != string(session.SubmitIntentDefault) {
		t.Fatalf("intent = %q, want %q", success.Intent, session.SubmitIntentDefault)
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

	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/submit", strings.NewReader(`{"message":"hello"}`))
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

// TestSessionSubmitAndMessageRejectPendingInteractionWith409 pins the #2892
// guard at the API: while the session is showing a permission prompt, a
// submit or message is refused synchronously with 409 pending_interaction
// instead of being accepted (202) and then typed into the prompt, where its
// Enter keystroke would approve the pending tool call.
func TestSessionSubmitAndMessageRejectPendingInteractionWith409(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Awaiting Approval")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "tmux-abc123",
		Kind:      "approval",
		Prompt:    "Bash: date > bash-ran.txt",
		Options:   []string{"Yes", "No"},
	})

	for _, tc := range []struct{ path, body string }{
		{"/submit", `{"message":"hello?"}`},
		{"/submit", `{"message":"hello?","intent":"follow_up"}`},
		{"/submit", `{"message":"hello?","intent":"interrupt_now"}`},
		{"/messages", `{"message":"hello?"}`},
	} {
		req := newPostRequest(cityURL(fs, "/session/")+info.ID+tc.path, strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusConflict {
			t.Fatalf("%s %s status = %d, want 409; body: %s", tc.path, tc.body, rec.Code, rec.Body.String())
		}
		var problem struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&problem); err != nil {
			t.Fatalf("decode problem: %v", err)
		}
		if problem.Code != "session-conflict" || !strings.HasPrefix(problem.Detail, "pending_interaction: ") {
			t.Fatalf("%s problem = %+v, want session-conflict with a pending_interaction: detail", tc.path, problem)
		}
		if !strings.Contains(problem.Detail, "tmux-abc123") {
			t.Fatalf("%s detail = %q, want it to name the pending request", tc.path, problem.Detail)
		}
	}

	for _, call := range fs.sp.Calls {
		if (call.Method == "Nudge" || call.Method == "NudgeNow" || call.Method == "SendKeys") && call.Name == info.SessionName {
			t.Fatalf("session received %s while a permission prompt was pending", call.Method)
		}
	}
	if pending, _ := fs.sp.Pending(info.SessionName); pending == nil {
		t.Fatal("pending interaction was cleared; it must stay pending")
	}
}
