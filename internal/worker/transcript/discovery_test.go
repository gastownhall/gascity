package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

func TestDiscoverPathPrefersClaudeSessionID(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "claude-project")
	slugDir := filepath.Join(base, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}

	keyed := filepath.Join(slugDir, "gc-123.jsonl")
	if err := os.WriteFile(keyed, []byte(`{}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(slugDir, "latest-session.jsonl")
	if err := os.WriteFile(fallback, []byte(`{}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := DiscoverPath([]string{base}, "claude/tmux-cli", workDir, "gc-123")
	if got != keyed {
		t.Fatalf("DiscoverPath() = %q, want %q", got, keyed)
	}
}

func TestDiscoverFallbackPathUsesClaudeLatestSession(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "claude-project")
	slugDir := filepath.Join(base, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(slugDir, "other-session.jsonl")
	if err := os.WriteFile(other, []byte(`{}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(slugDir, "latest-session.jsonl")
	if err := os.WriteFile(fallback, []byte(`{}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := DiscoverFallbackPath([]string{base}, "claude/tmux-cli", workDir, "gc-123")
	if got != fallback {
		t.Fatalf("DiscoverFallbackPath() = %q, want %q", got, fallback)
	}
}

func TestDiscoverFallbackPathUsesNewestClaudeLatestSessionAcrossAliases(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only /tmp <-> /private/tmp Claude project path alias")
	}

	base := t.TempDir()
	storedWorkDir := "/tmp/gcac/gctutenv-123/home/my-city"
	providerWorkDir := "/private/tmp/gcac/gctutenv-123/home/my-city"
	rawSlugDir := filepath.Join(base, sessionlog.ProjectSlug(storedWorkDir))
	aliasSlugDir := filepath.Join(base, sessionlog.ProjectSlug(providerWorkDir))
	for _, dir := range []string{rawSlugDir, aliasSlugDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	storedFallback := filepath.Join(rawSlugDir, "latest-session.jsonl")
	if err := os.WriteFile(storedFallback, []byte(`{}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(storedFallback, past, past); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(aliasSlugDir, "latest-session.jsonl")
	if err := os.WriteFile(want, []byte(`{}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := DiscoverFallbackPath([]string{base}, "claude/tmux-cli", storedWorkDir, "gc-123")
	if got != want {
		t.Fatalf("DiscoverFallbackPath() = %q, want newest fallback %q", got, want)
	}
}

func TestDiscoverPathCodexIgnoresGCSessionID(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "codex-project")

	slugDir := filepath.Join(base, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slugDir, "gc-123.jsonl"), []byte(`{}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(map[string]any{
		"type": "session_meta",
		"payload": map[string]string{
			"cwd": workDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	codexRoot := filepath.Join(base, "sessions")
	codexDir := filepath.Join(codexRoot, "2026", "04", "18")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	codexPath := filepath.Join(codexDir, "session.jsonl")
	if err := os.WriteFile(codexPath, append(payload, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	got := DiscoverPath([]string{codexRoot}, "codex/tmux-cli", workDir, "gc-123")
	if got != codexPath {
		t.Fatalf("DiscoverPath() = %q, want %q", got, codexPath)
	}
}

func TestDiscoverPathClaudeDoesNotScanCodexFallback(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "claude-project")

	payload, err := json.Marshal(map[string]any{
		"type": "session_meta",
		"payload": map[string]string{
			"cwd": workDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	codexRoot := filepath.Join(base, "sessions")
	codexDir := filepath.Join(codexRoot, "2026", "04", "18")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "session.jsonl"), append(payload, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	got := DiscoverPath([]string{codexRoot}, "claude/tmux-cli", workDir, "")
	if got != "" {
		t.Fatalf("DiscoverPath() = %q, want no Codex fallback for explicit Claude provider", got)
	}
}

func TestSupportsIDLookup(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{provider: "claude/tmux-cli", want: true},
		{provider: "codex/tmux-cli", want: false},
		{provider: "gemini/tmux-cli", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := SupportsIDLookup(tt.provider); got != tt.want {
				t.Fatalf("SupportsIDLookup(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}
