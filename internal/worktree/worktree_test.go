package worktree

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/testutil"
)

// runGit runs a git command in dir and fails the test on error. Strips
// repository-locating git env vars so host hooks cannot interfere.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return testutil.RunGit(t, dir, args...)
}

// initTestRepo creates a git repo with one commit and returns its path and
// the name of its initial branch.
func initTestRepo(t *testing.T) (string, string) {
	t.Helper()
	return testutil.InitGitRepo(t)
}

func managedSpec(repo, root, path, branch, base string) Spec {
	return Spec{
		RepoDir:    repo,
		Root:       root,
		Path:       path,
		Branch:     branch,
		Base:       base,
		BeadID:     "gc-test",
		StoreRef:   "gascity",
		Creator:    "test",
		Owner:      "test-owner",
		Generation: "1",
		Lifecycle:  LifecycleActive,
	}
}

// snapshotTree returns every path under dir, relative and sorted. Unlike
// snapshotDir it recurses, so it catches a write anywhere in the tree rather
// than only a new top-level entry.
func snapshotTree(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		found = append(found, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %q: %v", dir, err)
	}
	sort.Strings(found)
	return found
}

func publishRemoteRef(t *testing.T, repo, branch, commit string) {
	t.Helper()
	runGit(t, repo, "update-ref", "refs/remotes/origin/"+branch, commit)
}

