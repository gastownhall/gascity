package sessionlog

import (
	"path/filepath"
	"testing"
	"time"
)

// Codex records a failed turn on its task_complete event: the error field is
// an object carrying the provider message and Codex's error classification
// (codex_error_info), or null when the turn succeeded. See
// testdata/turns/codex/error.jsonl (codex-cli 0.159.1). Before this was read,
// a turn rejected by the model provider (HTTP 400 "model not supported")
// left only the user's prompt in the conversation, so a client saw an idle
// session with no reason.
const (
	codexErrTaskStartedLine  = `{"timestamp":"2026-10-14T22:35:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"019a3333-0000-7000-8000-000000000003","started_at":1791930900,"model_context_window":258400,"collaboration_mode_kind":"default"}}`
	codexErrUserLine         = `{"timestamp":"2026-10-14T22:35:00.010Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"startup prompt"}]}}`
	codexErrTaskCompleteLine = `{"timestamp":"2026-10-14T22:35:00.400Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"019a3333-0000-7000-8000-000000000003","last_agent_message":null,"completed_at":1791930900,"duration_ms":400,"error":{"message":"unexpected status 400 Bad Request: {\"detail\":\"The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account.\"}","codex_error_info":"bad_request"}}}`
	codexErrWantMessage      = `unexpected status 400 Bad Request: {"detail":"The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account."}`
)

func TestReadCodexFileErroredTaskCompleteIsSystemError(t *testing.T) {
	path := writeJSONL(t, codexErrTaskStartedLine, codexErrUserLine, codexErrTaskCompleteLine)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(sess.Messages); got != 2 {
		t.Fatalf("Messages = %d, want the prompt and the turn error: %+v", got, sess.Messages)
	}
	msg := sess.Messages[1]
	if msg.Type != "system" || msg.Subtype != "error" {
		t.Fatalf("turn error entry Type/Subtype = %q/%q, want system/error", msg.Type, msg.Subtype)
	}
	if msg.SystemEvent == nil {
		t.Fatal("turn error entry has no SystemEvent")
	}
	if msg.SystemEvent.Kind != "error" || msg.SystemEvent.Category != "provider_error" || msg.SystemEvent.Code != "bad_request" {
		t.Fatalf("SystemEvent = %+v, want kind=error category=provider_error code=bad_request", msg.SystemEvent)
	}
	if msg.SystemEvent.Message != codexErrWantMessage {
		t.Fatalf("SystemEvent.Message = %q, want %q", msg.SystemEvent.Message, codexErrWantMessage)
	}
	if text := msg.ContentBlocks()[0].Text; text != codexErrWantMessage {
		t.Fatalf("text = %q, want the provider message", text)
	}
	if msg.ParentUUID != sess.Messages[0].UUID {
		t.Fatalf("ParentUUID = %q, want %q", msg.ParentUUID, sess.Messages[0].UUID)
	}
	wantUUID := stableSyntheticEntryID("codex-event", []byte(codexErrTaskCompleteLine), "event_msg:task_complete")
	if msg.UUID != wantUUID {
		t.Fatalf("UUID = %q, want content-derived %q", msg.UUID, wantUUID)
	}
	if !msg.Timestamp.Equal(mustParseCodexTestTime(t, "2026-10-14T22:35:00.400Z")) {
		t.Fatalf("Timestamp = %v, want the task_complete time", msg.Timestamp)
	}
}

func TestReadCodexFileSuccessfulTaskCompleteAddsNoEntry(t *testing.T) {
	ok := `{"timestamp":"2026-10-14T22:35:00.400Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"019a3333-0000-7000-8000-000000000003","last_agent_message":"done","error":null}}`
	missing := `{"timestamp":"2026-10-14T22:36:00.400Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"019a3333-0000-7000-8000-000000000004","last_agent_message":"done"}}`
	path := writeJSONL(t, codexErrTaskStartedLine, codexErrUserLine, ok, missing)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(sess.Messages); got != 1 {
		t.Fatalf("Messages = %d, want only the prompt: %+v", got, sess.Messages)
	}
}

// Codex may also emit an error event_msg for the same failure before the
// task_complete that ends the turn. The conversation shows it once.
func TestReadCodexFileErrorEventThenErroredTaskCompleteShowsOnce(t *testing.T) {
	errorLine := `{"timestamp":"2026-10-14T22:35:00.300Z","type":"event_msg","payload":{"type":"error","message":"unexpected status 400 Bad Request: {\"detail\":\"The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account.\"}","codex_error_info":"bad_request"}}`
	path := writeJSONL(t, codexErrTaskStartedLine, codexErrUserLine, errorLine, codexErrTaskCompleteLine)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(sess.Messages); got != 2 {
		t.Fatalf("Messages = %d, want the prompt and one error: %+v", got, sess.Messages)
	}
	if sess.Messages[1].SystemEvent == nil || sess.Messages[1].SystemEvent.Message != codexErrWantMessage {
		t.Fatalf("error entry = %+v, want the provider message", sess.Messages[1])
	}
}

