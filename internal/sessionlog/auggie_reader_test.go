package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderFamilyAuggieAliases(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: "auggie", want: "auggie"},
		{provider: "auggie/tmux-cli", want: "auggie"},
		{provider: "wrapped/auggie", want: "auggie"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := ProviderFamily(tt.provider); got != tt.want {
				t.Fatalf("ProviderFamily(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}

func TestReadAuggieFileConvertsACPUpdates(t *testing.T) {
	path := writeAuggieJSONL(t,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"auggie-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Running checks."}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"auggie-session","update":{"sessionUpdate":"tool_call","toolCallId":"toolu-cmd","title":"launch-process","kind":"execute","status":"pending","rawInput":{"command":"npm test","cwd":"/work/project"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"auggie-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu-cmd","status":"completed","rawOutput":{"stdout":"ok\n","stderr":"","exitCode":0}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"auggie-session","update":{"sessionUpdate":"tool_call","toolCallId":"toolu-edit","title":"str-replace-editor","kind":"edit","status":"pending","rawInput":{"path":"src/app.ts","oldText":"old","newText":"new"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"auggie-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu-edit","status":"completed","content":[{"type":"diff","path":"src/app.ts","oldText":"old\n","newText":"new\n"}]}}}`,
	)

	session, err := ReadAuggieFile(path, 0)
	if err != nil {
		t.Fatalf("ReadAuggieFile() error = %v", err)
	}
	if session.ID != "auggie-session" {
		t.Fatalf("Session.ID = %q, want auggie-session", session.ID)
	}
	if got := len(session.Messages); got != 5 {
		t.Fatalf("len(Messages) = %d, want 5", got)
	}
	if !strings.HasPrefix(session.Messages[0].UUID, "auggie-") {
		t.Fatalf("Auggie synthetic UUID = %q, want auggie- prefix", session.Messages[0].UUID)
	}
	if blocks := session.Messages[0].ContentBlocks(); len(blocks) != 1 || blocks[0].Text != "Running checks." {
		t.Fatalf("assistant blocks = %+v, want text chunk", blocks)
	}
	cmdUse := session.Messages[1].ContentBlocks()[0]
	if cmdUse.Type != "tool_use" || cmdUse.ID != "toolu-cmd" || cmdUse.Name != "launch-process" {
		t.Fatalf("command tool use = %+v, want launch-process tool_use", cmdUse)
	}
	assertJSONHasString(t, cmdUse.Input, "command", "npm test")
	assertJSONHasString(t, cmdUse.Input, "working_dir", "/work/project")
	if strings.Contains(string(cmdUse.Input), "toolCallId") || strings.Contains(string(cmdUse.Input), "rawInput") {
		t.Fatalf("command input leaked Auggie ACP key: %s", cmdUse.Input)
	}

	cmdResult := session.Messages[2].ContentBlocks()[0]
	assertJSONHasString(t, cmdResult.Content, "stdout", "ok\n")
	assertJSONHasInt(t, cmdResult.Content, "exit_code", 0)
	if strings.Contains(string(cmdResult.Content), "exitCode") || strings.Contains(string(cmdResult.Content), "toolCallId") {
		t.Fatalf("command result leaked Auggie ACP key: %s", cmdResult.Content)
	}

	editUse := session.Messages[3].ContentBlocks()[0]
	assertJSONHasString(t, editUse.Input, "file_path", "src/app.ts")
	assertJSONHasString(t, editUse.Input, "old_string", "old")
	assertJSONHasString(t, editUse.Input, "new_string", "new")

	editResult := session.Messages[4].ContentBlocks()[0]
	assertJSONHasString(t, editResult.Content, "file_path", "src/app.ts")
	assertJSONHasString(t, editResult.Content, "patch", "*** Begin Patch\n*** Update File: src/app.ts\n@@\n-old\n+new\n*** End Patch")
	for _, forbidden := range []string{"oldText", "newText", "toolCallId"} {
		if strings.Contains(string(editResult.Content), forbidden) {
			t.Fatalf("edit result leaked Auggie ACP key %q: %s", forbidden, editResult.Content)
		}
	}
}

func TestReadProviderFileUsesAuggieReader(t *testing.T) {
	path := writeAuggieJSONL(t,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"dispatch-session","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"hello"}}}}`,
	)
	session, err := ReadProviderFile("auggie/tmux-cli", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile() error = %v", err)
	}
	if session.ID != "dispatch-session" || len(session.Messages) != 1 || session.Messages[0].Type != "assistant" {
		t.Fatalf("ReadProviderFile() = id %q messages %+v, want Auggie assistant transcript", session.ID, session.Messages)
	}
}

func TestReadAuggieFilePreservesNativeIDsThatUseKiroPrefix(t *testing.T) {
	path := writeAuggieJSONL(t,
		`{"id":"kiro-native-id","type":"AssistantMessage","sessionId":"auggie-native","message":{"role":"assistant","content":"native"}}`,
	)
	session, err := ReadAuggieFile(path, 0)
	if err != nil {
		t.Fatalf("ReadAuggieFile() error = %v", err)
	}
	if len(session.Messages) != 1 {
		t.Fatalf("len(Messages) = %d, want one", len(session.Messages))
	}
	if got := session.Messages[0].UUID; got != "kiro-native-id" {
		t.Fatalf("native UUID = %q, want kiro-native-id preserved verbatim", got)
	}
}

func TestFindAuggieSessionFileByIDAndWorkDir(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	path := filepath.Join(root, "auggie-session.jsonl")
	writeFile(t, path, `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonString(workDir)+`}}`+"\n")

	if got := FindAuggieSessionFileByID([]string{root}, workDir, "auggie-session"); got != path {
		t.Fatalf("FindAuggieSessionFileByID() = %q, want %q", got, path)
	}
	if got := FindAuggieSessionFileByID([]string{root}, workDir, "../escape"); got != "" {
		t.Fatalf("FindAuggieSessionFileByID traversal = %q, want empty", got)
	}
	if got := FindAuggieSessionFile([]string{root}, workDir); got != path {
		t.Fatalf("FindAuggieSessionFile() = %q, want %q", got, path)
	}
	if got := FindAuggieSessionFile([]string{root}, filepath.Join(t.TempDir(), "other")); got != "" {
		t.Fatalf("FindAuggieSessionFile() wrong workdir = %q, want empty", got)
	}
}

func writeAuggieJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auggie.jsonl")
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	return path
}