// snapshotDir returns the sorted entries of a directory, or nil when it does
// not exist. Used to assert dry-run filesystem purity.
func snapshotDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestVerifyValidWorktree(t *testing.T) {
	repo, base := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", "-b", "feat", wt, base)

	rep, err := Verify(Spec{RepoDir: repo, Path: wt, Branch: "feat"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.Path != wt || rep.Branch != "feat" {
		t.Errorf("report = %+v, want path %q branch feat", rep, wt)
	}
	if len(rep.Head) != 40 {
		t.Errorf("report.Head = %q, want 40-char SHA", rep.Head)
	}
}

func TestVerifyMissingPath(t *testing.T) {
	repo, _ := initTestRepo(t)
	missing := filepath.Join(t.TempDir(), "nope")
	_, err := Verify(Spec{RepoDir: repo, Path: missing, Branch: "feat"})
	if !errors.Is(err, ErrWorktreeMissing) {
		t.Errorf("Verify on missing path: err = %v, want ErrWorktreeMissing", err)
	}
}

func TestVerifyNotAWorktree(t *testing.T) {
	repo, _ := initTestRepo(t)
	plain := t.TempDir()
	_, err := Verify(Spec{RepoDir: repo, Path: plain, Branch: "feat"})
	if err == nil {
		t.Fatal("Verify on plain dir succeeded, want error")
	}
	if errors.Is(err, ErrWorktreeMissing) {
		t.Error("plain existing dir reported as ErrWorktreeMissing; must be a distinct error so Ensure never clobbers it")
	}
}

func TestVerifyWrongBranch(t *testing.T) {
	repo, base := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", "-b", "actual", wt, base)
	_, err := Verify(Spec{RepoDir: repo, Path: wt, Branch: "expected"})
	if err == nil || !strings.Contains(err.Error(), "expected") {
		t.Errorf("Verify wrong branch: err = %v, want branch mismatch mentioning %q", err, "expected")
	}
}

func TestVerifyDetachedHead(t *testing.T) {
	repo, base := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", "-b", "feat", wt, base)
	sha := runGit(t, wt, "rev-parse", "HEAD")
	runGit(t, wt, "checkout", "--detach", sha)
	_, err := Verify(Spec{RepoDir: repo, Path: wt, Branch: "feat"})
	if err == nil {
		t.Fatal("Verify on detached HEAD succeeded, want error")
	}
}

func TestVerifyDifferentRepo(t *testing.T) {
	repoA, _ := initTestRepo(t)
	repoB, baseB := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, repoB, "worktree", "add", "-b", "feat", wt, baseB)
	_, err := Verify(Spec{RepoDir: repoA, Path: wt, Branch: "feat"})
	if err == nil {
		t.Fatal("Verify against wrong repo succeeded, want repository identity error")
	}
}

func TestVerifySubdirOfWorktreeFails(t *testing.T) {
	repo, base := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", "-b", "feat", wt, base)
	sub := filepath.Join(wt, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	_, err := Verify(Spec{RepoDir: repo, Path: sub, Branch: "feat"})
	if err == nil {
		t.Fatal("Verify on worktree subdirectory succeeded, want error")
	}
}

func TestVerifySpecValidation(t *testing.T) {
	repo, _ := initTestRepo(t)
	cases := []Spec{
		{RepoDir: "", Path: "/tmp/x", Branch: "b"},
		{RepoDir: repo, Path: "", Branch: "b"},
		{RepoDir: repo, Path: "/tmp/x", Branch: ""},
		{RepoDir: repo, Path: "relative/path", Branch: "b"},
		{RepoDir: "relative", Path: "/tmp/x", Branch: "b"},
	}
	for i, spec := range cases {
		if _, err := Verify(spec); err == nil {
			t.Errorf("case %d: Verify(%+v) succeeded, want validation error", i, spec)
		}
		if _, err := Ensure(spec); err == nil {
			t.Errorf("case %d: Ensure(%+v) succeeded, want validation error", i, spec)
		}
	}
}

func TestEnsureCreatesNewBranchWorktree(t *testing.T) {
	repo, base := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	baseSHA := runGit(t, repo, "rev-parse", base)

	rep, err := Ensure(Spec{RepoDir: repo, Path: wt, Branch: "feat", Base: base})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !rep.Created || !rep.BranchCreated {
		t.Errorf("report = %+v, want Created=true BranchCreated=true", rep)
	}
	if rep.Head != baseSHA {
		t.Errorf("report.Head = %q, want base SHA %q", rep.Head, baseSHA)
	}
	// Postconditions on disk: attached HEAD on the right branch.
	if got := runGit(t, wt, "symbolic-ref", "HEAD"); got != "refs/heads/feat" {
		t.Errorf("worktree HEAD = %q, want refs/heads/feat", got)
	}
	if got := runGit(t, wt, "rev-parse", "HEAD"); got != baseSHA {
		t.Errorf("worktree HEAD SHA = %q, want %q", got, baseSHA)
	}
}

func TestEnsureIdempotent(t *testing.T) {
	repo, base := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := Ensure(Spec{RepoDir: repo, Path: wt, Branch: "feat", Base: base}); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	rep, err := Ensure(Spec{RepoDir: repo, Path: wt, Branch: "feat", Base: base})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if rep.Created || rep.BranchCreated {
		t.Errorf("second Ensure report = %+v, want Created=false BranchCreated=false", rep)
	}
}

func TestEnsureAttachesExistingBranch(t *testing.T) {
	repo, base := initTestRepo(t)
	runGit(t, repo, "branch", "feat", base)
	tip := runGit(t, repo, "rev-parse", "feat")
	wt := filepath.Join(t.TempDir(), "wt")

	rep, err := Ensure(Spec{RepoDir: repo, Path: wt, Branch: "feat"})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !rep.Created || rep.BranchCreated {
		t.Errorf("report = %+v, want Created=true BranchCreated=false", rep)
	}
	if got := runGit(t, repo, "rev-parse", "feat"); got != tip {
		t.Errorf("attaching moved branch tip from %q to %q", tip, got)
	}
}

func TestEnsureBaseRequiredForNewBranch(t *testing.T) {
	repo, _ := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	_, err := Ensure(Spec{RepoDir: repo, Path: wt, Branch: "feat"})
	if err == nil {
		t.Fatal("Ensure with no base and missing branch succeeded, want error")
	}
	if _, statErr := os.Stat(wt); statErr == nil {
		t.Error("failed Ensure created the worktree path")
	}
}

func TestEnsureUnresolvableBaseFails(t *testing.T) {
	repo, _ := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	_, err := Ensure(Spec{RepoDir: repo, Path: wt, Branch: "feat", Base: "no-such-ref"})
	if err == nil {
		t.Fatal("Ensure with unresolvable base succeeded, want error")
	}
	if _, statErr := os.Stat(wt); statErr == nil {
		t.Error("failed Ensure created the worktree path")
	}
	if out := runGit(t, repo, "branch", "--list", "feat"); out != "" {
		t.Errorf("failed Ensure created branch: %q", out)
	}
}

func TestEnsurePreservesLocalBaseIntent(t *testing.T) {
	// A base of "main" must resolve to the LOCAL main, even when a
	// same-named remote-tracking ref points elsewhere.
	repo, base := initTestRepo(t)
	localSHA := runGit(t, repo, "rev-parse", base)
	runGit(t, repo, "commit", "--allow-empty", "-m", "remote-ahead")
	remoteSHA := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "update-ref", "refs/remotes/origin/"+base, remoteSHA)
	runGit(t, repo, "reset", "--hard", localSHA)

	wt := filepath.Join(t.TempDir(), "wt")
	rep, err := Ensure(Spec{RepoDir: repo, Path: wt, Branch: "feat", Base: base})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if rep.Head != localSHA {
		t.Errorf("Ensure based branch on %q, want LOCAL %s %q (remote was %q)", rep.Head, base, localSHA, remoteSHA)
	}
}

func TestEnsureRefusesExistingNonWorktreePath(t *testing.T) {
	repo, base := initTestRepo(t)
	plain := t.TempDir()
	marker := filepath.Join(plain, "keep.txt")
	if err := os.WriteFile(marker, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Ensure(Spec{RepoDir: repo, Path: plain, Branch: "feat", Base: base})
	if err == nil {
		t.Fatal("Ensure over plain dir succeeded, want error")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Error("Ensure touched contents of a pre-existing non-worktree dir")
	}
}

func TestEnsureDryRunIsPure(t *testing.T) {
	repo, base := initTestRepo(t)
	parent := t.TempDir()
	wt := filepath.Join(parent, "wt")
	beforeParent := snapshotDir(t, parent)
	beforeBranches := runGit(t, repo, "branch", "--list")
	beforeWorktrees := runGit(t, repo, "worktree", "list", "--porcelain")

	rep, err := Ensure(Spec{RepoDir: repo, Path: wt, Branch: "feat", Base: base, DryRun: true})
	if err != nil {
		t.Fatalf("dry-run Ensure: %v", err)
	}
	if rep.Created || rep.BranchCreated {
		t.Errorf("dry-run report = %+v, want Created=false BranchCreated=false", rep)
	}
	if len(rep.Planned) == 0 {
		t.Error("dry-run report has no Planned actions")
	}
	if _, statErr := os.Stat(wt); statErr == nil {
		t.Error("dry-run created the worktree path")
	}
	if got := snapshotDir(t, parent); len(got) != len(beforeParent) {
		t.Errorf("dry-run mutated parent dir: before %v after %v", beforeParent, got)
	}
	if got := runGit(t, repo, "branch", "--list"); got != beforeBranches {
		t.Errorf("dry-run mutated branches: before %q after %q", beforeBranches, got)
	}
	if got := runGit(t, repo, "worktree", "list", "--porcelain"); got != beforeWorktrees {
		t.Errorf("dry-run mutated worktree list: before %q after %q", beforeWorktrees, got)
	}
}

func TestEnsureDryRunOnValidWorktree(t *testing.T) {
	repo, base := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := Ensure(Spec{RepoDir: repo, Path: wt, Branch: "feat", Base: base}); err != nil {
		t.Fatalf("setup Ensure: %v", err)
	}
	rep, err := Ensure(Spec{RepoDir: repo, Path: wt, Branch: "feat", Base: base, DryRun: true})
	if err != nil {
		t.Fatalf("dry-run Ensure on valid worktree: %v", err)
	}
	if rep.Created || len(rep.Planned) != 0 {
		t.Errorf("report = %+v, want no creation and no planned actions", rep)
	}
}

func TestEnsureDryRunStillFailsOnWrongState(t *testing.T) {
	// Dry-run must report the same error a real run would, without mutating.
	repo, base := initTestRepo(t)
	plain := t.TempDir()
	_, err := Ensure(Spec{RepoDir: repo, Path: plain, Branch: "feat", Base: base, DryRun: true})
	if err == nil {
		t.Fatal("dry-run Ensure over plain dir succeeded, want error")
	}
}

func TestRollbackRemovesCreatedState(t *testing.T) {
	repo, base := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g := git.New(repo)
	if err := g.WorktreeAddNewBranch(wt, "doomed", base); err != nil {
		t.Fatalf("WorktreeAddNewBranch: %v", err)
	}
	if err := rollbackCreated(g, wt, "doomed", true); err != nil {
		t.Fatalf("rollbackCreated: %v", err)
	}
	if _, statErr := os.Stat(wt); statErr == nil {
		t.Error("rollback left the worktree path in place")
	}
	if out := runGit(t, repo, "branch", "--list", "doomed"); out != "" {
		t.Errorf("rollback left branch in place: %q", out)
	}
}

func TestRollbackKeepsPreexistingBranch(t *testing.T) {
	repo, base := initTestRepo(t)
	runGit(t, repo, "branch", "keep", base)
	wt := filepath.Join(t.TempDir(), "wt")
	g := git.New(repo)
	if err := g.WorktreeAddExistingBranch(wt, "keep"); err != nil {
		t.Fatalf("WorktreeAddExistingBranch: %v", err)
	}
	if err := rollbackCreated(g, wt, "keep", false); err != nil {
		t.Fatalf("rollbackCreated: %v", err)
	}
	if _, statErr := os.Stat(wt); statErr == nil {
		t.Error("rollback left the worktree path in place")
	}
	if out := runGit(t, repo, "branch", "--list", "keep"); out == "" {
		t.Error("rollback deleted a pre-existing branch")
	}
}

func TestRollbackPreservesDirtyAttemptState(t *testing.T) {
	repo, base := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g := git.New(repo)
	if err := g.WorktreeAddNewBranch(wt, "dirty", base); err != nil {
		t.Fatalf("WorktreeAddNewBranch: %v", err)
	}
	marker := filepath.Join(wt, "uncommitted.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := rollbackCreated(g, wt, "dirty", true); err == nil {
		t.Fatal("rollbackCreated removed a dirty worktree")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep" {
		t.Fatalf("dirty WIP changed: data=%q err=%v", got, err)
	}
	if out := runGit(t, repo, "branch", "--list", "dirty"); out == "" {
		t.Fatal("rollback deleted the branch of a retained dirty worktree")
	}
}

func TestEnsureManagedWorktreePersistsAndVerifiesProvenance(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-test")
	spec := managedSpec(repo, root, wt, "work/gc-test", base)

	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if rep.Provenance == nil {
		t.Fatal("Ensure report has nil provenance")
	}
	// Provenance.Path is contractually the *canonical* spec path:
	// plannedProvenance runs it through canonicalPathAllowMissing so that
	// identity comparison is not defeated by symlinks, exactly as
	// canonicalCommonDir does for RepoIdentity. So the expectation has to be
	// canonicalized by the same helper rather than compared to the raw temp
	// path — on macOS t.TempDir() hands back the /var spelling of a directory
	// whose canonical form is /private/var, and a raw compare reads that
	// contract-honoring value as a mismatch.
	wantPath, err := canonicalPathAllowMissing(wt)
	if err != nil {
		t.Fatalf("canonicalPathAllowMissing(%q): %v", wt, err)
	}
	if rep.Provenance.BeadID != spec.BeadID ||
		rep.Provenance.StoreRef != spec.StoreRef ||
		rep.Provenance.Path != wantPath ||
		rep.Provenance.Branch != spec.Branch ||
		rep.Provenance.BaseRef != base ||
		rep.Provenance.BaseSHA == "" ||
		rep.Provenance.RepoIdentity == "" ||
		rep.Provenance.Creator != spec.Creator ||
		rep.Provenance.Owner != spec.Owner ||
		rep.Provenance.Generation != spec.Generation ||
		rep.Provenance.Lifecycle != LifecycleActive ||
		rep.Provenance.CreatedAt == nil ||
		rep.Provenance.CreatedAt.IsZero() ||
		rep.Provenance.AttemptID == "" {
		t.Fatalf("provenance = %+v, want complete durable identity", rep.Provenance)
	}

	manifest, err := provenanceFilePath(wt)
	if err != nil {
		t.Fatalf("provenanceFilePath: %v", err)
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", manifest, err)
	}
	var stored Provenance
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("Unmarshal provenance: %v", err)
	}
	// Compare by value: CreatedAt is a pointer, so struct equality would
	// compare addresses rather than the timestamps it records.
	if !provenanceEqual(stored, *rep.Provenance) {
		t.Fatalf("stored provenance = %+v, report = %+v", stored, *rep.Provenance)
	}

	verifySpec := spec
	verifySpec.BaseSHA = rep.Provenance.BaseSHA
	verified, err := Verify(verifySpec)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if verified.Provenance == nil || verified.Provenance.AttemptID != rep.Provenance.AttemptID {
		t.Fatalf("verified provenance = %+v, want attempt %q", verified.Provenance, rep.Provenance.AttemptID)
	}
}

func TestVerifyManagedWorktreeRejectsConflictingProvenanceAndPreservesWIP(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-test")
	spec := managedSpec(repo, root, wt, "work/gc-test", base)
	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	marker := filepath.Join(wt, "uncommitted.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"bead", func(s *Spec) { s.BeadID = "gc-other" }, "bead"},
		{"store", func(s *Spec) { s.StoreRef = "other" }, "store"},
		{"owner", func(s *Spec) { s.Owner = "other" }, "owner"},
		{"generation", func(s *Spec) { s.Generation = "2" }, "generation"},
		{"base sha", func(s *Spec) { s.BaseSHA = strings.Repeat("a", 40) }, "base SHA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conflict := spec
			conflict.BaseSHA = rep.Provenance.BaseSHA
			tt.mutate(&conflict)
			if _, err := Verify(conflict); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Verify conflict error = %v, want actionable %q mismatch", err, tt.want)
			}
			if _, err := Ensure(conflict); err == nil {
				t.Fatal("Ensure accepted conflicting existing provenance")
			}
			if got, err := os.ReadFile(marker); err != nil || string(got) != "keep" {
				t.Fatalf("existing WIP changed: data=%q err=%v", got, err)
			}
		})
	}
}

