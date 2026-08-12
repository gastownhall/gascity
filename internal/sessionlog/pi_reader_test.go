package sessionlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
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

func TestReadPiFilePreservesImageBlockMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"session","version":3,"id":"ses_pi_images","timestamp":"2026-02-02T00:00:00.000Z","cwd":"/tmp/gascity/pi-images"}
{"type":"message","id":"msg_user_1","parentId":null,"timestamp":"2026-02-02T00:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"look here"},{"type":"image","file_path":"screens/shot.png","image_url":"https://example.com/shot.png","mime_type":"image/png"},{"type":"image","file_path":"screens/local.png","image_url":"data:image/png;base64,ignored","media_type":"image/png"}],"timestamp":1770000000000}}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pi fixture: %v", err)
	}

	sess, err := ReadPiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadPiFile: %v", err)
	}
	blocks := sess.Messages[0].ContentBlocks()
	if len(blocks) != 3 {
		t.Fatalf("blocks = %#v, want text plus two image blocks", blocks)
	}
	if blocks[1].Type != "image" || blocks[1].FilePath != "screens/shot.png" || blocks[1].ImageURL != "https://example.com/shot.png" || blocks[1].MIMEType != "image/png" {
		t.Fatalf("blocks[1] = %+v, want external image metadata", blocks[1])
	}
	if blocks[2].Type != "image" || blocks[2].FilePath != "screens/local.png" || blocks[2].MIMEType != "image/png" {
		t.Fatalf("blocks[2] = %+v, want local image metadata", blocks[2])
	}
	if blocks[2].ImageURL != "" {
		t.Fatalf("blocks[2].ImageURL = %q, want inline data URL omitted from structured block", blocks[2].ImageURL)
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
	var toolInput struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(toolUseBlocks[0].Input, &toolInput); err != nil {
		t.Fatalf("unmarshal tool input: %v", err)
	}
	if toolInput.FilePath != "README.md" {
		t.Fatalf("tool input file_path = %q, want README.md", toolInput.FilePath)
	}
	toolResultBlocks := sess.Messages[2].ContentBlocks()
	if len(toolResultBlocks) != 1 || toolResultBlocks[0].Type != "tool_result" || toolResultBlocks[0].ToolUseID != "call-1" {
		t.Fatalf("tool_result blocks = %#v", toolResultBlocks)
	}
	if len(sess.OrphanedToolUseIDs) != 0 {
		t.Fatalf("OrphanedToolUseIDs = %#v, want none", sess.OrphanedToolUseIDs)
	}
}

func TestReadPiFileNormalizesOMPExecutionMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"session","version":3,"id":"ses_omp","timestamp":"2026-02-02T00:00:00.000Z","cwd":"/tmp/gascity/omp"}
{"type":"message","id":"msg_bash","parentId":null,"timestamp":"2026-02-02T00:00:01.000Z","message":{"role":"bashExecution","command":"go test ./...","output":"ok ./internal/api","exitCode":0,"canceled":false,"truncated":true,"timestamp":1770000001000}}
{"type":"message","id":"msg_python","parentId":"msg_bash","timestamp":"2026-02-02T00:00:02.000Z","message":{"role":"pythonExecution","code":"print('hello')","output":"hello\n","exitCode":0,"canceled":false,"timestamp":1770000002000}}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pi fixture: %v", err)
	}

	sess, err := ReadProviderFile("omp", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile(omp): %v", err)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sess.Messages))
	}

	bashBlocks := sess.Messages[0].ContentBlocks()
	if len(bashBlocks) != 1 || bashBlocks[0].Type != "tool_result" || bashBlocks[0].Name != "bash" {
		t.Fatalf("bash blocks = %#v", bashBlocks)
	}
	assertRawMetadata(t, bashBlocks[0].Content, map[string]any{
		"command":   "go test ./...",
		"output":    "ok ./internal/api",
		"exit_code": float64(0),
		"truncated": true,
	})

	pythonBlocks := sess.Messages[1].ContentBlocks()
	if len(pythonBlocks) != 1 || pythonBlocks[0].Type != "tool_result" || pythonBlocks[0].Name != "python" {
		t.Fatalf("python blocks = %#v", pythonBlocks)
	}
	assertRawMetadata(t, pythonBlocks[0].Content, map[string]any{
		"code":      "print('hello')",
		"output":    "hello",
		"exit_code": float64(0),
	})
}

