package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBranchExists(t *testing.T) {
	dir := initTestRepo(t)
	g := New(dir)
	branch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if !g.BranchExists(branch) {
		t.Errorf("BranchExists(%q) = false, want true", branch)
	}
	if g.BranchExists("no-such-branch") {
		t.Error("BranchExists(no-such-branch) = true, want false")
	}
}

func TestRevParseVerifyCommit(t *testing.T) {
	dir := initTestRepo(t)
	g := New(dir)
	branch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	sha, err := g.RevParseVerifyCommit(branch)
	if err != nil {
		t.Fatalf("RevParseVerifyCommit(%q): %v", branch, err)
	}
	if len(sha) != 40 {
		t.Errorf("RevParseVerifyCommit returned %q, want 40-char SHA", sha)
	}
	if _, err := g.RevParseVerifyCommit("no-such-ref"); err == nil {
		t.Error("RevParseVerifyCommit(no-such-ref) succeeded, want error")
	}
	// A tree-ish that is not a commit must be rejected.
	if _, err := g.RevParseVerifyCommit(sha + "^{tree}"); err == nil {
		t.Error("RevParseVerifyCommit of a tree object succeeded, want error")
	}
}

func TestWorktreeAddNewBranch(t *testing.T) {
	dir := initTestRepo(t)
	g := New(dir)
	base, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := g.WorktreeAddNewBranch(wtPath, "feature-x", base); err != nil {
		t.Fatalf("WorktreeAddNewBranch: %v", err)
	}
	wg := New(wtPath)
	branch, err := wg.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch in worktree: %v", err)
	}
	if branch != "feature-x" {
		t.Errorf("worktree branch = %q, want feature-x", branch)
	}
	// Branch tip must equal the base commit.
	baseSHA, err := g.RevParseVerifyCommit(base)
	if err != nil {
		t.Fatalf("RevParseVerifyCommit(base): %v", err)
	}
	tipSHA, err := g.RevParseVerifyCommit("feature-x")
	if err != nil {
		t.Fatalf("RevParseVerifyCommit(feature-x): %v", err)
	}
	if tipSHA != baseSHA {
		t.Errorf("feature-x tip = %s, want base %s", tipSHA, baseSHA)
	}
	// Creating over an existing branch must fail (no clobber).
	other := filepath.Join(t.TempDir(), "wt2")
	if err := g.WorktreeAddNewBranch(other, "feature-x", base); err == nil {
		t.Error("WorktreeAddNewBranch with existing branch succeeded, want error")
	}
}

func TestWorktreeAddExistingBranch(t *testing.T) {
	dir := initTestRepo(t)
	g := New(dir)
	base, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	runGit(t, dir, "branch", "feature-y", base)
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := g.WorktreeAddExistingBranch(wtPath, "feature-y"); err != nil {
		t.Fatalf("WorktreeAddExistingBranch: %v", err)
	}
	branch, err := New(wtPath).CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch in worktree: %v", err)
	}
	if branch != "feature-y" {
		t.Errorf("worktree branch = %q, want feature-y", branch)
	}
}

func TestBranchDelete(t *testing.T) {
	dir := initTestRepo(t)
	g := New(dir)
	base, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	runGit(t, dir, "branch", "doomed", base)
	if !g.BranchExists("doomed") {
		t.Fatal("setup: branch doomed missing")
	}
	if err := g.BranchDeleteIfMerged("doomed"); err != nil {
		t.Fatalf("BranchDeleteIfMerged: %v", err)
	}
	if g.BranchExists("doomed") {
		t.Error("branch doomed still exists after BranchDeleteIfMerged")
	}
}

func TestCommonDir(t *testing.T) {
	dir := initTestRepo(t)
	g := New(dir)
	common, err := g.CommonDir()
	if err != nil {
		t.Fatalf("CommonDir: %v", err)
	}
	if !filepath.IsAbs(common) {
		t.Errorf("CommonDir = %q, want absolute path", common)
	}
	// A worktree of the same repo shares the common dir.
	base, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := g.WorktreeAddNewBranch(wtPath, "common-check", base); err != nil {
		t.Fatalf("WorktreeAddNewBranch: %v", err)
	}
	wtCommon, err := New(wtPath).CommonDir()
	if err != nil {
		t.Fatalf("CommonDir in worktree: %v", err)
	}
	resolve := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", p, err)
		}
		return r
	}
	if resolve(wtCommon) != resolve(common) {
		t.Errorf("worktree CommonDir = %q, repo CommonDir = %q; want same", wtCommon, common)
	}
}

