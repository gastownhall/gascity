package tmux

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestStageStartFilesSurfacesKiroPreservationWarning(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()

	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(fallbackInstructions), 0o755); err != nil {
		t.Fatalf("mkdir Kiro overlay: %v", err)
	}
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("write Kiro fallback instructions: %v", err)
	}
	projectInstructions := filepath.Join(workDir, "AGENTS.md")
	if err := os.WriteFile(projectInstructions, []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("write project instructions: %v", err)
	}

	var warnings bytes.Buffer
	err := stageStartFiles(runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, &warnings)
	if err != nil {
		t.Fatalf("stageStartFiles: %v", err)
	}
	if got := warnings.String(); !strings.Contains(got, "overlay: preserving existing") || !strings.Contains(got, "AGENTS.md") {
		t.Fatalf("warnings = %q, want Kiro preservation warning", got)
	}
	data, err := os.ReadFile(projectInstructions)
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(data) != "project instructions" {
		t.Fatalf("AGENTS.md = %q, want project instructions preserved", string(data))
	}
}

func TestStageStartFilesKeepsScaffoldOutOfSpawnerCWD(t *testing.T) {
	root := t.TempDir()
	sharedWorktree := filepath.Join(root, "shared-builder")
	beadSlug := "ga-ajw1no-1-as-a-maintainer-i-can-reproduce-stray-session-scaffold-leakage"
	leakedWorkDir := filepath.Join(sharedWorktree, beadSlug)
	workDir := filepath.Join(root, "city", ".gc", "worktrees", "gascity", "builder", beadSlug)
	packOverlay := filepath.Join(root, "city", "packs", "core", "overlay")

	writeTmuxScaffoldFixture(t, filepath.Join(packOverlay, ".claude", "skills", "triage", "SKILL.md"), "---\nname: triage\n---\n")
	writeTmuxScaffoldFixture(t, filepath.Join(packOverlay, ".codex", "hooks.json"), `{"hooks":{"SessionStart":[]}}`+"\n")
	writeTmuxScaffoldFixture(t, filepath.Join(packOverlay, ".gc", "settings.json"), "{}\n")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", workDir, err)
	}
	if err := os.MkdirAll(sharedWorktree, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", sharedWorktree, err)
	}
	t.Chdir(sharedWorktree)

	var warnings bytes.Buffer
	err := stageStartFiles(runtime.Config{
		WorkDir:             workDir,
		ProviderName:        "codex",
		ProviderOverlayName: "codex",
		PackOverlayDirs:     []string{packOverlay},
	}, &warnings)
	if err != nil {
		t.Fatalf("stageStartFiles: %v", err)
	}

	for _, rel := range []string{
		filepath.Join(".claude", "skills", "triage", "SKILL.md"),
		filepath.Join(".codex", "hooks.json"),
	} {
		if _, err := os.Stat(filepath.Join(workDir, rel)); err != nil {
			t.Errorf("target scaffold %s missing under workdir %q: %v", rel, workDir, err)
		}
	}
	// A top-level .gc/ in the overlay source is a runtime mirror and must never
	// be staged into a session workdir (overlay.skipRuntimeMirror). The session's
	// own .gc/settings.json is staged separately through the hook-file path, not
	// copied verbatim from the pack overlay.
	if _, err := os.Stat(filepath.Join(workDir, ".gc", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("overlay .gc runtime mirror must not be staged under workdir %q (stat err = %v)", workDir, err)
	}
	if _, err := os.Stat(leakedWorkDir); err == nil {
		t.Fatalf("shared cwd contains stray bead-slug scaffold directory %q; scaffold must stay under %q", leakedWorkDir, workDir)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat leaked workdir %q: %v", leakedWorkDir, err)
	}
}

func TestStageStartFilesPreservesReconcilerOwnedHooksAndStagesSibling(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()
	overlayCodexDir := filepath.Join(packOverlay, "per-provider", "codex", ".codex")
	if err := os.MkdirAll(overlayCodexDir, 0o755); err != nil {
		t.Fatalf("mkdir codex overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overlayCodexDir, "hooks.json"), []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"gc prime"}]}]}}`), 0o644); err != nil {
		t.Fatalf("write overlay hooks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(packOverlay, "per-provider", "codex", "AGENTS.codex.md"), []byte("codex runtime guidance"), 0o644); err != nil {
		t.Fatalf("write overlay sibling: %v", err)
	}

	hookPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir canonical hooks: %v", err)
	}
	canonical := []byte(`{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"gc --city /city prime"}]}]}}`)
	if err := os.WriteFile(hookPath, canonical, 0o644); err != nil {
		t.Fatalf("write canonical hooks: %v", err)
	}

	err := stageStartFiles(runtime.Config{
		WorkDir:                       workDir,
		ProviderName:                  "codex",
		ProviderOverlayName:           "codex",
		PackOverlayDirs:               []string{packOverlay},
		ReconcilerOwnedMergeablePaths: []string{filepath.Join(".codex", "hooks.json")},
		CopyFiles: []runtime.CopyEntry{{
			Src: hookPath, RelDst: filepath.Join(".codex", "hooks.json"), Probed: true,
			ContentHash: runtime.HashPathContent(hookPath),
		}},
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("stageStartFiles: %v", err)
	}
	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read canonical hooks: %v", err)
	}
	if string(got) != string(canonical) {
		t.Fatalf("reconciler-owned hooks changed during tmux startup:\ngot:  %s\nwant: %s", got, canonical)
	}
	if _, err := os.Stat(filepath.Join(workDir, "AGENTS.codex.md")); err != nil {
		t.Fatalf("non-mergeable sibling should still stage: %v", err)
	}
}

func TestStageStartFilesFailsWhenOwnedSelfCopyIsMissing(t *testing.T) {
	workDir := t.TempDir()
	hookPath := filepath.Join(workDir, ".codex", "hooks.json")
	err := stageStartFiles(runtime.Config{
		WorkDir:                       workDir,
		ReconcilerOwnedMergeablePaths: []string{filepath.Join(".codex", "hooks.json")},
		CopyFiles: []runtime.CopyEntry{{
			Src: hookPath, RelDst: filepath.Join(".codex", "hooks.json"), Probed: true,
			ContentHash: "missing-but-expected",
		}},
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "reconciler-owned copy_file") {
		t.Fatalf("stageStartFiles error = %v, want missing owned self-copy failure", err)
	}
}

func writeTmuxScaffoldFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
