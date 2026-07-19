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
	if snapshot.Entries[3].Blocks[0].Kind != BlockKindToolResult {
		t.Fatalf("result block kind = %q, want %q", snapshot.Entries[3].Blocks[0].Kind, BlockKindToolResult)
	}
	if snapshot.Cursor.AfterEntryID != "a2" {
		t.Fatalf("Cursor.AfterEntryID = %q, want a2", snapshot.Cursor.AfterEntryID)
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
		`{"step_index":2,"type":"READ_FILE","created_at":"2026-04-04T09:00:02Z","tool_call_id":"call-done","content":"file data"}`,
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

	older, err := SessionLogAdapter{}.ReadTranscript(TranscriptRequest{
		Provider:       "antigravity/tmux-cli",
		TranscriptPath: path,
		BeforeEntryID:  "agy-2",
	})
	if err != nil {
		t.Fatalf("ReadTranscript older: %v", err)
	}
	if got := transcriptEntryIDs(older); strings.Join(got, ",") != "agy-0,agy-1" {
		t.Fatalf("older Antigravity transcript IDs = %v, want [agy-0 agy-1]", got)
	}

	rawNewer, err := SessionLogAdapter{}.ReadTranscript(TranscriptRequest{
		Provider:       "antigravity/tmux-cli",
		TranscriptPath: path,
		AfterEntryID:   "agy-2",
		Raw:            true,
	})
	if err != nil {
		t.Fatalf("ReadTranscript raw newer: %v", err)
	}
	if got := transcriptEntryIDs(rawNewer); strings.Join(got, ",") != "agy-3" {
		t.Fatalf("raw newer Antigravity transcript IDs = %v, want [agy-3]", got)
	}
	if len(rawNewer.RawMessages) != 1 {
		t.Fatalf("raw newer RawMessages = %d, want 1", len(rawNewer.RawMessages))
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
	if snapshot.TailState.LastEntryID != "codex-3" {
		t.Fatalf("TailState.LastEntryID = %q, want codex-3", snapshot.TailState.LastEntryID)
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
