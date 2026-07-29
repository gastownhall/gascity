package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsStrictlyUnderDirNormalizesSymlinkedAncestor pins the containment check
// that made every scheduled Mac Regression run red (ga-xbilek).
//
// reapClosedBeadWorktrees compares two paths that reach it in different forms:
// worktreePath comes from `git worktree list`, which reports canonical paths,
// while wtRoot is built from the configured city path, which may still contain
// a symlinked ancestor. On macOS that is unconditional — every $TMPDIR and /tmp
// path sits under /var -> private/var — so isStrictlyUnderDir saw a "../.."
// escape for a worktree plainly inside the city and skipped every candidate.
// Both the reap list and the protect list came back empty, which is exactly how
// all 16 TestReapClosedBeadWorktrees_* tests and both
// TestCityRuntimeTick_*Reap* tests failed on macOS.
//
// The symlink below plays the role /var -> private/var plays on macOS, so this
// test fails without the normalization on every platform, not just darwin.
func TestIsStrictlyUnderDirNormalizesSymlinkedAncestor(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	// dir as the reaper builds it: through the symlinked ancestor.
	dir := filepath.Join(link, "city", ".gc", "worktrees")
	// path as `git worktree list` reports it: fully resolved.
	resolvedWorktree := filepath.Join(realDir, "city", ".gc", "worktrees", "mrig", "builder", "ga-abc123")
	if err := os.MkdirAll(resolvedWorktree, 0o755); err != nil {
		t.Fatal(err)
	}

	if !isStrictlyUnderDir(dir, resolvedWorktree) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = false, want true: the worktree is inside the city, "+
			"the two arguments only disagree about symlink resolution", dir, resolvedWorktree)
	}
}

// TestIsStrictlyUnderDirStillRejectsEscapes proves the normalization did not
// weaken the guard: a genuinely outside path, and the directory itself, must
// still be rejected. Without this, "fix the false negative" could silently
// become "accept everything", which on this code path authorizes a recursive
// delete.
func TestIsStrictlyUnderDirStillRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "city", ".gc", "worktrees")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "elsewhere", "ga-abc123")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	if isStrictlyUnderDir(dir, outside) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = true, want false: path is outside the worktree root", dir, outside)
	}
	if isStrictlyUnderDir(dir, dir) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = true, want false: dir is not strictly under itself", dir, dir)
	}

	// A symlink that points OUT of the root must be rejected on its resolved
	// target, not accepted on its lexical position inside the root.
	escaping := filepath.Join(dir, "escape")
	if err := os.Symlink(outside, escaping); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if isStrictlyUnderDir(dir, escaping) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = true, want false: symlink resolves outside the worktree root",
			dir, escaping)
	}
}
