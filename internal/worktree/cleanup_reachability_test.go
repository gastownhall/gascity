package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCleanupRemovesSquashMergedWorktreeAfterRemoteBranchDeleted is the
// regression test for the gate this package previously got wrong.
//
// The normal end state of a merged bead is: the branch is squash-merged into
// the base and then deleted from the remote. From that moment no
// remote-tracking ref reaches the worktree's HEAD ever again, so a gate that
// asks "is this pushed?" latches on permanently and cleanup refuses forever,
// leaking a tree per merged bead. Asking "would removing this orphan commits?"
// is the question that actually protects work, and it answers no here because
// `git worktree remove` deletes the checkout rather than refs/heads (#4816).
func TestCleanupRemovesSquashMergedWorktreeAfterRemoteBranchDeleted(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-merged")
	spec := managedSpec(repo, root, wt, "work/gc-merged", base)
	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	spec.AttemptID = rep.Provenance.AttemptID

	// The end state after a squash-merge with the remote branch deleted: the
	// worktree HEAD is contained in the base, and NO remote-tracking ref
	// reaches it. The repo deliberately has no remote refs, which is that
	// condition in its simplest form.
	if refs := runGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/remotes"); refs != "" {
		t.Fatalf("precondition: expected no remote-tracking refs, got %q", refs)
	}

	// A push-state gate refuses here forever, because nothing on a remote
	// reaches HEAD. A reachability gate allows it, because the local base
	// branch still reaches HEAD and removing the checkout orphans nothing.
	report, cleanupErr := Cleanup(spec)
	if cleanupErr != nil {
		t.Fatalf("Cleanup refused a merged worktree whose remote branch was deleted: %+v (%v)", report, cleanupErr)
	}
	if !report.Removed || report.CleanupPending {
		t.Fatalf("Cleanup report = %+v, want removed with no pending action", report)
	}
	if _, statErr := os.Stat(wt); !os.IsNotExist(statErr) {
		t.Fatalf("worktree still present after cleanup: %v", statErr)
	}
}

// TestRollbackAttemptRefusesToOrphanCommittedWork covers the data-loss path a
// working-tree check cannot see. An agent that COMMITS leaves a clean status,
// so the dirty-worktree guard passes; only a delete that git can prove is safe
// keeps the commit from being orphaned when the branch ref is removed.
func TestRollbackAttemptRefusesToOrphanCommittedWork(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-committed")
	spec := managedSpec(repo, root, wt, "work/gc-committed", base)
	report, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	runGit(t, wt, "commit", "--allow-empty", "-m", "agent work, committed not pushed")
	committed := runGit(t, wt, "rev-parse", "HEAD")

	status := runGit(t, wt, "status", "--porcelain")
	if status != "" {
		t.Fatalf("precondition: committed work should leave a clean status, got %q", status)
	}

	if err := RollbackAttempt(spec, report); err == nil {
		t.Fatal("RollbackAttempt deleted a branch carrying committed work, want refusal")
	}

	// The commit must still be reachable from the branch.
	if got := runGit(t, repo, "rev-parse", "work/gc-committed"); got != committed {
		t.Fatalf("branch tip = %q, want the committed work %q to survive rollback", got, committed)
	}
}

// provenanceEqual compares two provenance records by value. CreatedAt is a
// pointer so that planned provenance can omit it, which means struct equality
// would compare addresses instead of the recorded timestamps.
func provenanceEqual(a, b Provenance) bool {
	switch {
	case (a.CreatedAt == nil) != (b.CreatedAt == nil):
		return false
	case a.CreatedAt != nil && !a.CreatedAt.Equal(*b.CreatedAt):
		return false
	}
	a.CreatedAt, b.CreatedAt = nil, nil
	return a == b
}
