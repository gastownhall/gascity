package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasKeyedTranscriptClaudeUnderscoreWorkspace(t *testing.T) {
	projects := filepath.Join(t.TempDir(), "private-config", "projects")
	directory := filepath.Join(projects, "-home-ubuntu-workspace-with-underscores")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	key := "11111111-2222-4333-8444-555555555555"
	if err := os.WriteFile(filepath.Join(directory, key+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"claude", "claude-moonshot"} {
		exists, probeable := HasKeyedTranscript([]string{projects}, provider, "/home/ubuntu/workspace_with_underscores", key)
		if !exists || !probeable {
			t.Fatalf("%s exact transcript = (%v, %v)", provider, exists, probeable)
		}
		exists, probeable = HasKeyedTranscript([]string{projects}, provider, "/home/ubuntu/workspace_with_underscores", "missing-key")
		if exists || !probeable {
			t.Fatalf("%s missing key = (%v, %v)", provider, exists, probeable)
		}
	}
}
