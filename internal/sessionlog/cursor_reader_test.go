package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderFamilyCursorAliases(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: "cursor", want: "cursor"},
		{provider: "cursor/tmux-cli", want: "cursor"},
		{provider: "cursor-agent", want: "cursor"},
		{provider: "wrapped/cursor", want: "cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := ProviderFamily(tt.provider); got != tt.want {
				t.Fatalf("ProviderFamily(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}

func TestReadCursorFileConvertsStreamJSON(t *testing.T) {
	path := writeCursorJSONL(t,
		`{"type":"system","subtype":"init","cwd":"/work/project","session_id":"cursor-session","model":"gpt-5"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"inspect files"}]},"session_id":"cursor-session"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Reading now."}]},"session_id":"cursor-session"}`,
		`{"type":"tool_call","subtype":"started","call_id":"call-read","tool_call":{"readToolCall":{"toolCallId":"call-read","args":{"path":"src/app.ts"}}},"session_id":"cursor-session"}`,
		`{"type":"tool_call","subtype":"completed","call_id":"call-read","tool_call":{"readToolCall":{"toolCallId":"call-read","args":{"path":"src/app.ts"},"result":{"success":{"content":"export const app = true;\n","isEmpty":false,"exceededLimit":false,"totalLines":1,"totalChars":25}}}},"session_id":"cursor-session"}`,
		`{"type":"tool_call","subtype":"started","call_id":"call-write","tool_call":{"writeToolCall":{"toolCallId":"call-write","args":{"path":"notes.txt","fileText":"hello cursor\n"}}},"session_id":"cursor-session"}`,
		`{"type":"tool_call","subtype":"completed","call_id":"call-write","tool_call":{"writeToolCall":{"toolCallId":"call-write","args":{"path":"notes.txt","fileText":"hello cursor\n"},"result":{"success":{"path":"notes.txt","linesCreated":1,"fileSize":13}}}},"session_id":"cursor-session"}`,
		`{"type":"tool_call","subtype":"started","call_id":"call-bash","tool_call":{"function":{"name":"Bash","arguments":{"command":"npm test"}}},"session_id":"cursor-session"}`,
		`{"type":"tool_call","subtype":"completed","call_id":"call-bash","tool_call":{"function":{"name":"Bash","arguments":{"command":"npm test"},"result":{"success":{"stdout":"ok\n","stderr":"","exitCode":0}}}},"session_id":"cursor-session"}`,
		`{"type":"result","subtype":"success","result":"done","is_error":false,"session_id":"cursor-session"}`,
	)

	session, err := ReadCursorFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCursorFile() error = %v", err)
	}
	if session.ID != "cursor-session" {
		t.Fatalf("Session.ID = %q, want cursor-session", session.ID)
	}
	if got := len(session.Messages); got != 9 {
		t.Fatalf("len(Messages) = %d, want user, assistant, 3 tool uses, 3 tool results, system result", got)
	}
	userBlocks := session.Messages[0].ContentBlocks()
	if session.Messages[0].Type != "user" || len(userBlocks) != 1 || userBlocks[0].Text != "inspect files" {
		t.Fatalf("user entry = %#v blocks %+v, want text prompt", session.Messages[0], userBlocks)
	}
	if blocks := session.Messages[1].ContentBlocks(); len(blocks) != 1 || blocks[0].Text != "Reading now." {
		t.Fatalf("assistant blocks = %+v, want assistant text", blocks)
	}

	readUse := session.Messages[2].ContentBlocks()[0]
	if readUse.Type != "tool_use" || readUse.ID != "call-read" || readUse.Name != "Read" {
		t.Fatalf("read tool use = %+v, want Read tool_use", readUse)
	}
	assertJSONHasString(t, readUse.Input, "file_path", "src/app.ts")
	if strings.Contains(string(readUse.Input), "readToolCall") || strings.Contains(string(readUse.Input), "toolCallId") {
		t.Fatalf("read input leaked Cursor-native key: %s", readUse.Input)
	}
	readResult := session.Messages[3].ContentBlocks()[0]
	assertJSONHasString(t, readResult.Content, "file_path", "src/app.ts")
	assertJSONHasString(t, readResult.Content, "content", "export const app = true;\n")
	assertJSONHasInt(t, readResult.Content, "total_lines", 1)
	for _, forbidden := range []string{"readToolCall", "toolCallId", "totalLines", "totalChars", "exceededLimit"} {
		if strings.Contains(string(readResult.Content), forbidden) {
			t.Fatalf("read result leaked Cursor-native key %q: %s", forbidden, readResult.Content)
		}
	}

	writeUse := session.Messages[4].ContentBlocks()[0]
	if writeUse.Type != "tool_use" || writeUse.ID != "call-write" || writeUse.Name != "Write" {
		t.Fatalf("write tool use = %+v, want Write tool_use", writeUse)
	}
	assertJSONHasString(t, writeUse.Input, "file_path", "notes.txt")
	assertJSONHasString(t, writeUse.Input, "content", "hello cursor\n")
	if strings.Contains(string(writeUse.Input), "fileText") {
		t.Fatalf("write input leaked Cursor-native fileText key: %s", writeUse.Input)
	}
	writeResult := session.Messages[5].ContentBlocks()[0]
	assertJSONHasString(t, writeResult.Content, "file_path", "notes.txt")
	assertJSONHasString(t, writeResult.Content, "content", "hello cursor\n")
	assertJSONHasInt(t, writeResult.Content, "num_lines", 1)
	for _, forbidden := range []string{"writeToolCall", "toolCallId", "fileText", "linesCreated", "fileSize"} {
		if strings.Contains(string(writeResult.Content), forbidden) {
			t.Fatalf("write result leaked Cursor-native key %q: %s", forbidden, writeResult.Content)
		}
	}

	bashUse := session.Messages[6].ContentBlocks()[0]
	if bashUse.Type != "tool_use" || bashUse.ID != "call-bash" || bashUse.Name != "Bash" {
		t.Fatalf("bash tool use = %+v, want Bash tool_use", bashUse)
	}
	assertJSONHasString(t, bashUse.Input, "command", "npm test")
	bashResult := session.Messages[7].ContentBlocks()[0]
	assertJSONHasString(t, bashResult.Content, "stdout", "ok\n")
	assertJSONHasInt(t, bashResult.Content, "exit_code", 0)
	if strings.Contains(string(bashResult.Content), "exitCode") {
		t.Fatalf("bash result leaked Cursor-native exitCode key: %s", bashResult.Content)
	}

	final := session.Messages[8]
	if final.Type != "system" || final.SystemEvent == nil || final.SystemEvent.Kind != "result" || final.SystemEvent.Message != "done" {
		t.Fatalf("final entry = %#v, want provider-neutral result system event", final)
	}
}

func TestReadCursorFileSkipsPartialAssistantFlushes(t *testing.T) {
	path := writeCursorJSONL(t,
		`{"type":"system","subtype":"init","cwd":"/work/project","session_id":"partial-session"}`,
		`{"type":"assistant","timestamp_ms":1800000000000,"message":{"role":"assistant","content":"visible partial"},"session_id":"partial-session"}`,
		`{"type":"assistant","timestamp_ms":1800000000001,"model_call_id":"call-model","message":{"role":"assistant","content":"pre-tool flush"},"session_id":"partial-session"}`,
		`{"type":"assistant","message":{"role":"assistant","content":"final duplicate"},"session_id":"partial-session"}`,
	)

	session, err := ReadCursorFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCursorFile() error = %v", err)
	}
	if got := len(session.Messages); got != 1 {
		t.Fatalf("len(Messages) = %d, want one partial assistant message", got)
	}
	if got := session.Messages[0].TextContent(); got != "visible partial" {
		t.Fatalf("assistant text = %q, want visible partial", got)
	}
}