func TestEnsureManagedWorktreeRefusesNonCanonicalOrNestedPath(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()

	t.Run("not direct child of configured root", func(t *testing.T) {
		spec := managedSpec(repo, root, filepath.Join(root, "outer", "inner"), "work/nested", base)
		if _, err := Ensure(spec); err == nil || !strings.Contains(err.Error(), "direct child") {
			t.Fatalf("Ensure nested path error = %v, want configured-root refusal", err)
		}
	})

	t.Run("inside registered worktree", func(t *testing.T) {
		outer := filepath.Join(root, "outer")
		runGit(t, repo, "worktree", "add", "-b", "outer", outer, base)
		spec := managedSpec(repo, outer, filepath.Join(outer, "inner"), "work/inner", base)
		if _, err := Ensure(spec); err == nil || !strings.Contains(err.Error(), "registered worktree") {
			t.Fatalf("Ensure nested registered-worktree error = %v, want refusal", err)
		}
	})
}

func TestRollbackAttemptRemovesOnlyAttemptCreatedState(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-test")
	spec := managedSpec(repo, root, wt, "work/gc-test", base)
	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := RollbackAttempt(spec, rep); err != nil {
		t.Fatalf("RollbackAttempt: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists after rollback: %v", err)
	}
	if out := runGit(t, repo, "branch", "--list", spec.Branch); out != "" {
		t.Fatalf("attempt-created branch still exists: %q", out)
	}

	preexisting := filepath.Join(root, "preexisting")
	runGit(t, repo, "branch", "keep", base)
	preSpec := managedSpec(repo, root, preexisting, "keep", base)
	preRep, err := Ensure(preSpec)
	if err != nil {
		t.Fatalf("Ensure pre-existing branch: %v", err)
	}
	if preRep.BranchCreated {
		t.Fatal("pre-existing branch reported as created")
	}
	if err := RollbackAttempt(preSpec, Report{
		Path: preRep.Path, Branch: preRep.Branch, Provenance: preRep.Provenance,
	}); err == nil {
		t.Fatal("RollbackAttempt accepted a report without Created=true")
	}
	if _, err := os.Stat(preexisting); err != nil {
		t.Fatalf("RollbackAttempt touched pre-existing state: %v", err)
	}
}

