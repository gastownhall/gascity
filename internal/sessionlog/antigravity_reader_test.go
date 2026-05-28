package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultAntigravitySearchPaths(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	got := DefaultAntigravitySearchPaths()
	want := filepath.Join(tmpHome, ".gemini", "antigravity-cli", "brain")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("DefaultAntigravitySearchPaths() = %v, want [%q]", got, want)
	}
}

func TestFindAntigravitySessionFileByID(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	sessionID := "18e4eb9f-1b1d-4dbc-966b-c06e3646f3c4"
	brainRoot := filepath.Join(tmpHome, ".gemini", "antigravity-cli", "brain")
	logDir := filepath.Join(brainRoot, sessionID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir setup: %v", err)
	}

	transcriptPath := filepath.Join(logDir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("fake history\n"), 0o644); err != nil {
		t.Fatalf("write file setup: %v", err)
	}

	// Test found
	got := FindAntigravitySessionFileByID(nil, "/my/workspace", sessionID)
	if got != transcriptPath {
		t.Fatalf("FindAntigravitySessionFileByID() = %q, want %q", got, transcriptPath)
	}

	// Test missing
	gotMissing := FindAntigravitySessionFileByID(nil, "/my/workspace", "non-existent-uuid")
	if gotMissing != "" {
		t.Fatalf("FindAntigravitySessionFileByID() missing case = %q, want empty", gotMissing)
	}
}

func TestFindAntigravitySessionFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	cliDir := filepath.Join(tmpHome, ".gemini", "antigravity-cli")
	brainRoot := filepath.Join(cliDir, "brain")
	historyPath := filepath.Join(cliDir, "history.jsonl")

	if err := os.MkdirAll(brainRoot, 0o755); err != nil {
		t.Fatalf("mkdir setup: %v", err)
	}

	workDir := "/Users/kevmoo/github/gascity"
	session1 := "ee5e687f-a1aa-4fdf-8e37-36c30a9183c6"
	session2 := "18e4eb9f-1b1d-4dbc-966b-c06e3646f3c4"

	// Mock conversation transcripts on disk
	for _, sessID := range []string{session1, session2} {
		logDir := filepath.Join(brainRoot, sessID, ".system_generated", "logs")
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			t.Fatalf("setup logs dir: %v", err)
		}
		tPath := filepath.Join(logDir, "transcript.jsonl")
		if err := os.WriteFile(tPath, []byte("fake history turns\n"), 0o644); err != nil {
			t.Fatalf("setup transcript: %v", err)
		}
	}

	// Mock historical log index mapping (session1 is older, session2 is latest)
	historyData := []AntigravityHistoryEntry{
		{
			Workspace:      workDir,
			ConversationID: session1,
			Timestamp:      1000,
		},
		{
			Workspace:      "/other/project",
			ConversationID: "other-id",
			Timestamp:      2000,
		},
		{
			Workspace:      workDir,
			ConversationID: session2,
			Timestamp:      3000, // Latest for our workspace!
		},
	}

	hfile, err := os.Create(historyPath)
	if err != nil {
		t.Fatalf("create history file: %v", err)
	}
	for _, entry := range historyData {
		b, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := hfile.Write(append(b, '\n')); err != nil {
			t.Fatalf("write history row: %v", err)
		}
	}
	hfile.Close()

	// 1. Verify resolution for target workDir picks the latest session (session2)
	got := FindAntigravitySessionFile(nil, workDir)
	wantPath := filepath.Join(brainRoot, session2, ".system_generated", "logs", "transcript.jsonl")
	if got != wantPath {
		t.Fatalf("FindAntigravitySessionFile() latest = %q, want %q", got, wantPath)
	}

	// 2. Verify resolution for non-matching workdir returns empty
	gotMissing := FindAntigravitySessionFile(nil, "/nonexistent/workspace")
	if gotMissing != "" {
		t.Fatalf("FindAntigravitySessionFile() missing case = %q, want empty", gotMissing)
	}
}
