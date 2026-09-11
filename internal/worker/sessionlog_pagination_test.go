package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// TestAttachStructuredToolDataWithContextPairsOffPageToolUse pins Finding 5: a
// Claude-style tool_result (no Name on the result block) whose matching tool_use
// is off the current page must still be typed from the full-session context,
// instead of degrading to plain text at the page boundary.
func TestAttachStructuredToolDataWithContextPairsOffPageToolUse(t *testing.T) {
	newResultEntry := func() HistoryEntry {
		return HistoryEntry{Blocks: []HistoryBlock{{
			Kind:      BlockKindToolResult,
			ToolUseID: "call-read",
			Content:   mustMarshalStructuredToolTest(t, "line1\nline2\n"),
		}}}
	}
	toolUseEntry := HistoryEntry{
		Actor: ActorAssistant,
		Blocks: []HistoryBlock{{
			Kind:      BlockKindToolUse,
			ToolUseID: "call-read",
			Name:      "Read",
			Input:     mustMarshalStructuredToolTest(t, map[string]any{"file_path": "/tmp/foo.go"}),
		}},
	}

	// Page-only (pre-fix behavior): the tool_use is off the page, so the result
	// cannot recover its read typing or the file path.
	pageOnly := attachStructuredToolData([]HistoryEntry{newResultEntry()})
	if got := pageOnly[0].Blocks[0].StructuredResult; got != nil && got.Kind == "read" {
		t.Fatalf("page-only result typed as read without tool_use context: %+v", got)
	}
	if got := pageOnly[0].Blocks[0].StructuredResult; got != nil && got.FilePath == "/tmp/foo.go" {
		t.Fatalf("page-only result recovered off-page file path: %+v", got)
	}

	// With full-session context: the off-page tool_use pairs the result, so it
	// keeps its typed read shape across the page boundary.
	page := []HistoryEntry{newResultEntry()}
	full := []HistoryEntry{toolUseEntry, newResultEntry()}
	page = attachStructuredToolDataWithContext(page, full)
	got := page[0].Blocks[0].StructuredResult
	if got == nil || got.Kind != "read" {
		t.Fatalf("context result = %+v, want kind=read paired from off-page tool_use", got)
	}
	if got.FilePath != "/tmp/foo.go" {
		t.Fatalf("context result FilePath = %q, want /tmp/foo.go from off-page tool_use input", got.FilePath)
	}
}

// TestLoadHistorySkipsCodexTailUsageOnBeforePage pins Finding 6: Codex tail
// usage is extracted from the file tail (the newest turns), so it must land on a
// page only when that page includes the tail. On an older "before" page the tail
// usages belong to newer, off-page turns and must not be back-filled onto the
// page's earlier assistants.
func TestLoadHistorySkipsCodexTailUsageOnBeforePage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-01-02T00:00:01Z","type":"turn_context","payload":{"model":"gpt-5-codex"}}`,
		`{"timestamp":"2026-01-02T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}}`,
		`{"timestamp":"2026-01-02T00:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}}`,
		`{"timestamp":"2026-01-02T00:00:04Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":110,"cached_input_tokens":10,"output_tokens":40,"reasoning_output_tokens":8,"total_tokens":150},"last_token_usage":{"input_tokens":110,"cached_input_tokens":10,"output_tokens":40,"reasoning_output_tokens":8,"total_tokens":150},"model_context_window":258400}}}`,
	)

	full, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{Provider: "codex/tmux-cli", TranscriptPath: path})
	if err != nil {
		t.Fatalf("LoadHistory(full) error = %v", err)
	}
	if len(full.Entries) != 2 {
		t.Fatalf("full entries = %d, want 2", len(full.Entries))
	}
	// The tail usage lands on the newest assistant when the page includes the tail.
	if full.Entries[1].Usage == nil {
		t.Fatal("newest assistant lost its tail usage on the full read")
	}

	// Scroll up: a "before" page excludes the tail, so its older assistant must
	// not inherit the newest, off-page turn's token counts.
	older, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "codex/tmux-cli",
		TranscriptPath: path,
		BeforeEntryID:  full.Entries[1].ID,
	})
	if err != nil {
		t.Fatalf("LoadHistory(before) error = %v", err)
	}
	if len(older.Entries) != 1 {
		t.Fatalf("before-page entries = %d, want 1 (older turn only); entries=%+v", len(older.Entries), older.Entries)
	}
	if older.Entries[0].Usage != nil {
		t.Fatalf("older-page assistant wrongly tagged with newer off-page tail usage: %+v", older.Entries[0].Usage)
	}
}