func TestRollbackAttemptRefusesWrongAttemptAndDirtyState(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-test")
	spec := managedSpec(repo, root, wt, "work/gc-test", base)
	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	wrongAttempt := rep
	wrongProvenance := *rep.Provenance
	wrongProvenance.AttemptID = "other-attempt"
	wrongAttempt.Provenance = &wrongProvenance
	if err := RollbackAttempt(spec, wrongAttempt); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("RollbackAttempt wrong attempt error = %v, want fenced refusal", err)
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("wrong-attempt rollback removed worktree: %v", statErr)
	}

	marker := filepath.Join(wt, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := RollbackAttempt(spec, rep); err == nil || !strings.Contains(err.Error(), "contains changes") {
		t.Fatalf("RollbackAttempt dirty error = %v, want WIP refusal", err)
	}
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "keep" {
		t.Fatalf("rollback changed dirty WIP: data=%q err=%v", got, readErr)
	}
}

func TestRollbackResultReportsCompleteAndIncompleteRollback(t *testing.T) {
	cause := errors.New("create failed")
	if got := rollbackResult(cause, nil).Error(); !strings.Contains(got, "rolled back") {
		t.Fatalf("rollbackResult complete = %q, want rolled-back evidence", got)
	}
	if got := rollbackResult(cause, errors.New("remove failed")).Error(); !strings.Contains(got, "rollback incomplete") {
		t.Fatalf("rollbackResult incomplete = %q, want incomplete evidence", got)
	}
	if provenanceAttempt(nil) != "" {
		t.Fatal("provenanceAttempt(nil) returned non-empty value")
	}
}

