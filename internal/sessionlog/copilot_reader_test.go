package sessionlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderFamilyCopilotAliases(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: "copilot", want: "copilot"},
		{provider: "copilot/tmux-cli", want: "copilot"},
		{provider: "github-copilot", want: "copilot"},
		{provider: "github-copilot/tmux-cli", want: "copilot"},
		{provider: "wrapped/copilot", want: "copilot"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := ProviderFamily(tt.provider); got != tt.want {
				t.Fatalf("ProviderFamily(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}

func TestReadCopilotFileConvertsMessagesAndToolExecutions(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"copilot-session","producer":"copilot-agent","selectedModel":"claude-sonnet-4.5","context":{"cwd":"/work/project"}},"id":"start-1","timestamp":"2026-03-04T02:30:58.550Z","parentId":null}`,
		`{"type":"user.message","data":{"content":"patch the app"},"id":"user-1","timestamp":"2026-03-04T02:31:00Z","parentId":"start-1"}`,
		`{"type":"assistant.message","data":{"content":"I will update the app.","model":"claude-sonnet-4.5","toolRequests":[{"toolCallId":"toolu-bash","name":"bash","arguments":"{\"command\":\"printf hello\"}"},{"toolCallId":"toolu-edit","name":"edit_file","arguments":{"path":"src/app.ts","oldString":"old","newString":"new"}}]},"id":"assistant-1","timestamp":"2026-03-04T02:31:01Z","parentId":"user-1"}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"toolu-bash","toolName":"bash","arguments":{"command":"printf hello"}},"id":"start-bash","timestamp":"2026-03-04T02:31:02Z","parentId":"assistant-1"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"toolu-bash","model":"claude-sonnet-4.5","success":true,"result":{"stdout":"hello\n","stderr":"","exitCode":0}},"id":"complete-bash","timestamp":"2026-03-04T02:31:03Z","parentId":"start-bash"}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"toolu-edit","toolName":"edit_file","arguments":{"path":"src/app.ts","oldString":"old","newString":"new"}},"id":"start-edit","timestamp":"2026-03-04T02:31:04Z","parentId":"complete-bash"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"toolu-edit","model":"claude-sonnet-4.5","success":true,"result":{"content":"Edited src/app.ts","filePath":"src/app.ts","patch":"*** Begin Patch\n*** Update File: src/app.ts\n@@\n-old\n+new\n*** End Patch","oldString":"old","newString":"new","originalFile":"old\n","replaceAll":false,"userModified":false}},"id":"complete-edit","timestamp":"2026-03-04T02:31:05Z","parentId":"start-edit"}`,
		`{"type":"session.skills_loaded","data":{"skills":["ignored"]},"id":"ignored-1","timestamp":"2026-03-04T02:31:06Z","parentId":"complete-edit"}`,
	)

	session, err := ReadCopilotFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCopilotFile() error = %v", err)
	}
	if session.ID != "copilot-session" {
		t.Fatalf("Session.ID = %q, want copilot-session", session.ID)
	}
	if got := len(session.Messages); got != 4 {
		t.Fatalf("len(Messages) = %d, want 4", got)
	}
	if session.Messages[0].Type != "user" || session.Messages[0].TextContent() != "patch the app" {
		t.Fatalf("user entry = %#v, want text prompt", session.Messages[0])
	}

	assistant := session.Messages[1]
	if assistant.Type != "assistant" {
		t.Fatalf("assistant type = %q, want assistant", assistant.Type)
	}
	blocks := assistant.ContentBlocks()
	if len(blocks) != 3 {
		t.Fatalf("assistant blocks = %+v, want text plus two tool_use blocks", blocks)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "I will update the app." {
		t.Fatalf("text block = %+v, want assistant text", blocks[0])
	}
	if blocks[1].Type != "tool_use" || blocks[1].ID != "toolu-bash" || blocks[1].Name != "bash" {
		t.Fatalf("bash tool block = %+v, want provider-neutral tool use", blocks[1])
	}
	assertJSONHasString(t, blocks[1].Input, "command", "printf hello")
	if strings.Contains(string(blocks[1].Input), "toolCallId") || strings.Contains(string(blocks[1].Input), "commandToRun") {
		t.Fatalf("bash input leaked Copilot-native keys: %s", blocks[1].Input)
	}
	if blocks[2].Type != "tool_use" || blocks[2].ID != "toolu-edit" || blocks[2].Name != "edit_file" {
		t.Fatalf("edit tool block = %+v, want provider-neutral tool use", blocks[2])
	}
	assertJSONHasString(t, blocks[2].Input, "file_path", "src/app.ts")
	assertJSONHasString(t, blocks[2].Input, "old_string", "old")
	assertJSONHasString(t, blocks[2].Input, "new_string", "new")
	if strings.Contains(string(blocks[2].Input), "oldString") || strings.Contains(string(blocks[2].Input), "newString") {
		t.Fatalf("edit input leaked Copilot-native keys: %s", blocks[2].Input)
	}

	bashResult := session.Messages[2]
	if bashResult.Type != "tool_result" || bashResult.ToolUseID != "toolu-bash" {
		t.Fatalf("bash result entry = %#v, want tool_result for toolu-bash", bashResult)
	}
	bashBlocks := bashResult.ContentBlocks()
	if len(bashBlocks) != 1 || bashBlocks[0].Type != "tool_result" || bashBlocks[0].IsError {
		t.Fatalf("bash result blocks = %+v, want non-error tool_result", bashBlocks)
	}
	assertJSONHasString(t, bashBlocks[0].Content, "stdout", "hello\n")
	assertJSONHasInt(t, bashBlocks[0].Content, "exit_code", 0)
	if strings.Contains(string(bashBlocks[0].Content), "exitCode") || strings.Contains(string(bashBlocks[0].Content), "toolCallId") {
		t.Fatalf("bash result leaked Copilot-native keys: %s", bashBlocks[0].Content)
	}

	editResult := session.Messages[3]
	if editResult.Type != "tool_result" || editResult.ToolUseID != "toolu-edit" {
		t.Fatalf("edit result entry = %#v, want tool_result for toolu-edit", editResult)
	}
	editBlocks := editResult.ContentBlocks()
	if len(editBlocks) != 1 || editBlocks[0].Type != "tool_result" || editBlocks[0].IsError {
		t.Fatalf("edit result blocks = %+v, want non-error tool_result", editBlocks)
	}
	assertJSONHasString(t, editBlocks[0].Content, "file_path", "src/app.ts")
	assertJSONHasString(t, editBlocks[0].Content, "old_string", "old")
	assertJSONHasString(t, editBlocks[0].Content, "new_string", "new")
	assertJSONHasString(t, editBlocks[0].Content, "patch", "*** Begin Patch\n*** Update File: src/app.ts\n@@\n-old\n+new\n*** End Patch")
	for _, forbidden := range []string{"filePath", "oldString", "newString", "toolCallId"} {
		if strings.Contains(string(editBlocks[0].Content), forbidden) {
			t.Fatalf("edit result leaked Copilot-native key %q: %s", forbidden, editBlocks[0].Content)
		}
	}
}

func TestReadCopilotFileEmitsStartOnlyToolUseWhenAssistantRequestIsAbsent(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"copilot-start-only","context":{"cwd":"/work/project"}},"id":"start-1","timestamp":"2026-03-04T02:30:58.550Z"}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"toolu-start","toolName":"bash","arguments":{"commandToRun":"npm test","workingDir":"/work/project"}},"id":"start-tool","timestamp":"2026-03-04T02:31:02Z","parentId":"start-1"}`,
	)

	session, err := ReadCopilotFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCopilotFile() error = %v", err)
	}
	if got := len(session.Messages); got != 1 {
		t.Fatalf("len(Messages) = %d, want one tool_use entry", got)
	}
	blocks := session.Messages[0].ContentBlocks()
	if len(blocks) != 1 || blocks[0].Type != "tool_use" || blocks[0].ID != "toolu-start" {
		t.Fatalf("blocks = %+v, want tool_use from execution_start", blocks)
	}
	assertJSONHasString(t, blocks[0].Input, "command", "npm test")
	if strings.Contains(string(blocks[0].Input), "commandToRun") {
		t.Fatalf("execution_start input leaked Copilot-native commandToRun key: %s", blocks[0].Input)
	}
}

