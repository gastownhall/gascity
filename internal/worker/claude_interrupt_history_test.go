package worker

import (
	"os"
	"path/filepath"
	"testing"
)

// Claude's native tool interruption closes the tool and leaves the prompt idle;
// it does not append an assistant end_turn or system turn_duration entry.
func TestClaudeToolInterruptHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	raw, err := os.ReadFile("testdata/claude-tool-interrupt.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := SessionLogAdapter{SearchPaths: []string{filepath.Dir(path)}}
	snapshot, err := adapter.LoadHistory(LoadRequest{Provider: "claude/tmux-cli", TranscriptPath: path, GCSessionID: "gc-owned"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TailState.Activity != TailActivityIdle || snapshot.TailState.Degraded || len(snapshot.TailState.OpenToolUseIDs) != 0 {
		t.Fatalf("interrupted native history did not settle: %+v", snapshot.TailState)
	}
	if len(snapshot.Entries) != 4 || snapshot.Entries[3].ID != "interrupted" || snapshot.Entries[3].Text != "[Request interrupted by user for tool use]" {
		t.Fatalf("interruption history was not preserved: %+v", snapshot.Entries)
	}
	activity, err := adapter.TailActivityForProvider("claude/tmux-cli", path)
	if err != nil || activity != TailActivityIdle {
		t.Fatalf("fast tail activity=%q, error=%v", activity, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"uuid":"followup","parentUuid":"interrupted","type":"user","message":{"role":"user","content":"Continue after the interruption"}}` + "\n")
	closeErr := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	after, err := adapter.LoadHistory(LoadRequest{Provider: "claude/tmux-cli", TranscriptPath: path, GCSessionID: "gc-owned"})
	if err != nil || after.TailState.Activity != TailActivityInTurn {
		t.Fatalf("follow-up did not reopen the native turn: snapshot=%+v, error=%v", after, err)
	}
}