func TestManagedDryRunDoesNotPublishProvenance(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-test")
	spec := managedSpec(repo, root, wt, "work/gc-test", base)
	spec.DryRun = true
	before := snapshotDir(t, root)
	commonDir := runGit(t, repo, "rev-parse", "--absolute-git-dir")
	beforeCommon := snapshotTree(t, commonDir)

	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure dry-run: %v", err)
	}
	if rep.Provenance == nil || rep.Provenance.CreatedAt != nil || rep.Provenance.AttemptID != "" {
		t.Fatalf("dry-run provenance = %+v, want planned identity without creation facts", rep.Provenance)
	}
	if after := snapshotDir(t, root); strings.Join(after, "\x00") != strings.Join(before, "\x00") {
		t.Fatalf("dry-run mutated root: before=%v after=%v", before, after)
	}
	// The workspace root is not the only place a plan could write. The
	// serialization lock lives under the repository's common git dir, so a
	// snapshot of the root alone would let a lock acquired ahead of the dry-run
	// return pass as pure. Snapshotting the whole common dir rather than just
	// the lock path also covers anything else a future plan might leave there.
	if after := snapshotTree(t, commonDir); strings.Join(after, "\x00") != strings.Join(beforeCommon, "\x00") {
		t.Fatalf("dry-run mutated the repository:\n  before=%v\n  after=%v", beforeCommon, after)
	}
}

