package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

func newSessionFakeState(t *testing.T) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	return fs
}

func createTestSession(t *testing.T, store beads.Store, sp *runtime.Fake, title string) session.Info {
	t.Helper()
	mgr := session.NewManager(store, sp)
	info, err := mgr.Create(context.Background(), "default", title, "echo test", "/tmp", "test", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return info
}

func writeNamedSessionJSONL(t *testing.T, searchBase, workDir, fileName string, lines ...string) {
	t.Helper()
	dir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName)
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandleSessionList(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	// Create two sessions.
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session A")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session B")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp listResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("got total %d, want 2", resp.Total)
	}
}

func TestHandleSessionListFilterByState(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Suspend")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Stay Active")

	// Suspend one.
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// List only active.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions?state=active", nil)
	srv.ServeHTTP(w, r)

	var resp listResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("got total %d, want 1 (only active)", resp.Total)
	}
}

func TestHandleSessionGet(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID, nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != info.ID {
		t.Errorf("got ID %q, want %q", resp.ID, info.ID)
	}
	if resp.Title != "My Session" {
		t.Errorf("got title %q, want %q", resp.Title, "My Session")
	}
	if resp.State != "active" {
		t.Errorf("got state %q, want %q", resp.State, "active")
	}
}

func TestHandleSessionGetNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/nonexistent", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleSessionSuspend(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Suspend")

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/suspend", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify the session is now suspended.
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != session.StateSuspended {
		t.Errorf("got state %q, want %q", got.State, session.StateSuspended)
	}
}

func TestHandleSessionClose(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Close")

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/close", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Session should no longer appear in default listing (excludes closed).
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	sessions, err := mgr.List("", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("got %d sessions after close, want 0", len(sessions))
	}
}

func TestHandleSessionNoCityStore(t *testing.T) {
	fs := newFakeState(t) // no cityBeadStore set
	srv := New(fs)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleSessionWake(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Held Session")

	// Set hold metadata.
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"held_until":   "9999-12-31T23:59:59Z",
		"sleep_reason": "user-hold",
	})

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/wake", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify hold cleared.
	b, _ := fs.cityBeadStore.Get(info.ID)
	if b.Metadata["held_until"] != "" {
		t.Errorf("held_until should be cleared, got %q", b.Metadata["held_until"])
	}
	if b.Metadata["sleep_reason"] != "" {
		t.Errorf("sleep_reason should be cleared, got %q", b.Metadata["sleep_reason"])
	}
}

func TestHandleSessionWakeClosed(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Closed Session")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	_ = mgr.Close(info.ID)

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/wake", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionGetByTemplateName(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Named Session")

	// Set template metadata on the bead so name resolution works.
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"template": "overseer",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/overseer", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != info.ID {
		t.Errorf("got ID %q, want %q", resp.ID, info.ID)
	}
}

func TestHandleSessionPatchTitle(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")

	body := `{"title":"Updated Title"}`
	req := httptest.NewRequest("PATCH", "/v0/session/"+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Title != "Updated Title" {
		t.Errorf("got title %q, want %q", resp.Title, "Updated Title")
	}
}

func TestHandleSessionPatchImmutableField(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")

	body := `{"template":"hacked"}`
	req := httptest.NewRequest("PATCH", "/v0/session/"+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
}

func TestHandleSessionListIncludesReason(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Held")

	// Set sleep reason on bead.
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"sleep_reason": "user-hold",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	// Parse into raw JSON to check for reason field.
	var raw struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(raw.Items))
	}
	var item sessionResponse
	if err := json.Unmarshal(raw.Items[0], &item); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	if item.Reason != "user-hold" {
		t.Errorf("got reason %q, want %q", item.Reason, "user-hold")
	}
}

func TestHandleSessionRename(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")

	body := `{"title":"Renamed"}`
	req := newPostRequest("/v0/session/"+info.ID+"/rename", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Title != "Renamed" {
		t.Errorf("got title %q, want %q", resp.Title, "Renamed")
	}
}

func TestHandleSessionRenameEmptyTitle(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")

	body := `{"title":""}`
	req := newPostRequest("/v0/session/"+info.ID+"/rename", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleSessionAmbiguousTemplateName(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	// Create two sessions with the same template name.
	info1 := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker 1")
	info2 := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker 2")
	_ = fs.cityBeadStore.SetMetadataBatch(info1.ID, map[string]string{"template": "worker"})
	_ = fs.cityBeadStore.SetMetadataBatch(info2.ID, map[string]string{"template": "worker"})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/worker", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d (ambiguous); body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionGetEnrichment(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Enriched Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID, nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !resp.Running {
		t.Error("running = false, want true for active session")
	}
	if resp.DisplayName != "Test" {
		t.Errorf("display_name = %q, want %q", resp.DisplayName, "Test")
	}
}

func TestHandleSessionListPeek(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	createTestSession(t, fs.cityBeadStore, fs.sp, "Peek Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions", nil)
	srv.ServeHTTP(w, r)

	var resp struct {
		Items []sessionResponse `json:"items"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) == 0 {
		t.Fatal("no sessions returned")
	}
	if resp.Items[0].LastOutput != "" {
		t.Errorf("last_output = %q without peek param, want empty", resp.Items[0].LastOutput)
	}
}

func TestHandleSessionCreate(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	body := `{"template":"myrig/worker"}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "sess-create-1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Template != "myrig/worker" {
		t.Errorf("Template = %q, want %q", resp.Template, "myrig/worker")
	}
	if resp.Title != "myrig/worker" {
		t.Errorf("Title = %q, want default %q", resp.Title, "myrig/worker")
	}
}

func TestHandleSessionMessageResumesSuspendedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Resume Me")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	req := newPostRequest("/v0/session/"+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`))
	req.Header.Set("Idempotency-Key", "sess-msg-1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if !fs.sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after POST /messages")
	}
	found := false
	for _, call := range fs.sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want Nudge hello", fs.sp.Calls)
	}
}