func TestReadCursorFileUsesNativeAndStableSyntheticEntryIDs(t *testing.T) {
	generation := `{"type":"user","generation_id":"generation-native","message":{"role":"user","content":"generation"},"session_id":"cursor-ids"}`
	call := `{"type":"assistant","call_id":"call-native","message":{"role":"assistant","content":"call"},"session_id":"cursor-ids"}`
	topLevel := `{"id":"event-native","type":"assistant","message":{"role":"assistant","content":"event"},"session_id":"cursor-ids"}`
	nestedTool := `{"type":"tool_call","subtype":"started","tool_call":{"readToolCall":{"toolCallId":"tool-native","args":{"path":"README.md"}}},"session_id":"cursor-ids"}`
	synthetic := `{"type":"assistant","message":{"role":"assistant","content":"synthetic"},"session_id":"cursor-ids"}`
	multipart := `{"hook_event_name":"afterFileEdit","file_path":"notes.txt","new_text":"updated","session_id":"cursor-ids"}`
	path := writeCursorJSONL(t, generation, call, topLevel, nestedTool, synthetic, multipart)

	session, err := ReadCursorFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCursorFile() error = %v", err)
	}
	if got := len(session.Messages); got != 7 {
		t.Fatalf("len(Messages) = %d, want five single entries plus tool use/result", got)
	}
	if got, want := session.Messages[0].UUID, stableSyntheticEntryID("cursor", []byte(generation), ""); got != want {
		t.Fatalf("generation-scoped entry UUID = %q, want stable record ID %q", got, want)
	}
	if got, want := session.Messages[1].UUID, stableSyntheticEntryID("cursor", []byte(call), ""); got != want {
		t.Fatalf("call entry UUID = %q, want record-derived entry ID %q", got, want)
	}
	if got := session.Messages[2].UUID; got != "event-native" {
		t.Fatalf("top-level entry UUID = %q, want native event ID", got)
	}
	if got, want := session.Messages[3].UUID, stableSyntheticEntryID("cursor", []byte(nestedTool), "use"); got != want {
		t.Fatalf("tool entry UUID = %q, want record-derived entry ID %q", got, want)
	}
	toolBlocks := session.Messages[3].ContentBlocks()
	if len(toolBlocks) != 1 || toolBlocks[0].ID != "tool-native" {
		t.Fatalf("tool blocks = %+v, want native tool call ID", toolBlocks)
	}
	if got, want := session.Messages[4].UUID, stableSyntheticEntryID("cursor", []byte(synthetic), ""); got != want {
		t.Fatalf("id-less message UUID = %q, want %q", got, want)
	}
	if got, want := session.Messages[5].UUID, stableSyntheticEntryID("cursor", []byte(multipart), "use"); got != want {
		t.Fatalf("id-less tool-use UUID = %q, want %q", got, want)
	}
	if got, want := session.Messages[6].UUID, stableSyntheticEntryID("cursor", []byte(multipart), "result"); got != want {
		t.Fatalf("id-less tool-result UUID = %q, want %q", got, want)
	}
	if session.Messages[5].UUID == session.Messages[6].UUID {
		t.Fatalf("multipart tool entries share UUID %q", session.Messages[5].UUID)
	}
}

