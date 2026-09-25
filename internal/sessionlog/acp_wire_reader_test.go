package sessionlog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// acpConversationFixturePath copies the committed conversation capture into a
// temp city at the canonical capture location and returns the copied path.
func acpConversationFixturePath(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "acp_wire", "conversation.jsonl"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return writeACPCapture(t, string(data))
}

// writeACPCapture writes content verbatim as a capture file inside a temp
// city and returns its path.
func writeACPCapture(t *testing.T, content string) string {
	t.Helper()
	path, err := citylayout.ACPTranscriptPath(t.TempDir(), "gc-s1", "1")
	if err != nil {
		t.Fatalf("ACPTranscriptPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write capture: %v", err)
	}
	return path
}

func acpHeaderLine(runtimeEpoch string) string {
	return `{"gc_acp_capture":1,"ts":"2026-09-25T10:00:00Z","session_id":"gc-s1","session_name":"w","continuation_epoch":"1","runtime_epoch":"` + runtimeEpoch + `","pid":1}`
}

func acpRecord(dir, msg string) string {
	return `{"ts":"2026-09-25T10:00:01Z","dir":"` + dir + `","msg":` + msg + `}`
}

func acpPromptMsg(id, text string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"session/prompt","params":{"sessionId":"acp-1","prompt":[{"type":"text","text":"` + text + `"}]}}`
}

func acpChunkMsg(kind, text string) string {
	return `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"` + kind + `","content":{"type":"text","text":"` + text + `"}}}}`
}

func acpResultMsg(id, result string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"result":` + result + `}`
}

func joinLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

type acpEntryShape struct {
	typ  string
	text string
}

func acpEntryText(e *Entry) string {
	var parts []string
	if e.SystemEvent != nil {
		parts = append(parts, "system:"+e.SystemEvent.Kind+":"+e.SystemEvent.Category)
	}
	if text := e.TextContent(); text != "" {
		return strings.Join(append(parts, text), "|")
	}
	for _, b := range e.ContentBlocks() {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "thinking":
			parts = append(parts, "thinking:"+b.Thinking)
		case "tool_use":
			parts = append(parts, "tool_use:"+b.ID+":"+b.Name)
		case "tool_result":
			parts = append(parts, "tool_result:"+b.ToolUseID)
		case "interaction":
			parts = append(parts, "interaction:"+b.State+":"+b.Action)
		}
	}
	return strings.Join(parts, "|")
}

func TestReadACPCaptureConversationGolden(t *testing.T) {
	path := acpConversationFixturePath(t)

	// The capture path decides the reader, whatever the provider family: a
	// pack-defined ACP provider may carry any name, including one of a native
	// family.
	for _, provider := range []string{"unreal", "claude", "kiro", "codex"} {
		sess, err := ReadProviderFile(provider, path, 0)
		if err != nil {
			t.Fatalf("ReadProviderFile(%q): %v", provider, err)
		}
		want := []acpEntryShape{
			{"user", "Fix the build.\n\nfile:///work/repo/main.go\n\nnotes body"},
			{"assistant", "thinking:Let me look."},
			{"assistant", "I will run the tests."},
			{"assistant", "tool_use:call-1:Run go test"},
			{"assistant", "interaction:pending:"},
			{"system", "interaction:resolved:allow-once"},
			{"tool_result", "tool_result:call-1"},
			{"assistant", "Tests pass."},
			{"user", "Now refactor it."},
			{"assistant", "Starting the refactor"},
			{"system", "system:turn_aborted:cancelled|Turn canceled"}, //nolint:misspell // ACP StopReason wire value
			{"user", "Try again."},
			{"system", "system:error:provider_error|model overloaded"},
			{"system", "system:agent_restart:|Agent restarted"},
			{"user", "Earlier question"},
			{"user", "Are you back?"},
			{"assistant", "Yes, I am back."},
		}
		var got []acpEntryShape
		for _, e := range sess.Messages {
			got = append(got, acpEntryShape{e.Type, acpEntryText(e)})
		}
		if len(got) != len(want) {
			t.Fatalf("provider %q: got %d entries, want %d:\n%v", provider, len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("provider %q entry %d = %+v, want %+v", provider, i, got[i], want[i])
			}
		}
		if sess.ID != "acp-sess-1" {
			t.Errorf("Session.ID = %q, want the ACP session id acp-sess-1", sess.ID)
		}
		if !sess.Diagnostics.MalformedTail || sess.Diagnostics.MalformedLineCount != 1 {
			t.Errorf("diagnostics = %+v, want one malformed line at the tail", sess.Diagnostics)
		}
	}
}