func TestReadPiFileNormalizesToolObjectsToNeutralKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"session","version":3,"id":"ses_tool","timestamp":"2026-02-02T00:00:00.000Z","cwd":"/tmp/gascity/phase2/pi"}
{"type":"message","id":"msg_assistant_1","parentId":null,"timestamp":"2026-02-02T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"Edit","arguments":{"filePath":"README.md","oldString":"old","newString":"new"}}],"timestamp":1770000001000}}
{"type":"message","id":"msg_tool_1","parentId":"msg_assistant_1","timestamp":"2026-02-02T00:00:02.000Z","message":{"role":"toolResult","toolCallId":"call-1","toolName":"Edit","content":{"output":"Edited README.md","filePath":"README.md","patch":"--- README.md\n+++ README.md\n@@\n-old\n+new","exitCode":0},"isError":false,"timestamp":1770000002000}}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pi fixture: %v", err)
	}

	sess, err := ReadPiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadPiFile: %v", err)
	}
	toolUseBlocks := sess.Messages[0].ContentBlocks()
	if len(toolUseBlocks) != 1 {
		t.Fatalf("tool use blocks = %d, want 1", len(toolUseBlocks))
	}
	var input struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(toolUseBlocks[0].Input, &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if input.FilePath != "README.md" || input.OldString != "old" || input.NewString != "new" {
		t.Fatalf("neutral input = %+v, want README.md old/new", input)
	}
	toolResultBlocks := sess.Messages[1].ContentBlocks()
	if len(toolResultBlocks) != 1 {
		t.Fatalf("tool result blocks = %d, want 1", len(toolResultBlocks))
	}
	var output struct {
		Output   string `json:"output"`
		FilePath string `json:"file_path"`
		Patch    string `json:"patch"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(toolResultBlocks[0].Content, &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if output.Output != "Edited README.md" || output.FilePath != "README.md" || !strings.Contains(output.Patch, "+new") || output.ExitCode != 0 {
		t.Fatalf("neutral output = %+v, want patch result", output)
	}
	for _, forbidden := range []string{"filePath", "oldString", "newString", "exitCode"} {
		if strings.Contains(string(toolUseBlocks[0].Input), forbidden) || strings.Contains(string(toolResultBlocks[0].Content), forbidden) {
			t.Fatalf("Pi normalized blocks leaked %s: input=%s content=%s", forbidden, toolUseBlocks[0].Input, toolResultBlocks[0].Content)
		}
	}
}

func TestReadPiFileReportsBranchesAndUsesAllEntriesForToolResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"session","version":3,"id":"ses_branch","timestamp":"2026-02-02T00:00:00.000Z","cwd":"/tmp/gascity/phase2/pi"}
{"type":"message","id":"msg_user_1","parentId":null,"timestamp":"2026-02-02T00:00:00.000Z","message":{"role":"user","content":"run tool","timestamp":1770000000000}}
{"type":"message","id":"msg_tool_result_off_branch","parentId":"msg_user_1","timestamp":"2026-02-02T00:00:02.000Z","message":{"role":"toolResult","toolCallId":"call-1","toolName":"read","content":"file data","isError":false,"timestamp":1770000002000}}
{"type":"message","id":"msg_assistant_active","parentId":"msg_user_1","timestamp":"2026-02-02T00:00:03.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"read","arguments":{"path":"README.md"}}],"timestamp":1770000003000}}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pi fixture: %v", err)
	}

	sess, err := ReadPiFile(path, 0)
	if err != nil {
		t.Fatalf("ReadPiFile: %v", err)
	}
	if !sess.HasBranches {
		t.Fatal("HasBranches = false, want true")
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want active user+assistant branch", len(sess.Messages))
	}
	if got := sess.Messages[1].UUID; got != "msg_assistant_active" {
		t.Fatalf("active tip = %q, want msg_assistant_active", got)
	}
	if len(sess.OrphanedToolUseIDs) != 0 {
		t.Fatalf("OrphanedToolUseIDs = %#v, want off-branch result to satisfy active tool use", sess.OrphanedToolUseIDs)
	}
}

func TestTruncatePiInterruptedTurnUsesTypedBoundaries(t *testing.T) {
	stablePrefix := `{"type":"session","version":3,"id":"ses_pi","cwd":"/tmp/project"}
{"type":"message","id":"u1","message":{"role":"user","content":"stable prompt"}}
{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":"stable response","stopReason":"stop"}}
`
	cases := []struct {
		name      string
		suffix    string
		want      string
		wantWrite bool
	}{
		{
			name: "completed assistant stays intact",
			suffix: `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"completed prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":"completed response","stopReason":"stop"}}
`,
			want: stablePrefix + `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"completed prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":"completed response","stopReason":"stop"}}
`,
		},
		{
			name: "pending user is removed before replacement prompt",
			suffix: `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"interrupted prompt"}}
`,
			want:      stablePrefix,
			wantWrite: true,
		},
		{
			name: "assistant without stop reason is removed before replacement prompt",
			suffix: `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"interrupted prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":"partial response"}}
`,
			want:      stablePrefix,
			wantWrite: true,
		},
		{
			name: "assistant tool call tail is removed before replacement prompt",
			suffix: `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"interrupted prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"read","arguments":{"path":"README.md"}}],"stopReason":"tool_use"}}
