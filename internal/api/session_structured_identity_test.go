package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/worker"
)

// A Codex session's key is captured by its SessionStart hook and written to
// the session store out of band, so the API can serve the same rollout first
// under the GC session ID and then under the provider session key. That
// metadata change does not move the transcript: the wire stream identity and
// the resume cursor must survive it.
func TestStructuredStreamIdentitySurvivesLateSessionKey(t *testing.T) {
	const providerKey = "01a0dd46-cf6d-7a30-97d2-31591a5aeb04"
	path := filepath.Join(t.TempDir(), "rollout-2026-09-26T10-33-20-"+providerKey+".jsonl")
	rollout := `{"timestamp":"2026-09-26T10:33:20.100Z","type":"session_meta","payload":{"id":"` + providerKey + `","cwd":"/work"}}
{"timestamp":"2026-09-26T10:33:20.101Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"timestamp":"2026-09-26T10:33:20.200Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Start up."}]}}
{"timestamp":"2026-09-26T10:33:21.300Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Ready."}]}}
{"timestamp":"2026-09-26T10:33:21.310Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1","last_agent_message":"Ready."}}
`
	if err := os.WriteFile(path, []byte(rollout), 0o600); err != nil {
		t.Fatal(err)
	}

	load := func(gcSessionID string) SessionStreamStructuredMessageEvent {
		t.Helper()
		snapshot, err := worker.SessionLogAdapter{}.LoadHistory(worker.LoadRequest{
			Provider:       "codex",
			TranscriptPath: path,
			GCSessionID:    gcSessionID,
		})
		if err != nil {
			t.Fatalf("LoadHistory(%q): %v", gcSessionID, err)
		}
		messages, _ := historySnapshotStructuredMessages(snapshot, false)
		return SessionStreamStructuredMessageEvent{
			ID:                 "gc-278",
			Format:             "structured",
			SchemaVersion:      sessionStructuredSchemaVersion,
			History:            structuredHistoryFromSnapshot(snapshot),
			StructuredMessages: messages,
		}
	}

	beforeKey := load("gc-278")
	afterKey := load(providerKey)
	if beforeKey.History.LogicalConversationID == afterKey.History.LogicalConversationID {
		t.Fatalf("fixture must change the logical conversation ID, both are %q", afterKey.History.LogicalConversationID)
	}
	if beforeKey.History.TranscriptStreamID != afterKey.History.TranscriptStreamID {
		t.Fatalf("transcript_stream_id changed when only the session key became visible: %q -> %q",
			beforeKey.History.TranscriptStreamID, afterKey.History.TranscriptStreamID)
	}

	initial := buildStructuredStreamUpdate("", beforeKey, false)
	update := buildStructuredStreamUpdate(initial.History.Cursor.ResumeToken, afterKey, false)
	if update == nil {
		t.Fatal("update = nil, want an upsert carrying the refreshed conversation IDs")
	}
	if update.Operation != sessionStructuredOperationUpsert || update.ResetReason != "" {
		t.Fatalf("operation = %q reset_reason = %q, want upsert without reset", update.Operation, update.ResetReason)
	}
	if update.History.LogicalConversationID != providerKey {
		t.Fatalf("logical_conversation_id = %q, want %q", update.History.LogicalConversationID, providerKey)
	}
}