func TestReadCopilotFileUsesNativeAndStableSyntheticEntryIDs(t *testing.T) {
	native := `{"type":"assistant.message","data":{"content":"native"},"id":"native-event-id","sessionId":"copilot-ids"}`
	synthetic := `{"type":"assistant.message","data":{"content":"synthetic"},"sessionId":"copilot-ids"}`
	tool := `{"type":"tool.execution_start","data":{"toolCallId":"toolu-native","toolName":"bash","arguments":{"command":"go test ./..."}},"sessionId":"copilot-ids"}`
	path := writeCopilotJSONL(t, native, synthetic, tool)

	session, err := ReadCopilotFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCopilotFile() error = %v", err)
	}
	if got := len(session.Messages); got != 3 {
		t.Fatalf("len(Messages) = %d, want two messages and one tool use", got)
	}
	if got := session.Messages[0].UUID; got != "native-event-id" {
		t.Fatalf("native entry UUID = %q, want provider event ID", got)
	}
	if got, want := session.Messages[1].UUID, stableSyntheticEntryID("copilot", []byte(synthetic), ""); got != want {
		t.Fatalf("id-less message UUID = %q, want %q", got, want)
	}
	if got, want := session.Messages[2].UUID, stableSyntheticEntryID("copilot", []byte(tool), ""); got != want {
		t.Fatalf("id-less tool entry UUID = %q, want %q", got, want)
	}
	blocks := session.Messages[2].ContentBlocks()
	if len(blocks) != 1 || blocks[0].ID != "toolu-native" {
		t.Fatalf("tool blocks = %+v, want native tool call ID", blocks)
	}
}

