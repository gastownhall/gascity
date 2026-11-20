package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexHooksOverlaySrc seeds a provider overlay dir with a codex hooks file
// (mergeable) and a non-mergeable sibling under per-provider/codex/, returning
// the overlay source root.
func codexHooksOverlaySrc(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	codexDir := filepath.Join(src, "per-provider", "codex", ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("mkdir codex overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "hooks.json"), []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("write codex hooks overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "per-provider", "codex", "AGENTS.codex.md"), []byte("codex"), 0o644); err != nil {
		t.Fatalf("write codex sibling overlay: %v", err)
	}
	return src
}

// TestStageProviderOverlayDirStagesCodexHooks locks the no-regression contract
// (invariant #3 / #2): the runtime task-worktree path (plain
// StageProviderOverlayDir, used by StageSessionWorkDir) still writes the codex
// hook file, which is the only hook source for live task sessions.
func TestStageProviderOverlayDirStagesCodexHooks(t *testing.T) {
	t.Parallel()

	src := codexHooksOverlaySrc(t)
	workDir := t.TempDir()

	if err := StageProviderOverlayDir(src, workDir, []string{"codex"}, nil); err != nil {
		t.Fatalf("StageProviderOverlayDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".codex", "hooks.json")); err != nil {
		t.Fatalf("runtime path must stage .codex/hooks.json (codex live-session hook source): %v", err)
	}
}

// TestStageSessionWorkDirStagesFunctionalCodexHooks is the no-regression test
// (invariant #2) at the session-staging boundary: StageSessionWorkDir, invoked
// on every codex task-session Start without an ownership hint, must still write
// a functional .codex/hooks.json (SessionStart present).
func TestStageSessionWorkDirStagesFunctionalCodexHooks(t *testing.T) {
	t.Parallel()

	src := codexHooksOverlaySrc(t)
	workDir := t.TempDir()

	if err := StageSessionWorkDir(Config{
		WorkDir:         workDir,
		ProviderName:    "codex",
		PackOverlayDirs: []string{src},
	}); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workDir, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("codex task worktree missing .codex/hooks.json (P1 regression): %v", err)
	}
	if !strings.Contains(string(data), "SessionStart") {
		t.Fatalf("staged codex hooks not functional, want SessionStart: %s", data)
	}
}

func TestStageSessionWorkDirPreservesReconcilerOwnedCodexHooks(t *testing.T) {
	t.Parallel()

	src := codexHooksOverlaySrc(t)
	geminiPath := filepath.Join(src, ".gemini", "settings.json")
	if err := os.MkdirAll(filepath.Dir(geminiPath), 0o755); err != nil {
		t.Fatalf("mkdir unrelated gemini overlay: %v", err)
	}
	if err := os.WriteFile(geminiPath, []byte(`{"hooks":{"BeforeAgent":[]}}`), 0o644); err != nil {
		t.Fatalf("write unrelated gemini overlay: %v", err)
	}
	workDir := t.TempDir()
	hookPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical codex hooks: %v", err)
	}
	canonical := []byte(`{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"gc --city /city prime"}]}]}}`)
	if err := os.WriteFile(hookPath, canonical, 0o644); err != nil {
		t.Fatalf("write canonical codex hooks: %v", err)
	}

	if err := StageSessionWorkDir(Config{
		WorkDir:                       workDir,
		ProviderName:                  "codex",
		PackOverlayDirs:               []string{src},
		ReconcilerOwnedMergeablePaths: []string{filepath.Join(".codex", "hooks.json")},
		CopyFiles: []CopyEntry{{
			Src: hookPath, RelDst: filepath.Join(".codex", "hooks.json"), Probed: true,
			ContentHash: HashPathContent(hookPath),
		}},
	}); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}
	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read canonical codex hooks: %v", err)
	}
	if string(got) != string(canonical) {
		t.Fatalf("reconciler-owned hooks changed during runtime staging:\ngot:  %s\nwant: %s", got, canonical)
	}
	if _, err := os.Stat(filepath.Join(workDir, "AGENTS.codex.md")); err != nil {
		t.Fatalf("non-mergeable sibling should still stage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".gemini", "settings.json")); err != nil {
		t.Fatalf("unowned mergeable sibling should still stage: %v", err)
	}
}
