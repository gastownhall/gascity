package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Claude 2.1.263 encodes an underscore in cwd as a hyphen in its project
// directory. Older transcript trees may retain the underscore. Discovery must
// preserve the requested conversation in either layout, even with newer files.
func TestDiscoverClaudeUnderscoreWorkspaceByExactKey(t *testing.T) {
	for _, layout := range []string{"current", "legacy"} {
		t.Run(layout, func(t *testing.T) {
			root := t.TempDir()
			workDir := "/projects/my_workspace"
			slugs := map[string]string{
				"current": "-projects-my-workspace",
				"legacy":  "-projects-my_workspace",
			}
			key := "289a8d06-9127-44ea-8ffe-4d03983c7254"
			want := filepath.Join(root, slugs[layout], key+".jsonl")
			for _, slug := range slugs {
				dir := filepath.Join(root, slug)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "unrelated-newer-session.jsonl"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(want, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(want, old, old); err != nil {
				t.Fatal(err)
			}
			for _, provider := range []string{"claude", "claude/tmux-cli"} {
				if got := DiscoverPath([]string{root}, provider, workDir, key); got != want {
					t.Errorf("%s discovery = %q, want exact %q", provider, got, want)
				}
				if present, probeable := HasKeyedTranscript([]string{root}, provider, workDir, key); !present || !probeable {
					t.Errorf("%s valid conversation considered stale: present=%v probeable=%v", provider, present, probeable)
				}
				if got := DiscoverPath([]string{root}, provider, workDir, "missing-key"); got != "" {
					t.Errorf("%s missing conversation borrowed %q", provider, got)
				}
				if present, probeable := HasKeyedTranscript([]string{root}, provider, workDir, "missing-key"); present || !probeable {
					t.Errorf("%s missing key probe: present=%v probeable=%v", provider, present, probeable)
				}
				// Encoding collisions must never admit the other workspace's
				// legacy underscore tree when its own keyed log is absent.
				if layout == "legacy" {
					if got := DiscoverPath([]string{root}, provider, strings.ReplaceAll(workDir, "_", "-"), key); got != "" {
						t.Errorf("hyphen workspace borrowed underscore legacy tree: %q", got)
					}
				}
			}
		})
	}
}