func TestReadCursorFileDoesNotUseGenerationIDAsEntryIdentity(t *testing.T) {
	first := `{"hook_event_name":"beforeShellExecution","generation_id":"generation-shared","command":"go test ./internal/api","session_id":"cursor-generation"}`
	second := `{"hook_event_name":"beforeShellExecution","generation_id":"generation-shared","command":"go test ./internal/worker","session_id":"cursor-generation"}`
	result := `{"hook_event_name":"afterShellExecution","generation_id":"generation-shared","command":"go test ./internal/api","stdout":"ok","exit_code":0,"session_id":"cursor-generation"}`
	path := writeCursorJSONL(t, first, second, result)

	session, err := ReadCursorFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCursorFile() error = %v", err)
	}
	if got := len(session.Messages); got != 3 {
		t.Fatalf("len(Messages) = %d, want two tool-use entries and one result", got)
	}
	wantEntryIDs := []string{
		stableSyntheticEntryID("cursor", []byte(first), "use"),
		stableSyntheticEntryID("cursor", []byte(second), "use"),
		stableSyntheticEntryID("cursor", []byte(result), "result"),
	}
	wantToolIDs := []string{
		stableSyntheticEntryID("cursor-tool", []byte(first), ""),
		stableSyntheticEntryID("cursor-tool", []byte(second), ""),
		stableSyntheticEntryID("cursor-tool", []byte(result), ""),
	}
	for i, entry := range session.Messages {
		if entry.UUID != wantEntryIDs[i] {
			t.Fatalf("generation entry %d UUID = %q, want %q", i, entry.UUID, wantEntryIDs[i])
		}
		blocks := entry.ContentBlocks()
		if len(blocks) != 1 {
			t.Fatalf("generation entry %d blocks = %+v, want one tool block", i, blocks)
		}
		toolID := blocks[0].ID
		if blocks[0].Type == "tool_result" {
			toolID = blocks[0].ToolUseID
		}
		if toolID != wantToolIDs[i] {
			t.Fatalf("generation entry %d tool ID = %q, want unique record ID %q", i, toolID, wantToolIDs[i])
		}
	}
	if session.Messages[0].UUID == session.Messages[1].UUID {
		t.Fatalf("generation-scoped hooks share entry UUID %q", session.Messages[0].UUID)
	}
}

