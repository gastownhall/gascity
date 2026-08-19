package git

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureAgentWorktreeCreatesDistinctTrees(t *testing.T) {
	repo := initTestRepo(t)
	city := t.TempDir()
	wt1 := filepath.Join(city, ".gc", "worktrees", "demo", "claude-1")
	wt2 := filepath.Join(city, ".gc", "worktrees", "demo", "claude-2")

	if err := EnsureAgentWorktree(repo, wt1, "claude-1"); err != nil {
		t.Fatalf("EnsureAgentWorktree(wt1): %v", err)
	}
	if err := EnsureAgentWorktree(repo, wt2, "claude-2"); err != nil {
		t.Fatalf("EnsureAgentWorktree(wt2): %v", err)
	}

	if !isGitWorktree(wt1) || !isGitWorktree(wt2) {
		t.Fatal("both paths should be git worktrees")
	}
	if samePath(wt1, wt2) {
		t.Fatal("worktrees must be distinct paths")
	}

	// Concurrent-style isolation: edits in wt1 must not appear in wt2.
	marker := filepath.Join(wt1, "agent1-only.txt")
	if err := os.WriteFile(marker, []byte("from-1"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt2, "agent1-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("wt2 should not see wt1 uncommitted file, stat err=%v", err)
	}
}

func TestEnsureAgentWorktreeIdempotent(t *testing.T) {
	repo := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if err := EnsureAgentWorktree(repo, wt, "worker-1"); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := EnsureAgentWorktree(repo, wt, "worker-1"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
}

func TestEnsureAgentWorktreeNonGitRigMakesDirectory(t *testing.T) {
	rig := t.TempDir()
	wt := filepath.Join(t.TempDir(), "slot")
	if err := EnsureAgentWorktree(rig, wt, "worker-1"); err != nil {
		t.Fatalf("EnsureAgentWorktree: %v", err)
	}
	info, err := os.Stat(wt)
	if err != nil || !info.IsDir() {
		t.Fatalf("want directory at %q, err=%v", wt, err)
	}
}

func TestEnsureAgentWorktreeRejectsSharedRigRoot(t *testing.T) {
	repo := initTestRepo(t)
	if err := EnsureAgentWorktree(repo, repo, "worker-1"); err == nil {
		t.Fatal("expected error when work dir equals rig root")
	}
}