func TestGitDirIsWorktreeSpecific(t *testing.T) {
	dir := initTestRepo(t)
	g := New(dir)
	base, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := g.WorktreeAddNewBranch(wtPath, "gitdir-check", base); err != nil {
		t.Fatalf("WorktreeAddNewBranch: %v", err)
	}

	mainGitDir, err := g.GitDir()
	if err != nil {
		t.Fatalf("GitDir main: %v", err)
	}
	wtGitDir, err := New(wtPath).GitDir()
	if err != nil {
		t.Fatalf("GitDir worktree: %v", err)
	}
	if !filepath.IsAbs(mainGitDir) || !filepath.IsAbs(wtGitDir) {
		t.Fatalf("GitDir paths must be absolute: main=%q worktree=%q", mainGitDir, wtGitDir)
	}
	if mainGitDir == wtGitDir {
		t.Fatalf("GitDir returned shared directory %q for both worktrees", mainGitDir)
	}
	if _, err := os.Stat(wtGitDir); err != nil {
		t.Fatalf("worktree GitDir %q is not readable: %v", wtGitDir, err)
	}
}

func TestHeadSymbolicRef(t *testing.T) {
	dir := initTestRepo(t)
	g := New(dir)
	branch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	ref, err := g.HeadSymbolicRef()
	if err != nil {
		t.Fatalf("HeadSymbolicRef: %v", err)
	}
	if ref != "refs/heads/"+branch {
		t.Errorf("HeadSymbolicRef = %q, want refs/heads/%s", ref, branch)
	}
	// Detached HEAD must return an error, never a ref.
	sha, err := g.RevParseVerifyCommit(branch)
	if err != nil {
		t.Fatalf("RevParseVerifyCommit: %v", err)
	}
	if err := g.CheckoutDetach(sha); err != nil {
		t.Fatalf("CheckoutDetach: %v", err)
	}
	if ref, err := g.HeadSymbolicRef(); err == nil {
		t.Errorf("HeadSymbolicRef on detached HEAD = %q, want error", ref)
	}
}

func TestTopLevel(t *testing.T) {
	dir := initTestRepo(t)
	sub := filepath.Join(dir, "nested", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	top, err := New(sub).TopLevel()
	if err != nil {
		t.Fatalf("TopLevel: %v", err)
	}
	resolve := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", p, err)
		}
		return r
	}
	if resolve(top) != resolve(dir) {
		t.Errorf("TopLevel from subdir = %q, want %q", top, dir)
	}
}

func TestWorktreeOpsRefuseOptionInjection(t *testing.T) {
	dir := initTestRepo(t)
	g := New(dir)
	// A branch or ref starting with "-" must not be interpreted as a flag.
	if g.BranchExists("--help") {
		t.Error("BranchExists(--help) = true")
	}
	if _, err := g.RevParseVerifyCommit("--help"); err == nil {
		t.Error("RevParseVerifyCommit(--help) succeeded, want error")
	}
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := g.WorktreeAddNewBranch(wtPath, "--force", "HEAD"); err == nil {
		t.Error("WorktreeAddNewBranch with flag-like branch succeeded, want error")
	}
	if _, statErr := os.Stat(wtPath); statErr == nil {
		t.Error("flag-like branch attempt created the worktree path")
	}
}

func TestWorktreeAddNeverDetaches(t *testing.T) {
	// Guard the bead's core invariant at the helper layer: the add helpers
	// must never produce a detached-HEAD worktree.
	dir := initTestRepo(t)
	g := New(dir)
	base, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := g.WorktreeAddNewBranch(wtPath, "attached", base); err != nil {
		t.Fatalf("WorktreeAddNewBranch: %v", err)
	}
	ref, err := New(wtPath).HeadSymbolicRef()
	if err != nil {
		t.Fatalf("HeadSymbolicRef: %v", err)
	}
	if !strings.HasPrefix(ref, "refs/heads/") {
		t.Errorf("worktree HEAD = %q, want refs/heads/*", ref)
	}
}