func TestReadACPCaptureTurnDetails(t *testing.T) {
	path := acpConversationFixturePath(t)
	sess, err := ReadProviderFile("unreal", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile: %v", err)
	}
	msgs := sess.Messages

	// The turn's final assistant message carries the stop reason and the
	// (unstable) ACP usage, translated to the snake_case keys history reads.
	var final struct {
		StopReason string         `json:"stop_reason"`
		Usage      map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(msgs[7].Message, &final); err != nil {
		t.Fatalf("decoding final message: %v", err)
	}
	if final.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", final.StopReason)
	}
	wantUsage := map[string]int{"input_tokens": 1200, "output_tokens": 80, "reasoning_tokens": 20, "cache_read_input_tokens": 300}
	for k, v := range wantUsage {
		if final.Usage[k] != v {
			t.Errorf("usage[%s] = %d, want %d (usage %v)", k, final.Usage[k], v, final.Usage)
		}
	}
	var canceled struct {
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(msgs[9].Message, &canceled); err != nil || canceled.StopReason != acpStopReasonCancelled {
		t.Errorf("canceled turn message stop_reason = %q (err %v), want %q", canceled.StopReason, err, acpStopReasonCancelled)
	}

	toolUse := msgs[3].ContentBlocks()[0]
	if !strings.Contains(string(toolUse.Input), `"command":"go test ./..."`) {
		t.Errorf("tool_use input = %s, want the raw command", toolUse.Input)
	}
	result := msgs[6]
	if result.ToolUseID != "call-1" {
		t.Errorf("tool_result ToolUseID = %q, want call-1", result.ToolUseID)
	}
	rb := result.ContentBlocks()[0]
	if rb.IsError || !strings.Contains(string(rb.Content), "ok  ./...") || rb.Name != "Run go test" {
		t.Errorf("tool_result block = %+v content %s", rb, rb.Content)
	}

	perm := msgs[4].ContentBlocks()[0]
	if perm.Kind != "approval" || perm.Prompt != "Run go test" || strings.Join(perm.Options, ",") != "allow-once,reject-once" {
		t.Errorf("permission block = %+v", perm)
	}
	outcome := msgs[5].ContentBlocks()[0]
	if outcome.RequestID != perm.RequestID || perm.RequestID == "" {
		t.Errorf("outcome request id %q does not resolve permission %q", outcome.RequestID, perm.RequestID)
	}

	errEntry := msgs[12]
	if errEntry.SystemEvent.Code != "-32603" || errEntry.SystemEvent.Message != "model overloaded" {
		t.Errorf("error entry system event = %+v", errEntry.SystemEvent)
	}

	// Every tool call finished in the last agent process, so none is open.
	if len(sess.OrphanedToolUseIDs) != 0 {
		t.Errorf("OrphanedToolUseIDs = %v, want none", sess.OrphanedToolUseIDs)
	}
}

func TestReadACPCaptureEntryIDsUniqueAndStable(t *testing.T) {
	path := acpConversationFixturePath(t)
	first, err := ReadProviderFile("unreal", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range first.Messages {
		if e.UUID == "" {
			t.Fatalf("entry %+v has no id", e)
		}
		if seen[e.UUID] {
			t.Fatalf("duplicate id %q", e.UUID)
		}
		seen[e.UUID] = true
	}
	// The same messageId in two agent processes still yields two ids.
	if first.Messages[2].UUID == first.Messages[16].UUID {
		t.Fatal("reused messageId across a restart produced a duplicate id")
	}

	// Complete the torn line and append a new turn: the file is append-only,
	// so every earlier id must survive.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	extended := string(data) + `ate","params":{"sessionId":"acp-sess-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}}}` + "\n" +
		acpRecord("out", acpPromptMsg("3", "more")) + "\n" +
		acpRecord("in", acpChunkMsg("agent_message_chunk", "more reply")) + "\n"
	if err := os.WriteFile(path, []byte(extended), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := ReadProviderFile("unreal", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile after append: %v", err)
	}
	if len(second.Messages) <= len(first.Messages) {
		t.Fatalf("appended file has %d entries, want more than %d", len(second.Messages), len(first.Messages))
	}
	for i, e := range first.Messages {
		if second.Messages[i].UUID != e.UUID {
			t.Fatalf("entry %d id changed after append: %q -> %q", i, e.UUID, second.Messages[i].UUID)
		}
	}
	if second.Diagnostics.MalformedTail || second.Diagnostics.MalformedLineCount != 0 {
		t.Fatalf("diagnostics after completing the torn line = %+v", second.Diagnostics)
	}
}

func TestReadACPCaptureRawKeepsEveryChunkRecord(t *testing.T) {
	path := acpConversationFixturePath(t)
	sess, err := ReadProviderFileRaw("unreal", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFileRaw: %v", err)
	}
	// msg-1 was reassembled from three chunk records around a usage update.
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(sess.Messages[2].Raw, &records); err != nil {
		t.Fatalf("reassembled entry raw is not an array of records: %v (%s)", err, sess.Messages[2].Raw)
	}
	if len(records) != 3 {
		t.Fatalf("reassembled entry raw holds %d records, want 3", len(records))
	}
	// A single-record entry keeps the record itself.
	var single map[string]json.RawMessage
	if err := json.Unmarshal(sess.Messages[0].Raw, &single); err != nil || string(single["dir"]) != `"out"` {
		t.Fatalf("prompt entry raw = %s, want its capture record", sess.Messages[0].Raw)
	}
	if got := len(sess.RawPayloadBytes()); got != len(sess.Messages) {
		t.Fatalf("RawPayloadBytes returned %d payloads for %d entries", got, len(sess.Messages))
	}
}

func TestReadACPCapturePagination(t *testing.T) {
	path := acpConversationFixturePath(t)
	all, err := ReadProviderFile("unreal", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile: %v", err)
	}
	cursor := all.Messages[8].UUID // "Now refactor it."

	older, err := ReadProviderFileOlder("unreal", path, 0, cursor)
	if err != nil {
		t.Fatalf("ReadProviderFileOlder: %v", err)
	}
	if len(older.Messages) != 8 || older.Messages[7].UUID != all.Messages[7].UUID {
		t.Fatalf("older page = %d entries, want the 8 before the cursor", len(older.Messages))
	}
	if older.Pagination == nil || !older.Pagination.HasNewerMessages {
		t.Fatalf("older pagination = %+v, want HasNewerMessages", older.Pagination)
	}

	newer, err := ReadProviderFileRawNewer("unreal", path, 0, cursor)
	if err != nil {
		t.Fatalf("ReadProviderFileRawNewer: %v", err)
	}
	if len(newer.Messages) != len(all.Messages)-9 || newer.Messages[0].UUID != all.Messages[9].UUID {
		t.Fatalf("newer page = %d entries, want the %d after the cursor", len(newer.Messages), len(all.Messages)-9)
	}
	if newer.Pagination == nil || !newer.Pagination.HasOlderMessages {
		t.Fatalf("newer pagination = %+v, want HasOlderMessages", newer.Pagination)
	}

	if _, err := ReadProviderFileOlder("unreal", path, 0, "acp-missing"); !errors.Is(err, ErrCursorNotFound) {
		t.Fatalf("unknown cursor error = %v, want ErrCursorNotFound", err)
	}
}

func TestReadACPCaptureChunkRuns(t *testing.T) {
	msgChunk := func(messageID, text string) string {
		return `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"agent_message_chunk","messageId":"` + messageID + `","content":{"type":"text","text":"` + text + `"}}}}`
	}
	path := writeACPCapture(t, joinLines(
		acpHeaderLine("1"),
		acpRecord("out", acpPromptMsg("2", "go")),
		// Two messages told apart only by messageId.
		acpRecord("in", msgChunk("a", "one ")),
		acpRecord("in", msgChunk("a", "two")),
		acpRecord("in", msgChunk("b", "three")),
		// A thought between chunks without messageId splits the run.
		acpRecord("in", acpChunkMsg("agent_message_chunk", "four ")),
		acpRecord("in", acpChunkMsg("agent_thought_chunk", "hmm")),
		acpRecord("in", acpChunkMsg("agent_message_chunk", "five")),
		// A drop marker is not an entry and does not split a run.
		`{"ts":"2026-09-25T10:00:02Z","dir":"meta","dropped":3}`,
		acpRecord("in", acpChunkMsg("agent_message_chunk", " six")),
		acpRecord("in", acpResultMsg("2", `{"stopReason":"end_turn"}`)),
		// A new turn never extends the previous turn's run.
		acpRecord("out", acpPromptMsg("3", "again")),
		acpRecord("in", acpChunkMsg("agent_message_chunk", "seven")),
	))
	sess, err := ReadProviderFile("unreal", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile: %v", err)
	}
	var got []string
	for _, e := range sess.Messages {
		got = append(got, e.Type+":"+acpEntryText(e))
	}
	want := []string{
		"user:go",
		"assistant:one two",
		"assistant:three",
		"assistant:four ",
		"assistant:thinking:hmm",
		"assistant:five six",
		"user:again",
		"assistant:seven",
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("entries:\n got  %q\n want %q", got, want)
	}
}

func TestReadACPCaptureOpenToolCallIsOrphaned(t *testing.T) {
	path := writeACPCapture(t, joinLines(
		acpHeaderLine("1"),
		acpRecord("out", acpPromptMsg("2", "go")),
		acpRecord("in", `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Edit","status":"pending"}}}`),
		acpRecord("in", `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"in_progress","rawInput":{"path":"a.go"}}}}`),
	))
	sess, err := ReadProviderFile("unreal", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile: %v", err)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("got %d entries, want prompt + tool_use (in_progress is not a result)", len(sess.Messages))
	}
	use := sess.Messages[1].ContentBlocks()[0]
	if !strings.Contains(string(use.Input), `"a.go"`) {
		t.Fatalf("tool_use input = %s, want the input from the later update", use.Input)
	}
	if !sess.OrphanedToolUseIDs["t1"] {
		t.Fatalf("OrphanedToolUseIDs = %v, want t1 open", sess.OrphanedToolUseIDs)
	}
}

func TestReadACPCaptureFailedToolAndDismissedPermissionOnRestart(t *testing.T) {
	path := writeACPCapture(t, joinLines(
		acpHeaderLine("1"),
		acpRecord("out", acpPromptMsg("2", "go")),
		acpRecord("in", `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Bash","status":"failed","rawOutput":{"error":"boom"}}}}`),
		acpRecord("in", `{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"acp-1","toolCall":{"toolCallId":"t2","title":"Write"},"options":[{"optionId":"ok","name":"OK","kind":"allow_once"}]}}`),
		acpHeaderLine("2"),
	))
	sess, err := ReadProviderFile("unreal", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile: %v", err)
	}
	var got []string
	for _, e := range sess.Messages {
		got = append(got, e.Type+":"+acpEntryText(e))
	}
	want := []string{
		"user:go",
		"assistant:tool_use:t1:Bash",
		"tool_result:tool_result:t1",
		"assistant:interaction:pending:",
		"system:system:agent_restart:|Agent restarted|interaction:dismissed:",
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("entries:\n got  %q\n want %q", got, want)
	}
	if !sess.Messages[2].ContentBlocks()[0].IsError {
		t.Fatal("failed tool call result is not an error")
	}
}

func TestReadACPCaptureHeaderRules(t *testing.T) {
	t.Run("missing header", func(t *testing.T) {
		path := writeACPCapture(t, joinLines(acpRecord("out", acpPromptMsg("2", "go"))))
		if _, err := ReadProviderFile("unreal", path, 0); err == nil {
			t.Fatal("capture without a header was accepted")
		}
	})
	t.Run("newer format", func(t *testing.T) {
		path := writeACPCapture(t, joinLines(`{"gc_acp_capture":2,"ts":"2026-09-25T10:00:00Z","session_id":"gc-s1"}`))
		if _, err := ReadProviderFile("unreal", path, 0); err == nil {
			t.Fatal("capture with an unknown format version was accepted")
		}
	})
	t.Run("torn header then header", func(t *testing.T) {
		path := writeACPCapture(t, `{"gc_acp_capt`+"\n"+joinLines(acpHeaderLine("2"), acpRecord("out", acpPromptMsg("2", "go"))))
		sess, err := ReadProviderFile("unreal", path, 0)
		if err != nil {
			t.Fatalf("ReadProviderFile: %v", err)
		}
		if len(sess.Messages) != 1 || sess.Diagnostics.MalformedLineCount != 1 || sess.Diagnostics.MalformedTail {
			t.Fatalf("got %d entries, diagnostics %+v", len(sess.Messages), sess.Diagnostics)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		path := writeACPCapture(t, "")
		sess, err := ReadProviderFile("unreal", path, 0)
		if err != nil {
			t.Fatalf("ReadProviderFile: %v", err)
		}
		if len(sess.Messages) != 0 {
			t.Fatalf("empty capture produced %d entries", len(sess.Messages))
		}
	})
}

func TestIsACPCapturePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/city/.gc/transcripts/acp/gc-1/1.jsonl", true},
		{".gc/transcripts/acp/gc-1/1.jsonl", true},
		{"/city/.gc/transcripts/acp/../acp/gc-1/1.jsonl", true},
		{"/city/.gc/transcripts/acpx/gc-1/1.jsonl", false},
		{"/city/gc/transcripts/acp/gc-1/1.jsonl", false},
		{"/home/u/.kiro/sessions/x.jsonl", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsACPCapturePath(tt.path); got != tt.want {
			t.Errorf("IsACPCapturePath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestACPCaptureOutsideCaptureRootKeepsFamilyReader(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "acp_wire", "conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	viaKiro, err := ReadProviderFile("kiro", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile(kiro): %v", err)
	}
	direct, err := ReadKiroFile(path, 0)
	if err != nil {
		t.Fatalf("ReadKiroFile: %v", err)
	}
	if len(viaKiro.Messages) != len(direct.Messages) {
		t.Fatalf("kiro dispatch changed: %d entries via ReadProviderFile, %d via ReadKiroFile", len(viaKiro.Messages), len(direct.Messages))
	}
}

func TestACPCaptureNotDiscoveredByWorkDirScans(t *testing.T) {
	path := acpConversationFixturePath(t)
	root := filepath.Dir(filepath.Dir(path))
	for _, provider := range []string{"claude", "codex", "kiro", "grok", "auggie", "cursor", "pi", "unreal"} {
		if got := FindSessionFileForProvider([]string{root}, provider, "/work/repo"); got != "" {
			t.Errorf("provider %q workdir scan returned capture %q", provider, got)
		}
	}
}

func TestACPCaptureTailActivity(t *testing.T) {
	permissionRequest := func(id string) string {
		return `{"jsonrpc":"2.0","id":` + id + `,"method":"session/request_permission","params":{"sessionId":"acp-1","toolCall":{"toolCallId":"t"},"options":[]}}`
	}
	bigChunk := acpRecord("in", acpChunkMsg("agent_message_chunk", strings.Repeat("x", 4000)))
	var many []string
	for i := 0; i < 60; i++ { // ~250 KiB: the prompt is far outside the first tail window
		many = append(many, bigChunk)
	}
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"fixture ends idle", "", "idle"},
		{"header only", joinLines(acpHeaderLine("1")), "idle"},
		{"prompt in flight", joinLines(acpHeaderLine("1"), acpRecord("out", acpPromptMsg("2", "go")), acpRecord("in", acpChunkMsg("agent_message_chunk", "x"))), "in-turn"},
		{"prompt answered", joinLines(acpHeaderLine("1"), acpRecord("out", acpPromptMsg("2", "go")), acpRecord("in", acpResultMsg("2", `{"stopReason":"end_turn"}`))), "idle"},
		{"prompt errored", joinLines(acpHeaderLine("1"), acpRecord("out", acpPromptMsg("2", "go")), acpRecord("in", `{"jsonrpc":"2.0","id":2,"error":{"code":-1,"message":"x"}}`)), "idle"},
		// gc's reply to the agent's request 2 shares the prompt's id but not
		// its direction: the prompt is still in flight.
		{"direction-aware ids", joinLines(acpHeaderLine("1"), acpRecord("out", acpPromptMsg("2", "go")), acpRecord("in", permissionRequest("2")), acpRecord("out", acpResultMsg("2", `{"outcome":{"outcome":"selected","optionId":"a"}}`))), "in-turn"},
		{"restart after unanswered prompt", joinLines(acpHeaderLine("1"), acpRecord("out", acpPromptMsg("2", "go")), acpHeaderLine("2")), "idle"},
		{"prompt beyond first window", joinLines(append([]string{acpHeaderLine("1"), acpRecord("out", acpPromptMsg("2", "go"))}, many...)...), "in-turn"},
		{"torn tail mid turn", joinLines(acpHeaderLine("1"), acpRecord("out", acpPromptMsg("2", "go"))) + `{"ts":"x","dir":"in","msg":{"jsonrpc"`, "in-turn"},
		{"no header", joinLines(acpRecord("in", acpChunkMsg("agent_message_chunk", "x"))), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			if tt.content == "" {
				path = acpConversationFixturePath(t)
			} else {
				path = writeACPCapture(t, tt.content)
			}
			meta, err := ExtractTailMeta(path)
			if err != nil {
				t.Fatalf("ExtractTailMeta: %v", err)
			}
			got := ""
			if meta != nil {
				got = meta.Activity
			}
			if got != tt.want {
				t.Fatalf("activity = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("search roots", func(t *testing.T) {
		path := writeACPCapture(t, joinLines(acpHeaderLine("1"), acpRecord("out", acpPromptMsg("2", "go"))))
		root := filepath.Dir(filepath.Dir(path))
		meta, err := ExtractTailMetaFromSearchPaths([]string{root}, path)
		if err != nil || meta == nil || meta.Activity != "in-turn" {
			t.Fatalf("ExtractTailMetaFromSearchPaths = %+v, %v; want in-turn", meta, err)
		}
		if _, err := ExtractTailMetaFromSearchPaths([]string{t.TempDir()}, path); err == nil {
			t.Fatal("capture outside the search roots was read")
		}
	})
}