func TestCleanupRemovesOnlyVerifiedMergedPushedWorktreeAndIsIdempotent(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-test")
	spec := managedSpec(repo, root, wt, "work/gc-test", base)
	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	publishRemoteRef(t, repo, base, rep.Head)
	spec.AttemptID = rep.Provenance.AttemptID

	cleaned, err := Cleanup(spec)
	if err != nil {
		t.Fatalf("Cleanup: %v (report: %+v)", err, cleaned)
	}
	if !cleaned.Removed || cleaned.AlreadyAbsent || cleaned.CleanupPending || cleaned.Error != nil {
		t.Fatalf("Cleanup report = %+v, want removed cleanly", cleaned)
	}
	if cleaned.Provenance == nil || cleaned.Provenance.AttemptID != rep.Provenance.AttemptID {
		t.Fatalf("Cleanup provenance = %+v, want verified attempt %q", cleaned.Provenance, rep.Provenance.AttemptID)
	}
	if _, statErr := os.Stat(wt); !os.IsNotExist(statErr) {
		t.Fatalf("worktree path remains after Cleanup: %v", statErr)
	}

	again, err := Cleanup(spec)
	if err != nil {
		t.Fatalf("second Cleanup: %v (report: %+v)", err, again)
	}
	if again.Removed || !again.AlreadyAbsent || again.CleanupPending || again.Error != nil {
		t.Fatalf("second Cleanup report = %+v, want idempotent already_absent", again)
	}
}

