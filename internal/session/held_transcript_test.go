package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// heldCodexInfo is a keyed Codex session whose rollout starts at startedAt.
func heldCodexInfo(workDir, key string, startedAt time.Time) Info {
	return Info{
		ID:         "gc-held",
		Provider:   "codex",
		WorkDir:    workDir,
		SessionKey: key,
		CreatedAt:  startedAt.Add(-time.Minute),
		LastWokeAt: startedAt.Add(time.Minute).Format(time.RFC3339Nano),
	}
}

// A held rollout under a home the search roots never name is the session's
// transcript when its provider's keyed lookup, rooted at one of the file's
// own ancestors, resolves the session key to it; nothing else the process
// holds is, and two held copies of the conversation are refused.
func TestResolveHeldTranscriptPath(t *testing.T) {
	const (
		key   = "019e9966-aaaa-7000-8000-26a2dd7e15b3"
		other = "019e9966-bbbb-7000-8000-26a2dd7e15b3"
	)
	workDir := "/data/projects/held-codex"
	startedAt := time.Date(2026, 11, 8, 9, 30, 0, 0, time.UTC)

	t.Run("the held rollout of the key", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "codex-city-maintainer", "sessions")
		want := writeCodexRolloutForAnchor(t, home, workDir, key, startedAt)
		held := []string{"/dev/pts/3", workDir, filepath.Join(t.TempDir(), "unrelated.log"), want}
		if got := ResolveHeldTranscriptPath(heldCodexInfo(workDir, key, startedAt), held, ""); got != want {
			t.Fatalf("ResolveHeldTranscriptPath = %q, want %q", got, want)
		}
	})
	t.Run("another conversation's held rollout", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "sessions")
		foreign := writeCodexRolloutForAnchor(t, home, workDir, other, startedAt)
		if got := ResolveHeldTranscriptPath(heldCodexInfo(workDir, key, startedAt), []string{foreign}, ""); got != "" {
			t.Fatalf("ResolveHeldTranscriptPath = %q, want none: the held rollout records another conversation", got)
		}
	})
	t.Run("a rollout named for the key in another work directory", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "sessions")
		elsewhere := writeCodexRolloutForAnchor(t, home, "/data/projects/elsewhere", key, startedAt)
		if got := ResolveHeldTranscriptPath(heldCodexInfo(workDir, key, startedAt), []string{elsewhere}, ""); got != "" {
			t.Fatalf("ResolveHeldTranscriptPath = %q, want none: the keyed lookup's own rule refuses it", got)
		}
	})
	t.Run("two held copies of the conversation", func(t *testing.T) {
		a := writeCodexRolloutForAnchor(t, filepath.Join(t.TempDir(), "sessions"), workDir, key, startedAt)
		b := writeCodexRolloutForAnchor(t, filepath.Join(t.TempDir(), "sessions"), workDir, key, startedAt)
		if got := ResolveHeldTranscriptPath(heldCodexInfo(workDir, key, startedAt), []string{a, b}, ""); got != "" {
			t.Fatalf("ResolveHeldTranscriptPath = %q, want none for two copies", got)
		}
	})
	t.Run("the same rollout held twice", func(t *testing.T) {
		want := writeCodexRolloutForAnchor(t, filepath.Join(t.TempDir(), "sessions"), workDir, key, startedAt)
		if got := ResolveHeldTranscriptPath(heldCodexInfo(workDir, key, startedAt), []string{want, want}, ""); got != want {
			t.Fatalf("ResolveHeldTranscriptPath = %q, want %q", got, want)
		}
	})
	t.Run("a keyless session", func(t *testing.T) {
		want := writeCodexRolloutForAnchor(t, filepath.Join(t.TempDir(), "sessions"), workDir, key, startedAt)
		info := heldCodexInfo(workDir, "", startedAt)
		if got := ResolveHeldTranscriptPath(info, []string{want}, ""); got != "" {
			t.Fatalf("ResolveHeldTranscriptPath = %q, want none without a key", got)
		}
	})
}

// The held lookup is the provider's own keyed lookup, not a Codex rule: a
// generic keyed transcript held open outside the search roots resolves too.
func TestResolveHeldTranscriptPathUsesTheProvidersKeyedLookup(t *testing.T) {
	workDir := "/data/projects/held-claude"
	const key = "019e9966-eeee-7000-8000-26a2dd7e15b3"
	slugDir := filepath.Join(t.TempDir(), "projects", sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(slugDir, key+".jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := Info{ID: "gc-held", Provider: "custom-claude-provider", ProviderKind: "claude", WorkDir: workDir, SessionKey: key}
	if got := ResolveHeldTranscriptPath(info, []string{want}, ""); got != want {
		t.Fatalf("ResolveHeldTranscriptPath = %q, want %q", got, want)
	}
}
