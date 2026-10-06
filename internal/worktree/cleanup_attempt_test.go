package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCleanupRefusesStaleAttemptAgainstReprovisionedWorktree is the regression
// test for ownership that identified the slot rather than the occupant.
//
// Bead, owner, and generation are all reproduced exactly by re-provisioning the
// same bead at the same path, so a cleanup request built from a finished attempt
// matched a live successor and removed somebody else's workspace. Binding the
// request to the attempt id the creating Ensure returned is what makes the two
// distinguishable.
func TestCleanupRefusesStaleAttemptAgainstReprovisionedWorktree(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-slot")
	spec := managedSpec(repo, root, wt, "work/gc-slot", base)

	first, err := Ensure(spec)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	stale := spec
	stale.AttemptID = first.Provenance.AttemptID
	if _, err := Cleanup(stale); err != nil {
		t.Fatalf("Cleanup of the attempt that created the worktree: %v", err)
	}

	// The same bead is re-provisioned at the same path: identical spec, so
	// identical bead, owner, and generation. Only the attempt id differs.
	second, err := Ensure(spec)
	if err != nil {
		t.Fatalf("re-provisioning Ensure: %v", err)
	}
	if second.Provenance.AttemptID == stale.AttemptID {
		t.Fatalf("re-provisioning reused attempt id %q, so the two workspaces are indistinguishable",
			stale.AttemptID)
	}

	report, cleanupErr := Cleanup(stale)
	if cleanupErr == nil {
		t.Fatal("Cleanup with a finished attempt id removed a re-provisioned workspace, want refusal")
	}
	if !report.CleanupPending || report.Error == nil || report.Error.Code != CleanupErrorStaleAttempt {
		t.Fatalf("Cleanup report = %+v, want structured stale-attempt refusal", report)
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("Cleanup removed the re-provisioned worktree: %v", statErr)
	}

	// The live attempt is still authorized, so the binding narrows the request
	// rather than wedging the path.
	live := spec
	live.AttemptID = second.Provenance.AttemptID
	if _, err := Cleanup(live); err != nil {
		t.Fatalf("Cleanup with the live attempt id: %v", err)
	}
}

// TestPathLockKeyFollowsSymlinkedAncestors covers two specs that name one
// workspace through different paths. A lock keyed on the literal path hands
// each caller its own lock file, which excludes nothing.
func TestPathLockKeyFollowsSymlinkedAncestors(t *testing.T) {
	repo, _ := initTestRepo(t)
	realRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Both leaves are checked: one whose parent exists, and one several levels
	// below a parent that does not exist yet. The second is the ordinary case
	// on first use, since creation makes the intermediate directories, and a
	// resolver that gives up when the parent is missing would key those two
	// callers to different lock files.
	for _, leaf := range []string{"wt", filepath.Join("not", "yet", "wt")} {
		viaReal, err := lockFilePath(repo, filepath.Join(realRoot, leaf))
		if err != nil {
			t.Fatalf("lockFilePath(real, %q): %v", leaf, err)
		}
		viaAlias, err := lockFilePath(repo, filepath.Join(alias, leaf))
		if err != nil {
			t.Fatalf("lockFilePath(alias, %q): %v", leaf, err)
		}
		if viaReal != viaAlias {
			t.Errorf("workspace %q locked two files:\n  via real  = %s\n  via alias = %s", leaf, viaReal, viaAlias)
		}
	}
}

// TestCleanupDoesNotLeaveALockFileBehind keeps the serialization lock out of
// the workspace root. The lock has to outlive the removal it guards, so it
// cannot live inside the workspace, and nothing deletes it afterwards; parking
// it beside the workspace would leave a permanent file per provisioned bead in
// a directory callers list expecting only workspaces.
func TestCleanupDoesNotLeaveALockFileBehind(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-lockfile")
	spec := managedSpec(repo, root, wt, "work/gc-lockfile", base)

	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	spec.AttemptID = rep.Provenance.AttemptID
	if _, err := Cleanup(spec); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		t.Errorf("root retains %q after ensure+cleanup, want an empty root", entry.Name())
	}
}

// TestConcurrentEnsureNeverDestroysTheWinnersWorkspace is the regression test
// for a lock that covered the write but not the observation the write was
// decided from.
//
// Ensure chooses whether to create from state it read before locking: is the
// path absent, does the branch exist. Serializing only the create makes the
// loser's `git worktree add` run strictly AFTER the winner's has succeeded,
// against a path the loser still believes is free. The add fails, and the
// rollback for that failure removes whatever worktree is registered at the
// path, which is now the winner's. The winner has already been told it holds a
// valid workspace.
//
// The assertion does not depend on which caller wins: exactly one must report
// Created, the other must report an already-ensured success, and the workspace
// must exist at the end.
func TestConcurrentEnsureNeverDestroysTheWinnersWorkspace(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-contended")
	spec := managedSpec(repo, root, wt, "work/gc-contended", base)

	type outcome struct {
		report Report
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			rep, err := Ensure(spec)
			results <- outcome{rep, err}
		}()
	}
	close(start)

	created := 0
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Errorf("Ensure under contention failed: %v", got.err)
			continue
		}
		if got.report.Created {
			created++
		}
	}
	if t.Failed() {
		return
	}
	if created != 1 {
		t.Fatalf("Created reported by %d of 2 concurrent Ensures, want exactly 1", created)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("workspace missing after contended Ensure: %v", err)
	}
	if _, err := Verify(spec); err != nil {
		t.Fatalf("workspace does not verify after contended Ensure: %v", err)
	}
}

// TestRollbackAttemptValidatesBeforeTouchingAnything covers the boundary an
// operation crosses when it acquires a resource before checking its inputs.
//
// git resolves an empty working directory against the calling process's own
// cwd, so an incomplete spec does not fail inertly: it walks up to whatever
// repository the caller happens to be standing in, writes there, and reports
// only the validation error. The test stands the process in a repository of
// its own so the damage is observable instead of landing in the developer's
// checkout.
func TestRollbackAttemptValidatesBeforeTouchingAnything(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-unvalidated")
	spec := managedSpec(repo, root, wt, "work/gc-unvalidated", base)

	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	ambient, _ := initTestRepo(t)
	t.Chdir(ambient)
	ambientGitDir := runGit(t, ambient, "rev-parse", "--absolute-git-dir")
	before := snapshotTree(t, ambientGitDir)

	incomplete := spec
	incomplete.RepoDir = ""
	if err := RollbackAttempt(incomplete, rep); err == nil {
		t.Fatal("RollbackAttempt accepted a spec with no repo dir")
	}

	if after := snapshotTree(t, ambientGitDir); strings.Join(after, "\x00") != strings.Join(before, "\x00") {
		t.Fatalf("rollback with an invalid spec wrote into the ambient repository:\n  before=%v\n  after=%v", before, after)
	}
}
