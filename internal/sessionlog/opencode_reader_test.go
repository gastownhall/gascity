package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadOpenCodeFileNormalizesExportedMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_export.json")
	body := `{
  "info": {
    "id": "ses_opencode_phase1",
    "directory": "/tmp/gascity/phase1/opencode"
  },
  "messages": [
    {
      "info": {"id":"msg_user_1","sessionID":"ses_opencode_phase1","role":"user","time":{"created":1770000000000},"agent":"build","model":{"providerID":"google","modelID":"gemini-2.5-flash"}},
      "parts": [{"id":"part_user_1","sessionID":"ses_opencode_phase1","messageID":"msg_user_1","type":"text","text":"hello opencode"}]
    },
    {
      "info": {"id":"msg_assistant_1","sessionID":"ses_opencode_phase1","role":"assistant","time":{"created":1770000001000},"parentID":"msg_user_1","providerID":"google","modelID":"gemini-2.5-flash","mode":"build","path":{"cwd":"/tmp/gascity/phase1/opencode","root":"/tmp/gascity/phase1/opencode"},"cost":0,"tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}},
      "parts": [{"id":"part_assistant_1","sessionID":"ses_opencode_phase1","messageID":"msg_assistant_1","type":"text","text":"hello from Gemini through OpenCode"}]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write export fixture: %v", err)
	}

	sess, err := ReadOpenCodeFile(path, 0)
	if err != nil {
		t.Fatalf("ReadOpenCodeFile: %v", err)
	}
	if sess.ID != "ses_opencode_phase1" {
		t.Fatalf("ID = %q, want ses_opencode_phase1", sess.ID)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sess.Messages))
	}
	if got := sess.Messages[0].TextContent(); got != "hello opencode" {
		t.Fatalf("user text = %q", got)
	}
	if got := sess.Messages[1].TextContent(); got != "hello from Gemini through OpenCode" {
		t.Fatalf("assistant text = %q", got)
	}
}

func TestReadOpenCodeFilePreservesRepeatedIdlessMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_export.json")
	repeated := `{"info":{"role":"assistant"},"parts":[{"type":"text","text":"repeat"}]}`
	writeRepeated := func(count int) {
		t.Helper()
		messages := strings.TrimSuffix(strings.Repeat(repeated+",", count), ",")
		body := `{"info":{"id":"session-1"},"messages":[` + messages + `]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write OpenCode fixture: %v", err)
		}
	}

	writeRepeated(2)
	before, err := ReadProviderFile("opencode/tmux-cli", path, 0)
	if err != nil {
		t.Fatalf("read two repeated id-less messages: %v", err)
	}
	beforeIDs := paginationEntryIDs(before.Messages)
	if len(beforeIDs) != 2 || beforeIDs[0] == beforeIDs[1] {
		t.Fatalf("entry IDs = %v, want two unique IDs", beforeIDs)
	}

	writeRepeated(3)
	after, err := ReadProviderFile("opencode/tmux-cli", path, 0)
	if err != nil {
		t.Fatalf("read after appending repeated id-less message: %v", err)
	}
	afterIDs := paginationEntryIDs(after.Messages)
	if len(afterIDs) != 3 {
		t.Fatalf("entry IDs after append = %v, want three entries", afterIDs)
	}
	if afterIDs[0] != beforeIDs[0] || afterIDs[1] != beforeIDs[1] {
		t.Fatalf("retained IDs changed after append: got %v, want prefix %v", afterIDs, beforeIDs)
	}
	if afterIDs[2] == afterIDs[0] || afterIDs[2] == afterIDs[1] {
		t.Fatalf("appended repeated message reused an existing ID: %v", afterIDs)
	}
}

func TestReadOpenCodeFileNormalizesTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_export.json")
	body := `{
  "info": {"id": "ses_tool", "directory": "/tmp/gascity/phase2/opencode"},
  "messages": [
    {
      "info": {"id":"msg_user_1","sessionID":"ses_tool","role":"user","time":{"created":1770000000000},"agent":"build","model":{"providerID":"google","modelID":"gemini-2.5-flash"}},
      "parts": [{"id":"part_user_1","sessionID":"ses_tool","messageID":"msg_user_1","type":"text","text":"read the file"}]
    },
    {
      "info": {"id":"msg_assistant_1","sessionID":"ses_tool","role":"assistant","time":{"created":1770000001000},"parentID":"msg_user_1","providerID":"google","modelID":"gemini-2.5-flash","mode":"build","path":{"cwd":"/tmp/gascity/phase2/opencode","root":"/tmp/gascity/phase2/opencode"},"cost":0,"tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}},
      "parts": [{"id":"part_tool_1","sessionID":"ses_tool","messageID":"msg_assistant_1","type":"tool","callID":"call-1","tool":"Read","state":{"status":"completed","input":{"path":"README.md"},"output":"file data","title":"Read README.md","metadata":{},"time":{"start":1770000001000,"end":1770000002000}}}]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write export fixture: %v", err)
	}

	sess, err := ReadOpenCodeFile(path, 0)
	if err != nil {
		t.Fatalf("ReadOpenCodeFile: %v", err)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sess.Messages))
	}
	blocks := sess.Messages[1].ContentBlocks()
	if len(blocks) != 2 {
		t.Fatalf("tool blocks = %d, want 2", len(blocks))
	}
	if blocks[0].Type != "tool_use" || blocks[0].Name != "Read" || blocks[0].ID != "call-1" {
		t.Fatalf("tool_use block = %#v", blocks[0])
	}
	if blocks[1].Type != "tool_result" || blocks[1].ToolUseID != "call-1" {
		t.Fatalf("tool_result block = %#v", blocks[1])
	}
	if len(sess.OrphanedToolUseIDs) != 0 {
		t.Fatalf("OrphanedToolUseIDs = %#v, want none", sess.OrphanedToolUseIDs)
	}
}

func TestReadOpenCodeFileNormalizesToolObjectsToNeutralKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_export.json")
	body := `{
  "info": {"id": "ses_tool", "directory": "/tmp/gascity/phase2/opencode"},
  "messages": [
    {
      "info": {"id":"msg_assistant_1","sessionID":"ses_tool","role":"assistant","time":{"created":1770000001000}},
      "parts": [{"id":"part_tool_1","type":"tool","callID":"call-1","tool":"Edit","state":{"status":"completed","input":{"filePath":"README.md","oldString":"old","newString":"new"},"output":{"filePath":"README.md","patch":"--- README.md\n+++ README.md\n@@\n-old\n+new","exitCode":0,"durationMs":12}}}]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write export fixture: %v", err)
	}

	sess, err := ReadOpenCodeFile(path, 0)
	if err != nil {
		t.Fatalf("ReadOpenCodeFile: %v", err)
	}
	blocks := sess.Messages[0].ContentBlocks()
	if len(blocks) != 2 {
		t.Fatalf("tool blocks = %d, want 2", len(blocks))
	}
	var input struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(blocks[0].Input, &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if input.FilePath != "README.md" || input.OldString != "old" || input.NewString != "new" {
		t.Fatalf("neutral input = %+v, want README.md old/new", input)
	}
	var output struct {
		FilePath   string `json:"file_path"`
		Patch      string `json:"patch"`
		ExitCode   int    `json:"exit_code"`
		DurationMs int    `json:"duration_ms"`
	}
	if err := json.Unmarshal(blocks[1].Content, &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if output.FilePath != "README.md" || !strings.Contains(output.Patch, "+new") || output.ExitCode != 0 || output.DurationMs != 12 {
		t.Fatalf("neutral output = %+v, want patch/exit/duration", output)
	}
	for _, forbidden := range []string{"filePath", "oldString", "newString", "exitCode", "durationMs"} {
		if strings.Contains(string(blocks[0].Input), forbidden) || strings.Contains(string(blocks[1].Content), forbidden) {
			t.Fatalf("OpenCode normalized blocks leaked %s: input=%s content=%s", forbidden, blocks[0].Input, blocks[1].Content)
		}
	}
}

func TestFindOpenCodeSessionFileMatchesExportDirectory(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	oldPath := filepath.Join(root, "old.json")
	newPath := filepath.Join(root, "nested", "new.json")
	for _, item := range []struct {
		path string
		id   string
	}{
		{oldPath, "old"},
		{newPath, "new"},
	} {
		body := `{"info":{"id":"` + item.id + `","directory":"` + filepath.ToSlash(workDir) + `"},"messages":[]}`
		if err := os.WriteFile(item.path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", item.path, err)
		}
	}

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(newPath, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got := FindOpenCodeSessionFile([]string{root}, workDir)
	if got != newPath {
		t.Fatalf("FindOpenCodeSessionFile() = %q, want %q", got, newPath)
	}
}