func TestHandleSessionTranscriptUsesSessionKey(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	writeNamedSessionJSONL(t, searchBase, workDir, "latest.jsonl",
		`{"uuid":"9","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"wrong file\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/transcript?tail=0", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionTranscriptResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "conversation" {
		t.Errorf("Format = %q, want %q", resp.Format, "conversation")
	}
	if len(resp.Turns) != 2 || resp.Turns[1].Text != "world" {
		t.Fatalf("Turns = %+v, want hello/world from session key file", resp.Turns)
	}
}

func TestHandleSessionTranscriptClosedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/transcript?tail=0", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionTranscriptResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Turns) != 1 || resp.Turns[0].Text != "hello" {
		t.Fatalf("Turns = %+v, want closed-session transcript", resp.Turns)
	}
}

func TestHandleSessionPendingAndRespond(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/pending", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("pending status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var pendingResp sessionPendingResponse
	if err := json.NewDecoder(w.Body).Decode(&pendingResp); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	if !pendingResp.Supported || pendingResp.Pending == nil || pendingResp.Pending.RequestID != "req-1" {
		t.Fatalf("pending response = %#v, want req-1", pendingResp)
	}

	respondReq := newPostRequest("/v0/session/"+info.ID+"/respond", strings.NewReader(`{"action":"approve"}`))
	respondReq.Header.Set("Idempotency-Key", "sess-respond-1")
	respondRec := httptest.NewRecorder()
	srv.ServeHTTP(respondRec, respondReq)

	if respondRec.Code != http.StatusAccepted {
		t.Fatalf("respond status = %d, want %d; body: %s", respondRec.Code, http.StatusAccepted, respondRec.Body.String())
	}
	if got := fs.sp.Responses[info.SessionName]; len(got) != 1 || got[0].Action != "approve" {
		t.Fatalf("responses = %#v, want single approve", got)
	}
}

func TestHandleSessionRespondMismatchedRequest(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	respondReq := newPostRequest("/v0/session/"+info.ID+"/respond", strings.NewReader(`{"request_id":"req-2","action":"approve"}`))
	respondRec := httptest.NewRecorder()
	srv.ServeHTTP(respondRec, respondReq)

	if respondRec.Code != http.StatusConflict {
		t.Fatalf("respond status = %d, want %d; body: %s", respondRec.Code, http.StatusConflict, respondRec.Body.String())
	}
}

func TestHandleSessionStreamSSEHeaders(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()
	<-done

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}
	if !strings.Contains(rec.Body.String(), "event: turn") || !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("stream body missing initial turn: %s", rec.Body.String())
	}
}

func TestHandleSessionStreamStoppedWithoutOutputReturnsNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{t.TempDir()}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "default", "No Output", "echo test", t.TempDir(), "test", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleSessionStreamClosedSessionReturnsSnapshot(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("closed session stream should return immediately")
	}

	if !strings.Contains(rec.Body.String(), "event: turn") || !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("stream body missing closed-session snapshot: %s", rec.Body.String())
	}
}

func TestStreamSessionTranscriptLogDoesNotSkipTurnsAcrossCompactionBoundaries(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	searchBase := t.TempDir()
	workDir := t.TempDir()
	logDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	logPath := filepath.Join(logDir, "session.jsonl")
	initial := strings.Join([]string{
		`{"uuid":"a","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"before compaction\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"cb0","parentUuid":"a","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"b","parentUuid":"cb0","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after first boundary\"}","timestamp":"2025-01-01T00:00:02Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	info := session.Info{ID: "sess-1", Template: "default"}
	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.streamSessionTranscriptLog(ctx, rec, info, logPath)
		close(done)
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.Body.String(), "after first boundary") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	appendFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, err = appendFile.WriteString(strings.Join([]string{
		`{"uuid":"c","parentUuid":"b","type":"user","message":"{\"role\":\"user\",\"content\":\"bridge turn\"}","timestamp":"2025-01-01T00:00:03Z"}`,
		`{"uuid":"cb1","parentUuid":"c","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:04Z"}`,
		`{"uuid":"d","parentUuid":"cb1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after second boundary\"}","timestamp":"2025-01-01T00:00:05Z"}`,
	}, "\n") + "\n")
	if closeErr := appendFile.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("append transcript: %v", err)
	}

	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "bridge turn") {
		t.Fatalf("stream body missing turn written before new compact boundary: %s", body)
	}
	if !strings.Contains(body, "after second boundary") {
		t.Fatalf("stream body missing turn written after new compact boundary: %s", body)
	}
}