func TestSessionLogAdapterPaginationProviderMatrix(t *testing.T) {
	t.Parallel()

	providers := []string{
		"claude/tmux-cli",
		"auggie/tmux-cli",
		"amp/tmux-cli",
		"codex/tmux-cli",
		"copilot/tmux-cli",
		"cursor/tmux-cli",
		"grok/tmux-cli",
		"kiro/tmux-cli",
		"gemini/tmux-cli",
		"kimi/tmux-cli",
		"mimocode/tmux-cli",
		"opencode/tmux-cli",
		"pi/tmux-cli",
		"antigravity/tmux-cli",
	}

	for _, provider := range providers {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			path := writeWorkerPaginationFixture(t, sessionlog.ProviderFamily(provider))
			adapter := SessionLogAdapter{}

			for _, raw := range []bool{false, true} {
				raw := raw
				t.Run(fmt.Sprintf("transcript/raw=%t", raw), func(t *testing.T) {
					all, err := adapter.ReadTranscript(TranscriptRequest{
						Provider:       provider,
						TranscriptPath: path,
						Raw:            raw,
					})
					if err != nil {
						t.Fatalf("ReadTranscript full: %v", err)
					}
					allIDs := transcriptEntryIDs(all)
					if len(allIDs) != 3 {
						t.Fatalf("full transcript IDs = %v, want three fixture entries", allIDs)
					}
					cursor := allIDs[1]

					older, err := adapter.ReadTranscript(TranscriptRequest{
						Provider:       provider,
						TranscriptPath: path,
						BeforeEntryID:  cursor,
						Raw:            raw,
					})
					if err != nil {
						t.Fatalf("ReadTranscript older: %v", err)
					}
					assertWorkerTranscriptPage(t, older, allIDs[:1], 3, false, true)

					newer, err := adapter.ReadTranscript(TranscriptRequest{
						Provider:       provider,
						TranscriptPath: path,
						AfterEntryID:   cursor,
						Raw:            raw,
					})
					if err != nil {
						t.Fatalf("ReadTranscript newer: %v", err)
					}
					assertWorkerTranscriptPage(t, newer, allIDs[2:], 3, true, false)

					_, err = adapter.ReadTranscript(TranscriptRequest{
						Provider:       provider,
						TranscriptPath: path,
						AfterEntryID:   "missing-entry",
						Raw:            raw,
					})
					assertSessionLogCursorNotFound(t, err)
				})
			}

			all, err := adapter.LoadHistory(LoadRequest{
				Provider:       provider,
				TranscriptPath: path,
			})
			if err != nil {
				t.Fatalf("LoadHistory full: %v", err)
			}
			allIDs := historyEntryIDs(all)
			if len(allIDs) != 3 {
				t.Fatalf("full history IDs = %v, want three fixture entries", allIDs)
			}
			cursor := allIDs[1]

			older, err := adapter.LoadHistory(LoadRequest{
				Provider:       provider,
				TranscriptPath: path,
				BeforeEntryID:  cursor,
			})
			if err != nil {
				t.Fatalf("LoadHistory older: %v", err)
			}
			assertWorkerHistoryPage(t, older, allIDs[:1], 3, false, true)

			newer, err := adapter.LoadHistory(LoadRequest{
				Provider:       provider,
				TranscriptPath: path,
				AfterEntryID:   cursor,
			})
			if err != nil {
				t.Fatalf("LoadHistory newer: %v", err)
			}
			assertWorkerHistoryPage(t, newer, allIDs[2:], 3, true, false)

			_, err = adapter.LoadHistory(LoadRequest{
				Provider:       provider,
				TranscriptPath: path,
				BeforeEntryID:  "missing-entry",
			})
			assertSessionLogCursorNotFound(t, err)
		})
	}
}

