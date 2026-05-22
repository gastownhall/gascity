package sessionlog

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadKimiFilePreservesNativeToolRows(t *testing.T) {
	path := writeKimiContext(t, filepath.Join(t.TempDir(), "sessions", "hash", "session-123", "context.jsonl"), []string{
		`{"role":"user","content":"read the file"}`,
		`{"role":"assistant","content":[],"tool_calls":[{"type":"function","id":"call-1","function":{"name":"Read","arguments":"{\"path\":\"README.md\"}"}}]}`,
		`{"role":"tool","content":[{"type":"text","text":"file data"}],"tool_call_id":"call-1"}`,
		`{"role":"assistant","content":"done"}`,
	})

	sess, err := ReadKimiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadKimiFile: %v", err)
	}
	if len(sess.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(sess.Messages))
	}
	toolUse := sess.Messages[1]
	toolUseBlocks := toolUse.ContentBlocks()
	if len(toolUseBlocks) != 1 {
		t.Fatalf("tool use blocks = %d, want 1", len(toolUseBlocks))
	}
	if toolUseBlocks[0].Type != "tool_use" || toolUseBlocks[0].ID != "call-1" || toolUseBlocks[0].Name != "Read" {
		t.Fatalf("tool use block = %#v, want call-1 Read tool_use", toolUseBlocks[0])
	}
	var toolInput struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(toolUseBlocks[0].Input, &toolInput); err != nil {
		t.Fatalf("unmarshal tool input: %v", err)
	}
	if toolInput.Path != "README.md" {
		t.Fatalf("tool input path = %q, want README.md", toolInput.Path)
	}
	toolResult := sess.Messages[2]
	if toolResult.Type != "result" {
		t.Fatalf("tool result type = %q, want result", toolResult.Type)
	}
	if toolResult.ToolUseID != "call-1" {
		t.Fatalf("tool result ToolUseID = %q, want call-1", toolResult.ToolUseID)
	}
	blocks := toolResult.ContentBlocks()
	if len(blocks) != 1 {
		t.Fatalf("tool result blocks = %d, want 1", len(blocks))
	}
	if blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "call-1" {
		t.Fatalf("tool result block = %#v, want call-1 tool_result", blocks[0])
	}
}

func TestReadKimiFileReportsOpenNativeToolCallTail(t *testing.T) {
	path := writeKimiContext(t, filepath.Join(t.TempDir(), "sessions", "hash", "session-123", "context.jsonl"), []string{
		`{"role":"user","content":"read the file"}`,
		`{"role":"assistant","content":[],"tool_calls":[{"type":"function","id":"call-open","function":{"name":"Read","arguments":"{\"path\":\"README.md\"}"}}]}`,
	})

	sess, err := ReadKimiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadKimiFile: %v", err)
	}
	if !sess.OrphanedToolUseIDs["call-open"] {
		t.Fatalf("OrphanedToolUseIDs = %#v, want call-open", sess.OrphanedToolUseIDs)
	}
}

func TestReadKimiFileNormalizesEmptyOrphanedToolUseIDs(t *testing.T) {
	path := writeKimiContext(t, filepath.Join(t.TempDir(), "sessions", "hash", "session-123", "context.jsonl"), []string{
		`{"role":"user","content":"hello"}`,
		`{"role":"assistant","content":"done"}`,
	})

	sess, err := ReadKimiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadKimiFile: %v", err)
	}
	if sess.OrphanedToolUseIDs != nil {
		t.Fatalf("OrphanedToolUseIDs = %#v, want nil for no orphaned tools", sess.OrphanedToolUseIDs)
	}
}

