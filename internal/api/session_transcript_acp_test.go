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

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/session"
)

// An ACP session's transcript is the JSON-RPC capture gc writes under the
// city; the structured transcript reassembles its streamed chunks into
// messages and carries the turn's stop reason and usage.
func TestHandleSessionTranscriptStructuredReadsACPCapture(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{t.TempDir()}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template:  "myrig/worker",
		Title:     "Chat",
		Command:   "unreal-acp",
		WorkDir:   t.TempDir(),
		Provider:  "unreal-acp",
		Transport: "acp",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	path, err := citylayout.ACPTranscriptPath(fs.CityPath(), info.ID, "1")
	if err != nil {
		t.Fatalf("ACPTranscriptPath: %v", err)
	}
	chunk := func(text string) string {
		return `{"ts":"2026-09-25T10:00:02Z","dir":"in","msg":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"` + text + `"}}}}}`
	}
	capture := strings.Join([]string{
		`{"gc_acp_capture":1,"ts":"2026-09-25T10:00:00Z","session_id":"` + info.ID + `","runtime_epoch":"1","pid":1}`,
		`{"ts":"2026-09-25T10:00:01Z","dir":"out","msg":{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"acp-1","prompt":[{"type":"text","text":"What is the codeword?"}]}}}`,
		chunk("The code"),
		chunk("word is "),
		chunk("BLUEBIRD."),
		`{"ts":"2026-09-25T10:00:03Z","dir":"in","msg":{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn","usage":{"inputTokens":120,"outputTokens":9}}}}`,
	}, "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(capture), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp sessionTranscriptGetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msgs := structuredTranscriptMessages(resp)
	if len(msgs) != 2 {
		t.Fatalf("got %d structured messages, want the prompt and one reassembled reply; body: %s", len(msgs), w.Body.String())
	}
	if msgs[0].Role != "user" || len(msgs[0].Blocks) != 1 || msgs[0].Blocks[0].Text != "What is the codeword?" {
		t.Fatalf("message 0 = %+v, want the user prompt", msgs[0])
	}
	reply := msgs[1]
	if reply.Role != "assistant" || len(reply.Blocks) != 1 || reply.Blocks[0].Text != "The codeword is BLUEBIRD." {
		t.Fatalf("message 1 = %+v, want one assistant message reassembled from three chunks", reply)
	}
	if reply.StopReason != "end_turn" || reply.Usage == nil || reply.Usage.InputTokens != 120 || reply.Usage.OutputTokens != 9 {
		t.Fatalf("reply stop_reason=%q usage=%+v, want end_turn with the turn's usage", reply.StopReason, reply.Usage)
	}
	assertNoStructuredWireLeak(t, w.Body.Bytes())

	get := func(format string) sessionTranscriptGetResponse {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format="+format+"&tail=0", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("format=%s: status %d; body: %s", format, w.Code, w.Body.String())
		}
		var resp sessionTranscriptGetResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("format=%s: decode: %v", format, err)
		}
		return resp
	}
	conversation := get("conversation")
	var turns []string
	for _, turn := range conversation.Turns {
		turns = append(turns, turn.Role+":"+turn.Text)
	}
	if strings.Join(turns, " | ") != "user:What is the codeword? | assistant:The codeword is BLUEBIRD." {
		t.Fatalf("conversation turns = %q, want the prompt and the reassembled reply", turns)
	}
	raw := get("raw")
	if raw.Messages == nil || len(*raw.Messages) != 2 {
		t.Fatalf("raw messages = %+v, want one frame per entry", raw.Messages)
	}
}