`,
			want:      stablePrefix,
			wantWrite: true,
		},
		{
			name: "matched tool result without final assistant is removed before replacement prompt",
			suffix: `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"interrupted prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"read","arguments":{"path":"README.md"}}],"stopReason":"tool_use"}}
{"type":"message","id":"t2","parentId":"a2","message":{"role":"toolResult","toolCallId":"call-1","toolName":"read","content":[{"type":"text","text":"file data"}]}}
`,
			want:      stablePrefix,
			wantWrite: true,
		},
		{
			name: "tool calls without stable IDs remain incomplete",
			suffix: `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"interrupted prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":[{"type":"toolCall","name":"read","arguments":{"path":"README.md"}}],"stopReason":"tool_use"}}
{"type":"message","id":"t2","parentId":"a2","message":{"role":"toolResult","toolName":"read","content":[{"type":"text","text":"file data"}]}}
{"type":"message","id":"a3","parentId":"t2","message":{"role":"assistant","content":"done","stopReason":"stop"}}
`,
			want:      stablePrefix,
			wantWrite: true,
		},
		{
			name: "matched tool result with final assistant completes turn",
			suffix: `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"completed prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"read","arguments":{"path":"README.md"}}],"stopReason":"tool_use"}}
{"type":"message","id":"t2","parentId":"a2","message":{"role":"toolResult","toolCallId":"call-1","toolName":"read","content":[{"type":"text","text":"file data"}]}}
{"type":"message","id":"a3","parentId":"t2","message":{"role":"assistant","content":"done","stopReason":"stop"}}
`,
			want: stablePrefix + `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"completed prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"read","arguments":{"path":"README.md"}}],"stopReason":"tool_use"}}
{"type":"message","id":"t2","parentId":"a2","message":{"role":"toolResult","toolCallId":"call-1","toolName":"read","content":[{"type":"text","text":"file data"}]}}
{"type":"message","id":"a3","parentId":"t2","message":{"role":"assistant","content":"done","stopReason":"stop"}}
`,
		},
		{
			name: "malformed tail after user is removed with its user",
			suffix: `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"interrupted prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":"unterminated"}
`,
			want:      stablePrefix,
			wantWrite: true,
		},
		{
			name: "malformed tail after completed assistant does not remove completed turn",
			suffix: `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"completed prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":"completed response","stopReason":"stop"}}
{"type":"custom_message","id":"tail","content":"unterminated"
`,
			want: stablePrefix + `{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"completed prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":"completed response","stopReason":"stop"}}
`,
			wantWrite: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := truncatePiSessionAfterLastUserMessage([]byte(stablePrefix + tc.suffix))
			if changed != tc.wantWrite {
				t.Fatalf("changed = %v, want %v", changed, tc.wantWrite)
			}
			if string(got) != tc.want {
				t.Fatalf("truncated transcript:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestResetPiInterruptedTurnIgnoresMirrorWriteFailureAfterNativeReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"session","version":3,"id":"ses_pi","cwd":"/tmp/project"}
{"type":"message","id":"u1","message":{"role":"user","content":"stable prompt"}}
{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":"stable response","stopReason":"stop"}}
{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"interrupted prompt"}}
{"type":"message","id":"a2","parentId":"u2","message":{"role":"assistant","content":"partial response"}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write pi fixture: %v", err)
	}
	mirrorDir := filepath.Join(t.TempDir(), "mirror-file")
	if err := os.WriteFile(mirrorDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write mirror blocker: %v", err)
	}

	var logs bytes.Buffer
	oldLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldLogOutput)

	if err := ResetPiInterruptedTurn(path, mirrorDir); err != nil {
		t.Fatalf("ResetPiInterruptedTurn: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(got), "interrupted prompt") || strings.Contains(string(got), "partial response") {
		t.Fatalf("native transcript was not reset despite mirror failure:\n%s", got)
	}
	if !strings.Contains(logs.String(), "pi mirror reset") || !strings.Contains(logs.String(), mirrorDir) {
		t.Fatalf("mirror failure log = %q, want path-bearing pi mirror reset diagnostic", logs.String())
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
		path    string
		id      string
		workDir string
	}{
		{oldPath, "old", filepath.Join(t.TempDir(), "other-project")},
		{newPath, "new", workDir},
	} {
		body := `{"type":"session","version":3,"id":"` + item.id + `","timestamp":"2026-02-02T00:00:00.000Z","cwd":"` + filepath.ToSlash(item.workDir) + `"}`
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

func TestFindPiSessionFileFailsClosedOnAmbiguousCWD(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	firstPath := filepath.Join(root, "first.jsonl")
	secondPath := filepath.Join(root, "second.jsonl")
	for _, item := range []struct {
		path string
		id   string
	}{
		{firstPath, "first"},
		{secondPath, "second"},
	} {
		body := `{"type":"session","version":3,"id":"` + item.id + `","cwd":"` + filepath.ToSlash(workDir) + `"}`
		if err := os.WriteFile(item.path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", item.path, err)
		}
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(secondPath, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if got := FindPiSessionFile([]string{root}, workDir); got != "" {
		t.Fatalf("FindPiSessionFile() = %q, want fail-closed empty path for ambiguous cwd", got)
	}
	if _, err := FindPiSessionFileStrict([]string{root}, workDir); !errors.Is(err, ErrAmbiguousPiSessionFile) {
		t.Fatalf("FindPiSessionFileStrict() error = %v, want ErrAmbiguousPiSessionFile", err)
	}
	if got := FindPiSessionFileByID([]string{root}, workDir, "first"); got != firstPath {
		t.Fatalf("FindPiSessionFileByID(first) = %q, want %q", got, firstPath)
	}
}
