package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Learning a provider resume key must not change the identity of a transcript
// already exposed to a client; the next append must remain an ordinary update.
func TestSessionHandleHistoryIdentitySurvivesCodexHookKeyPromotion(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	dayDir := filepath.Join(root, "sessions", "2026", "09", "06")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	key := "01a07957-329b-7a32-87c5-1d69abe93f1b"
	path := filepath.Join(dayDir, "rollout-2026-09-06T17-49-12-"+key+".jsonl")
	initial := fmt.Sprintf(`{"timestamp":"2026-09-06T17:49:12Z","type":"session_meta","payload":{"id":%q,"cwd":%q}}
{"timestamp":"2026-09-06T17:49:13Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"startup"}]}}
{"timestamp":"2026-09-06T17:49:14Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"ready"}]}}
`, key, workDir)
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	handle, store, _, _ := newTestSessionHandle(t, SessionSpec{
		Profile: ProfileCodexTmuxCLI, Template: "probe", Command: "codex",
		Provider: "codex", WorkDir: workDir,
		Resume: sessionpkg.ProviderResume{ResumeFlag: "resume", ResumeStyle: "subcommand"},
	})
	handle.adapter.SearchPaths = []string{filepath.Join(root, "sessions")}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if before.GCSessionID != handle.sessionID || before.LogicalConversationID != handle.sessionID {
		t.Fatalf("initial history identity = %q/%q, want GC session %q", before.GCSessionID, before.LogicalConversationID, handle.sessionID)
	}
	if err := store.SetMetadata(handle.sessionID, "session_key", key); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(`{"timestamp":"2026-09-06T17:49:15Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"BB request"}]}}
{"timestamp":"2026-09-06T17:49:16Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"BB reply"}]}}
`)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append transcript: %v; close: %v", writeErr, closeErr)
	}
	after, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if before.GCSessionID != after.GCSessionID || before.LogicalConversationID != after.LogicalConversationID ||
		before.ProviderSessionID != after.ProviderSessionID || before.TranscriptStreamID != after.TranscriptStreamID {
		t.Fatalf("hook promotion changed history identity: before GC=%q logical=%q provider=%q path=%q; after GC=%q logical=%q provider=%q path=%q",
			before.GCSessionID, before.LogicalConversationID, before.ProviderSessionID, before.TranscriptStreamID,
			after.GCSessionID, after.LogicalConversationID, after.ProviderSessionID, after.TranscriptStreamID)
	}
	if len(after.Entries) != 4 || after.Entries[0].ID != before.Entries[0].ID || after.Entries[1].ID != before.Entries[1].ID {
		t.Fatalf("history lost its original prefix or new turn: %+v", after.Entries)
	}
}
