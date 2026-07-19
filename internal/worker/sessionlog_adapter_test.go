package worker

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionLogAdapterTailMetaForProviderUsesCodexSchema(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "2026", "04", "16", "rollout-codex-meta.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir codex transcript dir: %v", err)
	}
	writeLines(t, path,
		`{"timestamp":"2026-04-16T21:49:30.901Z","type":"turn_context","payload":{"model":"gpt-5.5"}}`,
		`{"timestamp":"2026-04-16T21:49:45.100Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":15562,"cached_input_tokens":10624},"model_context_window":258400}}}`,
	)

	meta, err := (SessionLogAdapter{SearchPaths: []string{root}}).TailMetaForProvider("codex/tmux-cli", path)
	if err != nil {
		t.Fatalf("TailMetaForProvider(codex): %v", err)
	}
	if meta == nil || meta.ContextUsage == nil {
		t.Fatalf("TailMetaForProvider(codex) = %#v, want model and context usage", meta)
	}
	if got, want := meta.Model, "gpt-5.5"; got != want {
		t.Errorf("Model = %q, want %q", got, want)
	}
	if got, want := meta.ContextUsage.InputTokens, 15_562; got != want {
		t.Errorf("InputTokens = %d, want %d", got, want)
	}
}

func TestSessionLogAdapterLoadHistoryClaude(t *testing.T) {
	t.Parallel()

	workDir := "/tmp/project"
	base := t.TempDir()
	slug := strings.NewReplacer("/", "-", ".", "-").Replace(workDir)
	transcriptDir := filepath.Join(base, slug)
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}

	path := filepath.Join(transcriptDir, "sess-claude.jsonl")
	lines := []string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`,
		`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"},{"type":"tool_use","id":"tool-1","name":"Read","input":{"path":"README.md"}}],"model":"claude-sonnet","stop_reason":"tool_use","usage":{"input_tokens":1000}},"timestamp":"2025-01-01T00:00:01Z","sessionId":"provider-claude"}`,
		`{"uuid":"c1","type":"system","subtype":"compact_boundary","logicalParentUuid":"a1","timestamp":"2025-01-01T00:00:02Z","sessionId":"provider-claude"}`,
		`{"uuid":"r1","parentUuid":"c1","type":"result","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"file contents"}],"is_error":false},"timestamp":"2025-01-01T00:00:03Z","sessionId":"provider-claude"}`,
		`{"uuid":"a2","parentUuid":"r1","type":"assistant","message":{"role":"assistant","content":"done","model":"claude-sonnet","stop_reason":"end_turn","usage":{"input_tokens":1200}},"timestamp":"2025-01-01T00:00:04Z","sessionId":"provider-claude"}`,
	}
	writeLines(t, path, lines...)

	adapter := SessionLogAdapter{SearchPaths: []string{base}}
	discovered := adapter.DiscoverTranscript("claude/tmux-cli", workDir, "sess-claude")
	if discovered != path {
		t.Fatalf("DiscoverTranscript() = %q, want %q", discovered, path)
	}

	snapshot, err := adapter.LoadHistory(LoadRequest{
		Provider:        "claude/tmux-cli",
		TranscriptPath:  path,
		GCSessionID:     "gc-1",
		TailCompactions: 0,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if snapshot.LogicalConversationID != "gc-1" {
		t.Fatalf("LogicalConversationID = %q, want gc-1", snapshot.LogicalConversationID)
	}
	if snapshot.Continuity.Status != ContinuityStatusCompacted {
		t.Fatalf("Continuity.Status = %q, want %q", snapshot.Continuity.Status, ContinuityStatusCompacted)
	}
	if snapshot.TailState.Activity != TailActivityIdle {
		t.Fatalf("TailState.Activity = %q, want %q", snapshot.TailState.Activity, TailActivityIdle)
	}
	if got := len(snapshot.Entries); got != 5 {
		t.Fatalf("len(Entries) = %d, want 5", got)
	}
	if snapshot.Entries[1].Blocks[1].Kind != BlockKindToolUse {
		t.Fatalf("assistant tool block kind = %q, want %q", snapshot.Entries[1].Blocks[1].Kind, BlockKindToolUse)
	}
	if snapshot.Entries[1].Model != "claude-sonnet" || snapshot.Entries[1].StopReason != "tool_use" {
		t.Fatalf("assistant metadata = model %q stop %q, want claude-sonnet/tool_use", snapshot.Entries[1].Model, snapshot.Entries[1].StopReason)
	}
	if snapshot.Entries[1].Usage == nil || snapshot.Entries[1].Usage.InputTokens != 1000 {
		t.Fatalf("assistant usage = %+v, want input_tokens 1000", snapshot.Entries[1].Usage)
	}
	if snapshot.Entries[3].Blocks[0].Kind != BlockKindToolResult {
		t.Fatalf("result block kind = %q, want %q", snapshot.Entries[3].Blocks[0].Kind, BlockKindToolResult)
	}
	if snapshot.Entries[4].Model != "claude-sonnet" || snapshot.Entries[4].StopReason != "end_turn" {
		t.Fatalf("final metadata = model %q stop %q, want claude-sonnet/end_turn", snapshot.Entries[4].Model, snapshot.Entries[4].StopReason)
	}
	if snapshot.Entries[4].Usage == nil || snapshot.Entries[4].Usage.InputTokens != 1200 {
		t.Fatalf("final usage = %+v, want input_tokens 1200", snapshot.Entries[4].Usage)
	}
	if snapshot.Cursor.AfterEntryID != "a2" {
		t.Fatalf("Cursor.AfterEntryID = %q, want a2", snapshot.Cursor.AfterEntryID)
	}
}

func TestSessionLogAdapterLoadHistoryCarriesImageBlockMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sess-image.jsonl")
	writeLines(t, path,
		`{"uuid":"u1","type":"user","message":{"role":"user","content":[{"type":"text","text":"inspect this"},{"type":"image","file_path":"screens/shot.png","image_url":"https://example.com/shot.png","mime_type":"image/png"}]},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`,
	)

	adapter := SessionLogAdapter{}
	snapshot, err := adapter.LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
		GCSessionID:    "gc-image",
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if len(snapshot.Entries) != 1 || len(snapshot.Entries[0].Blocks) != 2 {
		t.Fatalf("snapshot entries = %+v, want one text and one image block", snapshot.Entries)
	}
	image := snapshot.Entries[0].Blocks[1]
	if image.Kind != BlockKindImage || image.FilePath != "screens/shot.png" || image.ImageURL != "https://example.com/shot.png" || image.MIMEType != "image/png" {
		t.Fatalf("image block = %+v, want provider-neutral image metadata", image)
	}
}

func TestSessionLogAdapterLoadHistoryCarriesUserPromptMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sess-prompt-metadata.jsonl")
	prompt := strings.Join([]string{
		"Please inspect this.",
		"<ide_opened_file>The user opened the file /tmp/project/src/app.ts in the IDE</ide_opened_file>",
		"<ide_selection>const answer = 42;</ide_selection>",
		"",
		"User uploaded files:",
		"- diagram.png (12 KB, image/png): /tmp/uploads/diagram.png",
	}, "\n")
	writeLines(t, path,
		fmt.Sprintf(`{"uuid":"u1","type":"user","message":{"role":"user","content":%q},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`, prompt),
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if len(snapshot.Entries) != 1 {
		t.Fatalf("entries = %+v, want one user entry", snapshot.Entries)
	}
	got := snapshot.Entries[0].UserPrompt
	if got == nil {
		t.Fatal("UserPrompt = nil, want typed prompt metadata")
	}
	if got.Text != "Please inspect this." {
		t.Fatalf("prompt text = %q, want cleaned prompt", got.Text)
	}
	if len(got.OpenedFiles) != 1 || got.OpenedFiles[0] != "/tmp/project/src/app.ts" {
		t.Fatalf("opened files = %#v, want IDE file path", got.OpenedFiles)
	}
	if len(got.Selections) != 1 || got.Selections[0].Text != "const answer = 42;" {
		t.Fatalf("selections = %#v, want selected text", got.Selections)
	}
	if len(got.UploadedFiles) != 1 {
		t.Fatalf("uploaded files = %#v, want one upload", got.UploadedFiles)
	}
	upload := got.UploadedFiles[0]
	if upload.OriginalName != "diagram.png" || upload.Size != "12 KB" || upload.MIMEType != "image/png" || upload.FilePath != "/tmp/uploads/diagram.png" {
		t.Fatalf("upload = %+v, want parsed upload metadata", upload)
	}
}