// codex_error_info is a snake_case enum; its variants that carry data (an
// HTTP status) serialize as a one-key object rather than a string. Such an
// error must still be surfaced, keyed by the variant name.
func TestReadCodexFileErrorWithStructuredErrorInfo(t *testing.T) {
	errorLine := `{"timestamp":"2026-10-14T22:37:14.000Z","type":"event_msg","payload":{"type":"error","message":"stream failed","codex_error_info":{"response_stream_connection_failed":{"http_status_code":400}}}}`
	taskComplete := `{"timestamp":"2026-10-14T22:38:00.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"019a4444-0000-7000-8000-000000000004","error":{"message":"model rejected","codex_error_info":{"http_connection_failed":{"http_status_code":400}}}}}`
	stringError := `{"timestamp":"2026-10-14T22:39:00.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"019a5555-0000-7000-8000-000000000005","error":"plain failure"}}`
	path := writeJSONL(t, errorLine, taskComplete, stringError)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(sess.Messages); got != 3 {
		t.Fatalf("Messages = %d, want 3 errors: %+v", got, sess.Messages)
	}
	for i, want := range []struct{ code, message string }{
		{"response_stream_connection_failed", "stream failed"},
		{"http_connection_failed", "model rejected"},
		{"", "plain failure"},
	} {
		ev := sess.Messages[i].SystemEvent
		if ev == nil {
			t.Fatalf("[%d] SystemEvent is nil", i)
		}
		if ev.Kind != "error" || ev.Code != want.code || ev.Message != want.message {
			t.Errorf("[%d] SystemEvent = %+v, want kind=error code=%q message=%q", i, ev, want.code, want.message)
		}
	}
}

func mustParseCodexTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return ts
}

// testdata/codex/task_complete_api_error_0.162.1.jsonl ends with the
// task_complete line codex-cli 0.162.1 wrote, verbatim, when a ChatGPT-account
// login was launched with a model its plan does not serve (the lines before it
// are a minimal synthesized turn). Its error.message is itself the provider's
// JSON API error and codex_error_info is the catch-all "other".
func TestReadCodexFileCodex0162TaskCompleteAPIError(t *testing.T) {
	sess, err := ReadCodexFile(filepath.Join("testdata", "codex", "task_complete_api_error_0.162.1.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(sess.Messages); got != 2 {
		t.Fatalf("Messages = %d, want the prompt and the turn error: %+v", got, sess.Messages)
	}
	msg := sess.Messages[1]
	want := SystemEvent{
		Kind:     "error",
		Category: "provider_error",
		Code:     "http_400:invalid_request_error",
		Message:  "The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account.",
	}
	if msg.Type != "system" || msg.SystemEvent == nil || *msg.SystemEvent != want {
		t.Fatalf("turn error entry = %+v (SystemEvent %+v), want %+v", msg, msg.SystemEvent, want)
	}
	if text := msg.ContentBlocks()[0].Text; text != want.Message {
		t.Fatalf("text = %q, want the provider's inner message", text)
	}
}

// A provider error message that is a JSON API error is presented by its inner
// message, with the HTTP status and error type in the code when Codex's own
// classification is the catch-all. Anything else is shown as written.
func TestCodexSystemEventUnwrapsJSONAPIErrorMessage(t *testing.T) {
	apiErr := `{"type":"error","status":429,"error":{"type":"rate_limit_error","message":"Slow down."}}`
	for _, tc := range []struct {
		name string
		em   codexEventMsg
		want SystemEvent
	}{
		{
			name: "catch-all info takes status and type",
			em:   codexEventMsg{Type: "error", Message: apiErr, CodexErrorInfo: []byte(`"other"`)},
			want: SystemEvent{Kind: "error", Category: "provider_error", Code: "http_429:rate_limit_error", Message: "Slow down."},
		},
		{
			name: "no info takes status and type",
			em:   codexEventMsg{Type: "error", Message: apiErr},
			want: SystemEvent{Kind: "error", Category: "provider_error", Code: "http_429:rate_limit_error", Message: "Slow down."},
		},
		{
			name: "specific info is kept",
			em:   codexEventMsg{Type: "error", Message: apiErr, CodexErrorInfo: []byte(`"usage_limit_exceeded"`)},
			want: SystemEvent{Kind: "error", Category: "usage_limit", Code: "usage_limit_exceeded", Message: "Slow down."},
		},
		{
			name: "type without status",
			em:   codexEventMsg{Type: "error", Message: `{"error":{"type":"invalid_request_error","message":"Bad."}}`},
			want: SystemEvent{Kind: "error", Category: "provider_error", Code: "invalid_request_error", Message: "Bad."},
		},
		{
			name: "plain text stays raw",
			em:   codexEventMsg{Type: "error", Message: "unexpected status 400", CodexErrorInfo: []byte(`"other"`)},
			want: SystemEvent{Kind: "error", Category: "provider_error", Code: "other", Message: "unexpected status 400"},
		},
		{
			name: "JSON without an inner message stays raw",
			em:   codexEventMsg{Type: "error", Message: `{"type":"error","status":500}`},
			want: SystemEvent{Kind: "error", Category: "provider_error", Message: `{"type":"error","status":500}`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexSystemEvent(tc.em); *got != tc.want {
				t.Fatalf("codexSystemEvent = %+v, want %+v", *got, tc.want)
			}
		})
	}
}
