package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderFamilyGrokAliases(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: "grok", want: "grok"},
		{provider: "grok/tmux-cli", want: "grok"},
		{provider: "wrapped/grok", want: "grok"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := ProviderFamily(tt.provider); got != tt.want {
				t.Fatalf("ProviderFamily(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}

func TestReadGrokFileConvertsACPUpdates(t *testing.T) {
	path := writeGrokJSONL(t,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Running tests."}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"tool_call","toolCallId":"toolu-cmd","title":"run_terminal_cmd","kind":"execute","status":"pending","rawInput":{"command":"go test ./...","cwd":"/work/project"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu-cmd","status":"completed","rawOutput":{"stdout":"ok\n","stderr":"","exitCode":0}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"tool_call","toolCallId":"toolu-edit","title":"search_replace","kind":"edit","status":"pending","rawInput":{"path":"src/app.ts","oldText":"old","newText":"new"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu-edit","status":"completed","content":[{"type":"diff","path":"src/app.ts","oldText":"old\n","newText":"new\n"}]}}}`,
	)

	session, err := ReadGrokFile(path, 0)
	if err != nil {
		t.Fatalf("ReadGrokFile() error = %v", err)
	}
	if session.ID != "grok-session" {
		t.Fatalf("Session.ID = %q, want grok-session", session.ID)
	}
	if got := len(session.Messages); got != 5 {
		t.Fatalf("len(Messages) = %d, want 5", got)
	}
	if !strings.HasPrefix(session.Messages[0].UUID, "grok-") {
		t.Fatalf("Grok synthetic UUID = %q, want grok- prefix", session.Messages[0].UUID)
	}
	if blocks := session.Messages[0].ContentBlocks(); len(blocks) != 1 || blocks[0].Text != "Running tests." {
		t.Fatalf("assistant blocks = %+v, want text chunk", blocks)
	}
	cmdUse := session.Messages[1].ContentBlocks()[0]
	if cmdUse.Type != "tool_use" || cmdUse.ID != "toolu-cmd" || cmdUse.Name != "run_terminal_cmd" {
		t.Fatalf("command tool use = %+v, want run_terminal_cmd tool_use", cmdUse)
	}
	assertJSONHasString(t, cmdUse.Input, "command", "go test ./...")
	assertJSONHasString(t, cmdUse.Input, "working_dir", "/work/project")
	if strings.Contains(string(cmdUse.Input), "toolCallId") || strings.Contains(string(cmdUse.Input), "rawInput") {
		t.Fatalf("command input leaked Grok ACP key: %s", cmdUse.Input)
	}

	cmdResult := session.Messages[2].ContentBlocks()[0]
	assertJSONHasString(t, cmdResult.Content, "stdout", "ok\n")
	assertJSONHasInt(t, cmdResult.Content, "exit_code", 0)
	if strings.Contains(string(cmdResult.Content), "exitCode") || strings.Contains(string(cmdResult.Content), "toolCallId") {
		t.Fatalf("command result leaked Grok ACP key: %s", cmdResult.Content)
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
			t.Fatalf("edit result leaked Grok ACP key %q: %s", forbidden, editResult.Content)
		}
	}
}

func TestReadProviderFileUsesGrokReader(t *testing.T) {
	path := writeGrokJSONL(t,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"dispatch-session","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"hello"}}}}`,
	)
	session, err := ReadProviderFile("grok/tmux-cli", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile() error = %v", err)
	}
	if session.ID != "dispatch-session" || len(session.Messages) != 1 || session.Messages[0].Type != "assistant" {
		t.Fatalf("ReadProviderFile() = id %q messages %+v, want Grok assistant transcript", session.ID, session.Messages)
	}
}

func TestFindGrokSessionFileByIDAndWorkDir(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	path := filepath.Join(root, "grok-session.jsonl")
	writeFile(t, path, `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+jsonString(workDir)+`}}`+"\n")

	if got := FindGrokSessionFileByID([]string{root}, workDir, "grok-session"); got != path {
		t.Fatalf("FindGrokSessionFileByID() = %q, want %q", got, path)
	}
	if got := FindGrokSessionFileByID([]string{root}, workDir, "../escape"); got != "" {
		t.Fatalf("FindGrokSessionFileByID traversal = %q, want empty", got)
	}
	if got := FindGrokSessionFile([]string{root}, workDir); got != path {
		t.Fatalf("FindGrokSessionFile() = %q, want %q", got, path)
	}
	if got := FindGrokSessionFile([]string{root}, filepath.Join(t.TempDir(), "other")); got != "" {
		t.Fatalf("FindGrokSessionFile() wrong workdir = %q, want empty", got)
	}
}

func writeGrokJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grok.jsonl")
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	return path
}
