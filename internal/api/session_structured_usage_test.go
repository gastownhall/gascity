package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/worker"
)

// Codex writes the token_count for a model call after the tool output that
// call requested, so its usage is attached to the tool-call entry once a
// client already holds the output after it. Refreshing that accounting must
// reach clients as an ordinary upsert, not as a history_rewritten reset that
// aborts the in-progress turn.
func TestStructuredStreamUpsertsLateCodexToolCallUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-2026-09-26T10-11-09-01a0dd32-7fd4-7862-98d4-773a63898471.jsonl")
	lines := []string{
		`{"timestamp":"2026-09-26T10:11:10.000Z","type":"session_meta","payload":{"id":"01a0dd32-7fd4-7862-98d4-773a63898471","cwd":"/work"}}`,
		`{"timestamp":"2026-09-26T10:11:10.001Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}`,
		`{"timestamp":"2026-09-26T10:11:10.100Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Create the file and read it back."}]}}`,
		`{"timestamp":"2026-09-26T10:11:12.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"printf 'x\\\\n' > proof.txt\"}","call_id":"call_1"}}`,
		`{"timestamp":"2026-09-26T10:11:12.100Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"Process exited with code 0\nOutput:\n"}}`,
		`{"timestamp":"2026-09-26T10:11:12.102Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1200,"cached_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":10,"total_tokens":1240},"last_token_usage":{"input_tokens":1200,"cached_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":10,"total_tokens":1240},"model_context_window":258400}}}`,
	}

	project := func(count int) SessionStreamStructuredMessageEvent {
		t.Helper()
		data := ""
		for _, line := range lines[:count] {
			data += line + "\n"
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := worker.SessionLogAdapter{}.LoadHistory(worker.LoadRequest{
			Provider:       "codex",
			TranscriptPath: path,
			GCSessionID:    "gc-1",
		})
		if err != nil {
			t.Fatalf("LoadHistory(%d lines): %v", count, err)
		}
		messages, _ := historySnapshotStructuredMessages(snapshot, false)
		return SessionStreamStructuredMessageEvent{
			ID:                 "gc-1",
			Format:             "structured",
			SchemaVersion:      sessionStructuredSchemaVersion,
			History:            structuredHistoryFromSnapshot(snapshot),
			StructuredMessages: messages,
		}
	}

	beforeUsage := project(len(lines) - 1)
	afterUsage := project(len(lines))
	toolCall := afterUsage.StructuredMessages[len(afterUsage.StructuredMessages)-2]
	if toolCall.Usage == nil {
		t.Fatalf("fixture must attach usage to the tool-call entry before the tail, got %+v", toolCall)
	}
	if beforeUsage.StructuredMessages[len(beforeUsage.StructuredMessages)-2].Usage != nil {
		t.Fatal("fixture must not attach usage before its token_count is written")
	}

	initial := buildStructuredStreamUpdate("", beforeUsage, false)
	update := buildStructuredStreamUpdate(initial.History.Cursor.ResumeToken, afterUsage, false)
	if update == nil {
		t.Fatal("update = nil, want an upsert")
	}
	if update.Operation != sessionStructuredOperationUpsert || update.ResetReason != "" {
		t.Fatalf("operation = %q reset_reason = %q, want upsert without reset", update.Operation, update.ResetReason)
	}

	// Content changes before the cursor are still rewrites.
	rewritten := afterUsage
	rewritten.StructuredMessages = append([]SessionStructuredMessage(nil), afterUsage.StructuredMessages...)
	rewritten.StructuredMessages[0].Blocks = []SessionStructuredBlock{{Type: "text", Text: "edited"}}
	if got := buildStructuredStreamUpdate(initial.History.Cursor.ResumeToken, rewritten, false); got == nil || got.ResetReason != sessionStructuredResetHistoryRewritten {
		t.Fatalf("content rewrite update = %+v, want history_rewritten reset", got)
	}
}