func TestSessionLogAdapterLoadHistoryCarriesStructuredClaudeEditResult(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sess-claude-edit.jsonl")
	writeLines(t, path,
		`{"uuid":"a1","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"edit-1","name":"Edit","input":{"file_path":"README.md","old_string":"old","new_string":"new"}}]},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`,
		`{"uuid":"r1","parentUuid":"a1","type":"tool_result","toolUseID":"edit-1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"edit-1","content":"updated"}]},"toolUseResult":{"filePath":"README.md","structuredPatch":[{"oldStart":3,"oldLines":1,"newStart":3,"newLines":1,"lines":["-old","+new"]}]},"timestamp":"2025-01-01T00:00:01Z","sessionId":"provider-claude"}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	result := snapshot.Entries[1].Blocks[0].StructuredResult
	if result == nil {
		t.Fatal("StructuredResult = nil, want typed edit result")
	}
	if result.Kind != "edit" || result.FilePath != "README.md" {
		t.Fatalf("StructuredResult = %+v, want edit result for README.md", result)
	}
	if len(result.PatchHunks) != 1 {
		t.Fatalf("PatchHunks = %+v, want one hunk", result.PatchHunks)
	}
	hunk := result.PatchHunks[0]
	if hunk.OldStart != 3 || hunk.NewStart != 3 || hunk.Lines[0] != "-old" || hunk.Lines[1] != "+new" {
		t.Fatalf("PatchHunks[0] = %+v, want typed lines at line 3", hunk)
	}
	content := string(snapshot.Entries[1].Blocks[0].Content)
	if strings.Contains(content, "toolUseResult") || strings.Contains(content, "structuredPatch") || strings.Contains(content, "filePath") {
		t.Fatalf("worker tool result content leaked Claude-native keys: %s", content)
	}
	if !strings.Contains(content, "patch_hunks") || !strings.Contains(content, "file_path") {
		t.Fatalf("worker tool result content = %s, want neutral patch_hunks/file_path", content)
	}
}

func TestSessionLogAdapterLoadHistoryCarriesStructuredClaudeReadSidecarResult(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sess-claude-read.jsonl")
	writeLines(t, path,
		`{"uuid":"a1","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"read-1","name":"Read","input":{"file_path":"src/app.ts"}}]},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`,
		`{"uuid":"r1","parentUuid":"a1","type":"tool_result","toolUseID":"read-1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"read-1","content":"read complete"}]},"toolUseResult":{"type":"text","file":{"filePath":"src/app.ts","content":"line 12\nline 13\n","numLines":2,"startLine":12,"totalLines":24,"language":"typescript"}},"timestamp":"2025-01-01T00:00:01Z","sessionId":"provider-claude"}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	result := snapshot.Entries[1].Blocks[0].StructuredResult
	if result == nil {
		t.Fatal("StructuredResult = nil, want typed read result")
	}
	if result.Kind != "read" || result.FilePath != "src/app.ts" || result.Content != "line 12\nline 13\n" || result.Language != "typescript" {
		t.Fatalf("StructuredResult = %+v, want typed read result with content/language", result)
	}
	if result.NumLines != 2 || result.StartLine != 12 || result.TotalLines != 24 {
		t.Fatalf("read line metadata = %+v, want num/start/total 2/12/24", result)
	}
	content := string(snapshot.Entries[1].Blocks[0].Content)
	for _, forbidden := range []string{"toolUseResult", "filePath", "numLines", "startLine", "totalLines"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("worker tool result content leaked Claude-native key %q: %s", forbidden, content)
		}
	}
}

func TestSessionLogAdapterLoadHistoryCarriesStructuredCodexShellResults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-01-02T00:00:01Z","type":"response_item","payload":{"type":"function_call","call_id":"read-1","name":"exec_command","arguments":"{\"cmd\":\"nl -ba src/app.ts | sed -n '12,14p'\"}"}}`,
		`{"timestamp":"2026-01-02T00:00:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"read-1","output":"Command: nl -ba src/app.ts | sed -n '12,14p'\nOutput:\n    12\tline 12\n    13\tline 13\n    14\tline 14\n"}}`,
		`{"timestamp":"2026-01-02T00:00:03Z","type":"response_item","payload":{"type":"function_call","call_id":"grep-1","name":"exec_command","arguments":"{\"cmd\":\"rg -n \\\"needle\\\" README.md src/app.ts\"}"}}`,
		`{"timestamp":"2026-01-02T00:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"grep-1","output":"Command: rg -n \"needle\" README.md src/app.ts\nOutput:\nREADME.md:1:needle\nsrc/app.ts:7:needle\n"}}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "codex/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	readResult := snapshot.Entries[1].Blocks[0].StructuredResult
	if readResult == nil {
		t.Fatal("read StructuredResult = nil")
	}
	if readResult.Kind != "read" || readResult.Content != "line 12\nline 13\nline 14\n" {
		t.Fatalf("read StructuredResult = %+v, want line-number-stripped read", readResult)
	}
	if readResult.StartLine != 12 || readResult.TotalLines != 14 || readResult.NumLines != 3 {
		t.Fatalf("read range = start %d total %d lines %d, want 12/14/3", readResult.StartLine, readResult.TotalLines, readResult.NumLines)
	}

	grepResult := snapshot.Entries[3].Blocks[0].StructuredResult
	if grepResult == nil {
		t.Fatal("grep StructuredResult = nil")
	}
	if grepResult.Kind != "grep" || grepResult.Mode != "content" {
		t.Fatalf("grep StructuredResult = %+v, want content grep", grepResult)
	}
	for _, want := range []string{"README.md", "src/app.ts"} {
		if !stringSliceContains(grepResult.Filenames, want) {
			t.Fatalf("grep filenames = %+v, missing %s", grepResult.Filenames, want)
		}
	}
}

func TestSessionLogAdapterLoadHistoryCarriesStructuredCopilotToolResults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeLines(t, path,
		`{"type":"session.start","data":{"sessionId":"copilot-session","producer":"copilot-agent","selectedModel":"claude-sonnet-4.5","context":{"cwd":"/work/project"}},"id":"start-1","timestamp":"2026-03-04T02:30:58.550Z"}`,
		`{"type":"assistant.message","data":{"content":"I will update the app.","model":"claude-sonnet-4.5","toolRequests":[{"toolCallId":"toolu-bash","name":"bash","arguments":"{\"command\":\"printf hello\"}"},{"toolCallId":"toolu-edit","name":"edit_file","arguments":{"path":"src/app.ts","oldString":"old","newString":"new"}}]},"id":"assistant-1","timestamp":"2026-03-04T02:31:01Z","parentId":"start-1"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"toolu-bash","model":"claude-sonnet-4.5","success":true,"result":{"stdout":"hello\n","stderr":"","exitCode":0}},"id":"complete-bash","timestamp":"2026-03-04T02:31:03Z","parentId":"assistant-1"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"toolu-edit","model":"claude-sonnet-4.5","success":true,"result":{"content":"Edited src/app.ts","filePath":"src/app.ts","patch":"*** Begin Patch\n*** Update File: src/app.ts\n@@\n-old\n+new\n*** End Patch","oldString":"old","newString":"new","originalFile":"old\n","replaceAll":false,"userModified":false}},"id":"complete-edit","timestamp":"2026-03-04T02:31:05Z","parentId":"complete-bash"}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "copilot/tmux-cli",
		TranscriptPath: path,
		GCSessionID:    "gc-copilot",
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if snapshot.ProviderSessionID != "copilot-session" || snapshot.LogicalConversationID != "gc-copilot" {
		t.Fatalf("snapshot ids = provider %q logical %q, want copilot-session/gc-copilot", snapshot.ProviderSessionID, snapshot.LogicalConversationID)
	}
	if got := len(snapshot.Entries); got != 3 {
		t.Fatalf("len(Entries) = %d, want assistant plus two results", got)
	}

	assistant := snapshot.Entries[0]
	if assistant.Model != "claude-sonnet-4.5" {
		t.Fatalf("assistant model = %q, want claude-sonnet-4.5", assistant.Model)
	}
	if len(assistant.Blocks) != 3 {
		t.Fatalf("assistant blocks = %+v, want text plus two tool uses", assistant.Blocks)
	}
	bashInput := assistant.Blocks[1].StructuredInput
	if bashInput == nil || bashInput.Kind != "command" || bashInput.Command != "printf hello" {
		t.Fatalf("bash StructuredInput = %+v, want command input", bashInput)
	}
	editInput := assistant.Blocks[2].StructuredInput
	if editInput == nil || editInput.Kind != "patch" || editInput.FilePath != "src/app.ts" || !strings.Contains(editInput.Patch, "-old\n+new") {
		t.Fatalf("edit StructuredInput = %+v, want neutral patch input", editInput)
	}

	bashResult := snapshot.Entries[1].Blocks[0].StructuredResult
	if bashResult == nil {
		t.Fatal("bash StructuredResult = nil")
	}
	if bashResult.Kind != "bash" || bashResult.Stdout != "hello\n" || bashResult.ExitCode == nil || *bashResult.ExitCode != 0 {
		t.Fatalf("bash StructuredResult = %+v, want stdout and exit code", bashResult)
	}

	editResult := snapshot.Entries[2].Blocks[0].StructuredResult
	if editResult == nil {
		t.Fatal("edit StructuredResult = nil")
	}
	if editResult.Kind != "edit" || editResult.FilePath != "src/app.ts" || editResult.OldString != "old" || editResult.NewString != "new" || editResult.OriginalFile != "old\n" {
		t.Fatalf("edit StructuredResult = %+v, want edit metadata", editResult)
	}
	if len(editResult.PatchHunks) != 1 {
		t.Fatalf("edit PatchHunks = %+v, want one hunk", editResult.PatchHunks)
	}
	hunk := editResult.PatchHunks[0]
	if hunk.FilePath != "src/app.ts" || hunk.Lines[0] != "-old" || hunk.Lines[1] != "+new" {
		t.Fatalf("edit hunk = %+v, want src/app.ts old/new lines", hunk)
	}
	content := string(snapshot.Entries[2].Blocks[0].Content)
	for _, forbidden := range []string{"toolCallId", "filePath", "oldString", "newString", "replaceAll", "userModified"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("Copilot result leaked native key %q: %s", forbidden, content)
		}
	}
}

