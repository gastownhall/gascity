package transcript

import (
	"crypto/md5"
	"encoding/hex"
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

func TestDiscoverPathKimiPrefersSessionKey(t *testing.T) {
	base := t.TempDir()
	workDir := "/tmp/gascity/phase1/kimi"
	workHash := md5Hex(workDir)
	keyed := filepath.Join(base, "sessions", workHash, "session-key", "context.jsonl")
	if err := os.MkdirAll(filepath.Dir(keyed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyed, []byte(`{"role":"user","content":"keyed"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(base, "sessions", workHash, "newer-session", "context.jsonl")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte(`{"role":"user","content":"newer"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(keyed, past, past); err != nil {
		t.Fatal(err)
	}

	got := DiscoverPath([]string{base}, "kimi/tmux-cli", workDir, "session-key")
	if !samePath(got, keyed) {
		t.Fatalf("DiscoverPath() = %q, want keyed Kimi transcript %q", got, keyed)
	}
}

func samePath(a, b string) bool {
	if a == b {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}

func TestDiscoverPathKimiSessionKeyMissDoesNotUseNewestWorkdirTranscript(t *testing.T) {
	base := t.TempDir()
	workDir := "/tmp/gascity/phase1/kimi"
	workHash := md5Hex(workDir)
	other := filepath.Join(base, "sessions", workHash, "newer-session", "context.jsonl")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte(`{"role":"user","content":"newer"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := DiscoverPath([]string{base}, "kimi/tmux-cli", workDir, "missing-session")
	if got != "" {
		t.Fatalf("DiscoverPath() = %q, want empty on missing Kimi session key", got)
	}
}

func TestDiscoverPathPiPrefersProviderSessionID(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "pi-project")

	target := filepath.Join(base, "target.jsonl")
	other := filepath.Join(base, "other.jsonl")
	for _, item := range []struct {
		path string
		id   string
	}{
		{target, "target-session"},
		{other, "other-session"},
	} {
		body := `{"type":"session","id":"` + item.id + `","cwd":"` + filepath.ToSlash(workDir) + `"}`
		if err := os.WriteFile(item.path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", item.path, err)
		}
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(other, future, future); err != nil {
		t.Fatal(err)
	}

	got := DiscoverPath([]string{base}, "pi/tmux-cli", workDir, "target-session")
	if got != target {
		t.Fatalf("DiscoverPath() = %q, want %q", got, target)
	}
}

func TestDiscoverPathAntigravityFallsBackForProvisionalGCSessionID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	fixtureRoot := t.TempDir()
	brainRoot := filepath.Join(fixtureRoot, "brain")
	workDir := filepath.Join(t.TempDir(), "antigravity-project")
	convID := "750fa972-4c56-4215-99b9-893382aee2b4"
	transcriptPath := filepath.Join(brainRoot, convID, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("mkdir transcript: %v", err)
	}
	if err := os.WriteFile(transcriptPath, []byte(`{"step_index":0,"type":"USER_INPUT","content":"hello"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	cachePath := filepath.Join(fixtureRoot, "cache", "last_conversations.json")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	cache, err := json.Marshal(map[string]string{workDir: convID})
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(cachePath, cache, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	got := DiscoverPath([]string{brainRoot}, "antigravity/tmux-cli", workDir, "gc-1")
	if got != transcriptPath {
		t.Fatalf("DiscoverPath() = %q, want %q", got, transcriptPath)
	}
	gotFallback := DiscoverFallbackPath([]string{brainRoot}, "antigravity/tmux-cli", workDir, "gc-1")
	if gotFallback != transcriptPath {
		t.Fatalf("DiscoverFallbackPath() = %q, want %q", gotFallback, transcriptPath)
	}
	gotExplicitMiss := DiscoverPath([]string{brainRoot}, "antigravity/tmux-cli", workDir, "missing-provider-conversation")
	if gotExplicitMiss != "" {
		t.Fatalf("DiscoverPath() explicit miss = %q, want empty", gotExplicitMiss)
	}
}

func TestDiscoverPathAntigravityPrefersProviderConversationID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	fixtureRoot := t.TempDir()
	brainRoot := filepath.Join(fixtureRoot, "brain")
	workDir := filepath.Join(t.TempDir(), "antigravity-project")
	targetID := "750fa972-4c56-4215-99b9-893382aee2b4"
	fallbackID := "18e4eb9f-1b1d-4dbc-966b-c06e3646f3c4"
	targetPath := filepath.Join(brainRoot, targetID, ".system_generated", "logs", "transcript.jsonl")
	fallbackPath := filepath.Join(brainRoot, fallbackID, ".system_generated", "logs", "transcript.jsonl")
	for _, path := range []string{targetPath, fallbackPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir transcript: %v", err)
		}
		if err := os.WriteFile(path, []byte(`{"step_index":0,"type":"USER_INPUT","content":"hello"}`+"\n"), 0o644); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
	}
	cachePath := filepath.Join(fixtureRoot, "cache", "last_conversations.json")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	cache, err := json.Marshal(map[string]string{workDir: fallbackID})
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(cachePath, cache, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	got := DiscoverPath([]string{brainRoot}, "antigravity/tmux-cli", workDir, targetID)
	if got != targetPath {
		t.Fatalf("DiscoverPath() = %q, want provider conversation path %q", got, targetPath)
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
		{provider: "kimi/tmux-cli", want: true},
		{provider: "opencode/tmux-cli", want: false},
		{provider: "pi/tmux-cli", want: true},
		{provider: "antigravity/tmux-cli", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := SupportsIDLookup(tt.provider); got != tt.want {
				t.Fatalf("SupportsIDLookup(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}

func TestHasKeyedTranscript(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "claude-project")
	slugDir := filepath.Join(base, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keyed := filepath.Join(slugDir, "gc-present.jsonl")
	if err := os.WriteFile(keyed, []byte(`{}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("claude present", func(t *testing.T) {
		exists, probeable := HasKeyedTranscript([]string{base}, "claude/tmux-cli", workDir, "gc-present")
		if !probeable || !exists {
			t.Fatalf("HasKeyedTranscript() = (exists=%v, probeable=%v), want (true, true)", exists, probeable)
		}
	})

	t.Run("claude missing", func(t *testing.T) {
		exists, probeable := HasKeyedTranscript([]string{base}, "claude/tmux-cli", workDir, "gc-missing")
		if !probeable || exists {
			t.Fatalf("HasKeyedTranscript() = (exists=%v, probeable=%v), want (false, true)", exists, probeable)
		}
	})

	t.Run("codex not probeable", func(t *testing.T) {
		// codex resolves transcripts by cwd/date, not a session-id-keyed file,
		// so it must report !probeable regardless of what is on disk.
		_, probeable := HasKeyedTranscript([]string{base}, "codex/tmux-cli", workDir, "gc-present")
		if probeable {
			t.Fatal("HasKeyedTranscript(codex) probeable = true, want false")
		}
	})

	t.Run("unknown provider not probeable", func(t *testing.T) {
		// Unknown/custom providers must not be probed: we cannot assume their
		// on-disk layout, so absence is not a reliable stale-resume signal.
		for _, p := range []string{"true", "openai", ""} {
			if _, probeable := HasKeyedTranscript([]string{base}, p, workDir, "gc-present"); probeable {
				t.Fatalf("HasKeyedTranscript(%q) probeable = true, want false", p)
			}
		}
	})

	t.Run("antigravity present", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		brainRoot := filepath.Join(t.TempDir(), "brain")
		sessionID := "750fa972-4c56-4215-99b9-893382aee2b4"
		targetPath := filepath.Join(brainRoot, sessionID, ".system_generated", "logs", "transcript.jsonl")
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(targetPath, []byte(`{}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		exists, probeable := HasKeyedTranscript([]string{brainRoot}, "antigravity/tmux-cli", "some-workdir", sessionID)
		if !probeable || !exists {
			t.Fatalf("HasKeyedTranscript() = (exists=%v, probeable=%v), want (true, true)", exists, probeable)
		}
	})

	t.Run("antigravity missing", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		brainRoot := filepath.Join(t.TempDir(), "brain")
		sessionID := "750fa972-4c56-4215-99b9-893382aee2b4"
		exists, probeable := HasKeyedTranscript([]string{brainRoot}, "antigravity/tmux-cli", "some-workdir", sessionID)
		if !probeable || exists {
			t.Fatalf("HasKeyedTranscript() = (exists=%v, probeable=%v), want (false, true)", exists, probeable)
		}
	})

	t.Run("empty inputs", func(t *testing.T) {
		if _, probeable := HasKeyedTranscript([]string{base}, "claude/tmux-cli", "", "gc-present"); probeable {
			t.Fatal("empty workDir probeable = true, want false")
		}
		if _, probeable := HasKeyedTranscript([]string{base}, "claude/tmux-cli", workDir, ""); probeable {
			t.Fatal("empty sessionKey probeable = true, want false")
		}
	})
}

func md5Hex(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}
