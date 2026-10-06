package overlay

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestCopyDirForProvidersWithSkip_SkipsMergeablePerProviderFile locks the core
// of the codex-hooks-drift fix: when the build_desired_state home-dir
// staging path passes an IsMergeablePath skip, the reconciler-owned mergeable
// hook file (.codex/hooks.json, shipped under per-provider/codex/) must NOT be
// staged, so a subsequent hooks.Install pass is the sole writer. Non-mergeable
// siblings and universal files must still be copied.
func TestCopyDirForProvidersWithSkip_SkipsMergeablePerProviderFile(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Universal, non-mergeable file (must always copy).
	mustWriteFile(t, filepath.Join(src, "AGENTS.md"), []byte("universal"), 0o644)
	// Per-provider mergeable file (must be skipped when skip is supplied).
	mustMkdirAll(t, filepath.Join(src, "per-provider", "codex", ".codex"))
	mustWriteFile(t, filepath.Join(src, "per-provider", "codex", ".codex", "hooks.json"), []byte(`{"hooks":{}}`), 0o644)
	// Per-provider non-mergeable file (must still copy).
	mustWriteFile(t, filepath.Join(src, "per-provider", "codex", "AGENTS.codex.md"), []byte("codex"), 0o644)

	skip := func(relPath string, isDir bool) bool {
		return !isDir && IsMergeablePath(relPath)
	}
	if err := CopyDirForProvidersWithSkip(src, dst, []string{"codex"}, skip, io.Discard); err != nil {
		t.Fatalf("CopyDirForProvidersWithSkip: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("mergeable .codex/hooks.json staged despite skip (err=%v); hooks.Install must be sole writer", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "AGENTS.md")); err != nil || string(got) != "universal" {
		t.Fatalf("universal AGENTS.md = %q err=%v, want %q staged", string(got), err, "universal")
	}
	if got, err := os.ReadFile(filepath.Join(dst, "AGENTS.codex.md")); err != nil || string(got) != "codex" {
		t.Fatalf("per-provider non-mergeable AGENTS.codex.md = %q err=%v, want %q staged", string(got), err, "codex")
	}
}

// TestCopyDirForProvidersWithSkip_StillSkipsRuntimeMirrors locks the guard that
// a caller-supplied SkipFunc is strictly additive: supplying a skip must never
// re-enable staging of the runtime `.gc` mirror, in either the universal or the
// per-provider copy phase. TestCopyDirForProviders_SkipsRuntimeMirrors covers
// the nil-skip path; this is its non-nil counterpart.
func TestCopyDirForProvidersWithSkip_StillSkipsRuntimeMirrors(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mustWriteFile(t, filepath.Join(src, "AGENTS.md"), []byte("universal"), 0o644)
	// Universal-phase runtime mirror.
	mustMkdirAll(t, filepath.Join(src, ".gc", "agents", "mayor"))
	mustWriteFile(t, filepath.Join(src, ".gc", "agents", "mayor", "AGENTS.md"), []byte("mirror"), 0o644)
	// Per-provider-phase runtime mirror (flattens to a top-level .gc/).
	mustMkdirAll(t, filepath.Join(src, "per-provider", "codex", ".gc", "worktrees", "polecat"))
	mustWriteFile(t, filepath.Join(src, "per-provider", "codex", ".gc", "worktrees", "polecat", "AGENTS.md"), []byte("mirror"), 0o644)
	mustWriteFile(t, filepath.Join(src, "per-provider", "codex", "AGENTS.codex.md"), []byte("codex"), 0o644)

	skip := func(relPath string, isDir bool) bool {
		return !isDir && IsMergeablePath(relPath)
	}
	if err := CopyDirForProvidersWithSkip(src, dst, []string{"codex"}, skip, io.Discard); err != nil {
		t.Fatalf("CopyDirForProvidersWithSkip: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("runtime .gc mirror staged despite the unconditional guard, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "AGENTS.md")); err != nil {
		t.Fatalf("universal file should still be copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "AGENTS.codex.md")); err != nil {
		t.Fatalf("per-provider file should still be copied: %v", err)
	}
}

// TestCopyDirForProviders_StagesMergeableFileWithoutSkip is the contrast case:
// the runtime task-worktree path passes no skip, so the mergeable codex hook
// file is still staged there (that path is codex's sole hook source for live
// task sessions and must not regress).
func TestCopyDirForProviders_StagesMergeableFileWithoutSkip(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mustMkdirAll(t, filepath.Join(src, "per-provider", "codex", ".codex"))
	mustWriteFile(t, filepath.Join(src, "per-provider", "codex", ".codex", "hooks.json"), []byte(`{"hooks":{}}`), 0o644)

	if err := CopyDirForProviders(src, dst, []string{"codex"}, io.Discard); err != nil {
		t.Fatalf("CopyDirForProviders: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".codex", "hooks.json")); err != nil {
		t.Fatalf("runtime path must still stage .codex/hooks.json: %v", err)
	}
}