func TestReadKimiFileNativeToolCallArgumentShapes(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
		want      any
	}{
		{
			name:      "raw object",
			arguments: `{"path":"README.md"}`,
			want:      map[string]any{"path": "README.md"},
		},
		{
			name:      "invalid json string",
			arguments: `"raw text"`,
			want:      "raw text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeKimiContext(t, filepath.Join(t.TempDir(), "sessions", "hash", "session-123", "context.jsonl"), []string{
				`{"role":"assistant","content":[],"tool_calls":[{"type":"function","id":"call-1","function":{"name":"Read","arguments":` + tt.arguments + `}}]}`,
			})

			sess, err := ReadKimiFile(path, 0)
			if err != nil {
				t.Fatalf("ReadKimiFile: %v", err)
			}
			blocks := sess.Messages[0].ContentBlocks()
			if len(blocks) != 1 {
				t.Fatalf("tool use blocks = %d, want 1", len(blocks))
			}
			var got any
			if err := json.Unmarshal(blocks[0].Input, &got); err != nil {
				t.Fatalf("unmarshal tool input: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("tool input = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReadKimiFileSkipsDegenerateNativeToolCalls(t *testing.T) {
	path := writeKimiContext(t, filepath.Join(t.TempDir(), "sessions", "hash", "session-123", "context.jsonl"), []string{
		`{"role":"assistant","content":[],"tool_calls":[{}]}`,
	})

	sess, err := ReadKimiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadKimiFile: %v", err)
	}
	blocks := sess.Messages[0].ContentBlocks()
	if len(blocks) != 0 {
		t.Fatalf("tool use blocks = %#v, want none", blocks)
	}
}

func TestReadKimiFileDiagnosticsAndContentShapes(t *testing.T) {
	path := writeKimiContext(t, filepath.Join(t.TempDir(), "session-abc", "context.jsonl"), []string{
		`{"role":"_system_prompt","content":"ignore"}`,
		`{bad json`,
		`{"role":"user","content":"plain text"}`,
		`{"role":"assistant","content":[{"type":"text","text":"block text"}]}`,
		`{"role":"system","content":"system text"}`,
		`{"role":"_usage","token_count":3}`,
	})

	sess, err := ReadKimiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadKimiFile: %v", err)
	}
	if sess.Diagnostics.MalformedLineCount != 1 {
		t.Fatalf("MalformedLineCount = %d, want 1", sess.Diagnostics.MalformedLineCount)
	}
	if sess.Diagnostics.MalformedTail {
		t.Fatal("MalformedTail = true, want false for non-tail malformed line")
	}
	if len(sess.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(sess.Messages))
	}
	var first MessageContent
	if err := json.Unmarshal(sess.Messages[0].Message, &first); err != nil {
		t.Fatalf("unmarshal first message: %v", err)
	}
	var firstText string
	if err := json.Unmarshal(first.Content, &firstText); err != nil {
		t.Fatalf("unmarshal first content: %v", err)
	}
	if firstText != "plain text" {
		t.Fatalf("first content = %q, want plain text", firstText)
	}
	blocks := sess.Messages[1].ContentBlocks()
	if len(blocks) != 1 || blocks[0].Text != "block text" {
		t.Fatalf("assistant blocks = %#v, want text block", blocks)
	}
}

func TestReadKimiFileReportsMalformedTail(t *testing.T) {
	path := writeKimiContext(t, filepath.Join(t.TempDir(), "session-abc", "context.jsonl"), []string{
		`{"role":"user","content":"plain text"}`,
		`{bad json`,
	})

	sess, err := ReadKimiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadKimiFile: %v", err)
	}
	if !sess.Diagnostics.MalformedTail {
		t.Fatal("MalformedTail = false, want true")
	}
}

func TestKimiSessionIDAndWorkDirHash(t *testing.T) {
	if got := kimiSessionID(filepath.Join("sessions", "hash", "session-123", "context.jsonl")); got != "session-123" {
		t.Fatalf("kimiSessionID(directory path) = %q, want session-123", got)
	}
	if got := kimiSessionID("session-abc.jsonl"); got != "session-abc" {
		t.Fatalf("kimiSessionID(file path) = %q, want session-abc", got)
	}
	if got := kimiWorkDirHash(""); got != "" {
		t.Fatalf("kimiWorkDirHash(empty) = %q, want empty", got)
	}
	if got := kimiWorkDirHash("/tmp/gascity/phase1/kimi"); got != "5decc6790b1207964f31266c8258989e" {
		t.Fatalf("kimiWorkDirHash() = %q, want Kimi CLI 1.42.0 lexical-path MD5", got)
	}
}

func TestFindKimiSessionFileByIDUsesSessionKey(t *testing.T) {
	base := t.TempDir()
	workDir := "/tmp/gascity/phase1/kimi"
	workHash := kimiWorkDirHash(workDir)
	oldPath := writeKimiContext(t, filepath.Join(base, "sessions", workHash, "old-session", "context.jsonl"), []string{
		`{"role":"user","content":"old"}`,
	})
	newPath := writeKimiContext(t, filepath.Join(base, "sessions", workHash, "new-session", "context.jsonl"), []string{
		`{"role":"user","content":"new"}`,
	})
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, past, past); err != nil {
		t.Fatal(err)
	}

	if got := FindKimiSessionFile([]string{base}, workDir); got != newPath {
		t.Fatalf("FindKimiSessionFile() = %q, want newest %q", got, newPath)
	}
	if got := FindKimiSessionFileByID([]string{base}, workDir, "old-session"); got != oldPath {
		t.Fatalf("FindKimiSessionFileByID() = %q, want keyed %q", got, oldPath)
	}
}

