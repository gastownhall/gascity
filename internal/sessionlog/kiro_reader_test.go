package sessionlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderFamilyKiroAliases(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: "kiro", want: "kiro"},
		{provider: "kiro/tmux-cli", want: "kiro"},
		{provider: "wrapped/kiro", want: "kiro"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := ProviderFamily(tt.provider); got != tt.want {
				t.Fatalf("ProviderFamily(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}

func TestReadKiroFileConvertsACPUpdates(t *testing.T) {
	path := writeKiroJSONL(t,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"kiro-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Applying edits."}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"kiro-session","update":{"sessionUpdate":"tool_call","toolCallId":"toolu-bash","title":"bash","kind":"execute","status":"pending","rawInput":{"command":"printf hello","cwd":"/work/project"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"kiro-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu-bash","status":"completed","rawOutput":{"stdout":"hello\n","stderr":"","exitCode":0}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"kiro-session","update":{"sessionUpdate":"ToolCall","toolCallId":"toolu-edit","title":"write","kind":"edit","status":"pending","rawInput":{"path":"src/app.ts","content":"new file\n"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"kiro-session","update":{"sessionUpdate":"ToolCallUpdate","toolCallId":"toolu-edit","status":"completed","content":[{"type":"diff","path":"src/app.ts","oldText":"old line\n","newText":"new line\n"}]}}}`,
	)

	session, err := ReadKiroFile(path, 0)
	if err != nil {
		t.Fatalf("ReadKiroFile() error = %v", err)
	}
	if session.ID != "kiro-session" {
		t.Fatalf("Session.ID = %q, want kiro-session", session.ID)
	}
	if got := len(session.Messages); got != 5 {
		t.Fatalf("len(Messages) = %d, want 5", got)
	}

	assistant := session.Messages[0]
	if assistant.Type != "assistant" {
		t.Fatalf("assistant type = %q, want assistant", assistant.Type)
	}
	assistantBlocks := assistant.ContentBlocks()
	if len(assistantBlocks) != 1 || assistantBlocks[0].Type != "text" || assistantBlocks[0].Text != "Applying edits." {
		t.Fatalf("assistant blocks = %+v, want text chunk", assistantBlocks)
	}

	bashUse := session.Messages[1].ContentBlocks()[0]
	if bashUse.Type != "tool_use" || bashUse.ID != "toolu-bash" || bashUse.Name != "bash" {
		t.Fatalf("bash tool use = %+v, want provider-neutral bash tool use", bashUse)
	}
	assertJSONHasString(t, bashUse.Input, "command", "printf hello")
	assertJSONHasString(t, bashUse.Input, "working_dir", "/work/project")
	for _, forbidden := range []string{"toolCallId", "rawInput"} {
		if strings.Contains(string(bashUse.Input), forbidden) {
			t.Fatalf("bash input leaked Kiro-native key %q: %s", forbidden, bashUse.Input)
		}
	}

	bashResult := session.Messages[2].ContentBlocks()[0]
	if bashResult.Type != "tool_result" || bashResult.ToolUseID != "toolu-bash" || bashResult.IsError {
		t.Fatalf("bash result = %+v, want successful tool_result", bashResult)
	}
	assertJSONHasString(t, bashResult.Content, "stdout", "hello\n")
	assertJSONHasInt(t, bashResult.Content, "exit_code", 0)
	for _, forbidden := range []string{"exitCode", "rawOutput", "toolCallId"} {
		if strings.Contains(string(bashResult.Content), forbidden) {
			t.Fatalf("bash result leaked Kiro-native key %q: %s", forbidden, bashResult.Content)
		}
	}

	editUse := session.Messages[3].ContentBlocks()[0]
	if editUse.Type != "tool_use" || editUse.ID != "toolu-edit" || editUse.Name != "write" {
		t.Fatalf("edit tool use = %+v, want write tool use", editUse)
	}
	assertJSONHasString(t, editUse.Input, "file_path", "src/app.ts")
	assertJSONHasString(t, editUse.Input, "content", "new file\n")

	editResult := session.Messages[4].ContentBlocks()[0]
	if editResult.Type != "tool_result" || editResult.ToolUseID != "toolu-edit" || editResult.IsError {
		t.Fatalf("edit result = %+v, want successful edit result", editResult)
	}
	assertJSONHasString(t, editResult.Content, "file_path", "src/app.ts")
	assertJSONHasString(t, editResult.Content, "patch", "*** Begin Patch\n*** Update File: src/app.ts\n@@\n-old line\n+new line\n*** End Patch")
	for _, forbidden := range []string{"oldText", "newText", "toolCallId"} {
		if strings.Contains(string(editResult.Content), forbidden) {
			t.Fatalf("edit result leaked Kiro-native key %q: %s", forbidden, editResult.Content)
		}
	}
}

func TestReadKiroFileConvertsPersistedHistoryFrames(t *testing.T) {
	path := writeKiroJSONL(t,
		`{"type":"AssistantMessage","sessionId":"kiro-native","message":{"role":"assistant","content":[{"type":"text","text":"I will inspect it."},{"type":"toolUse","id":"toolu-read","name":"read","input":{"filePath":"src/app.ts"}}]}}`,
		`{"type":"ToolResults","sessionId":"kiro-native","message":{"role":"tool","content":[{"type":"toolResult","toolUseId":"toolu-read","content":{"filePath":"src/app.ts","content":"const answer = 42;\n","numLines":1},"isError":false}]}}`,
	)

	session, err := ReadKiroFile(path, 0)
	if err != nil {
		t.Fatalf("ReadKiroFile() error = %v", err)
	}
	if session.ID != "kiro-native" {
		t.Fatalf("Session.ID = %q, want kiro-native", session.ID)
	}
	if got := len(session.Messages); got != 2 {
		t.Fatalf("len(Messages) = %d, want 2", got)
	}
	blocks := session.Messages[0].ContentBlocks()
	if len(blocks) != 2 || blocks[1].Type != "tool_use" || blocks[1].ID != "toolu-read" {
		t.Fatalf("assistant blocks = %+v, want text and read tool use", blocks)
	}
	assertJSONHasString(t, blocks[1].Input, "file_path", "src/app.ts")
	if strings.Contains(string(blocks[1].Input), "filePath") {
		t.Fatalf("native input key leaked through Kiro persisted frame: %s", blocks[1].Input)
	}
	result := session.Messages[1].ContentBlocks()[0]
	assertJSONHasString(t, result.Content, "file_path", "src/app.ts")
	assertJSONHasString(t, result.Content, "content", "const answer = 42;\n")
	assertJSONHasInt(t, result.Content, "num_lines", 1)
	if strings.Contains(string(result.Content), "filePath") || strings.Contains(string(result.Content), "numLines") {
		t.Fatalf("native result key leaked through Kiro persisted frame: %s", result.Content)
	}
}

func TestReadKiroFileUsesNativeAndStableSyntheticEntryIDs(t *testing.T) {
	native := `{"id":"native-message-id","type":"AssistantMessage","sessionId":"kiro-ids","message":{"role":"assistant","content":"native"}}`
	synthetic := `{"type":"AssistantMessage","sessionId":"kiro-ids","message":{"role":"assistant","content":"synthetic"}}`
	multipart := `{"type":"ToolResults","sessionId":"kiro-ids","message":{"role":"tool","content":[{"type":"toolResult","toolUseId":"toolu-one","content":"one"},{"type":"toolResult","toolUseId":"toolu-two","content":"two"}]}}`
	path := writeKiroJSONL(t, native, synthetic, multipart)

	session, err := ReadKiroFile(path, 0)
	if err != nil {
		t.Fatalf("ReadKiroFile() error = %v", err)
	}
	if got := len(session.Messages); got != 4 {
		t.Fatalf("len(Messages) = %d, want native, synthetic, and two tool results", got)
	}
	if got := session.Messages[0].UUID; got != "native-message-id" {
		t.Fatalf("native entry UUID = %q, want provider ID", got)
	}
	if got, want := session.Messages[1].UUID, stableSyntheticEntryID("kiro", []byte(synthetic), ""); got != want {
		t.Fatalf("id-less entry UUID = %q, want %q", got, want)
	}
	for i, wantToolUseID := range []string{"toolu-one", "toolu-two"} {
		entry := session.Messages[i+2]
		wantID := stableSyntheticEntryID("kiro", []byte(multipart), fmt.Sprintf("%d", i))
		if entry.UUID != wantID {
			t.Fatalf("tool result %d UUID = %q, want %q", i, entry.UUID, wantID)
		}
		if entry.ToolUseID != wantToolUseID {
			t.Fatalf("tool result %d ToolUseID = %q, want %q", i, entry.ToolUseID, wantToolUseID)
		}
	}
	if session.Messages[2].UUID == session.Messages[3].UUID {
		t.Fatalf("multipart tool results share UUID %q", session.Messages[2].UUID)
	}
}

func TestReadKiroFileMultipartNativeRecordCannotAliasNativeEntryID(t *testing.T) {
	multipart := `{"id":"x","type":"ToolResults","sessionId":"kiro-native-alias","message":{"role":"tool","content":[{"type":"toolResult","toolUseId":"toolu-one","content":"one"},{"type":"toolResult","toolUseId":"toolu-two","content":"two"}]}}`
	native := `{"id":"x-0","type":"AssistantMessage","sessionId":"kiro-native-alias","message":{"role":"assistant","content":"native x-0"}}`
	path := writeKiroJSONL(t, multipart, native)

	session, err := ReadKiroFile(path, 0)
	if err != nil {
		t.Fatalf("ReadKiroFile() error = %v", err)
	}
	if got := len(session.Messages); got != 3 {
		t.Fatalf("len(Messages) = %d, want two tool results plus assistant", got)
	}
	want := []string{
		stableSyntheticEntryID("kiro", []byte(multipart), "0"),
		stableSyntheticEntryID("kiro", []byte(multipart), "1"),
		"x-0",
	}
	for i, entry := range session.Messages {
		if entry.UUID != want[i] {
			t.Fatalf("message %d UUID = %q, want %q", i, entry.UUID, want[i])
		}
	}
}

func TestReadKiroFileNumericIDCannotAliasNativeStringID(t *testing.T) {
	numeric := `{"id":1,"type":"AssistantMessage","sessionId":"kiro-numeric-alias","message":{"role":"assistant","content":"numeric"}}`
	native := `{"id":"kiro-1","type":"AssistantMessage","sessionId":"kiro-numeric-alias","message":{"role":"assistant","content":"native string"}}`
	path := writeKiroJSONL(t, numeric, native)

	session, err := ReadProviderFile("kiro/tmux-cli", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile() error = %v", err)
	}
	want := []string{
		stableSyntheticEntryID("kiro", []byte(numeric), ""),
		"kiro-1",
	}
	if len(session.Messages) != len(want) {
		t.Fatalf("len(Messages) = %d, want %d", len(session.Messages), len(want))
	}
	for i, entry := range session.Messages {
		if entry.UUID != want[i] {
			t.Fatalf("message %d UUID = %q, want %q", i, entry.UUID, want[i])
		}
	}
}

func TestReadProviderFileUsesKiroReader(t *testing.T) {
	path := writeKiroJSONL(t,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"dispatch-session","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"hello"}}}}`,
	)

	session, err := ReadProviderFile("kiro/tmux-cli", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile() error = %v", err)
	}
	if session.ID != "dispatch-session" || len(session.Messages) != 1 || session.Messages[0].Type != "assistant" {
		t.Fatalf("ReadProviderFile() = id %q messages %+v, want Kiro assistant transcript", session.ID, session.Messages)
	}
}

func TestReadKiroFileDiagnostics(t *testing.T) {
	t.Run("malformed interior line", func(t *testing.T) {
		path := writeKiroJSONL(t,
			`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"diag","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"before"}}}}`,
			`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"diag","update":{"sessionUpdate":"tool_call"`,
			`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"diag","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"after"}}}}`,
		)
		session, err := ReadKiroFile(path, 0)
		if err != nil {
			t.Fatalf("ReadKiroFile() error = %v", err)
		}
		if session.Diagnostics.MalformedLineCount != 1 || session.Diagnostics.MalformedTail {
			t.Fatalf("Diagnostics = %+v, want one malformed interior line", session.Diagnostics)
		}
		if len(session.Messages) != 2 {
			t.Fatalf("len(Messages) = %d, want readable prefix/suffix preserved", len(session.Messages))
		}
	})

	t.Run("malformed tail", func(t *testing.T) {
		path := writeKiroJSONL(t,
			`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"diag","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"before"}}}}`,
			`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"diag","update":{"sessionUpdate":"tool_call"`,
		)
		session, err := ReadKiroFile(path, 0)
		if err != nil {
			t.Fatalf("ReadKiroFile() error = %v", err)
		}
		if session.Diagnostics.MalformedLineCount != 1 || !session.Diagnostics.MalformedTail {
			t.Fatalf("Diagnostics = %+v, want malformed tail", session.Diagnostics)
		}
		if len(session.Messages) != 1 {
			t.Fatalf("len(Messages) = %d, want readable prefix preserved", len(session.Messages))
		}
	})
}

func TestFindKiroSessionFileByIDAndWorkDir(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	path := filepath.Join(root, "session-123.jsonl")
	writeFile(t, filepath.Join(root, "session-123.json"), fmt.Sprintf(`{"id":"session-123","cwd":%s}`, jsonString(workDir)))
	writeFile(t, path, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-123","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"hello"}}}}`+"\n")

	if got := FindKiroSessionFileByID([]string{root}, workDir, "session-123"); got != path {
		t.Fatalf("FindKiroSessionFileByID() = %q, want %q", got, path)
	}
	if got := FindKiroSessionFileByID([]string{root}, workDir, "../escape"); got != "" {
		t.Fatalf("FindKiroSessionFileByID traversal = %q, want empty", got)
	}
	if got := FindKiroSessionFile([]string{root}, workDir); got != path {
		t.Fatalf("FindKiroSessionFile() = %q, want %q", got, path)
	}
	if got := FindKiroSessionFile([]string{root}, filepath.Join(t.TempDir(), "other")); got != "" {
		t.Fatalf("FindKiroSessionFile() wrong workdir = %q, want empty", got)
	}
}

func writeKiroJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	return path
}