func TestSessionLogAdapterLoadHistoryCarriesStructuredKiroToolResults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeLines(t, path,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"kiro-session","update":{"sessionUpdate":"tool_call","toolCallId":"toolu-bash","title":"bash","kind":"execute","status":"pending","rawInput":{"command":"printf hello"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"kiro-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu-bash","status":"completed","rawOutput":{"stdout":"hello\n","stderr":"","exitCode":0}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"kiro-session","update":{"sessionUpdate":"tool_call","toolCallId":"toolu-edit","title":"write","kind":"edit","status":"pending","rawInput":{"path":"src/app.ts","content":"new file\n"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"kiro-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu-edit","status":"completed","content":[{"type":"diff","path":"src/app.ts","oldText":"old line\n","newText":"new line\n"}]}}}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "kiro/tmux-cli",
		TranscriptPath: path,
		GCSessionID:    "gc-kiro",
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if snapshot.ProviderSessionID != "kiro-session" || snapshot.LogicalConversationID != "gc-kiro" {
		t.Fatalf("snapshot ids = provider %q logical %q, want kiro-session/gc-kiro", snapshot.ProviderSessionID, snapshot.LogicalConversationID)
	}
	if got := len(snapshot.Entries); got != 4 {
		t.Fatalf("len(Entries) = %d, want two tool uses plus two results", got)
	}

	bashInput := snapshot.Entries[0].Blocks[0].StructuredInput
	if bashInput == nil || bashInput.Kind != "command" || bashInput.Command != "printf hello" {
		t.Fatalf("bash StructuredInput = %+v, want command input", bashInput)
	}
	bashResult := snapshot.Entries[1].Blocks[0].StructuredResult
	if bashResult == nil {
		t.Fatal("bash StructuredResult = nil")
	}
	if bashResult.Kind != "bash" || bashResult.Stdout != "hello\n" || bashResult.ExitCode == nil || *bashResult.ExitCode != 0 {
		t.Fatalf("bash StructuredResult = %+v, want stdout and exit code", bashResult)
	}

	editInput := snapshot.Entries[2].Blocks[0].StructuredInput
	if editInput == nil || editInput.Kind != "write" || editInput.FilePath != "src/app.ts" || editInput.Text != "new file\n" {
		t.Fatalf("edit StructuredInput = %+v, want write input", editInput)
	}
	editResult := snapshot.Entries[3].Blocks[0].StructuredResult
	if editResult == nil {
		t.Fatal("edit StructuredResult = nil")
	}
	if editResult.Kind != "write" || editResult.FilePath != "src/app.ts" || !strings.Contains(editResult.Patch, "-old line\n+new line") {
		t.Fatalf("edit StructuredResult = %+v, want write result with result-side patch", editResult)
	}
	if len(editResult.PatchHunks) != 1 || editResult.PatchHunks[0].FilePath != "src/app.ts" {
		t.Fatalf("edit PatchHunks = %+v, want one typed src/app.ts hunk", editResult.PatchHunks)
	}
	content := string(snapshot.Entries[3].Blocks[0].Content)
	for _, forbidden := range []string{"toolCallId", "rawOutput", "oldText", "newText"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("Kiro result leaked native key %q: %s", forbidden, content)
		}
	}
}

