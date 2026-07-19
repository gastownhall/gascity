package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderFamilyAmpAliases(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: "amp", want: "amp"},
		{provider: "amp/tmux-cli", want: "amp"},
		{provider: "sourcegraph-amp", want: "amp"},
		{provider: "wrapped/amp", want: "amp"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := ProviderFamily(tt.provider); got != tt.want {
				t.Fatalf("ProviderFamily(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}

func TestReadAmpFileConvertsStreamJSON(t *testing.T) {
	path := writeAmpJSONL(t,
		`{"type":"system","subtype":"init","cwd":"/work/project","session_id":"T-amp-session","tools":["Bash","edit_file"],"mcp_servers":[]}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"update and test"}]},"parent_tool_use_id":null,"session_id":"T-amp-session"}`,
		`{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"I will patch it."},{"type":"tool_use","id":"toolu-bash","name":"Bash","input":{"command":"npm test"}},{"type":"tool_use","id":"toolu-edit","name":"edit_file","input":{"filePath":"src/app.ts","oldString":"old","newString":"new"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,"output_tokens":5,"max_tokens":968000}},"parent_tool_use_id":null,"session_id":"T-amp-session"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu-bash","content":"{\"stdout\":\"ok\\n\",\"stderr\":\"\",\"exitCode\":0}","is_error":false}]},"parent_tool_use_id":null,"session_id":"T-amp-session"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu-edit","content":"{\"filePath\":\"src/app.ts\",\"patch\":\"*** Begin Patch\\n*** Update File: src/app.ts\\n@@\\n-old\\n+new\\n*** End Patch\",\"oldString\":\"old\",\"newString\":\"new\"}","is_error":false}]},"parent_tool_use_id":null,"session_id":"T-amp-session"}`,
		`{"type":"result","subtype":"success","duration_ms":7363,"is_error":false,"num_turns":2,"result":"done","session_id":"T-amp-session","usage":{"input_tokens":20,"output_tokens":10,"max_tokens":968000}}`,
	)

	session, err := ReadAmpFile(path, 0)
	if err != nil {
		t.Fatalf("ReadAmpFile() error = %v", err)
	}
	if session.ID != "T-amp-session" {
		t.Fatalf("Session.ID = %q, want T-amp-session", session.ID)
	}
	if got := len(session.Messages); got != 5 {
		t.Fatalf("len(Messages) = %d, want user, assistant, two tool results, system result", got)
	}
	userBlocks := session.Messages[0].ContentBlocks()
	if session.Messages[0].Type != "user" || len(userBlocks) != 1 || userBlocks[0].Text != "update and test" {
		t.Fatalf("user entry = %#v blocks %+v, want text prompt", session.Messages[0], userBlocks)
	}

	assistant := session.Messages[1]
	if assistant.Type != "assistant" {
		t.Fatalf("assistant type = %q, want assistant", assistant.Type)
	}
	blocks := assistant.ContentBlocks()
	if len(blocks) != 3 {
		t.Fatalf("assistant blocks = %+v, want text plus two tool uses", blocks)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "I will patch it." {
		t.Fatalf("text block = %+v, want assistant text", blocks[0])
	}
	if blocks[1].Type != "tool_use" || blocks[1].ID != "toolu-bash" || blocks[1].Name != "Bash" {
		t.Fatalf("bash tool block = %+v, want Bash tool_use", blocks[1])
	}
	assertJSONHasString(t, blocks[1].Input, "command", "npm test")
	if blocks[2].Type != "tool_use" || blocks[2].ID != "toolu-edit" || blocks[2].Name != "edit_file" {
		t.Fatalf("edit tool block = %+v, want edit_file tool_use", blocks[2])
	}
	assertJSONHasString(t, blocks[2].Input, "file_path", "src/app.ts")
	assertJSONHasString(t, blocks[2].Input, "old_string", "old")
	assertJSONHasString(t, blocks[2].Input, "new_string", "new")
	if strings.Contains(string(blocks[2].Input), "filePath") || strings.Contains(string(blocks[2].Input), "oldString") {
		t.Fatalf("assistant tool input leaked Amp-native camelCase keys: %s", blocks[2].Input)
	}

	bashResult := session.Messages[2].ContentBlocks()[0]
	if bashResult.Type != "tool_result" || bashResult.ToolUseID != "toolu-bash" || bashResult.IsError {
		t.Fatalf("bash result = %+v, want successful tool_result", bashResult)
	}
	assertJSONHasString(t, bashResult.Content, "stdout", "ok\n")
	assertJSONHasInt(t, bashResult.Content, "exit_code", 0)
	if strings.Contains(string(bashResult.Content), "exitCode") {
		t.Fatalf("bash result leaked Amp-native camelCase key: %s", bashResult.Content)
	}

	editResult := session.Messages[3].ContentBlocks()[0]
	if editResult.Type != "tool_result" || editResult.ToolUseID != "toolu-edit" || editResult.IsError {
		t.Fatalf("edit result = %+v, want successful tool_result", editResult)
	}
	assertJSONHasString(t, editResult.Content, "file_path", "src/app.ts")
	assertJSONHasString(t, editResult.Content, "patch", "*** Begin Patch\n*** Update File: src/app.ts\n@@\n-old\n+new\n*** End Patch")
	for _, forbidden := range []string{"filePath", "oldString", "newString", "tool_use_id"} {
		if strings.Contains(string(editResult.Content), forbidden) {
			t.Fatalf("edit result leaked Amp-native key %q: %s", forbidden, editResult.Content)
		}
	}

	final := session.Messages[4]
	if final.Type != "system" || final.SystemEvent == nil || final.SystemEvent.Kind != "result" || final.SystemEvent.Message != "done" {
		t.Fatalf("final entry = %#v, want provider-neutral result system event", final)
	}
}

func TestReadAmpFileConvertsToolErrors(t *testing.T) {
	path := writeAmpJSONL(t,
		`{"type":"system","subtype":"init","cwd":"/work/project","session_id":"T-amp-error","tools":["Bash"],"mcp_servers":[]}`,
		`{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu-denied","name":"Bash","input":{"command":"rm -rf build"}}],"stop_reason":"tool_use"},"parent_tool_use_id":null,"session_id":"T-amp-error"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu-denied","content":"permission denied","is_error":true}]},"parent_tool_use_id":null,"session_id":"T-amp-error"}`,
	)

	session, err := ReadAmpFile(path, 0)
	if err != nil {
		t.Fatalf("ReadAmpFile() error = %v", err)
	}
	if got := len(session.Messages); got != 2 {
		t.Fatalf("len(Messages) = %d, want tool use and result", got)
	}
	result := session.Messages[1].ContentBlocks()[0]
	if !result.IsError {
		t.Fatalf("IsError = false, want true; result = %+v", result)
	}
	if got := jsonStringValue(result.Content); got != "permission denied" {
		t.Fatalf("result content = %s, want permission denied string", result.Content)
	}
}

func TestReadProviderFileUsesAmpReader(t *testing.T) {
	path := writeAmpJSONL(t,
		`{"type":"system","subtype":"init","cwd":"/work/project","session_id":"T-dispatch","tools":[],"mcp_servers":[]}`,
		`{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"},"parent_tool_use_id":null,"session_id":"T-dispatch"}`,
	)

	session, err := ReadProviderFile("amp/tmux-cli", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile() error = %v", err)
	}
	if session.ID != "T-dispatch" || len(session.Messages) != 1 || session.Messages[0].Type != "assistant" {
		t.Fatalf("ReadProviderFile() = id %q messages %+v, want Amp assistant transcript", session.ID, session.Messages)
	}
}

func TestReadAmpFileDiagnostics(t *testing.T) {
	path := writeAmpJSONL(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"before"}]},"session_id":"T-diag"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text"`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"after"}]},"session_id":"T-diag"}`,
	)

	session, err := ReadAmpFile(path, 0)
	if err != nil {
		t.Fatalf("ReadAmpFile() error = %v", err)
	}
	if session.Diagnostics.MalformedLineCount != 1 || session.Diagnostics.MalformedTail {
		t.Fatalf("Diagnostics = %+v, want one malformed interior line", session.Diagnostics)
	}
	if len(session.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want readable prefix/suffix preserved", len(session.Messages))
	}
}

func TestFindAmpSessionFileByIDAndWorkDir(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	path := filepath.Join(root, "T-session.jsonl")
	writeFile(t, path, `{"type":"system","subtype":"init","cwd":`+jsonString(workDir)+`,"session_id":"T-session","tools":[],"mcp_servers":[]}`+"\n")

	if got := FindAmpSessionFileByID([]string{root}, workDir, "T-session"); got != path {
		t.Fatalf("FindAmpSessionFileByID() = %q, want %q", got, path)
	}
	if got := FindAmpSessionFileByID([]string{root}, workDir, "../escape"); got != "" {
		t.Fatalf("FindAmpSessionFileByID traversal = %q, want empty", got)
	}
	if got := FindAmpSessionFile([]string{root}, workDir); got != path {
		t.Fatalf("FindAmpSessionFile() = %q, want %q", got, path)
	}
	if got := FindAmpSessionFile([]string{root}, filepath.Join(t.TempDir(), "other")); got != "" {
		t.Fatalf("FindAmpSessionFile() wrong workdir = %q, want empty", got)
	}
}

func writeAmpJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stream.jsonl")
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	return path
}