func TestReadCopilotFileConvertsToolErrors(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"copilot-error","context":{"cwd":"/work/project"}},"id":"start-1","timestamp":"2026-03-04T02:30:58.550Z"}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"toolu-denied","toolName":"bash","arguments":{"command":"rm -rf build"}},"id":"start-tool","timestamp":"2026-03-04T02:31:02Z","parentId":"start-1"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"toolu-denied","success":false,"error":{"message":"Permission denied and could not request permission from user","code":"denied"}},"id":"complete-tool","timestamp":"2026-03-04T02:31:03Z","parentId":"start-tool"}`,
	)

	session, err := ReadCopilotFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCopilotFile() error = %v", err)
	}
	if got := len(session.Messages); got != 2 {
		t.Fatalf("len(Messages) = %d, want tool use and result", got)
	}
	result := session.Messages[1]
	blocks := result.ContentBlocks()
	if len(blocks) != 1 || !blocks[0].IsError {
		t.Fatalf("result blocks = %+v, want error tool result", blocks)
	}
	assertJSONHasString(t, blocks[0].Content, "error", "Permission denied and could not request permission from user")
	assertJSONHasString(t, blocks[0].Content, "code", "denied")
}

func TestReadProviderFileUsesCopilotReader(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"provider-dispatch","context":{"cwd":"/work/project"}},"id":"start-1","timestamp":"2026-03-04T02:30:58.550Z"}`,
		`{"type":"user.message","data":{"content":"hello"},"id":"user-1","timestamp":"2026-03-04T02:31:00Z","parentId":"start-1"}`,
	)

	session, err := ReadProviderFile("github-copilot/tmux-cli", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile() error = %v", err)
	}
	if session.ID != "provider-dispatch" || len(session.Messages) != 1 || session.Messages[0].Type != "user" {
		t.Fatalf("ReadProviderFile() = id %q messages %+v, want Copilot user transcript", session.ID, session.Messages)
	}
}