func TestSessionLogAdapterLoadHistoryCarriesStructuredAmpToolResults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "stream.jsonl")
	writeLines(t, path,
		`{"type":"system","subtype":"init","cwd":"/work/project","session_id":"T-amp-session","tools":["Bash","edit_file"],"mcp_servers":[]}`,
		`{"type":"assistant","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu-bash","name":"Bash","input":{"command":"npm test"}},{"type":"tool_use","id":"toolu-edit","name":"edit_file","input":{"filePath":"src/app.ts","oldString":"old","newString":"new"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5,"max_tokens":968000}},"parent_tool_use_id":null,"session_id":"T-amp-session"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu-bash","content":"{\"stdout\":\"ok\\n\",\"stderr\":\"\",\"exitCode\":0}","is_error":false}]},"parent_tool_use_id":null,"session_id":"T-amp-session"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu-edit","content":"{\"filePath\":\"src/app.ts\",\"patch\":\"*** Begin Patch\\n*** Update File: src/app.ts\\n@@\\n-old\\n+new\\n*** End Patch\",\"oldString\":\"old\",\"newString\":\"new\"}","is_error":false}]},"parent_tool_use_id":null,"session_id":"T-amp-session"}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "amp/tmux-cli",
		TranscriptPath: path,
		GCSessionID:    "gc-amp",
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if snapshot.ProviderSessionID != "T-amp-session" || snapshot.LogicalConversationID != "gc-amp" {
		t.Fatalf("snapshot ids = provider %q logical %q, want T-amp-session/gc-amp", snapshot.ProviderSessionID, snapshot.LogicalConversationID)
	}
	if got := len(snapshot.Entries); got != 3 {
		t.Fatalf("len(Entries) = %d, want assistant plus two results", got)
	}

	assistant := snapshot.Entries[0]
	if assistant.Usage == nil || assistant.Usage.InputTokens != 10 || assistant.Usage.OutputTokens != 5 {
		t.Fatalf("assistant usage = %+v, want Amp usage", assistant.Usage)
	}
	bashInput := assistant.Blocks[0].StructuredInput
	if bashInput == nil || bashInput.Kind != "command" || bashInput.Command != "npm test" {
		t.Fatalf("bash StructuredInput = %+v, want command input", bashInput)
	}
	editInput := assistant.Blocks[1].StructuredInput
	if editInput == nil || editInput.Kind != "patch" || editInput.FilePath != "src/app.ts" || !strings.Contains(editInput.Patch, "-old\n+new") {
		t.Fatalf("edit StructuredInput = %+v, want neutral patch input", editInput)
	}

	bashResult := snapshot.Entries[1].Blocks[0].StructuredResult
	if bashResult == nil {
		t.Fatal("bash StructuredResult = nil")
	}
	if bashResult.Kind != "bash" || bashResult.Stdout != "ok\n" || bashResult.ExitCode == nil || *bashResult.ExitCode != 0 {
		t.Fatalf("bash StructuredResult = %+v, want stdout and exit code", bashResult)
	}
	editResult := snapshot.Entries[2].Blocks[0].StructuredResult
	if editResult == nil {
		t.Fatal("edit StructuredResult = nil")
	}
	if editResult.Kind != "edit" || editResult.FilePath != "src/app.ts" || editResult.OldString != "old" || editResult.NewString != "new" {
		t.Fatalf("edit StructuredResult = %+v, want edit metadata", editResult)
	}
	if len(editResult.PatchHunks) != 1 || editResult.PatchHunks[0].FilePath != "src/app.ts" {
		t.Fatalf("edit PatchHunks = %+v, want one typed src/app.ts hunk", editResult.PatchHunks)
	}
	content := string(snapshot.Entries[2].Blocks[0].Content)
	for _, forbidden := range []string{"tool_use_id", "filePath", "oldString", "newString", "exitCode"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("Amp result leaked native key %q: %s", forbidden, content)
		}
	}
}

func TestSessionLogAdapterLoadHistoryCarriesStructuredGrokACPToolResults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeLines(t, path,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"tool_call","toolCallId":"toolu-bash","title":"run_terminal_cmd","kind":"execute","status":"pending","rawInput":{"command":"printf hello"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu-bash","status":"completed","rawOutput":{"stdout":"hello\n","stderr":"","exitCode":0}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"tool_call","toolCallId":"toolu-edit","title":"search_replace","kind":"edit","status":"pending","rawInput":{"path":"src/app.ts","oldText":"old","newText":"new"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"grok-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu-edit","status":"completed","content":[{"type":"diff","path":"src/app.ts","oldText":"old\n","newText":"new\n"}]}}}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "grok/tmux-cli",
		TranscriptPath: path,
		GCSessionID:    "gc-grok",
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if snapshot.ProviderSessionID != "grok-session" || snapshot.LogicalConversationID != "gc-grok" {
		t.Fatalf("snapshot ids = provider %q logical %q, want grok-session/gc-grok", snapshot.ProviderSessionID, snapshot.LogicalConversationID)
	}
	if got := len(snapshot.Entries); got != 4 {
		t.Fatalf("len(Entries) = %d, want two tool uses plus two results", got)
	}

	bashInput := snapshot.Entries[0].Blocks[0].StructuredInput
	if bashInput == nil || bashInput.Kind != "command" || bashInput.Command != "printf hello" {
		t.Fatalf("bash StructuredInput = %+v, want command input", bashInput)
	}
	bashResult := snapshot.Entries[1].Blocks[0].StructuredResult
	if bashResult == nil {
		t.Fatal("bash StructuredResult = nil")
	}
	if bashResult.Kind != "bash" || bashResult.Stdout != "hello\n" || bashResult.ExitCode == nil || *bashResult.ExitCode != 0 {
		t.Fatalf("bash StructuredResult = %+v, want stdout and exit code", bashResult)
	}

	editInput := snapshot.Entries[2].Blocks[0].StructuredInput
	if editInput == nil || editInput.Kind != "patch" || editInput.FilePath != "src/app.ts" || !strings.Contains(editInput.Patch, "-old\n+new") {
		t.Fatalf("edit StructuredInput = %+v, want neutral patch input", editInput)
	}
	editResult := snapshot.Entries[3].Blocks[0].StructuredResult
	if editResult == nil {
		t.Fatal("edit StructuredResult = nil")
	}
	if editResult.Kind != "edit" || editResult.FilePath != "src/app.ts" || editResult.OldString != "old" || editResult.NewString != "new" {
		t.Fatalf("edit StructuredResult = %+v, want edit result with result-side patch", editResult)
	}
	if len(editResult.PatchHunks) != 1 || editResult.PatchHunks[0].FilePath != "src/app.ts" {
		t.Fatalf("edit PatchHunks = %+v, want one typed src/app.ts hunk", editResult.PatchHunks)
	}
	content := string(snapshot.Entries[3].Blocks[0].Content)
	for _, forbidden := range []string{"toolCallId", "rawOutput", "rawInput", "oldText", "newText"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("Grok result leaked native key %q: %s", forbidden, content)
		}
	}
}

func TestSessionLogAdapterLoadHistoryCarriesStructuredAuggieACPToolResults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeLines(t, path,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"auggie-session","update":{"sessionUpdate":"tool_call","toolCallId":"toolu-bash","title":"launch-process","kind":"execute","status":"pending","rawInput":{"command":"printf hello"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"auggie-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu-bash","status":"completed","rawOutput":{"stdout":"hello\n","stderr":"","exitCode":0}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"auggie-session","update":{"sessionUpdate":"tool_call","toolCallId":"toolu-edit","title":"str-replace-editor","kind":"edit","status":"pending","rawInput":{"path":"src/app.ts","oldText":"old","newText":"new"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"auggie-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu-edit","status":"completed","content":[{"type":"diff","path":"src/app.ts","oldText":"old\n","newText":"new\n"}]}}}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "auggie/tmux-cli",
		TranscriptPath: path,
		GCSessionID:    "gc-auggie",
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if snapshot.ProviderSessionID != "auggie-session" || snapshot.LogicalConversationID != "gc-auggie" {
		t.Fatalf("snapshot ids = provider %q logical %q, want auggie-session/gc-auggie", snapshot.ProviderSessionID, snapshot.LogicalConversationID)
	}
	if got := len(snapshot.Entries); got != 4 {
		t.Fatalf("len(Entries) = %d, want two tool uses plus two results", got)
	}

	bashInput := snapshot.Entries[0].Blocks[0].StructuredInput
	if bashInput == nil || bashInput.Kind != "command" || bashInput.Command != "printf hello" {
		t.Fatalf("bash StructuredInput = %+v, want command input", bashInput)
	}
	bashResult := snapshot.Entries[1].Blocks[0].StructuredResult
	if bashResult == nil {
		t.Fatal("bash StructuredResult = nil")
	}
	if bashResult.Kind != "bash" || bashResult.Stdout != "hello\n" || bashResult.ExitCode == nil || *bashResult.ExitCode != 0 {
		t.Fatalf("bash StructuredResult = %+v, want stdout and exit code", bashResult)
	}

	editInput := snapshot.Entries[2].Blocks[0].StructuredInput
	if editInput == nil || editInput.Kind != "patch" || editInput.FilePath != "src/app.ts" || !strings.Contains(editInput.Patch, "-old\n+new") {
		t.Fatalf("edit StructuredInput = %+v, want neutral patch input", editInput)
	}
	editResult := snapshot.Entries[3].Blocks[0].StructuredResult
	if editResult == nil {
		t.Fatal("edit StructuredResult = nil")
	}
	if editResult.Kind != "edit" || editResult.FilePath != "src/app.ts" || editResult.OldString != "old" || editResult.NewString != "new" {
		t.Fatalf("edit StructuredResult = %+v, want edit result with result-side patch", editResult)
	}
	if len(editResult.PatchHunks) != 1 || editResult.PatchHunks[0].FilePath != "src/app.ts" {
		t.Fatalf("edit PatchHunks = %+v, want one typed src/app.ts hunk", editResult.PatchHunks)
	}
	content := string(snapshot.Entries[3].Blocks[0].Content)
	for _, forbidden := range []string{"toolCallId", "rawOutput", "rawInput", "oldText", "newText"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("Auggie result leaked native key %q: %s", forbidden, content)
		}
	}
}

func TestSessionLogAdapterLoadHistoryCarriesCodexCommandFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-01-02T00:00:01Z","type":"response_item","payload":{"type":"function_call","call_id":"cmd-1","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"}}`,
		`{"timestamp":"2026-01-02T00:00:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"cmd-1","output":"{\"stdout\":\"\",\"stderr\":\"boom\\n\",\"exitCode\":2}"}}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "codex/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if len(snapshot.Entries) != 2 || len(snapshot.Entries[1].Blocks) != 1 {
		t.Fatalf("snapshot entries = %+v, want command result block", snapshot.Entries)
	}
	block := snapshot.Entries[1].Blocks[0]
	if !block.IsError {
		t.Fatalf("IsError = false, want true for nonzero Codex command exit; block = %+v", block)
	}
	result := block.StructuredResult
	if result == nil {
		t.Fatal("StructuredResult = nil")
	}
	if result.Kind != "bash" || result.Stderr != "boom\n" || result.ExitCode == nil || *result.ExitCode != 2 {
		t.Fatalf("StructuredResult = %+v, want bash stderr/exit_code", result)
	}
}

func TestSessionLogAdapterLoadHistoryCarriesCodexTokenUsage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-01-02T00:00:01Z","type":"turn_context","payload":{"model":"gpt-5-codex"}}`,
		`{"timestamp":"2026-01-02T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ready"}]}}`,
		`{"timestamp":"2026-01-02T00:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":110,"cached_input_tokens":10,"output_tokens":40,"reasoning_output_tokens":8,"total_tokens":150},"last_token_usage":{"input_tokens":110,"cached_input_tokens":10,"output_tokens":40,"reasoning_output_tokens":8,"total_tokens":150},"model_context_window":258400}}}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "codex/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if len(snapshot.Entries) != 1 {
		t.Fatalf("entries = %d, want one visible response item", len(snapshot.Entries))
	}

	entry := snapshot.Entries[0]
	if entry.Model != "gpt-5-codex" {
		t.Fatalf("entry.Model = %q, want gpt-5-codex", entry.Model)
	}
	if entry.Usage == nil {
		t.Fatal("entry.Usage = nil, want token_count usage")
	}
	if entry.Usage.InputTokens != 100 || entry.Usage.CacheReadTokens != 10 || entry.Usage.OutputTokens != 40 || entry.Usage.ReasoningTokens != 8 {
		t.Fatalf("entry.Usage = %+v, want input/cache/output/reasoning 100/10/40/8", entry.Usage)
	}
	if entry.Usage.ContextUsedTokens != 110 || entry.Usage.ContextWindowTokens != 258400 {
		t.Fatalf("entry.Usage context = %+v, want used/window 110/258400", entry.Usage)
	}
}

func TestSessionLogAdapterLoadHistoryCanonicalizesThinkingTextAndSignature(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sess-thinking.jsonl")
	writeLines(t, path,
		`{"uuid":"a1","type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private reasoning","signature":"encrypted"}]},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if len(snapshot.Entries) != 1 || len(snapshot.Entries[0].Blocks) != 1 {
		t.Fatalf("snapshot entries = %+v, want one thinking block", snapshot.Entries)
	}
	block := snapshot.Entries[0].Blocks[0]
	if block.Kind != BlockKindThinking {
		t.Fatalf("block.Kind = %q, want thinking", block.Kind)
	}
	if block.Text != "private reasoning" || block.Signature != "encrypted" {
		t.Fatalf("block = %+v, want canonical thinking text and signature", block)
	}
}