func TestCleanupRefusesDirtyWorktreeWithoutRemovingWIP(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-test")
	spec := managedSpec(repo, root, wt, "work/gc-test", base)
	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	publishRemoteRef(t, repo, base, rep.Head)
	spec.AttemptID = rep.Provenance.AttemptID
	marker := filepath.Join(wt, "uncommitted.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	report, err := Cleanup(spec)
	if err == nil {
		t.Fatal("Cleanup dirty worktree succeeded, want refusal")
	}
	if !report.CleanupPending || report.Error == nil || report.Error.Code != CleanupErrorDirty {
		t.Fatalf("Cleanup report = %+v, want structured dirty cleanup_pending refusal", report)
	}
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "keep" {
		t.Fatalf("dirty WIP changed: data=%q err=%v", got, readErr)
	}
}

func TestCleanupRefusesUnpushedCommits(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-test")
	spec := managedSpec(repo, root, wt, "work/gc-test", base)
	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	spec.AttemptID = rep.Provenance.AttemptID
	publishRemoteRef(t, repo, base, runGit(t, repo, "rev-parse", base))
	runGit(t, wt, "commit", "--allow-empty", "-m", "local-only")

	report, cleanupErr := Cleanup(spec)
	if cleanupErr == nil {
		t.Fatal("Cleanup unpushed worktree succeeded, want refusal")
	}
	// The commit is local-only but still reachable from the worktree's own
	// branch, and `git worktree remove` deletes the checkout rather than
	// refs/heads, so removal would not orphan it. The reachability gate
	// therefore passes and the merge gate is what refuses: the commit is not
	// contained in the base. Cleanup still declines, which is the point.
	if !report.CleanupPending || report.Error == nil || report.Error.Code != CleanupErrorUnmerged {
		t.Fatalf("Cleanup report = %+v, want structured unmerged cleanup_pending refusal", report)
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("Cleanup removed unpushed worktree: %v", statErr)
	}
}

func TestCleanupRefusesPushedButUnmergedCommits(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-test")
	spec := managedSpec(repo, root, wt, "work/gc-test", base)
	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	spec.AttemptID = rep.Provenance.AttemptID
	runGit(t, wt, "commit", "--allow-empty", "-m", "pushed-not-merged")
	tip := runGit(t, wt, "rev-parse", "HEAD")
	publishRemoteRef(t, repo, spec.Branch, tip)

	report, cleanupErr := Cleanup(spec)
	if cleanupErr == nil {
		t.Fatal("Cleanup unmerged worktree succeeded, want refusal")
	}
	if !report.CleanupPending || report.Error == nil || report.Error.Code != CleanupErrorUnmerged {
		t.Fatalf("Cleanup report = %+v, want structured unmerged cleanup_pending refusal", report)
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("Cleanup removed unmerged worktree: %v", statErr)
	}
}

func TestCleanupRefusesAmbiguousOrMismatchedOwnership(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()

	t.Run("existing path is not a registered worktree", func(t *testing.T) {
		path := filepath.Join(root, "plain")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		spec := managedSpec(repo, root, path, "work/plain", base)
		spec.AttemptID = "attempt-that-never-existed"
		report, err := Cleanup(spec)
		if err == nil {
			t.Fatal("Cleanup ambiguous plain path succeeded, want refusal")
		}
		if !report.CleanupPending || report.Error == nil || report.Error.Code != CleanupErrorAmbiguous {
			t.Fatalf("Cleanup report = %+v, want structured ambiguous-path refusal", report)
		}
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("Cleanup removed ambiguous path: %v", statErr)
		}
	})

	t.Run("provenance owner does not match", func(t *testing.T) {
		path := filepath.Join(root, "owned")
		spec := managedSpec(repo, root, path, "work/owned", base)
		rep, err := Ensure(spec)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		conflict := spec
		conflict.AttemptID = rep.Provenance.AttemptID
		conflict.Owner = "other-owner"
		report, cleanupErr := Cleanup(conflict)
		if cleanupErr == nil {
			t.Fatal("Cleanup mismatched provenance succeeded, want refusal")
		}
		if !report.CleanupPending || report.Error == nil || report.Error.Code != CleanupErrorOwnership {
			t.Fatalf("Cleanup report = %+v, want structured ownership refusal", report)
		}
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("Cleanup removed mismatched worktree: %v", statErr)
		}
	})
}

func TestCleanupRequiresManagedSpecAndRefusesMissingRegisteredPath(t *testing.T) {
	repo, base := initTestRepo(t)

	t.Run("unmanaged spec", func(t *testing.T) {
		report, err := Cleanup(Spec{
			RepoDir: repo,
			Path:    filepath.Join(t.TempDir(), "missing"),
			Branch:  "work/unmanaged",
			Base:    base,
		})
		if err == nil {
			t.Fatal("Cleanup unmanaged spec succeeded, want refusal")
		}
		if !report.CleanupPending || report.Error == nil || report.Error.Code != CleanupErrorInvalidSpec {
			t.Fatalf("Cleanup report = %+v, want invalid_spec", report)
		}
	})

	t.Run("registered path disappeared", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "owned")
		spec := managedSpec(repo, root, path, "work/disappeared", base)
		rep, err := Ensure(spec)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		spec.AttemptID = rep.Provenance.AttemptID
		moved := filepath.Join(root, "moved-aside")
		if err := os.Rename(path, moved); err != nil {
			t.Fatalf("Rename worktree aside: %v", err)
		}

		report, cleanupErr := Cleanup(spec)
		if cleanupErr == nil {
			t.Fatal("Cleanup missing registered path succeeded, want refusal")
		}
		if !report.CleanupPending || report.Error == nil || report.Error.Code != CleanupErrorAmbiguous {
			t.Fatalf("Cleanup report = %+v, want ambiguous stale-registration refusal", report)
		}
		if _, statErr := os.Stat(moved); statErr != nil {
			t.Fatalf("Cleanup touched moved worktree contents: %v", statErr)
		}
	})
}