func TestSessionLogAdapterPaginationKeepsTranscriptGlobalTailMetadata(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeLines(t, path,
		`{"uuid":"u0","type":"user","message":{"role":"user","content":"zero"},"sessionId":"provider-claude"}`,
		`{"uuid":"u1","parentUuid":"u0","type":"user","message":{"role":"user","content":"one"},"sessionId":"provider-claude"}`,
		`{"uuid":"compact-1","parentUuid":"u1","type":"system","subtype":"compact_boundary","logicalParentUuid":"u1","sessionId":"provider-claude"}`,
		`{"uuid":"pending-1","parentUuid":"compact-1","type":"assistant","message":{"role":"assistant","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?","options":["approve","deny"]}]},"sessionId":"provider-claude"}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
		BeforeEntryID:  "u1",
	})
	if err != nil {
		t.Fatalf("LoadHistory before u1: %v", err)
	}

	assertWorkerHistoryPage(t, snapshot, []string{"u0"}, 4, false, true)
	if snapshot.Cursor.AfterEntryID != "pending-1" {
		t.Fatalf("Cursor.AfterEntryID = %q, want transcript tip pending-1", snapshot.Cursor.AfterEntryID)
	}
	if snapshot.TailState.LastEntryID != "pending-1" {
		t.Fatalf("TailState.LastEntryID = %q, want transcript tip pending-1", snapshot.TailState.LastEntryID)
	}
	if got := snapshot.TailState.PendingInteractionIDs; !reflect.DeepEqual(got, []string{"approval-1"}) {
		t.Fatalf("PendingInteractionIDs = %v, want [approval-1]", got)
	}
	if snapshot.Continuity.Status != ContinuityStatusCompacted {
		t.Fatalf("Continuity.Status = %q, want %q", snapshot.Continuity.Status, ContinuityStatusCompacted)
	}
	if snapshot.Continuity.CompactionCount != 1 {
		t.Fatalf("Continuity.CompactionCount = %d, want transcript total 1", snapshot.Continuity.CompactionCount)
	}
}

func TestSessionLogAdapterPaginationKeepsResolvedInteractionOutOfGlobalPending(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeLines(t, path,
		`{"uuid":"u0","type":"user","message":{"role":"user","content":"zero"},"sessionId":"provider-claude"}`,
		`{"uuid":"u1","parentUuid":"u0","type":"user","message":{"role":"user","content":"one"},"sessionId":"provider-claude"}`,
		`{"uuid":"pending-1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":"pending","prompt":"Allow Read?"}]},"sessionId":"provider-claude"}`,
		`{"uuid":"resolved-1","parentUuid":"pending-1","type":"user","message":{"role":"user","content":[{"type":"interaction","request_id":"approval-1","kind":"approval","state":"resolved","action":"approve"}]},"sessionId":"provider-claude"}`,
	)

	snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
		BeforeEntryID:  "u1",
	})
	if err != nil {
		t.Fatalf("LoadHistory before u1: %v", err)
	}

	assertWorkerHistoryPage(t, snapshot, []string{"u0"}, 4, false, true)
	if snapshot.Cursor.AfterEntryID != "resolved-1" || snapshot.TailState.LastEntryID != "resolved-1" {
		t.Fatalf("global tip = cursor %q tail %q, want resolved-1", snapshot.Cursor.AfterEntryID, snapshot.TailState.LastEntryID)
	}
	if len(snapshot.TailState.PendingInteractionIDs) != 0 {
		t.Fatalf("PendingInteractionIDs = %v, want none after off-page resolution", snapshot.TailState.PendingInteractionIDs)
	}
}

func TestSessionLogAdapterRejectsBeforeAndAfterConsistently(t *testing.T) {
	t.Parallel()

	path := writeWorkerPaginationFixture(t, "claude/tmux-cli")
	adapter := SessionLogAdapter{}
	const want = "before and after entry IDs are mutually exclusive"

	_, transcriptErr := adapter.ReadTranscript(TranscriptRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
		BeforeEntryID:  "claude-1",
		AfterEntryID:   "claude-1",
	})
	if transcriptErr == nil || !strings.Contains(transcriptErr.Error(), want) {
		t.Fatalf("ReadTranscript both-cursor error = %v, want %q", transcriptErr, want)
	}
	if !errors.Is(transcriptErr, ErrTranscriptCursorConflict) {
		t.Fatalf("ReadTranscript both-cursor error = %v, want ErrTranscriptCursorConflict", transcriptErr)
	}

	_, historyErr := adapter.LoadHistory(LoadRequest{
		Provider:       "claude/tmux-cli",
		TranscriptPath: path,
		BeforeEntryID:  "claude-1",
		AfterEntryID:   "claude-1",
	})
	if historyErr == nil || !strings.Contains(historyErr.Error(), want) {
		t.Fatalf("LoadHistory both-cursor error = %v, want %q", historyErr, want)
	}
	if !errors.Is(historyErr, ErrTranscriptCursorConflict) {
		t.Fatalf("LoadHistory both-cursor error = %v, want ErrTranscriptCursorConflict", historyErr)
	}
}

func TestSessionLogAdapterPropagatesDuplicateEntryID(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeLines(t, path,
		`{"type":"user.message","data":{"content":"zero"},"id":"duplicate"}`,
		`{"type":"assistant.message","data":{"content":"one"},"id":"duplicate"}`,
		`{"type":"user.message","data":{"content":"two"},"id":"copilot-2"}`,
	)
	adapter := SessionLogAdapter{}

	_, transcriptErr := adapter.ReadTranscript(TranscriptRequest{
		Provider:       "copilot/tmux-cli",
		TranscriptPath: path,
	})
	assertSessionLogDuplicateEntryID(t, transcriptErr)
	_, historyErr := adapter.LoadHistory(LoadRequest{
		Provider:       "copilot/tmux-cli",
		TranscriptPath: path,
	})
	assertSessionLogDuplicateEntryID(t, historyErr)

	_, transcriptErr = adapter.ReadTranscript(TranscriptRequest{
		Provider:       "copilot/tmux-cli",
		TranscriptPath: path,
		AfterEntryID:   "duplicate",
	})
	assertSessionLogDuplicateEntryID(t, transcriptErr)

	_, historyErr = adapter.LoadHistory(LoadRequest{
		Provider:       "copilot/tmux-cli",
		TranscriptPath: path,
		AfterEntryID:   "duplicate",
	})
	assertSessionLogDuplicateEntryID(t, historyErr)
}

func TestTranscriptPaginationErrorBoundaryAliasesSessionLogTypes(t *testing.T) {
	t.Parallel()

	cursorSource := &sessionlog.CursorNotFoundError{
		Direction: sessionlog.CursorDirectionAfter,
		EntryID:   "missing-entry",
	}
	if !errors.Is(cursorSource, ErrTranscriptCursorNotFound) {
		t.Fatalf("cursor error = %v, want ErrTranscriptCursorNotFound identity", cursorSource)
	}
	var cursorTarget *TranscriptCursorNotFoundError
	if !errors.As(cursorSource, &cursorTarget) {
		t.Fatalf("cursor error type = %T, want *TranscriptCursorNotFoundError", cursorSource)
	}
	if cursorTarget.Direction != TranscriptCursorDirectionAfter || cursorTarget.EntryID != "missing-entry" {
		t.Fatalf("cursor target = %+v, want after/missing-entry", cursorTarget)
	}

	duplicateSource := &sessionlog.DuplicateEntryIDError{EntryID: "duplicate"}
	if !errors.Is(duplicateSource, ErrTranscriptDuplicateEntryID) {
		t.Fatalf("duplicate error = %v, want ErrTranscriptDuplicateEntryID identity", duplicateSource)
	}
	var duplicateTarget *TranscriptDuplicateEntryIDError
	if !errors.As(duplicateSource, &duplicateTarget) {
		t.Fatalf("duplicate error type = %T, want *TranscriptDuplicateEntryIDError", duplicateSource)
	}
	if duplicateTarget.EntryID != "duplicate" {
		t.Fatalf("duplicate target entry ID = %q, want duplicate", duplicateTarget.EntryID)
	}
}

func TestSessionHandleTranscriptAndHistoryPropagateCursorErrors(t *testing.T) {
	handle, _, _, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  "/tmp/gascity/phase1/claude",
		Provider: "claude",
	})
	handle.adapter.SearchPaths = []string{
		filepath.Join("workertest", "testdata", "fixtures", "claude", "fresh"),
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, transcriptErr := handle.Transcript(context.Background(), TranscriptRequest{AfterEntryID: "missing-entry"})
	assertSessionLogCursorNotFound(t, transcriptErr)
	_, historyErr := handle.History(context.Background(), HistoryRequest{AfterEntryID: "missing-entry"})
	assertSessionLogCursorNotFound(t, historyErr)

	_, transcriptErr = handle.Transcript(context.Background(), TranscriptRequest{
		BeforeEntryID: "c-u1",
		AfterEntryID:  "c-u1",
	})
	if !errors.Is(transcriptErr, ErrTranscriptCursorConflict) {
		t.Fatalf("Transcript both-cursor error = %v, want ErrTranscriptCursorConflict", transcriptErr)
	}
	_, historyErr = handle.History(context.Background(), HistoryRequest{
		BeforeEntryID: "c-u1",
		AfterEntryID:  "c-u1",
	})
	if !errors.Is(historyErr, ErrTranscriptCursorConflict) {
		t.Fatalf("History both-cursor error = %v, want ErrTranscriptCursorConflict", historyErr)
	}
}

func TestSessionHandleHistoryCursorPagesBypassContinuityCache(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	handle, _, _, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  Profile("copilot/tmux-cli"),
		Template: "probe",
		Title:    "Probe",
		Command:  "copilot",
		WorkDir:  workDir,
		Provider: "copilot",
	})
	handle.adapter.SearchPaths = []string{root}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	path := filepath.Join(root, "copilot-session", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir Copilot fixture: %v", err)
	}
	writeLines(t, path,
		fmt.Sprintf(`{"type":"session.start","data":{"cwd":%q}}`, workDir),
		`{"type":"user.message","data":{"content":"zero"},"id":"copilot-0"}`,
		`{"type":"assistant.message","data":{"content":"one"},"id":"copilot-1"}`,
		`{"type":"user.message","data":{"content":"two"},"id":"copilot-2"}`,
	)

	full, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatalf("History full: %v", err)
	}
	if got := historyEntryIDs(full); !reflect.DeepEqual(got, []string{"copilot-0", "copilot-1", "copilot-2"}) {
		t.Fatalf("full history IDs = %v, want [copilot-0 copilot-1 copilot-2]", got)
	}

	newer, err := handle.History(context.Background(), HistoryRequest{AfterEntryID: "copilot-1"})
	if err != nil {
		t.Fatalf("History after: %v", err)
	}
	assertWorkerHistoryPage(t, newer, []string{"copilot-2"}, 3, true, false)

	older, err := handle.History(context.Background(), HistoryRequest{BeforeEntryID: "copilot-1"})
	if err != nil {
		t.Fatalf("History before: %v", err)
	}
	assertWorkerHistoryPage(t, older, []string{"copilot-0"}, 3, false, true)
}

func TestSessionHandleTranscriptAndHistoryPropagateDuplicateEntryID(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	handle, _, _, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  Profile("copilot/tmux-cli"),
		Template: "probe",
		Title:    "Probe",
		Command:  "copilot",
		WorkDir:  workDir,
		Provider: "copilot",
	})
	handle.adapter.SearchPaths = []string{root}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	path := filepath.Join(root, "copilot-session", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir Copilot fixture: %v", err)
	}
	writeLines(t, path,
		fmt.Sprintf(`{"type":"session.start","data":{"cwd":%q}}`, workDir),
		`{"type":"user.message","data":{"content":"zero"},"id":"duplicate"}`,
		`{"type":"assistant.message","data":{"content":"one"},"id":"duplicate"}`,
		`{"type":"user.message","data":{"content":"two"},"id":"copilot-2"}`,
	)

	_, transcriptErr := handle.Transcript(context.Background(), TranscriptRequest{AfterEntryID: "duplicate"})
	assertSessionLogDuplicateEntryID(t, transcriptErr)
	_, historyErr := handle.History(context.Background(), HistoryRequest{AfterEntryID: "duplicate"})
	assertSessionLogDuplicateEntryID(t, historyErr)
}

func assertSessionLogCursorNotFound(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrTranscriptCursorNotFound) {
		t.Fatalf("error = %v, want ErrTranscriptCursorNotFound", err)
	}
	var cursorErr *TranscriptCursorNotFoundError
	if !errors.As(err, &cursorErr) {
		t.Fatalf("error type = %T, want *TranscriptCursorNotFoundError", err)
	}
}

func assertSessionLogDuplicateEntryID(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrTranscriptDuplicateEntryID) {
		t.Fatalf("error = %v, want ErrTranscriptDuplicateEntryID", err)
	}
	var duplicateErr *TranscriptDuplicateEntryIDError
	if !errors.As(err, &duplicateErr) {
		t.Fatalf("error type = %T, want *TranscriptDuplicateEntryIDError", err)
	}
	if duplicateErr.EntryID != "duplicate" {
		t.Fatalf("duplicate entry ID = %q, want duplicate", duplicateErr.EntryID)
	}
}

func assertWorkerTranscriptPage(t *testing.T, result *TranscriptResult, wantIDs []string, total int, wantOlder, wantNewer bool) {
	t.Helper()
	if got := transcriptEntryIDs(result); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("transcript page IDs = %v, want %v", got, wantIDs)
	}
	if result.Session.Pagination == nil || result.Session.Pagination.TotalMessageCount != total || result.Session.Pagination.ReturnedMessageCount != len(wantIDs) {
		t.Fatalf("transcript pagination = %+v, want total=%d returned=%d", result.Session.Pagination, total, len(wantIDs))
	}
	assertWorkerPaginationFlags(t, result.Session.Pagination, wantOlder, wantNewer)
}

func assertWorkerHistoryPage(t *testing.T, snapshot *HistorySnapshot, wantIDs []string, total int, wantOlder, wantNewer bool) {
	t.Helper()
	if got := historyEntryIDs(snapshot); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("history page IDs = %v, want %v", got, wantIDs)
	}
	if snapshot.Pagination == nil || snapshot.Pagination.TotalMessageCount != total || snapshot.Pagination.ReturnedMessageCount != len(wantIDs) {
		t.Fatalf("history pagination = %+v, want total=%d returned=%d", snapshot.Pagination, total, len(wantIDs))
	}
	assertWorkerPaginationFlags(t, snapshot.Pagination, wantOlder, wantNewer)
}

func assertWorkerPaginationFlags(t *testing.T, pagination *TranscriptPagination, wantOlder, wantNewer bool) {
	t.Helper()
	wire, err := json.Marshal(pagination)
	if err != nil {
		t.Fatalf("marshal pagination: %v", err)
	}
	var flags struct {
		HasOlderMessages bool `json:"has_older_messages"`
		HasNewerMessages bool `json:"has_newer_messages"`
	}
	if err := json.Unmarshal(wire, &flags); err != nil {
		t.Fatalf("decode pagination flags: %v", err)
	}
	if flags.HasOlderMessages != wantOlder || flags.HasNewerMessages != wantNewer {
		t.Fatalf("pagination flags = older:%t newer:%t, want older:%t newer:%t; wire=%s", flags.HasOlderMessages, flags.HasNewerMessages, wantOlder, wantNewer, wire)
	}
}

func writeWorkerPaginationFixture(t *testing.T, family string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	var body string

	switch family {
	case "claude/tmux-cli":
		body = strings.Join([]string{
			`{"uuid":"claude-0","type":"user","message":{"role":"user","content":"zero"}}`,
			`{"uuid":"claude-1","parentUuid":"claude-0","type":"assistant","message":{"role":"assistant","content":"one"}}`,
			`{"uuid":"claude-2","parentUuid":"claude-1","type":"user","message":{"role":"user","content":"two"}}`,
		}, "\n") + "\n"
	case "auggie", "grok", "kiro":
		body = strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"session/update","params":{"sessionId":"acp-session","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"zero"}}}}`,
			`{"jsonrpc":"2.0","id":2,"method":"session/update","params":{"sessionId":"acp-session","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"one"}}}}`,
			`{"jsonrpc":"2.0","id":3,"method":"session/update","params":{"sessionId":"acp-session","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"two"}}}}`,
		}, "\n") + "\n"
	case "amp":
		body = strings.Join([]string{
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"zero"}]},"session_id":"amp-session"}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"one"}]},"session_id":"amp-session"}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"two"}]},"session_id":"amp-session"}`,
		}, "\n") + "\n"
	case "codex":
		body = strings.Join([]string{
			`{"timestamp":"2026-01-01T00:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"zero"}]}}`,
			`{"timestamp":"2026-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"one"}]}}`,
			`{"timestamp":"2026-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"two"}]}}`,
		}, "\n") + "\n"
	case "copilot":
		body = strings.Join([]string{
			`{"type":"user.message","data":{"content":"zero"},"id":"copilot-0"}`,
			`{"type":"assistant.message","data":{"content":"one"},"id":"copilot-1"}`,
			`{"type":"user.message","data":{"content":"two"},"id":"copilot-2"}`,
		}, "\n") + "\n"
	case "cursor":
		body = strings.Join([]string{
			`{"type":"user","message":{"role":"user","content":"zero"},"session_id":"cursor-session"}`,
			`{"type":"assistant","message":{"role":"assistant","content":"one"},"session_id":"cursor-session"}`,
			`{"type":"user","message":{"role":"user","content":"two"},"session_id":"cursor-session"}`,
		}, "\n") + "\n"
	case "gemini":
		path = filepath.Join(dir, "session.json")
		body = `{"sessionId":"gemini-session","messages":[` +
			`{"id":"gemini-0","type":"user","content":"zero"},` +
			`{"id":"gemini-1","type":"gemini","content":"one"},` +
			`{"id":"gemini-2","type":"user","content":"two"}` +
			`]}`
	case "kimi":
		body = strings.Join([]string{
			`{"role":"user","content":"zero"}`,
			`{"role":"assistant","content":"one"}`,
			`{"role":"user","content":"two"}`,
		}, "\n") + "\n"
	case "mimocode", "opencode":
		path = filepath.Join(dir, "session.json")
		body = `{"info":{"id":"opencode-session","directory":"/tmp/project"},"messages":[` +
			`{"info":{"id":"opencode-0","role":"user"},"parts":[{"type":"text","text":"zero"}]},` +
			`{"info":{"id":"opencode-1","role":"assistant"},"parts":[{"type":"text","text":"one"}]},` +
			`{"info":{"id":"opencode-2","role":"user"},"parts":[{"type":"text","text":"two"}]}` +
			`]}`
	case "pi":
		body = strings.Join([]string{
			`{"type":"session","version":3,"id":"pi-session","cwd":"/tmp/project"}`,
			`{"type":"message","id":"pi-0","parentId":null,"message":{"role":"user","content":"zero"}}`,
			`{"type":"message","id":"pi-1","parentId":"pi-0","message":{"role":"assistant","content":"one"}}`,
			`{"type":"message","id":"pi-2","parentId":"pi-1","message":{"role":"user","content":"two"}}`,
		}, "\n") + "\n"
	case "antigravity":
		body = strings.Join([]string{
			`{"step_index":0,"type":"USER_INPUT","content":"zero"}`,
			`{"step_index":1,"type":"PLANNER_RESPONSE","content":"one"}`,
			`{"step_index":2,"type":"USER_INPUT","content":"two"}`,
		}, "\n") + "\n"
	default:
		t.Fatalf("no worker pagination fixture for provider family %q", family)
	}

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write worker pagination fixture: %v", err)
	}
	return path
}