func TestReadCursorFileKeepsEntryAndToolIdentityDomainsDistinct(t *testing.T) {
	numericID := `{"id":1,"type":"assistant","message":{"role":"assistant","content":"numeric"},"session_id":"cursor-aliases"}`
	nativeStringID := `{"id":"cursor-tool-1","type":"assistant","message":{"role":"assistant","content":"native string"},"session_id":"cursor-aliases"}`
	toolCall := `{"hook_event_name":"beforeShellExecution","call_id":"x","command":"go test ./internal/api","session_id":"cursor-aliases"}`
	nativeSuffixID := `{"id":"x-use","type":"assistant","message":{"role":"assistant","content":"native suffix"},"session_id":"cursor-aliases"}`
	path := writeCursorJSONL(t, numericID, nativeStringID, toolCall, nativeSuffixID)

	session, err := ReadCursorFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCursorFile() error = %v", err)
	}
	if got := len(session.Messages); got != 4 {
		t.Fatalf("len(Messages) = %d, want four distinguishable records", got)
	}
	wantEntryIDs := []string{
		stableSyntheticEntryID("cursor", []byte(numericID), ""),
		"cursor-tool-1",
		stableSyntheticEntryID("cursor", []byte(toolCall), "use"),
		"x-use",
	}
	for i, want := range wantEntryIDs {
		if got := session.Messages[i].UUID; got != want {
			t.Fatalf("entry %d UUID = %q, want %q", i, got, want)
		}
	}
	blocks := session.Messages[2].ContentBlocks()
	if len(blocks) != 1 || blocks[0].ID != "x" {
		t.Fatalf("tool blocks = %+v, want call correlation ID x", blocks)
	}
}

func TestReadProviderFileUsesCursorReader(t *testing.T) {
	path := writeCursorJSONL(t,
		`{"type":"system","subtype":"init","cwd":"/work/project","session_id":"dispatch-session"}`,
		`{"type":"assistant","message":{"role":"assistant","content":"hello"},"session_id":"dispatch-session"}`,
	)
	session, err := ReadProviderFile("cursor/tmux-cli", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile() error = %v", err)
	}
	if session.ID != "dispatch-session" || len(session.Messages) != 1 || session.Messages[0].Type != "assistant" {
		t.Fatalf("ReadProviderFile() = id %q messages %+v, want Cursor assistant transcript", session.ID, session.Messages)
	}
}

func TestFindCursorSessionFileByIDAndWorkDir(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	path := filepath.Join(root, "cursor-session.jsonl")
	writeFile(t, path, `{"type":"system","subtype":"init","cwd":`+jsonString(workDir)+`,"session_id":"cursor-session"}`+"\n")

	if got := FindCursorSessionFileByID([]string{root}, workDir, "cursor-session"); got != path {
		t.Fatalf("FindCursorSessionFileByID() = %q, want %q", got, path)
	}
	if got := FindCursorSessionFileByID([]string{root}, workDir, "../escape"); got != "" {
		t.Fatalf("FindCursorSessionFileByID traversal = %q, want empty", got)
	}
	if got := FindCursorSessionFile([]string{root}, workDir); got != path {
		t.Fatalf("FindCursorSessionFile() = %q, want %q", got, path)
	}
	if got := FindCursorSessionFile([]string{root}, filepath.Join(t.TempDir(), "other")); got != "" {
		t.Fatalf("FindCursorSessionFile() wrong workdir = %q, want empty", got)
	}
}

func writeCursorJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cursor.jsonl")
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	return path
}
