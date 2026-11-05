package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// TestTranscriptPathClaudeLayoutNeverReadsCodexTree pins the narrowed lookup:
// for a session whose recorded provider would send the generic finder into the
// Codex tree, the Claude-layout lookup reports no transcript instead of
// returning a Codex rollout, while still resolving a Claude slug transcript.
func TestTranscriptPathClaudeLayoutNeverReadsCodexTree(t *testing.T) {
	workDir := t.TempDir()
	claudeRoot := t.TempDir()
	codexRoot := t.TempDir()
	searchPaths := []string{claudeRoot, codexRoot}

	mgr := newTestManager(t, beads.NewMemStore(), runtime.NewFake())
	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper", Title: "codex-shaped", Command: "codex", WorkDir: workDir,
		Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	dayDir := filepath.Join(codexRoot, "2026", "06", "12")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	rollout := filepath.Join(dayDir, "rollout-2026-06-12T10-00-00-019e0000-aaaa-7000-8000-000000000001.jsonl")
	cwd, err := json.Marshal(workDir)
	if err != nil {
		t.Fatalf("Marshal workDir: %v", err)
	}
	meta := `{"type":"session_meta","payload":{"id":"019e0000-aaaa-7000-8000-000000000001","cwd":` + string(cwd) + `}}` + "\n"
	if err := os.WriteFile(rollout, []byte(meta), 0o644); err != nil {
		t.Fatalf("WriteFile rollout: %v", err)
	}

	generic, _, err := mgr.TranscriptPathClassified(info.ID, searchPaths)
	if err != nil {
		t.Fatalf("TranscriptPathClassified: %v", err)
	}
	if generic != rollout {
		t.Fatalf("generic lookup = %q, want the Codex rollout %q (fixture must be reachable)", generic, rollout)
	}

	path, lookup, err := mgr.TranscriptPathClaudeLayout(info.ID, searchPaths)
	if err != nil {
		t.Fatalf("TranscriptPathClaudeLayout: %v", err)
	}
	if path != "" || lookup != TranscriptAbsent {
		t.Fatalf("Claude-layout lookup = (%q, %v), want (\"\", TranscriptAbsent)", path, lookup)
	}

	slugDir := filepath.Join(claudeRoot, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll slug: %v", err)
	}
	want := filepath.Join(slugDir, "latest.jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile slug: %v", err)
	}
	path, lookup, err = mgr.TranscriptPathClaudeLayout(info.ID, searchPaths)
	if err != nil {
		t.Fatalf("TranscriptPathClaudeLayout after slug write: %v", err)
	}
	if path != want || lookup != TranscriptFound {
		t.Fatalf("Claude-layout lookup = (%q, %v), want (%q, TranscriptFound)", path, lookup, want)
	}
}
