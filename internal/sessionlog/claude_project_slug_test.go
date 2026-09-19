package sessionlog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindSessionFileByIDClaudeUnderscoreWorkspace(t *testing.T) {
	root := t.TempDir()
	workDir := "/home/ubuntu/workspace_with_underscores"
	key := "11111111-2222-4333-8444-555555555555"
	for _, slug := range []string{"-home-ubuntu-workspace-with-underscores", "-home-ubuntu-workspace_with_underscores"} {
		t.Run(slug, func(t *testing.T) {
			projects := filepath.Join(root, slug)
			directory := filepath.Join(projects, slug)
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(directory, key+".jsonl")
			if err := os.WriteFile(want, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := FindSessionFileByID([]string{projects}, workDir, key); got != want {
				t.Fatalf("keyed transcript = %q, want %q", got, want)
			}
			if got := FindSessionFileByID([]string{projects}, workDir, "another-session"); got != "" {
				t.Fatalf("unrelated key resolved to %q", got)
			}
		})
	}
}
