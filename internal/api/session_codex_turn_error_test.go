package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// codexTurnErrorRollout is a Codex rollout whose only turn was rejected by
// the model provider: Codex ends the turn with a task_complete whose error
// field carries the message. Before the reader surfaced it, the API showed
// only the prompt and an idle session, with no reason.
var codexTurnErrorRollout = []string{
	`{"timestamp":"2026-10-14T22:35:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"019a3333-0000-7000-8000-000000000003","started_at":1791930900}}`,
	`{"timestamp":"2026-10-14T22:35:00.010Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"startup prompt"}]}}`,
	`{"timestamp":"2026-10-14T22:35:00.400Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"019a3333-0000-7000-8000-000000000003","last_agent_message":null,"error":{"message":"The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account.","codex_error_info":"bad_request"}}}`,
}

const codexTurnErrorMessage = "The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account."

func newCodexTurnErrorSession(t *testing.T) (*fakeState, http.Handler, string) {
	t.Helper()
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeStructuredCodexFixture(t, searchBase, info.WorkDir, "2026-10-14T22-35-00", info.SessionKey, codexTurnErrorRollout)
	return fs, h, info.ID
}

func TestHandleSessionTranscriptStructuredSurfacesCodexTurnError(t *testing.T) {
	fs, h, id := newCodexTurnErrorSession(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+id+"/transcript?format=structured&tail=0", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.Bytes()
	var resp sessionTranscriptGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msgs := structuredTranscriptMessages(resp)
	if len(msgs) != 2 {
		t.Fatalf("StructuredMessages = %+v, want the prompt and the turn error", msgs)
	}
	last := msgs[1]
	want := SessionStructuredSystemEvent{Kind: "error", Category: "provider_error", Code: "bad_request", Message: codexTurnErrorMessage}
	if last.Role != "system" || last.SystemEvent == nil || *last.SystemEvent != want {
		t.Fatalf("last message = %+v (system_event %+v), want system error %+v", last, last.SystemEvent, want)
	}
	assertNoStructuredWireLeak(t, body)
}

func TestHandleSessionTranscriptConversationSurfacesCodexTurnError(t *testing.T) {
	fs, h, id := newCodexTurnErrorSession(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+id+"/transcript?tail=0", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp sessionTranscriptResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var sawError bool
	for _, turn := range resp.Turns {
		if turn.Role == "system" && strings.Contains(turn.Text, codexTurnErrorMessage) {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("conversation turns = %+v, want a system turn carrying the Codex turn error", resp.Turns)
	}
}

func TestHandleSessionStreamSurfacesCodexTurnError(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeStructuredCodexFixture(t, searchBase, info.WorkDir, "2026-10-14T22-35-00", info.SessionKey, codexTurnErrorRollout)

	// The stream stays open for live updates; the deadline only ends it
	// after the initial snapshot has been written.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, codexTurnErrorMessage) {
		t.Fatalf("session stream missing the Codex turn error: %s", body)
	}
	if strings.Contains(body, "codex_error_info") || strings.Contains(body, "task_complete") {
		t.Fatalf("session stream leaked provider-native Codex fields: %s", body)
	}
}

// codex0162TaskCompleteAPIErrorLine is the task_complete line codex-cli 0.162.1
// wrote, verbatim, when the provider rejected the model (see
// internal/sessionlog/testdata/codex/task_complete_api_error_0.162.1.jsonl).
// Its error.message is itself a JSON API error.
const codex0162TaskCompleteAPIErrorLine = `{"timestamp":"2026-10-14T22:35:00.564Z","type":"event_msg","payload":{"type":"task_complete","root_turn_id":"01a13c8d-84c2-73f1-af00-f385a6bad53e","turn_id":"01a13c8d-84c2-73f1-af00-f385a6bad53e","last_agent_message":null,"error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account.\"}}","codex_error_info":"other"},"started_at":1792017270,"completed_at":1792017300,"duration_ms":30547}}`

func TestHandleSessionTranscriptSurfacesCodex0162TaskCompleteAPIError(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeStructuredCodexFixture(t, searchBase, info.WorkDir, "2026-10-14T22-34-30", info.SessionKey, []string{
		`{"timestamp":"2026-10-14T22:34:30.017Z","type":"event_msg","payload":{"type":"task_started","root_turn_id":"01a13c8d-84c2-73f1-af00-f385a6bad53e","turn_id":"01a13c8d-84c2-73f1-af00-f385a6bad53e","started_at":1792017270}}`,
		`{"timestamp":"2026-10-14T22:34:30.030Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"startup prompt"}]}}`,
		codex0162TaskCompleteAPIErrorLine,
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.Bytes()
	var resp sessionTranscriptGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msgs := structuredTranscriptMessages(resp)
	if len(msgs) != 2 {
		t.Fatalf("StructuredMessages = %+v, want the prompt and the turn error", msgs)
	}
	want := SessionStructuredSystemEvent{Kind: "error", Category: "provider_error", Code: "http_400:invalid_request_error", Message: codexTurnErrorMessage}
	last := msgs[1]
	if last.Role != "system" || last.SystemEvent == nil || *last.SystemEvent != want {
		t.Fatalf("last message = %+v (system_event %+v), want system error %+v", last, last.SystemEvent, want)
	}
	if len(last.Blocks) != 1 || last.Blocks[0].Text != codexTurnErrorMessage {
		t.Fatalf("blocks = %+v, want the provider's inner message", last.Blocks)
	}
	assertNoStructuredWireLeak(t, body)
}
