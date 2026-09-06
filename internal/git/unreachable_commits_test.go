package git

import (
	"path/filepath"
	"testing"
)

// pushedWorktreeOnDeletedRemoteBranch builds the exact shape a merged polecat
// worktree ends up in: a worktree on its own branch whose commit was pushed to
// origin and whose remote branch was then deleted (as the merge queue does
// after squash-merging it). It returns the worktree path.
func pushedWorktreeOnDeletedRemoteBranch(t *testing.T) string {
	t.Helper()
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")

	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "commit", "--allow-empty", "-m", "init")
	runGit(t, clone, "push", "origin", "HEAD")

	wtPath := filepath.Join(t.TempDir(), "wt")
	runGit(t, clone, "worktree", "add", "-b", "polecat/ga-1", wtPath)
	runGit(t, wtPath, "config", "user.email", "test@test.com")
	runGit(t, wtPath, "config", "user.name", "Test")
	runGit(t, wtPath, "commit", "--allow-empty", "-m", "work")
	runGit(t, wtPath, "push", "origin", "polecat/ga-1")

	// The merge queue squash-merges the branch and deletes it from origin.
	// The squashed commit on the default branch carries a different SHA, so
	// nothing on any remote-tracking ref reaches this worktree's HEAD anymore.
	runGit(t, clone, "push", "origin", "--delete", "polecat/ga-1")
	runGit(t, clone, "fetch", "--prune", "origin")
	return wtPath
}

// TestHasUnreachableCommits_FalseWhenLocalBranchStillReachesHEAD is the
// regression test for the monotonic worktree leak: after the merge queue
// deletes a merged branch from origin, HasUnpushedCommits reports true forever
// because no remote-tracking ref reaches HEAD. That is the wrong question for a
// caller deciding whether removing the worktree destroys work — `git worktree
// remove` deletes the checkout, not refs/heads, so commits still reachable from
// a local branch survive. HasUnreachableCommits asks the right question and
// must report false here.
func TestHasUnreachableCommits_FalseWhenLocalBranchStillReachesHEAD(t *testing.T) {
	wtPath := pushedWorktreeOnDeletedRemoteBranch(t)
	g := New(wtPath)

	if !g.HasUnpushedCommits() {
		t.Fatal("test precondition: HasUnpushedCommits() = false, want true (no remote ref reaches HEAD after the branch was deleted)")
	}
	has, err := g.HasUnreachableCommitsResult()
	if err != nil {
		t.Fatalf("HasUnreachableCommitsResult() error = %v, want nil", err)
	}
	if has {
		t.Error("HasUnreachableCommitsResult() = true for a worktree whose local branch still reaches HEAD, want false: removing the worktree leaves refs/heads intact")
	}
}

// TestHasUnreachableCommits_TrueWhenDetachedHEADHoldsOrphanCommits pins the
// safety half: a detached HEAD carrying a commit no branch, tag, or remote
// reaches would be orphaned by worktree removal, so the probe must report true.
func TestHasUnreachableCommits_TrueWhenDetachedHEADHoldsOrphanCommits(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")

	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "commit", "--allow-empty", "-m", "init")
	runGit(t, clone, "push", "origin", "HEAD")

	wtPath := filepath.Join(t.TempDir(), "wt")
	runGit(t, clone, "worktree", "add", "--detach", wtPath)
	runGit(t, wtPath, "config", "user.email", "test@test.com")
	runGit(t, wtPath, "config", "user.name", "Test")
	runGit(t, wtPath, "commit", "--allow-empty", "-m", "orphan work")

	g := New(wtPath)
	has, err := g.HasUnreachableCommitsResult()
	if err != nil {
		t.Fatalf("HasUnreachableCommitsResult() error = %v, want nil", err)
	}
	if !has {
		t.Error("HasUnreachableCommitsResult() = false for a detached HEAD holding an unreferenced commit, want true")
	}
}

// TestHasUnreachableCommits_FalseForFullyPushedRepo covers the ordinary clean
// case: HEAD is reachable from its remote-tracking branch, so nothing is at risk.
func TestHasUnreachableCommits_FalseForFullyPushedRepo(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")

	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "commit", "--allow-empty", "-m", "init")
	runGit(t, clone, "push", "origin", "HEAD")

	g := New(clone)
	has, err := g.HasUnreachableCommitsResult()
	if err != nil {
		t.Fatalf("HasUnreachableCommitsResult() error = %v, want nil", err)
	}
	if has {
		t.Error("HasUnreachableCommitsResult() = true for a fully-pushed repo, want false")
	}
}

// TestHasUnreachableCommits_FalseForRepoWithNoRemote pins the deliberate
// difference from HasUnpushedCommits: a repo with no remote at all has every
// commit reachable from its own branch, so nothing is orphaned by removing the
// checkout even though nothing was ever pushed.
func TestHasUnreachableCommits_FalseForRepoWithNoRemote(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)

	if !g.HasUnpushedCommits() {
		t.Fatal("test precondition: HasUnpushedCommits() = false for a repo with no remote, want true")
	}
	has, err := g.HasUnreachableCommitsResult()
	if err != nil {
		t.Fatalf("HasUnreachableCommitsResult() error = %v, want nil", err)
	}
	if has {
		t.Error("HasUnreachableCommitsResult() = true for a repo whose branch reaches every commit, want false")
	}
}

// TestHasUnreachableCommitsResult_ReturnsProbeError pins fail-closed semantics:
// the error surfaces to callers that need the reason, and the bool wrapper
// reports true so a failed probe never reads as "safe to delete".
func TestHasUnreachableCommitsResult_ReturnsProbeError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	g := New(dir)

	if _, err := g.HasUnreachableCommitsResult(); err == nil {
		t.Fatal("HasUnreachableCommitsResult() error = nil, want probe error")
	}
	if !g.HasUnreachableCommits() {
		t.Error("HasUnreachableCommits() should fail closed on probe errors")
	}
}
