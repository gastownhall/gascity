package sessionlog

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRepeatedSyntheticProviderRecordsHaveUniqueAppendStableEntryIDs(t *testing.T) {
	tests := []struct {
		provider string
		line     string
	}{
		{
			provider: "kiro/tmux-cli",
			line:     `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"repeat"}}}}`,
		},
		{
			provider: "auggie/tmux-cli",
			line:     `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"repeat"}}}}`,
		},
		{
			provider: "grok/tmux-cli",
			line:     `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"repeat"}}}}`,
		},
		{
			provider: "amp/tmux-cli",
			line:     `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"repeat"}]},"session_id":"session-1"}`,
		},
		{
			provider: "cursor/tmux-cli",
			line:     `{"hook_event_name":"afterAgentResponse","response":"repeat","session_id":"session-1"}`,
		},
		{
			provider: "copilot/tmux-cli",
			line:     `{"type":"assistant.message","data":{"content":"repeat"},"sessionId":"session-1"}`,
		},
	}

	for _, tt := range tests {
		t.Run(ProviderFamily(tt.provider), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			writeRepeatedSyntheticRecords(t, path, tt.line, 2)
			before, err := ReadProviderFile(tt.provider, path, 0)
			if err != nil {
				t.Fatalf("read two repeated records: %v", err)
			}
			beforeIDs := paginationEntryIDs(before.Messages)
			if len(beforeIDs) != 2 {
				t.Fatalf("entry IDs = %v, want two entries", beforeIDs)
			}
			if beforeIDs[0] == beforeIDs[1] {
				t.Fatalf("repeated records reused entry ID %q", beforeIDs[0])
			}

			writeRepeatedSyntheticRecords(t, path, tt.line, 3)
			after, err := ReadProviderFile(tt.provider, path, 0)
			if err != nil {
				t.Fatalf("read after appending a repeated record: %v", err)
			}
			afterIDs := paginationEntryIDs(after.Messages)
			if len(afterIDs) != 3 {
				t.Fatalf("entry IDs after append = %v, want three entries", afterIDs)
			}
			if !reflect.DeepEqual(afterIDs[:2], beforeIDs) {
				t.Fatalf("retained IDs changed after append: got %v, want %v", afterIDs[:2], beforeIDs)
			}
			if afterIDs[2] == afterIDs[0] || afterIDs[2] == afterIDs[1] {
				t.Fatalf("appended repeated record reused an existing ID: %v", afterIDs)
			}
		})
	}
}

func writeRepeatedSyntheticRecords(t *testing.T, path, line string, count int) {
	t.Helper()
	body := ""
	for range count {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write repeated-record fixture: %v", err)
	}
}
