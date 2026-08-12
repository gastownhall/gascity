package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// writeCodexRolloutForAnchor writes a minimal Codex rollout transcript whose
// session_meta cwd is workDir and whose payload timestamp is startedAt, laid out
// in the YYYY/MM/DD date tree the time-window resolver scans. It returns the
// rollout path.
func writeCodexRolloutForAnchor(t *testing.T, root, workDir, sessionID string, startedAt time.Time) string {
	t.Helper()
	day := startedAt.In(time.Local)
	dayDir := filepath.Join(root, day.Format("2006"), day.Format("01"), day.Format("02"))
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dayDir, "rollout-"+startedAt.UTC().Format("2006-01-02T15-04-05")+"-"+sessionID+".jsonl")
	meta := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"cwd":%q,"timestamp":%q}}`,
		startedAt.Format(time.RFC3339Nano), sessionID, workDir, startedAt.Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveCodexTranscriptBySessionOrderAnchorsOnAwakeStartedAt proves the
// same-workdir Codex fallback anchors each session's transcript window on the
// immutable awake_started_at rather than the later creation_complete_at. Once a
// session sleeps or drains, last_woke_at and pending_create_started_at are
// cleared, so without awake_started_at the resolver falls through to
// creation_complete_at — stamped several seconds after the rollout's
// session_meta timestamp — and filters the true transcript out of the
// [start-2s, end) window, which is exactly the historical session this fallback
// exists to recover.
func TestResolveCodexTranscriptBySessionOrderAnchorsOnAwakeStartedAt(t *testing.T) {
	root := t.TempDir()
	workDir := "/data/projects/myproject"
	const provider = "codex"

	// Target session A rolled out at startA; a later sibling B fixes A's window
	// end. Both are slept: last_woke_at and pending_create_started_at are blank,
	// awake_started_at aligns with the rollout, and creation_complete_at lands 5s
	// later — late enough that a creation_complete_at anchor (windowStart =
	// creation_complete_at - 2s) would exclude the rollout from the window.
	startA := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	startB := startA.Add(30 * time.Second)

	pathA := writeCodexRolloutForAnchor(t, root, workDir, "019e3e8e-3591-7532-a1ef-8b9e882bea2f", startA)
	writeCodexRolloutForAnchor(t, root, workDir, "019e3e8e-ffff-7000-a1ef-8b9e882bea2f", startB)

	sessions := []Info{
		infoFromPersistedBead(sleptCodexSessionBead("sess-a", workDir, provider, startA)),
		infoFromPersistedBead(sleptCodexSessionBead("sess-b", workDir, provider, startB)),
	}

	got := ResolveCodexTranscriptBySessionOrder([]string{root}, provider, workDir, "sess-a", sessions)
	if got != pathA {
		t.Fatalf("ResolveCodexTranscriptBySessionOrder() = %q, want %q (awake_started_at window must include the rollout)", got, pathA)
	}
}

func TestResolveKeyedTranscriptPathCodexUsesExactSessionKey(t *testing.T) {
	root := t.TempDir()
	workDir := "/data/projects/keyed-codex"
	startedAt := time.Date(2026, 7, 15, 14, 30, 0, 0, time.UTC)
	const (
		targetKey = "019e9966-aaaa-7000-8000-26a2dd7e15b3"
		decoyKey  = "019e9966-bbbb-7000-8000-26a2dd7e15b3"
	)

	want := writeCodexRolloutForAnchor(t, root, workDir, targetKey, startedAt)
	writeCodexRolloutForAnchor(t, root, workDir, decoyKey, startedAt.Add(time.Minute))

	info := Info{
		Provider:   "codex",
		WorkDir:    workDir,
		SessionKey: targetKey,
		CreatedAt:  startedAt.Add(-time.Minute),
		LastWokeAt: startedAt.Add(time.Minute).Format(time.RFC3339Nano),
	}

	if got := ResolveKeyedTranscriptPath(info, []string{root}); got != want {
		t.Fatalf("ResolveKeyedTranscriptPath() = %q, want exact keyed rollout %q", got, want)
	}
}

func TestResolveKeyedTranscriptPathsResolvesCodexPageByExactSessionKey(t *testing.T) {
	root := t.TempDir()
	workDir := "/data/projects/keyed-codex-page"
	startedAt := time.Date(2026, 7, 15, 14, 30, 0, 0, time.UTC)
	const (
		firstKey  = "019e9966-1111-7000-8000-26a2dd7e15b3"
		secondKey = "019e9966-2222-7000-8000-26a2dd7e15b3"
	)
	firstPath := writeCodexRolloutForAnchor(t, root, workDir, firstKey, startedAt)
	secondPath := writeCodexRolloutForAnchor(t, root, workDir, secondKey, startedAt.Add(time.Second))

	infos := []Info{
		{ID: "gc-first", ProviderKind: "codex", WorkDir: workDir, SessionKey: firstKey, CreatedAt: startedAt.Add(-time.Minute), LastWokeAt: startedAt.Add(time.Minute).Format(time.RFC3339)},
		{ID: "gc-second", Provider: "remote-openai", BuiltinAncestor: "builtin:codex", WorkDir: workDir, SessionKey: secondKey, CreatedAt: startedAt.Add(-time.Minute), LastWokeAt: startedAt.Add(time.Minute).Format(time.RFC3339)},
		{ID: "gc-keyless", ProviderKind: "codex", WorkDir: workDir, CreatedAt: startedAt},
	}

	got := ResolveKeyedTranscriptPaths(infos, []string{root}, "")
	if got["gc-first"] != firstPath {
		t.Errorf("first path = %q, want %q", got["gc-first"], firstPath)
	}
	if got["gc-second"] != secondPath {
		t.Errorf("second path = %q, want %q", got["gc-second"], secondPath)
	}
	if path, ok := got["gc-keyless"]; ok || path != "" {
		t.Errorf("keyless path = %q, present=%v; want absent", path, ok)
	}
}

func TestResolveKeyedTranscriptPathsUsesFallbackProvider(t *testing.T) {
	root := t.TempDir()
	workDir := "/data/projects/keyed-codex-workspace-default"
	startedAt := time.Date(2026, 7, 15, 14, 30, 0, 0, time.UTC)
	const sessionKey = "019e9966-3333-7000-8000-26a2dd7e15b3"
	want := writeCodexRolloutForAnchor(t, root, workDir, sessionKey, startedAt)
	info := Info{
		ID:         "gc-legacy-provider",
		WorkDir:    workDir,
		SessionKey: sessionKey,
		CreatedAt:  startedAt.Add(-time.Minute),
		LastWokeAt: startedAt.Add(time.Minute).Format(time.RFC3339),
	}

	got := ResolveKeyedTranscriptPaths([]Info{info}, []string{root}, "codex")
	if got[info.ID] != want {
		t.Fatalf("fallback-provider path = %q, want exact Codex rollout %q", got[info.ID], want)
	}
}

func TestResolveKeyedTranscriptPathDoesNotFallBack(t *testing.T) {
	root := t.TempDir()
	workDir := "/data/projects/keyed-no-fallback"
	startedAt := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	const decoyKey = "019e9966-cccc-7000-8000-26a2dd7e15b3"
	writeCodexRolloutForAnchor(t, root, workDir, decoyKey, startedAt)

	base := Info{
		Provider:   "codex",
		WorkDir:    workDir,
		CreatedAt:  startedAt.Add(-time.Minute),
		LastWokeAt: startedAt.Add(time.Minute).Format(time.RFC3339),
	}
	tests := []struct {
		name string
		info Info
	}{
		{
			name: "missing keyed Codex rollout",
			info: func() Info {
				info := base
				info.SessionKey = "019e9966-dddd-7000-8000-26a2dd7e15b3"
				return info
			}(),
		},
		{
			name: "keyless Codex session",
			info: base,
		},
		{
			name: "Gemini has no exact keyed lookup",
			info: Info{
				Provider:   "gemini",
				WorkDir:    workDir,
				SessionKey: decoyKey,
				CreatedAt:  base.CreatedAt,
				LastWokeAt: base.LastWokeAt,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveKeyedTranscriptPath(tt.info, []string{root}); got != "" {
				t.Fatalf("ResolveKeyedTranscriptPath() = %q, want empty (no exact keyed match)", got)
			}
		})
	}
}

func TestResolveKeyedTranscriptPathUsesGenericKeyedDiscovery(t *testing.T) {
	root := t.TempDir()
	workDir := "/data/projects/keyed-claude"
	const sessionKey = "019e9966-eeee-7000-8000-26a2dd7e15b3"
	slugDir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(slugDir, sessionKey+".jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info := Info{
		Provider:     "custom-claude-provider",
		ProviderKind: "claude",
		WorkDir:      workDir,
		SessionKey:   sessionKey,
	}
	if got := ResolveKeyedTranscriptPath(info, []string{root}); got != want {
		t.Fatalf("ResolveKeyedTranscriptPath() = %q, want generic keyed path %q", got, want)
	}
}

// sleptCodexSessionBead builds a same-workdir Codex session bead in the
// slept/drained shape: last_woke_at and pending_create_started_at are cleared,
// awake_started_at pins the rollout start, and creation_complete_at is 5s later.
func sleptCodexSessionBead(id, workDir, provider string, awakeStart time.Time) beads.Bead {
	return beads.Bead{
		ID: id,
		Metadata: map[string]string{
			"work_dir":                  workDir,
			"provider":                  provider,
			"last_woke_at":              "",
			"pending_create_started_at": "",
			"awake_started_at":          awakeStart.Format(time.RFC3339Nano),
			"creation_complete_at":      awakeStart.Add(5 * time.Second).Format(time.RFC3339),
		},
	}
}