func TestFindKimiSessionFileFollowsSymlinkedRoots(t *testing.T) {
	base := t.TempDir()
	accountRoot := t.TempDir()
	workDir := "/tmp/gascity/phase1/kimi"
	workHash := kimiWorkDirHash(workDir)
	want := writeKimiContext(t, filepath.Join(accountRoot, workHash, "session-key", "context.jsonl"), []string{
		`{"role":"user","content":"via symlink"}`,
	})
	if err := os.Symlink(accountRoot, filepath.Join(base, "account-a")); err != nil {
		t.Fatal(err)
	}

	if got := FindKimiSessionFile([]string{base}, workDir); got != want {
		t.Fatalf("FindKimiSessionFile() = %q, want symlinked root transcript %q", got, want)
	}
	if got := FindKimiSessionFileByID([]string{base}, workDir, "session-key"); got != want {
		t.Fatalf("FindKimiSessionFileByID() = %q, want symlinked root transcript %q", got, want)
	}
}

func TestFindKimiSessionFileLogsMissingWorkDirHashDiagnostic(t *testing.T) {
	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	}()

	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "other-workdir-hash", "session-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	workDir := "/tmp/gascity/missing-kimi-workdir"
	workHash := kimiWorkDirHash(workDir)

	if got := FindKimiSessionFile([]string{base}, workDir); got != "" {
		t.Fatalf("FindKimiSessionFile() = %q, want empty", got)
	}
	logText := logs.String()
	if !strings.Contains(logText, "expected workdir hash "+`"`+workHash+`"`) {
		t.Fatalf("missing Kimi hash diagnostic %q in logs:\n%s", workHash, logText)
	}
	if !strings.Contains(logText, "check Kimi CLI version and workdir path hashing") {
		t.Fatalf("missing Kimi version/hash guidance in logs:\n%s", logText)
	}
}

func TestFindKimiSessionFileByIDRejectsTraversalSessionID(t *testing.T) {
	base := t.TempDir()
	workDir := "/tmp/gascity/phase1/kimi"
	if got := FindKimiSessionFileByID([]string{base}, workDir, "../escape"); got != "" {
		t.Fatalf("FindKimiSessionFileByID traversal = %q, want empty", got)
	}
}

func TestReadProviderFileNewerDispatchesKimi(t *testing.T) {
	path := writeKimiContext(t, filepath.Join(t.TempDir(), "sessions", "hash", "session-123", "context.jsonl"), []string{
		`{"role":"user","content":"hello"}`,
	})
	sess, err := ReadProviderFileNewer("kimi/tmux-cli", path, 0, "ignored")
	if err != nil {
		t.Fatalf("ReadProviderFileNewer: %v", err)
	}
	if sess.ID != "session-123" || len(sess.Messages) != 1 {
		t.Fatalf("ReadProviderFileNewer session = id %q messages %d, want Kimi reader output", sess.ID, len(sess.Messages))
	}
	rawSess, err := ReadProviderFileRawNewer("kimi/tmux-cli", path, 0, "ignored")
	if err != nil {
		t.Fatalf("ReadProviderFileRawNewer: %v", err)
	}
	if rawSess.ID != "session-123" || len(rawSess.Messages) != 1 {
		t.Fatalf("ReadProviderFileRawNewer session = id %q messages %d, want Kimi reader output", rawSess.ID, len(rawSess.Messages))
	}
}

func TestReadProviderFileKimiAppliesMessageIDCursors(t *testing.T) {
	path := writeKimiContext(t, filepath.Join(t.TempDir(), "sessions", "hash", "session-123", "context.jsonl"), []string{
		`{"role":"user","content":"first"}`,
		`{"role":"assistant","content":"second"}`,
		`{"role":"user","content":"third"}`,
		`{"role":"assistant","content":"fourth"}`,
	})

	newer, err := ReadProviderFileNewer("kimi/tmux-cli", path, 0, "kimi-1")
	if err != nil {
		t.Fatalf("ReadProviderFileNewer: %v", err)
	}
	if got := kimiEntryIDs(newer.Messages); !reflect.DeepEqual(got, []string{"kimi-2", "kimi-3"}) {
		t.Fatalf("newer Kimi message IDs = %v, want [kimi-2 kimi-3]", got)
	}

	older, err := ReadProviderFileOlder("kimi/tmux-cli", path, 0, "kimi-2")
	if err != nil {
		t.Fatalf("ReadProviderFileOlder: %v", err)
	}
	if got := kimiEntryIDs(older.Messages); !reflect.DeepEqual(got, []string{"kimi-0", "kimi-1"}) {
		t.Fatalf("older Kimi message IDs = %v, want [kimi-0 kimi-1]", got)
	}

	rawNewer, err := ReadProviderFileRawNewer("kimi/tmux-cli", path, 0, "kimi-2")
	if err != nil {
		t.Fatalf("ReadProviderFileRawNewer: %v", err)
	}
	if got := kimiEntryIDs(rawNewer.Messages); !reflect.DeepEqual(got, []string{"kimi-3"}) {
		t.Fatalf("raw newer Kimi message IDs = %v, want [kimi-3]", got)
	}
}

func writeKimiContext(t *testing.T, path string, lines []string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func kimiEntryIDs(entries []*Entry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.UUID)
	}
	return ids
}