func TestReadCopilotFileDiagnostics(t *testing.T) {
	t.Run("malformed interior line", func(t *testing.T) {
		path := writeCopilotJSONL(t,
			`{"type":"user.message","data":{"content":"before"},"id":"user-1","timestamp":"2026-03-04T02:31:00Z"}`,
			`{"type":"tool.execution_complete","data":{"toolCallId":"broken","result":{"content":"unterminated"}`,
			`{"type":"assistant.message","data":{"content":"after"},"id":"assistant-1","timestamp":"2026-03-04T02:31:01Z"}`,
		)
		session, err := ReadCopilotFile(path, 0)
		if err != nil {
			t.Fatalf("ReadCopilotFile() error = %v", err)
		}
		if session.Diagnostics.MalformedLineCount != 1 || session.Diagnostics.MalformedTail {
			t.Fatalf("Diagnostics = %+v, want one malformed interior line", session.Diagnostics)
		}
		if len(session.Messages) != 2 {
			t.Fatalf("len(Messages) = %d, want readable prefix/suffix preserved", len(session.Messages))
		}
	})

	t.Run("malformed tail", func(t *testing.T) {
		path := writeCopilotJSONL(t,
			`{"type":"user.message","data":{"content":"before"},"id":"user-1","timestamp":"2026-03-04T02:31:00Z"}`,
			`{"type":"tool.execution_complete","data":{"toolCallId":"broken","result":{"content":"unterminated"}`,
		)
		session, err := ReadCopilotFile(path, 0)
		if err != nil {
			t.Fatalf("ReadCopilotFile() error = %v", err)
		}
		if session.Diagnostics.MalformedLineCount != 1 || !session.Diagnostics.MalformedTail {
			t.Fatalf("Diagnostics = %+v, want malformed tail", session.Diagnostics)
		}
		if len(session.Messages) != 1 {
			t.Fatalf("len(Messages) = %d, want readable prefix preserved", len(session.Messages))
		}
	})
}

func TestFindCopilotSessionFileByIDAndWorkDir(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	sessionDir := filepath.Join(root, "session-123")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	path := filepath.Join(sessionDir, "events.jsonl")
	writeFile(t, filepath.Join(sessionDir, "workspace.yaml"), fmt.Sprintf("id: session-123\ncwd: %q\n", workDir))
	writeFile(t, path, `{"type":"session.start","data":{"sessionId":"session-123","context":{"cwd":`+jsonString(workDir)+`}},"id":"start","timestamp":"2026-03-04T02:30:58.550Z"}`+"\n")

	if got := FindCopilotSessionFileByID([]string{root}, workDir, "session-123"); got != path {
		t.Fatalf("FindCopilotSessionFileByID() = %q, want %q", got, path)
	}
	if got := FindCopilotSessionFileByID([]string{root}, workDir, "../escape"); got != "" {
		t.Fatalf("FindCopilotSessionFileByID traversal = %q, want empty", got)
	}
	if got := FindCopilotSessionFile([]string{root}, workDir); got != path {
		t.Fatalf("FindCopilotSessionFile() = %q, want %q", got, path)
	}
	if got := FindCopilotSessionFile([]string{root}, filepath.Join(t.TempDir(), "other")); got != "" {
		t.Fatalf("FindCopilotSessionFile() wrong workdir = %q, want empty", got)
	}
}

func writeCopilotJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	return path
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func jsonString(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func assertJSONHasString(t *testing.T, raw json.RawMessage, key, want string) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	var got string
	if err := json.Unmarshal(object[key], &got); err != nil {
		t.Fatalf("unmarshal key %q from %s: %v", key, raw, err)
	}
	if got != want {
		t.Fatalf("field %q = %q, want %q in %s", key, got, want, raw)
	}
}

func assertJSONHasInt(t *testing.T, raw json.RawMessage, key string, want int) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	var got int
	if err := json.Unmarshal(object[key], &got); err != nil {
		t.Fatalf("unmarshal key %q from %s: %v", key, raw, err)
	}
	if got != want {
		t.Fatalf("field %q = %d, want %d in %s", key, got, want, raw)
	}
}