// Rollback belongs to Ensure alone. When verifyCreatedWorktree also rolled
// back, a successful first rollback left the second one removing an absent
// path, and the caller saw "rollback incomplete" for a workspace that had in
// fact been cleaned up.
func TestVerifyCreatedWorktreeLeavesRollbackToEnsure(t *testing.T) {
	repo, base := initTestRepo(t)
	missing := filepath.Join(t.TempDir(), "never-created")
	spec := Spec{RepoDir: repo, Path: missing, Branch: "feature", Base: "main"}
	_, err := verifyCreatedWorktree(spec, false, base)
	if err == nil {
		t.Fatal("verifyCreatedWorktree on a missing path succeeded, want error")
	}
	if strings.Contains(err.Error(), "rollback") || strings.Contains(err.Error(), "rolled back") {
		t.Errorf("verifyCreatedWorktree performed its own rollback: %v", err)
	}
}

// Rollback deletes only a branch it can show is its own. A branch that
// exists at some other commit was created by someone else between this
// attempt's probe and its failed create, and must survive.
func TestRollbackAfterFailedCreateKeepsBranchNotAtOurBase(t *testing.T) {
	repo, base := initTestRepo(t)
	repoGit := git.New(repo)
	runGit(t, repo, "commit", "--allow-empty", "-m", "second")
	other := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	if other == base {
		t.Fatal("setup: second commit did not move HEAD")
	}
	runGit(t, repo, "branch", "rival", other)

	err := rollbackAfterFailedCreate(repoGit, filepath.Join(t.TempDir(), "absent"), "rival", true, base)
	if err == nil || !strings.Contains(err.Error(), "refusing to delete branch") {
		t.Fatalf("rollback err = %v, want a refusal to delete", err)
	}
	if exists, existsErr := repoGit.BranchExists("rival"); existsErr != nil || !exists {
		t.Errorf("branch rival = %v, %v after rollback; want it kept", exists, existsErr)
	}
}

// A workspace carrying published provenance was completed by some other
// Ensure: this attempt fails before its own publish step, so a registration
// with provenance is a rival's and must survive rollback.
func TestRollbackAfterFailedCreateKeepsWorktreeWithPublishedProvenance(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	path := filepath.Join(root, "rival")
	rep, err := Ensure(managedSpec(repo, root, path, "rival-branch", base))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if rep.Provenance == nil {
		t.Fatal("setup: Ensure published no provenance")
	}

	rollbackErr := rollbackAfterFailedCreate(git.New(repo), path, "rival-branch", false, base)
	if rollbackErr == nil || !strings.Contains(rollbackErr.Error(), "published provenance") {
		t.Fatalf("rollback err = %v, want a refusal citing published provenance", rollbackErr)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("rival worktree removed by rollback: %v", statErr)
	}
}

// Ownership is inconclusive when provenance exists but cannot be read.
// Only its absence licenses removal.
func TestRollbackAfterFailedCreateKeepsWorktreeWithUnreadableProvenance(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	path := filepath.Join(root, "rival")
	if _, err := Ensure(managedSpec(repo, root, path, "rival-branch", base)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	manifest, err := provenanceFilePath(path)
	if err != nil {
		t.Fatalf("provenanceFilePath: %v", err)
	}
	if err := os.WriteFile(manifest, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupting provenance: %v", err)
	}

	rollbackErr := rollbackAfterFailedCreate(git.New(repo), path, "rival-branch", false, base)
	if rollbackErr == nil || !strings.Contains(rollbackErr.Error(), "ownership is inconclusive") {
		t.Fatalf("rollback err = %v, want an inconclusive-ownership refusal", rollbackErr)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("rival worktree removed on inconclusive ownership: %v", statErr)
	}
}