func TestSessionLogAdapterLoadHistoryAntigravityOpenToolUseIDs(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	writeLines(t, path,
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:01Z","content":"checking","tool_calls":[{"id":"call-open","name":"Read","args":{"path":"README.md"}}]}`,
	)

	snapshot, err := SessionLogAdapter{}.LoadHistory(LoadRequest{
		Provider:       "antigravity/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if len(snapshot.TailState.OpenToolUseIDs) != 1 || snapshot.TailState.OpenToolUseIDs[0] != "call-open" {
		t.Fatalf("OpenToolUseIDs = %#v, want [call-open]", snapshot.TailState.OpenToolUseIDs)
	}
}

func TestSessionLogAdapterLoadHistoryAntigravityCompletedToolUseIDs(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	writeLines(t, path,
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:01Z","content":"checking","tool_calls":[{"id":"call-done","name":"Read","args":{"path":"README.md"}}]}`,
		`{"step_index":2,"type":"READ_FILE","status":"failed","created_at":"2026-04-04T09:00:02Z","tool_call_id":"call-done","content":"file read failed"}`,
	)

	snapshot, err := SessionLogAdapter{}.LoadHistory(LoadRequest{
		Provider:       "antigravity/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if len(snapshot.TailState.OpenToolUseIDs) != 0 {
		t.Fatalf("OpenToolUseIDs = %#v, want none for completed tool use", snapshot.TailState.OpenToolUseIDs)
	}
	if len(snapshot.Entries) != 2 || len(snapshot.Entries[1].Blocks) != 1 {
		t.Fatalf("entries = %+v, want assistant plus one result", snapshot.Entries)
	}
	if !snapshot.Entries[1].Blocks[0].IsError {
		t.Fatalf("antigravity result IsError = false, want true from failed status: %+v", snapshot.Entries[1].Blocks[0])
	}
}

func TestSessionLogAdapterReadTranscriptAntigravityHonorsCursors(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	writeLines(t, path,
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-04-04T09:00:00Z","content":"first"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:01Z","content":"second"}`,
		`{"step_index":2,"type":"USER_INPUT","created_at":"2026-04-04T09:00:02Z","content":"third"}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:03Z","content":"fourth"}`,
	)

	full, err := SessionLogAdapter{}.ReadTranscript(TranscriptRequest{
		Provider:       "antigravity/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("ReadTranscript full: %v", err)
	}
	allIDs := transcriptEntryIDs(full)
	if len(allIDs) != 4 {
		t.Fatalf("full Antigravity transcript IDs = %v, want 4 entries", allIDs)
	}

	older, err := SessionLogAdapter{}.ReadTranscript(TranscriptRequest{
		Provider:       "antigravity/tmux-cli",
		TranscriptPath: path,
		BeforeEntryID:  allIDs[2],
	})
	if err != nil {
		t.Fatalf("ReadTranscript older: %v", err)
	}
	if got, want := transcriptEntryIDs(older), allIDs[:2]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("older Antigravity transcript IDs = %v, want %v", got, want)
	}

	rawNewer, err := SessionLogAdapter{}.ReadTranscript(TranscriptRequest{
		Provider:       "antigravity/tmux-cli",
		TranscriptPath: path,
		AfterEntryID:   allIDs[2],
		Raw:            true,
	})
	if err != nil {
		t.Fatalf("ReadTranscript raw newer: %v", err)
	}
	if got, want := transcriptEntryIDs(rawNewer), allIDs[3:]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("raw newer Antigravity transcript IDs = %v, want %v", got, want)
	}
	if len(rawNewer.RawMessages) != 1 {
		t.Fatalf("raw newer RawMessages = %d, want 1", len(rawNewer.RawMessages))
	}
}

func TestSessionLogAdapterReadTranscriptRawEmitsMultipartRecordsOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		record   string
	}{
		{
			name:     "kiro tool results",
			provider: "kiro/tmux-cli",
			record:   `{"type":"ToolResults","sessionId":"kiro-raw","message":{"role":"tool","content":[{"type":"toolResult","toolUseId":"toolu-one","content":"one"},{"type":"toolResult","toolUseId":"toolu-two","content":"two"}]}}`,
		},
		{
			name:     "amp tool results",
			provider: "amp/tmux-cli",
			record:   `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu-one","content":"one"},{"type":"tool_result","tool_use_id":"toolu-two","content":"two"}]},"session_id":"amp-raw"}`,
		},
		{
			name:     "cursor file edit",
			provider: "cursor/tmux-cli",
			record:   `{"hook_event_name":"afterFileEdit","file_path":"notes.txt","new_text":"updated","session_id":"cursor-raw"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "session.jsonl")
			writeLines(t, path, tt.record, tt.record)

			result, err := (SessionLogAdapter{}).ReadTranscript(TranscriptRequest{
				Provider:       tt.provider,
				TranscriptPath: path,
				Raw:            true,
			})
			if err != nil {
				t.Fatalf("ReadTranscript() error = %v", err)
			}
			if got := len(result.Session.Messages); got != 4 {
				t.Fatalf("normalized messages = %d, want two children per provider record", got)
			}
			if got := len(result.RawMessages); got != 2 {
				t.Fatalf("raw messages = %d, want each repeated provider record exactly once", got)
			}
			for i, raw := range result.RawMessages {
				if string(raw) != tt.record {
					t.Fatalf("raw message %d = %s, want byte-exact source record", i, raw)
				}
			}
		})
	}
}

func TestSessionLogAdapterLoadHistoryHonorsCursors(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	writeLines(t, path,
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-04-04T09:00:00Z","content":"first"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:01Z","content":"second"}`,
		`{"step_index":2,"type":"USER_INPUT","created_at":"2026-04-04T09:00:02Z","content":"third"}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","created_at":"2026-04-04T09:00:03Z","content":"fourth"}`,
	)

	full, err := SessionLogAdapter{}.LoadHistory(LoadRequest{
		Provider:       "antigravity/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory full: %v", err)
	}
	allIDs := historyEntryIDs(full)
	if len(allIDs) != 4 {
		t.Fatalf("full Antigravity history IDs = %v, want 4 entries", allIDs)
	}

	older, err := SessionLogAdapter{}.LoadHistory(LoadRequest{
		Provider:       "antigravity/tmux-cli",
		TranscriptPath: path,
		BeforeEntryID:  allIDs[2],
	})
	if err != nil {
		t.Fatalf("LoadHistory older: %v", err)
	}
	if got, want := historyEntryIDs(older), allIDs[:2]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("older history IDs = %v, want %v", got, want)
	}
	if older.Pagination == nil || older.Pagination.ReturnedMessageCount != 2 || older.Pagination.TotalMessageCount != 4 {
		t.Fatalf("older pagination = %+v, want returned/total counts 2/4", older.Pagination)
	}

	newer, err := SessionLogAdapter{}.LoadHistory(LoadRequest{
		Provider:       "antigravity/tmux-cli",
		TranscriptPath: path,
		AfterEntryID:   allIDs[2],
	})
	if err != nil {
		t.Fatalf("LoadHistory newer: %v", err)
	}
	if got, want := historyEntryIDs(newer), allIDs[3:]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("newer history IDs = %v, want %v", got, want)
	}
	if newer.Pagination == nil || newer.Pagination.ReturnedMessageCount != 1 || newer.Pagination.TotalMessageCount != 4 {
		t.Fatalf("newer pagination = %+v, want returned/total counts 1/4", newer.Pagination)
	}
}

func TestSessionLogAdapterDiscoverTranscriptExplicitIDFailsClosed(t *testing.T) {
	t.Parallel()

	workDir := "/tmp/project"
	base := t.TempDir()
	slug := strings.NewReplacer("/", "-", ".", "-").Replace(workDir)
	transcriptDir := filepath.Join(base, slug)
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}

	otherPath := filepath.Join(transcriptDir, "different-session.jsonl")
	writeLines(t, otherPath,
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2025-01-01T00:00:00Z"}`,
	)

	adapter := SessionLogAdapter{SearchPaths: []string{base}}
	discovered := adapter.DiscoverTranscript("claude/tmux-cli", workDir, "missing-session")
	if discovered != "" {
		t.Fatalf("DiscoverTranscript() = %q, want empty string when explicit session ID is missing", discovered)
	}
}

func transcriptEntryIDs(result *TranscriptResult) []string {
	ids := make([]string, 0, len(result.Session.Messages))
	for _, entry := range result.Session.Messages {
		ids = append(ids, entry.UUID)
	}
	return ids
}

