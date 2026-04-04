package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if snapshot.Entries[3].Blocks[0].Kind != BlockKindToolResult {
		t.Fatalf("result block kind = %q, want %q", snapshot.Entries[3].Blocks[0].Kind, BlockKindToolResult)
	}
	if snapshot.Cursor.AfterEntryID != "a2" {
		t.Fatalf("Cursor.AfterEntryID = %q, want a2", snapshot.Cursor.AfterEntryID)
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
		`{"timestamp":"2026-01-02T00:00:02Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"Read"}}`,
		`{"timestamp":"2026-01-02T00:00:03Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"file"}}`,
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
	if snapshot.TailState.LastEntryID != "codex-3" {
		t.Fatalf("TailState.LastEntryID = %q, want codex-3", snapshot.TailState.LastEntryID)
	}
	if snapshot.Entries[1].Blocks[0].Kind != BlockKindToolUse {
		t.Fatalf("function call block kind = %q, want %q", snapshot.Entries[1].Blocks[0].Kind, BlockKindToolUse)
	}
	if snapshot.Entries[2].Blocks[0].Kind != BlockKindToolResult {
		t.Fatalf("function output block kind = %q, want %q", snapshot.Entries[2].Blocks[0].Kind, BlockKindToolResult)
	}
	if !snapshot.Entries[2].Blocks[0].Derived {
		t.Fatalf("expected codex tool_result block to be derived")
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
    {"id":"m2","timestamp":"2026-01-02T00:00:01Z","type":"gemini","content":"reply","thoughts":[{"subject":"plan","description":"check file"}],"toolCalls":[{"id":"tool-2","name":"Read","args":{"path":"README.md"},"result":[{"functionResponse":{"id":"tool-2","response":{"output":"contents"}}}]}]}
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
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	data := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
