package sessionlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadPiFileNormalizesNativeMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"session","version":3,"id":"ses_pi_phase1","timestamp":"2026-02-02T00:00:00.000Z","cwd":"/tmp/gascity/phase1/pi"}
{"type":"message","id":"msg_user_1","parentId":null,"timestamp":"2026-02-02T00:00:00.000Z","message":{"role":"user","content":"hello pi","timestamp":1770000000000}}
{"type":"message","id":"msg_assistant_1","parentId":"msg_user_1","timestamp":"2026-02-02T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hello from Ollama Cloud through Pi"}],"provider":"ollama-cloud","model":"gpt-oss:20b","timestamp":1770000001000}}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pi fixture: %v", err)
	}

	sess, err := ReadPiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadPiFile: %v", err)
	}
	if sess.ID != "ses_pi_phase1" {
		t.Fatalf("ID = %q, want ses_pi_phase1", sess.ID)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sess.Messages))
	}
	if got := sess.Messages[0].TextContent(); got != "hello pi" {
		t.Fatalf("user text = %q", got)
	}
	if got := sess.Messages[1].TextContent(); got != "hello from Ollama Cloud through Pi" {
		t.Fatalf("assistant text = %q", got)
	}
}

func TestReadPiFileNormalizesTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"session","version":3,"id":"ses_tool","timestamp":"2026-02-02T00:00:00.000Z","cwd":"/tmp/gascity/phase2/pi"}
{"type":"message","id":"msg_user_1","parentId":null,"timestamp":"2026-02-02T00:00:00.000Z","message":{"role":"user","content":"read the file","timestamp":1770000000000}}
{"type":"message","id":"msg_assistant_1","parentId":"msg_user_1","timestamp":"2026-02-02T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"read","arguments":{"path":"README.md"}}],"provider":"ollama-cloud","model":"gpt-oss:20b","timestamp":1770000001000}}
{"type":"message","id":"msg_tool_1","parentId":"msg_assistant_1","timestamp":"2026-02-02T00:00:02.000Z","message":{"role":"toolResult","toolCallId":"call-1","toolName":"read","content":[{"type":"text","text":"file data"}],"isError":false,"timestamp":1770000002000}}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pi fixture: %v", err)
	}

	sess, err := ReadPiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadPiFile: %v", err)
	}
	if len(sess.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(sess.Messages))
	}
	toolUseBlocks := sess.Messages[1].ContentBlocks()
	if len(toolUseBlocks) != 1 || toolUseBlocks[0].Type != "tool_use" || toolUseBlocks[0].ID != "call-1" {
		t.Fatalf("tool_use blocks = %#v", toolUseBlocks)
	}
	toolResultBlocks := sess.Messages[2].ContentBlocks()
	if len(toolResultBlocks) != 1 || toolResultBlocks[0].Type != "tool_result" || toolResultBlocks[0].ToolUseID != "call-1" {
		t.Fatalf("tool_result blocks = %#v", toolResultBlocks)
	}
	if len(sess.OrphanedToolUseIDs) != 0 {
		t.Fatalf("OrphanedToolUseIDs = %#v, want none", sess.OrphanedToolUseIDs)
	}
}

func TestFindPiSessionFileMatchesSessionCWD(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(root, "--tmp--project"), 0o755); err != nil {
		t.Fatalf("mkdir pi tree: %v", err)
	}
	oldPath := filepath.Join(root, "old.jsonl")
	newPath := filepath.Join(root, "--tmp--project", "new.jsonl")
	for _, item := range []struct {
		path string
		id   string
	}{
		{oldPath, "old"},
		{newPath, "new"},
	} {
		body := `{"type":"session","version":3,"id":"` + item.id + `","timestamp":"2026-02-02T00:00:00.000Z","cwd":"` + filepath.ToSlash(workDir) + `"}`
		if err := os.WriteFile(item.path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", item.path, err)
		}
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(newPath, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got := FindPiSessionFile([]string{root}, workDir)
	if got != newPath {
		t.Fatalf("FindPiSessionFile() = %q, want %q", got, newPath)
	}
}