func historyEntryIDs(snapshot *HistorySnapshot) []string {
	ids := make([]string, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		ids = append(ids, entry.ID)
	}
	return ids
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestSessionLogAdapterLoadHistoryKimiToolResultError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "context.jsonl")
	writeLines(t, path,
		`{"role":"assistant","content":[],"tool_calls":[{"type":"function","id":"call-kimi-read","function":{"name":"Read","arguments":"{\"path\":\"README.md\"}"}}]}`,
		`{"role":"tool","content":[{"type":"text","text":"read failed"}],"tool_call_id":"call-kimi-read","is_error":true}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "kimi/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if len(snapshot.Entries) != 2 || len(snapshot.Entries[1].Blocks) != 1 {
		t.Fatalf("entries = %+v, want assistant plus one result", snapshot.Entries)
	}
	if !snapshot.Entries[1].Blocks[0].IsError {
		t.Fatalf("kimi result IsError = false, want true from is_error: %+v", snapshot.Entries[1].Blocks[0])
	}
}

func TestSessionLogAdapterDiscoverTranscriptKimiKeyedMissFailsClosed(t *testing.T) {
	t.Parallel()

	workDir := "/tmp/kimi-project"
	base := t.TempDir()
	workHash := kimiTestWorkDirHash(workDir)
	otherPath := filepath.Join(base, "sessions", workHash, "different-session", "context.jsonl")
	if err := os.MkdirAll(filepath.Dir(otherPath), 0o755); err != nil {
		t.Fatalf("mkdir kimi transcript dir: %v", err)
	}
	writeLines(t, otherPath, `{"role":"user","content":"not this session"}`)

	adapter := SessionLogAdapter{SearchPaths: []string{base}}
	discovered := adapter.DiscoverTranscript("kimi/tmux-cli", workDir, "missing-session")
	if discovered != "" {
		t.Fatalf("DiscoverTranscript() = %q, want empty string when explicit Kimi session ID is missing", discovered)
	}
}

func TestSessionLogAdapterDiscoverTranscriptKimiEmptyKeyFailsClosedWhenAmbiguous(t *testing.T) {
	t.Parallel()

	workDir := "/tmp/kimi-project"
	base := t.TempDir()
	workHash := kimiTestWorkDirHash(workDir)
	firstPath := filepath.Join(base, "sessions", workHash, "first-session", "context.jsonl")
	secondPath := filepath.Join(base, "sessions", workHash, "second-session", "context.jsonl")
	if err := os.MkdirAll(filepath.Dir(firstPath), 0o755); err != nil {
		t.Fatalf("mkdir first kimi transcript dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(secondPath), 0o755); err != nil {
		t.Fatalf("mkdir second kimi transcript dir: %v", err)
	}
	writeLines(t, firstPath, `{"role":"user","content":"first"}`)
	writeLines(t, secondPath, `{"role":"user","content":"second"}`)

	adapter := SessionLogAdapter{SearchPaths: []string{base}}
	discovered := adapter.DiscoverTranscript("kimi/tmux-cli", workDir, "")
	if discovered != "" {
		t.Fatalf("DiscoverTranscript() = %q, want empty string when Kimi workdir has multiple sessions and no key", discovered)
	}
}

func TestSessionLogAdapterDiscoverTranscriptPiExplicitIDFailsClosed(t *testing.T) {
	t.Parallel()

	workDir := filepath.Join(t.TempDir(), "pi-project")
	base := t.TempDir()
	path := filepath.Join(base, "other.jsonl")
	writeLines(t, path,
		fmt.Sprintf(`{"type":"session","id":"other-session","cwd":%q}`, workDir),
		`{"type":"message","id":"u1","message":{"role":"user","content":"pending"}}`,
	)

	adapter := SessionLogAdapter{SearchPaths: []string{base}}
	discovered := adapter.DiscoverTranscript("pi/tmux-cli", workDir, "missing-session")
	if discovered != "" {
		t.Fatalf("DiscoverTranscript() = %q, want empty string when explicit Pi session ID is missing", discovered)
	}
}

func TestSessionLogAdapterLoadHistoryNormalizesOMPExecutionResults(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeLines(t, path,
		`{"type":"session","version":3,"id":"ses-omp","timestamp":"2026-02-02T00:00:00.000Z","cwd":"/tmp/gascity/omp"}`,
		`{"type":"message","id":"msg-bash","parentId":null,"timestamp":"2026-02-02T00:00:01.000Z","message":{"role":"bashExecution","command":"go test ./...","output":"ok ./internal/api","exitCode":0,"canceled":false,"truncated":true,"timestamp":1770000001000}}`,
		`{"type":"message","id":"msg-python","parentId":"msg-bash","timestamp":"2026-02-02T00:00:02.000Z","message":{"role":"pythonExecution","code":"print('hello')","output":"hello\n","exitCode":0,"canceled":false,"timestamp":1770000002000}}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "omp/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if len(snapshot.Entries) != 2 {
		t.Fatalf("entries = %d, want two execution results", len(snapshot.Entries))
	}

	bash := snapshot.Entries[0].Blocks[0]
	if bash.Kind != BlockKindToolResult || bash.Name != "bash" {
		t.Fatalf("bash block = %+v, want bash tool_result", bash)
	}
	if bash.StructuredResult == nil {
		t.Fatal("bash StructuredResult = nil")
	}
	if bash.StructuredResult.Kind != "bash" || bash.StructuredResult.Stdout != "ok ./internal/api" {
		t.Fatalf("bash StructuredResult = %+v, want typed bash stdout", bash.StructuredResult)
	}
	if bash.StructuredResult.ExitCode == nil || *bash.StructuredResult.ExitCode != 0 || !bash.StructuredResult.Truncated {
		t.Fatalf("bash exit/truncated = %+v, want exit 0 truncated true", bash.StructuredResult)
	}

	python := snapshot.Entries[1].Blocks[0]
	if python.Kind != BlockKindToolResult || python.Name != "python" {
		t.Fatalf("python block = %+v, want python tool_result", python)
	}
	if python.StructuredResult == nil {
		t.Fatal("python StructuredResult = nil")
	}
	if python.StructuredResult.Kind != "python" || python.StructuredResult.Code != "print('hello')" || python.StructuredResult.Stdout != "hello" {
		t.Fatalf("python StructuredResult = %+v, want typed python code/stdout", python.StructuredResult)
	}
	if python.StructuredResult.ExitCode == nil || *python.StructuredResult.ExitCode != 0 {
		t.Fatalf("python exit = %+v, want exit 0", python.StructuredResult)
	}
}

