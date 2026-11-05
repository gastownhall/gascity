package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestModuleGoFilesExcludesUntrackedNestedWorktree pins the false-fail this
// bead exists to close (ga-5em): a nested sibling git worktree registered
// under the scanned root — the common gitignored worktrees/<bead> pool-slot
// pattern — must never contribute duplicate source to the census/boundary
// walk. Its files live in the nested worktree's own git index, never in this
// tree's, so a walk scoped to this tree's tracked files must not see them.
func TestModuleGoFilesExcludesUntrackedNestedWorktree(t *testing.T) {
	root := t.TempDir()
	runModuleScanGit(t, root, "init", "-q")
	runModuleScanGit(t, root, "config", "user.email", "fixture@example.com")
	runModuleScanGit(t, root, "config", "user.name", "fixture")

	writeModuleScanFile(t, root, "tracked.go", "package fixture\n")
	runModuleScanGit(t, root, "add", "tracked.go")

	writeModuleScanFile(t, root, filepath.Join("worktrees", "sibling", "tracked.go"), "package fixture\n")

	got := moduleGoFiles(t, root)
	if len(got) != 1 || got[0] != "tracked.go" {
		t.Fatalf("moduleGoFiles(%s) = %v, want exactly [\"tracked.go\"]; the walk must be scoped to git-tracked files so a nested sibling worktree's checkout is never counted", root, got)
	}
}

func runModuleScanGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeModuleScanFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