// The first record is the one the reporter of gastownhall/gascity#9064 pasted,
// redacted as posted ("msg_011C…", "aaebcbb3-…"). The rest are synthetic
// boundary rows. The only persisted-record relation the reporter documented is
// inputTokens >= cacheReadTokens + cacheWriteTokens on every record checked;
// this does not validate any wider persisted schema.
func TestExtractCopilotTailUsageAccounting(t *testing.T) {
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"selectedModel":"unrelated"}}`,
		`{"type":"session.usage_record","data":{"usage":{"model":"claude-opus-5.5","inputTokens":33701,"outputTokens":134,"cacheReadTokens":0,"cacheWriteTokens":33697,"reasoningTokens":0,"cost":15.0,"apiCallId":"msg_011C…"}},"id":"aaebcbb3-…","timestamp":"2026-11-10T21:22:22.735Z"}`,
		`{"type":"session.usage_record","data":{"usage":{"model":"claude-opus-5.5","inputTokens":999,"outputTokens":1,"apiCallId":"msg_011C…"}}}`,
		// Input exactly covered by cache reads and writes: the invariant's boundary.
		`{"type":"session.usage_record","data":{"usage":{"model":"next-model","inputTokens":40,"outputTokens":3,"cacheReadTokens":30,"cacheWriteTokens":10,"reasoningTokens":5,"apiCallId":"call-boundary"}}}`,
		// Past the boundary, negative, incomplete or non-integer rows are skipped.
		`{"type":"session.usage_record","data":{"usage":{"model":"bad","inputTokens":40,"outputTokens":2,"cacheReadTokens":30,"cacheWriteTokens":11,"apiCallId":"bad-cache-sum"}}}`,
		`{"type":"session.usage_record","data":{"usage":{"model":"bad","inputTokens":1,"outputTokens":2,"cacheReadTokens":3,"apiCallId":"bad-read"}}}`,
		`{"type":"session.usage_record","data":{"usage":{"model":"bad","inputTokens":1,"outputTokens":2}}}`,
		`{"type":"session.usage_record","data":{"usage":{"inputTokens":1,"outputTokens":2,"apiCallId":"no-model"}}}`,
		`{"type":"session.usage_record","data":{"usage":{"model":"bad","inputTokens":"no","outputTokens":2,"apiCallId":"bad-type"}}}`,
		`{"type":"session.usage_record","data":{"usage":{"model":"bad","inputTokens":-1,"outputTokens":2,"apiCallId":"bad-negative"}}}`,
		`{malformed`,
	)
	got, err := ExtractCopilotTailUsageFromSearchPaths([]string{filepath.Dir(path)}, path, "")
	if err != nil || len(got) != 2 {
		t.Fatalf("usage = %+v,%v", got, err)
	}
	a := got[0]
	if a.MessageID != "msg_011C…" || a.EntryUUID != "msg_011C…" || a.Model != "claude-opus-5.5" || a.InputTokens != 4 || a.OutputTokens != 134 || a.CacheReadTokens != 0 || a.CacheCreationTokens != 33697 || a.ReasoningTokens != 0 || a.Timestamp.IsZero() || a.HasReportedCost || a.ReportedCostUSD != 0 {
		t.Fatalf("reporter record accounting = %+v", a)
	}
	// The relation at its boundary leaves no uncached input; reasoning above
	// output is reported as given, not treated as a malformed record.
	if b := got[1]; b.MessageID != "call-boundary" || b.InputTokens != 0 || b.OutputTokens != 3 || b.CacheReadTokens != 30 || b.CacheCreationTokens != 10 || b.ReasoningTokens != 5 {
		t.Fatalf("boundary accounting = %+v", b)
	}
	if _, err := ExtractCopilotTailUsageFromSearchPaths([]string{t.TempDir()}, path, ""); err == nil {
		t.Fatal("escaped path accepted")
	}
	if _, err := ExtractCopilotTailUsageFromSearchPaths([]string{filepath.Dir(path)}, filepath.Join(filepath.Dir(path), "missing"), ""); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestExtractCopilotTailUsageCursorGrowth(t *testing.T) {
	first := `{"type":"session.usage_record","data":{"usage":{"model":"fixture-model","inputTokens":1,"outputTokens":2,"apiCallId":"old-call"}}}`
	padding := strings.Repeat(`{"type":"assistant.message","data":{"content":"padding"}}`+"\n", 2000)
	last := `{"type":"session.usage_record","data":{"usage":{"model":"fixture-model","inputTokens":3,"outputTokens":4,"apiCallId":"new-call"}}}`
	path := writeCopilotJSONL(t, first, padding, last)
	got, err := ExtractCopilotTailUsageFromSearchPaths([]string{filepath.Dir(path)}, path, "old-call")
	if err != nil || len(got) != 2 || got[0].MessageID != "old-call" || got[1].MessageID != "new-call" {
		t.Fatalf("cursor growth = %+v,%v", got, err)
	}
}