func TestSessionLogAdapterDiscoverTranscriptAntigravityProvisionalIDUsesLastConversation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	workDir := filepath.Join(t.TempDir(), "antigravity-project")
	fixtureRoot := t.TempDir()
	brainRoot := filepath.Join(fixtureRoot, "brain")
	convID := "750fa972-4c56-4215-99b9-893382aee2b4"
	path := filepath.Join(brainRoot, convID, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	writeLines(t, path, `{"step_index":0,"type":"USER_INPUT","content":"hello"}`)

	cachePath := filepath.Join(fixtureRoot, "cache", "last_conversations.json")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	if err := os.WriteFile(cachePath, []byte(fmt.Sprintf("{%q:%q}\n", workDir, convID)), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	adapter := SessionLogAdapter{SearchPaths: []string{brainRoot}}
	discovered := adapter.DiscoverTranscript("antigravity/tmux-cli", workDir, "gc-1")
	if discovered != path {
		t.Fatalf("DiscoverTranscript() = %q, want %q", discovered, path)
	}
}

func TestSessionLogAdapterLoadHistoryCodex(t *testing.T) {
	t.Parallel()

	workDir := "/tmp/codex-project"
	base := t.TempDir()
	dayDir := filepath.Join(base, "2026", "01", "02")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatalf("mkdir codex tree: %v", err)
	}

	path := filepath.Join(dayDir, "rollout-1.jsonl")
	lines := []string{
		fmt.Sprintf(`{"timestamp":"2026-01-02T00:00:00Z","type":"session_meta","payload":{"cwd":%q}}`, workDir),
		`{"timestamp":"2026-01-02T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"hello codex"}]}}`,
		`{"timestamp":"2026-01-02T00:00:02Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":{"patch":"*** Begin Patch\n*** End Patch"}}}`,
		`{"timestamp":"2026-01-02T00:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call-1","output":{"output":"Success. Updated files."}}}`,
		`{"timestamp":"2026-01-02T00:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"done"}]}}`,
	}
	writeLines(t, path, lines...)

	adapter := SessionLogAdapter{SearchPaths: []string{base}}
	discovered := adapter.DiscoverTranscript("codex/tmux-cli", workDir, "")
	if discovered != path {
		t.Fatalf("DiscoverTranscript() = %q, want %q", discovered, path)
	}

	snapshot, err := adapter.LoadHistory(LoadRequest{
		Provider:              "codex/tmux-cli",
		TranscriptPath:        path,
		LogicalConversationID: "codex-logical",
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if snapshot.LogicalConversationID != "codex-logical" {
		t.Fatalf("LogicalConversationID = %q, want codex-logical", snapshot.LogicalConversationID)
	}
	if snapshot.Continuity.Status != ContinuityStatusContinuous {
		t.Fatalf("Continuity.Status = %q, want %q", snapshot.Continuity.Status, ContinuityStatusContinuous)
	}
	lastEntryID := snapshot.Entries[len(snapshot.Entries)-1].ID
	if snapshot.TailState.LastEntryID != lastEntryID {
		t.Fatalf("TailState.LastEntryID = %q, want final entry ID %q", snapshot.TailState.LastEntryID, lastEntryID)
	}
	if snapshot.Entries[1].Blocks[0].Kind != BlockKindToolUse {
		t.Fatalf("function call block kind = %q, want %q", snapshot.Entries[1].Blocks[0].Kind, BlockKindToolUse)
	}
	if got := strings.TrimSpace(string(snapshot.Entries[1].Blocks[0].Input)); got != `{"patch":"*** Begin Patch\n*** End Patch"}` {
		t.Fatalf("custom tool call input = %s, want patch payload", got)
	}
	if snapshot.Entries[2].Blocks[0].Kind != BlockKindToolResult {
		t.Fatalf("function output block kind = %q, want %q", snapshot.Entries[2].Blocks[0].Kind, BlockKindToolResult)
	}
	if got := strings.TrimSpace(string(snapshot.Entries[2].Blocks[0].Content)); got != `{"output":"Success. Updated files."}` {
		t.Fatalf("custom tool output content = %s, want output payload", got)
	}
}

func TestSessionLogAdapterLoadHistoryGemini(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	projectDir := filepath.Join(base, "project-a", "chats")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir gemini tree: %v", err)
	}

	path := filepath.Join(projectDir, "session-1.json")
	body := `{
  "sessionId": "gem-session",
  "messages": [
    {"id":"m1","timestamp":"2026-01-02T00:00:00Z","type":"user","content":"hello"},
    {"id":"m2","timestamp":"2026-01-02T00:00:01Z","type":"gemini","model":"gemini-2.5-pro","tokens":{"input":100,"output":20,"cache":{"read":5,"write":3}},"content":"reply","thoughts":[{"subject":"plan","description":"check file"}],"toolCalls":[{"id":"tool-2","name":"Read","status":"failed","args":{"path":"README.md"},"result":[{"functionResponse":{"id":"tool-2","response":{"output":"contents","status":"error"}}}]}]}
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write gemini session: %v", err)
	}

	adapter := SessionLogAdapter{}
	snapshot, err := adapter.LoadHistory(LoadRequest{
		Provider:       "gemini/tmux-cli",
		TranscriptPath: path,
		GCSessionID:    "gc-gem",
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if got := len(snapshot.Entries); got != 2 {
		t.Fatalf("len(Entries) = %d, want 2", got)
	}
	if snapshot.Entries[1].Blocks[0].Kind != BlockKindThinking {
		t.Fatalf("first gemini block = %q, want %q", snapshot.Entries[1].Blocks[0].Kind, BlockKindThinking)
	}
	if snapshot.Entries[1].Blocks[2].Kind != BlockKindToolUse {
		t.Fatalf("tool call block = %q, want %q", snapshot.Entries[1].Blocks[2].Kind, BlockKindToolUse)
	}
	if snapshot.Entries[1].Blocks[3].Kind != BlockKindToolResult {
		t.Fatalf("tool result block = %q, want %q", snapshot.Entries[1].Blocks[3].Kind, BlockKindToolResult)
	}
	if !snapshot.Entries[1].Blocks[3].IsError {
		t.Fatal("tool result IsError = false, want true from Gemini status")
	}
	if snapshot.Entries[1].Model != "gemini-2.5-pro" {
		t.Fatalf("gemini model = %q, want gemini-2.5-pro", snapshot.Entries[1].Model)
	}
	if snapshot.Entries[1].Usage == nil {
		t.Fatal("gemini usage = nil, want tokens usage")
	}
	if snapshot.Entries[1].Usage.InputTokens != 100 || snapshot.Entries[1].Usage.OutputTokens != 20 ||
		snapshot.Entries[1].Usage.CacheReadTokens != 5 || snapshot.Entries[1].Usage.CacheCreationTokens != 3 {
		t.Fatalf("gemini usage = %+v, want input/output/cache read/write 100/20/5/3", snapshot.Entries[1].Usage)
	}
}

func TestSessionLogAdapterLoadHistoryGeminiErrorMessage(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeLines(t, path,
		`{"sessionId":"gemini-error-message","kind":"main"}`,
		`{"id":"err-1","timestamp":"2026-06-21T17:08:12Z","type":"error","content":[{"text":"Gemini stream interrupted"}]}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "gemini/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if len(snapshot.Entries) != 1 {
		t.Fatalf("entries = %+v, want one Gemini error entry", snapshot.Entries)
	}
	entry := snapshot.Entries[0]
	if entry.Actor != ActorSystem {
		t.Fatalf("entry.Actor = %q, want %q", entry.Actor, ActorSystem)
	}
	if len(entry.Blocks) != 1 || entry.Blocks[0].Kind != BlockKindText {
		t.Fatalf("entry.Blocks = %+v, want one text block", entry.Blocks)
	}
	if entry.Blocks[0].Text != "Gemini stream interrupted" {
		t.Fatalf("entry text = %q, want Gemini stream interrupted", entry.Blocks[0].Text)
	}
	if entry.SystemEvent == nil {
		t.Fatalf("entry.SystemEvent is nil, want Gemini provider error event")
	}
	if entry.SystemEvent.Kind != "error" || entry.SystemEvent.Category != "provider_error" || entry.SystemEvent.Message != "Gemini stream interrupted" {
		t.Fatalf("entry.SystemEvent = %+v, want provider-neutral Gemini error", entry.SystemEvent)
	}
}

func TestSessionLogAdapterLoadHistoryOpenCodeCarriesInfoMetadata(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session-opencode.json")
	body := `{
  "info": {"id":"opencode-metadata","directory":"/tmp/gascity/opencode"},
  "messages": [
    {"info":{"id":"opencode-1","sessionID":"opencode-metadata","role":"assistant","time":{"created":1780272000000},"providerID":"google","modelID":"gemini-2.5-flash","tokens":{"input":12,"output":3,"reasoning":5,"cache":{"read":4,"write":2}}},"parts":[{"id":"part-text","type":"text","text":"metadata ready"}]}
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write opencode export: %v", err)
	}

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "opencode/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if len(snapshot.Entries) != 1 {
		t.Fatalf("entries = %+v, want one OpenCode assistant entry", snapshot.Entries)
	}
	entry := snapshot.Entries[0]
	if entry.Model != "gemini-2.5-flash" {
		t.Fatalf("entry.Model = %q, want gemini-2.5-flash", entry.Model)
	}
	if entry.Usage == nil {
		t.Fatal("entry.Usage = nil, want OpenCode info.tokens usage")
	}
	if entry.Usage.InputTokens != 12 || entry.Usage.OutputTokens != 3 || entry.Usage.ReasoningTokens != 5 ||
		entry.Usage.CacheReadTokens != 4 || entry.Usage.CacheCreationTokens != 2 {
		t.Fatalf("entry.Usage = %+v, want input/output/reasoning/cache read/write 12/3/5/4/2", entry.Usage)
	}
}

func TestSessionLogAdapterMarksMalformedTailDegraded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sess-claude.jsonl")
	body := strings.Join([]string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`,
		`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":"done","model":"claude-sonnet","stop_reason":"end_turn","usage":{"input_tokens":1200}},"timestamp":"2025-01-01T00:00:04Z","sessionId":"provider-claude"}`,
	}, "\n") + "\n" + `{"uuid":"torn","type":"assistant","message":`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write torn transcript: %v", err)
	}

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if snapshot.Continuity.Status != ContinuityStatusDegraded {
		t.Fatalf("Continuity.Status = %q, want %q", snapshot.Continuity.Status, ContinuityStatusDegraded)
	}
	if snapshot.TailState.DegradedReason != "malformed_tail" {
		t.Fatalf("TailState.DegradedReason = %q, want malformed_tail", snapshot.TailState.DegradedReason)
	}
	if len(snapshot.Diagnostics) != 1 {
		t.Fatalf("Diagnostics len = %d, want 1", len(snapshot.Diagnostics))
	}
	if snapshot.Diagnostics[0].Code != "malformed_tail" {
		t.Fatalf("Diagnostics[0].Code = %q, want malformed_tail", snapshot.Diagnostics[0].Code)
	}
	if got := len(snapshot.Entries); got != 2 {
		t.Fatalf("Entries len = %d, want readable prefix entries", got)
	}
}

func TestSessionLogAdapterPreservesDurableInteractionHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sess-claude.jsonl")
	writeLines(t, path,
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"run a tool"},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`,
		`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"],"metadata":{"tool_name":"Read","attempt":2,"details":{"source":"test"}}}]},"timestamp":"2025-01-01T00:00:01Z","sessionId":"provider-claude"}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if got := snapshot.TailState.PendingInteractionIDs; len(got) != 1 || got[0] != "approval-1" {
		t.Fatalf("PendingInteractionIDs = %+v, want [approval-1]", got)
	}
	if got := len(snapshot.Entries); got != 2 {
		t.Fatalf("Entries len = %d, want 2", got)
	}
	blocks := snapshot.Entries[1].Blocks
	if len(blocks) != 1 {
		t.Fatalf("interaction entry blocks = %d, want 1", len(blocks))
	}
	block := blocks[0]
	if block.Kind != BlockKindInteraction {
		t.Fatalf("block kind = %q, want %q", block.Kind, BlockKindInteraction)
	}
	if block.Interaction == nil {
		t.Fatal("block interaction = nil, want payload")
	}
	if block.Interaction.RequestID != "approval-1" {
		t.Fatalf("RequestID = %q, want approval-1", block.Interaction.RequestID)
	}
	if block.Interaction.State != InteractionStatePending {
		t.Fatalf("State = %q, want %q", block.Interaction.State, InteractionStatePending)
	}
	if block.Interaction.Metadata["tool_name"] != "Read" {
		t.Fatalf("metadata tool_name = %q, want Read", block.Interaction.Metadata["tool_name"])
	}
	if block.Interaction.Metadata["attempt"] != "2" {
		t.Fatalf("metadata attempt = %q, want 2", block.Interaction.Metadata["attempt"])
	}
	if block.Interaction.Metadata["details"] != `{"source":"test"}` {
		t.Fatalf("metadata details = %q, want object JSON", block.Interaction.Metadata["details"])
	}
}

func TestSessionLogAdapterResolvedInteractionClearsTailPending(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sess-claude.jsonl")
	writeLines(t, path,
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"run a tool"},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`,
		`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"]}]},"timestamp":"2025-01-01T00:00:01Z","sessionId":"provider-claude"}`,
		`{"uuid":"u2","parentUuid":"a1","type":"user","message":{"role":"user","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":"resolved","action":"approve"}]},"timestamp":"2025-01-01T00:00:02Z","sessionId":"provider-claude"}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if len(snapshot.TailState.PendingInteractionIDs) != 0 {
		t.Fatalf("PendingInteractionIDs = %+v, want none after resolved interaction", snapshot.TailState.PendingInteractionIDs)
	}
	last := snapshot.Entries[len(snapshot.Entries)-1]
	if len(last.Blocks) != 1 || last.Blocks[0].Interaction == nil {
		t.Fatalf("last entry blocks = %+v, want resolved interaction block", last.Blocks)
	}
	if last.Blocks[0].Interaction.State != InteractionStateResolved {
		t.Fatalf("resolved state = %q, want %q", last.Blocks[0].Interaction.State, InteractionStateResolved)
	}
}

func TestSessionLogAdapterCodexResolvedInteractionClearsTailPending(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-01-02T00:00:00Z","type":"response_item","payload":{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?"}}`,
		`{"timestamp":"2026-01-02T00:00:01Z","type":"response_item","payload":{"type":"interaction","request_id":"approval-1","kind":"approval","state":"resolved","action":"approve"}}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "codex/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if got := len(snapshot.Entries); got != 2 {
		t.Fatalf("Entries len = %d, want 2", got)
	}
	if snapshot.Entries[0].ID == snapshot.Entries[1].ID {
		t.Fatalf("interaction lifecycle reused history entry ID %q", snapshot.Entries[0].ID)
	}
	if snapshot.Cursor.AfterEntryID != snapshot.Entries[1].ID {
		t.Fatalf("Cursor.AfterEntryID = %q, want %q", snapshot.Cursor.AfterEntryID, snapshot.Entries[1].ID)
	}
	if len(snapshot.TailState.PendingInteractionIDs) != 0 {
		t.Fatalf("PendingInteractionIDs = %+v, want none after resolved interaction", snapshot.TailState.PendingInteractionIDs)
	}
	if snapshot.Entries[1].Blocks[0].Interaction.State != InteractionStateResolved {
		t.Fatalf("resolved state = %q, want %q", snapshot.Entries[1].Blocks[0].Interaction.State, InteractionStateResolved)
	}
}

func TestSessionLogAdapterGeminiResolvedInteractionClearsTailPending(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	body := `{
  "sessionId": "gemini-interaction",
  "messages": [
    {"id":"m1","timestamp":"2026-01-02T00:00:00Z","type":"gemini","content":"approval needed","interactions":[{"request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?"}]},
    {"id":"m2","timestamp":"2026-01-02T00:00:01Z","type":"user","content":"approved","interactions":[{"request_id":"approval-1","kind":"approval","state":"resolved","action":"approve"}]}
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write gemini transcript: %v", err)
	}

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "gemini/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if got := len(snapshot.Entries); got != 2 {
		t.Fatalf("Entries len = %d, want 2", got)
	}
	if len(snapshot.TailState.PendingInteractionIDs) != 0 {
		t.Fatalf("PendingInteractionIDs = %+v, want none after resolved Gemini interaction", snapshot.TailState.PendingInteractionIDs)
	}
	last := snapshot.Entries[len(snapshot.Entries)-1]
	if len(last.Blocks) != 2 || last.Blocks[1].Interaction == nil {
		t.Fatalf("last entry blocks = %+v, want text and resolved interaction blocks", last.Blocks)
	}
	if last.Blocks[1].Interaction.State != InteractionStateResolved {
		t.Fatalf("resolved state = %q, want %q", last.Blocks[1].Interaction.State, InteractionStateResolved)
	}
}

func TestSessionLogAdapterMarksCodexMalformedInteriorDegraded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-01-02T00:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"hello"}]}}`,
		`not json`,
		`{"timestamp":"2026-01-02T00:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"done"}]}}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "codex/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if snapshot.Continuity.Status != ContinuityStatusDegraded {
		t.Fatalf("Continuity.Status = %q, want %q", snapshot.Continuity.Status, ContinuityStatusDegraded)
	}
	if snapshot.TailState.Degraded {
		t.Fatalf("TailState.Degraded = true, want false for interior malformed JSONL")
	}
	if len(snapshot.Diagnostics) != 1 {
		t.Fatalf("Diagnostics len = %d, want 1", len(snapshot.Diagnostics))
	}
	if snapshot.Diagnostics[0].Code != "malformed_jsonl" {
		t.Fatalf("Diagnostics[0].Code = %q, want malformed_jsonl", snapshot.Diagnostics[0].Code)
	}
	if got := len(snapshot.Entries); got != 2 {
		t.Fatalf("Entries len = %d, want valid codex entries", got)
	}
}

func TestSessionLogAdapterPreservesCompactionEvidenceWhenDegraded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sess-claude.jsonl")
	writeLines(t, path,
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`,
		`{"uuid":"c1","type":"system","subtype":"compact_boundary","logicalParentUuid":"u1","timestamp":"2025-01-01T00:00:01Z","sessionId":"provider-claude"}`,
		`not json`,
		`{"uuid":"a1","parentUuid":"c1","type":"assistant","message":{"role":"assistant","content":"done","model":"claude-sonnet","stop_reason":"end_turn"},"timestamp":"2025-01-01T00:00:02Z","sessionId":"provider-claude"}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if snapshot.Continuity.Status != ContinuityStatusDegraded {
		t.Fatalf("Continuity.Status = %q, want %q", snapshot.Continuity.Status, ContinuityStatusDegraded)
	}
	if snapshot.Continuity.CompactionCount != 1 {
		t.Fatalf("Continuity.CompactionCount = %d, want 1", snapshot.Continuity.CompactionCount)
	}
	if snapshot.TailState.Degraded {
		t.Fatalf("TailState.Degraded = true, want false for interior malformed JSONL")
	}
	if len(snapshot.Diagnostics) != 1 || snapshot.Diagnostics[0].Code != "malformed_jsonl" {
		t.Fatalf("Diagnostics = %+v, want malformed_jsonl", snapshot.Diagnostics)
	}
}

func TestSessionLogAdapterKeepsAllMalformedHistoryUnknown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sess-claude.jsonl")
	writeLines(t, path, `not json`)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}

	if snapshot.Continuity.Status != ContinuityStatusUnknown {
		t.Fatalf("Continuity.Status = %q, want %q", snapshot.Continuity.Status, ContinuityStatusUnknown)
	}
	if len(snapshot.Diagnostics) != 1 || snapshot.Diagnostics[0].Code != "malformed_tail" {
		t.Fatalf("Diagnostics = %+v, want malformed_tail", snapshot.Diagnostics)
	}
	if got := len(snapshot.Entries); got != 0 {
		t.Fatalf("Entries len = %d, want 0", got)
	}
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	data := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func kimiTestWorkDirHash(workDir string) string {
	sum := md5.Sum([]byte(workDir))
	return hex.EncodeToString(sum[:])
}
