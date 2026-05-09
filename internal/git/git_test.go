package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
)

// initTestRepo creates a git repo with one commit in a temp directory.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "commit", "--allow-empty", "-m", "init")
	return dir
}

// runGit runs a git command in dir and fails the test on error.
// Strips git env vars to prevent interference from pre-commit hooks.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if gitEnvBlacklist[k] {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
	}
}

func TestIsRepo(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)
	if !g.IsRepo() {
		t.Error("IsRepo() = false, want true")
	}

	notRepo := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(notRepo))
	g2 := New(notRepo)
	if g2.IsRepo() {
		t.Error("IsRepo() = true for non-repo, want false")
	}
}

func TestCurrentBranch(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)
	branch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	// Default branch is typically "master" or "main" depending on git config.
	if branch == "" {
		t.Error("CurrentBranch returned empty string")
	}
}

func TestDefaultBranch_NoRemote(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)
	branch, err := g.DefaultBranch()
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if branch != "main" {
		t.Errorf("DefaultBranch() = %q, want %q (fallback)", branch, "main")
	}
}

// TestDefaultBranch_FromOriginHEAD exercises the symref parsing path and
// verifies that branch names containing slashes round-trip correctly.
// Regression test for the bug where strings.LastIndex(ref, "/") truncated
// "refs/remotes/origin/user/feature" to "feature".
func TestDefaultBranch_FromOriginHEAD(t *testing.T) {
	tests := []struct {
		name   string
		branch string
	}{
		{"plain branch", "main"},
		{"single slash", "boylec/develop"},
		{"nested slashes", "team/feature/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up a bare remote and a clone that tracks it, so
			// refs/remotes/origin/HEAD can be wired to the target ref.
			bare := t.TempDir()
			runGit(t, bare, "init", "--bare")

			clone := t.TempDir()
			runGit(t, clone, "clone", bare, ".")
			runGit(t, clone, "config", "user.email", "test@test.com")
			runGit(t, clone, "config", "user.name", "Test")
			runGit(t, clone, "commit", "--allow-empty", "-m", "init")

			// Create the target ref under refs/remotes/origin/ and point
			// origin/HEAD at it. symbolic-ref is permissive about its
			// target so we don't need to push the branch first.
			target := "refs/remotes/origin/" + tt.branch
			runGit(t, clone, "update-ref", target, "HEAD")
			runGit(t, clone, "symbolic-ref", "refs/remotes/origin/HEAD", target)

			g := New(clone)
			got, err := g.DefaultBranch()
			if err != nil {
				t.Fatalf("DefaultBranch: %v", err)
			}
			if got != tt.branch {
				t.Errorf("DefaultBranch() = %q, want %q", got, tt.branch)
			}
		})
	}
}

func TestProbeDefaultBranch_FromOriginHEAD(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")

	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "commit", "--allow-empty", "-m", "init")

	target := "refs/remotes/origin/master"
	runGit(t, clone, "update-ref", target, "HEAD")
	runGit(t, clone, "symbolic-ref", "refs/remotes/origin/HEAD", target)

	g := New(clone)
	got := g.ProbeDefaultBranch()
	if got != "master" {
		t.Errorf("ProbeDefaultBranch() = %q, want %q", got, "master")
	}
}

func TestProbeDefaultBranch_FallsBackToCurrentBranch(t *testing.T) {
	repo := initTestRepo(t)
	// Force a known branch name; the test repo's default may be "main"
	// or "master" depending on the host's git init.defaultBranch.
	runGit(t, repo, "checkout", "-b", "develop")
	g := New(repo)
	got := g.ProbeDefaultBranch()
	if got != "develop" {
		t.Errorf("ProbeDefaultBranch() = %q, want %q (current branch fallback)", got, "develop")
	}
}

func TestProbeDefaultBranch_NoRepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	g := New(dir)
	got := g.ProbeDefaultBranch()
	if got != "" {
		t.Errorf("ProbeDefaultBranch() = %q, want empty (no repo)", got)
	}
}

func TestWorktreeRemove(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)

	wtPath := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", "-b", "to-remove", wtPath)

	if err := g.WorktreeRemove(wtPath, false); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}

	// Directory should be gone.
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists after remove")
	}
}

func TestWorktreeRemoveForce(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)

	wtPath := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", "-b", "dirty-wt", wtPath)

	// Create an uncommitted file to make the worktree dirty.
	if err := os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Force remove should succeed even with dirty worktree.
	if err := g.WorktreeRemove(wtPath, true); err != nil {
		t.Fatalf("WorktreeRemove(force): %v", err)
	}
}

func TestWorktreeList(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)

	wtPath := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", "-b", "listed", wtPath)

	worktrees, err := g.WorktreeList()
	if err != nil {
		t.Fatalf("WorktreeList: %v", err)
	}

	// Should have at least 2: the main repo and the worktree.
	if len(worktrees) < 2 {
		t.Fatalf("len(worktrees) = %d, want >= 2", len(worktrees))
	}

	// Find our worktree.
	var found bool
	for _, wt := range worktrees {
		if testutil.CanonicalPath(wt.Path) == testutil.CanonicalPath(wtPath) {
			found = true
			if wt.Branch != "listed" {
				t.Errorf("worktree branch = %q, want %q", wt.Branch, "listed")
			}
		}
	}
	if !found {
		t.Errorf("worktree at %q not found in list", wtPath)
	}
}

// TestWorktreeList_NestedSiblings verifies the algorithmic assumption used
// by NestedWorktreePruneCheck: when worktree B is created at a path that
// lies inside worktree A's working tree, git treats them as siblings in
// the same admin dir. WorktreeList() from any of A, B, or the main repo
// returns all three entries with each entry's true on-disk path.
//
// This is the foundation for "find nested worktrees" — we walk per-agent
// homes, list siblings, and filter by path containment to identify nested
// entries.
func TestWorktreeList_NestedSiblings(t *testing.T) {
	repo := initTestRepo(t)

	// Outer worktree (the "agent home").
	home := filepath.Join(t.TempDir(), "home")
	runGit(t, repo, "worktree", "add", "-b", "home-branch", home)

	// Nested worktree, path lies inside `home`. Equivalent to the polecat
	// "$(pwd)/worktrees/<issue>" pattern from mol-polecat-work.toml.
	nested := filepath.Join(home, "worktrees", "task-x")
	runGit(t, home, "worktree", "add", "-b", "task-x-branch", nested)

	// Listing from the home worktree returns all three siblings.
	gHome := New(home)
	wts, err := gHome.WorktreeList()
	if err != nil {
		t.Fatalf("WorktreeList from home: %v", err)
	}
	gotPaths := make(map[string]string)
	for _, wt := range wts {
		gotPaths[testutil.CanonicalPath(wt.Path)] = wt.Branch
	}

	wantHome := testutil.CanonicalPath(home)
	wantNested := testutil.CanonicalPath(nested)
	wantRepo := testutil.CanonicalPath(repo)

	if _, ok := gotPaths[wantHome]; !ok {
		t.Errorf("home worktree %q missing from list; got %v", wantHome, gotPaths)
	}
	if br := gotPaths[wantNested]; br != "task-x-branch" {
		t.Errorf("nested worktree branch = %q (path %q), want task-x-branch; full list: %v",
			br, wantNested, gotPaths)
	}
	if _, ok := gotPaths[wantRepo]; !ok {
		t.Errorf("main repo %q missing from list; got %v", wantRepo, gotPaths)
	}

	// Listing from inside the nested worktree must produce the same set.
	gNested := New(nested)
	wts2, err := gNested.WorktreeList()
	if err != nil {
		t.Fatalf("WorktreeList from nested: %v", err)
	}
	if len(wts2) != len(wts) {
		t.Errorf("WorktreeList from nested returned %d entries; from home returned %d (must match)",
			len(wts2), len(wts))
	}

	// Path containment is the discriminator the doctor check uses to
	// classify "nested" vs "agent home" vs "main repo". Verify it works
	// on canonical paths.
	if !strings.HasPrefix(wantNested+string(filepath.Separator), wantHome+string(filepath.Separator)) {
		t.Errorf("nested path %q is not a strict subpath of home %q", wantNested, wantHome)
	}
	if strings.HasPrefix(wantHome+string(filepath.Separator), wantNested+string(filepath.Separator)) {
		t.Errorf("home %q must not be classified as inside nested %q", wantHome, wantNested)
	}
}

func TestHasUncommittedWork_Clean(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)
	if g.HasUncommittedWork() {
		t.Error("HasUncommittedWork() = true for clean repo, want false")
	}
}

func TestHasUncommittedWork_Dirty(t *testing.T) {
	repo := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := New(repo)
	if !g.HasUncommittedWork() {
		t.Error("HasUncommittedWork() = false for dirty repo, want true")
	}
}

func TestHasUnpushedCommits_NoneWhenClean(t *testing.T) {
	// Create a bare remote and clone it so there's a tracking branch.
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")

	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "commit", "--allow-empty", "-m", "init")
	runGit(t, clone, "push", "origin", "HEAD")

	g := New(clone)
	if g.HasUnpushedCommits() {
		t.Error("HasUnpushedCommits() = true for fully-pushed repo, want false")
	}
}

func TestHasUnpushedCommits_DetectsLocal(t *testing.T) {
	// Create a bare remote and clone it.
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")

	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "commit", "--allow-empty", "-m", "init")
	runGit(t, clone, "push", "origin", "HEAD")

	// Create a worktree with a local-only commit.
	wtPath := filepath.Join(t.TempDir(), "wt")
	runGit(t, clone, "worktree", "add", "-b", "feature", wtPath)
	runGit(t, wtPath, "config", "user.email", "test@test.com")
	runGit(t, wtPath, "config", "user.name", "Test")
	runGit(t, wtPath, "commit", "--allow-empty", "-m", "local work")

	g := New(wtPath)
	if !g.HasUnpushedCommits() {
		t.Error("HasUnpushedCommits() = false for worktree with local commit, want true")
	}
}

func TestHasUnpushedCommits_NoRemote(t *testing.T) {
	// A repo with no remote has no remote branches → all commits are "unpushed".
	repo := initTestRepo(t)
	g := New(repo)
	if !g.HasUnpushedCommits() {
		t.Error("HasUnpushedCommits() = false for repo with no remote, want true")
	}
}

func TestHasUnpushedCommitsResult_ReturnsProbeError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	g := New(dir)
	if _, err := g.HasUnpushedCommitsResult(); err == nil {
		t.Fatal("HasUnpushedCommitsResult() error = nil, want probe error")
	}
	if !g.HasUnpushedCommits() {
		t.Error("HasUnpushedCommits() should fail closed on probe errors")
	}
}

func TestHasStashes_NoneWhenClean(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)
	if g.HasStashes() {
		t.Error("HasStashes() = true for clean repo, want false")
	}
}

func TestHasStashes_DetectsStash(t *testing.T) {
	repo := initTestRepo(t)
	// Create a file and stash it.
	if err := os.WriteFile(filepath.Join(repo, "stash-me.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "stash-me.txt")
	runGit(t, repo, "stash")

	g := New(repo)
	if !g.HasStashes() {
		t.Error("HasStashes() = false for repo with stash, want true")
	}
}

func TestHasStashesResult_ReturnsProbeError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	g := New(dir)
	if _, err := g.HasStashesResult(); err == nil {
		t.Fatal("HasStashesResult() error = nil, want probe error")
	}
	if !g.HasStashes() {
		t.Error("HasStashes() should fail closed on probe errors")
	}
}

func TestFetch(t *testing.T) {
	// Create a bare remote and clone it.
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")

	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "commit", "--allow-empty", "-m", "init")
	runGit(t, clone, "push", "origin", "HEAD")

	g := New(clone)
	if err := g.Fetch(); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

func TestFetch_NoRemote(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)
	if err := g.Fetch(); err == nil {
		t.Error("expected error fetching repo with no remote")
	}
}

func TestStashAndPop(t *testing.T) {
	repo := initTestRepo(t)

	// Create a dirty file.
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := New(repo)
	if !g.HasUncommittedWork() {
		t.Fatal("expected dirty repo")
	}

	// Stash the changes.
	if err := g.Stash("test-stash"); err != nil {
		t.Fatalf("Stash: %v", err)
	}
	if g.HasUncommittedWork() {
		t.Error("repo still dirty after stash")
	}
	if !g.HasStashes() {
		t.Error("expected stash after Stash()")
	}

	// Pop the stash.
	if err := g.StashPop(); err != nil {
		t.Fatalf("StashPop: %v", err)
	}
	if !g.HasUncommittedWork() {
		t.Error("repo should be dirty after stash pop")
	}
}

func TestStash_CleanRepo(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)
	// Stashing a clean repo: behavior varies by git version.
	// Some return exit 1 ("No local changes to save"), some return 0.
	// Just verify it doesn't create a stash entry.
	_ = g.Stash("empty")
	// A clean repo should have no stash entries regardless.
	if g.HasStashes() {
		t.Error("clean repo should have no stashes after stash attempt")
	}
}

func TestStashPop_NoStash(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)
	if err := g.StashPop(); err == nil {
		t.Error("expected error popping empty stash")
	}
}

func TestPullRebase(t *testing.T) {
	// Create a bare remote and clone it.
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")

	clone := t.TempDir()
	runGit(t, clone, "clone", bare, ".")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "commit", "--allow-empty", "-m", "init")
	runGit(t, clone, "push", "origin", "HEAD")

	// Make an upstream change.
	clone2 := t.TempDir()
	runGit(t, clone2, "clone", bare, ".")
	runGit(t, clone2, "config", "user.email", "test@test.com")
	runGit(t, clone2, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(clone2, "upstream.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone2, "add", "upstream.txt")
	runGit(t, clone2, "commit", "-m", "upstream change")
	runGit(t, clone2, "push", "origin", "HEAD")

	// Fetch and pull --rebase in original clone.
	g := New(clone)
	if err := g.Fetch(); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Get the current branch name.
	branch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}

	if err := g.PullRebase("origin", branch); err != nil {
		t.Fatalf("PullRebase: %v", err)
	}

	// Verify the upstream file now exists.
	if _, err := os.Stat(filepath.Join(clone, "upstream.txt")); err != nil {
		t.Errorf("upstream.txt not found after pull --rebase: %v", err)
	}
}

func TestWorktreePrune(t *testing.T) {
	repo := initTestRepo(t)
	g := New(repo)

	// Prune on a clean repo should not fail.
	if err := g.WorktreePrune(); err != nil {
		t.Fatalf("WorktreePrune: %v", err)
	}
}

func TestParseWorktreeList(t *testing.T) {
	output := `worktree /home/user/repo
HEAD abc123
branch refs/heads/main

worktree /home/user/repo-wt
HEAD def456
branch refs/heads/feature-1

`
	wts := parseWorktreeList(output)
	if len(wts) != 2 {
		t.Fatalf("len(worktrees) = %d, want 2", len(wts))
	}
	if wts[0].Path != "/home/user/repo" {
		t.Errorf("wts[0].Path = %q, want %q", wts[0].Path, "/home/user/repo")
	}
	if wts[0].Branch != "main" {
		t.Errorf("wts[0].Branch = %q, want %q", wts[0].Branch, "main")
	}
	if wts[1].Path != "/home/user/repo-wt" {
		t.Errorf("wts[1].Path = %q, want %q", wts[1].Path, "/home/user/repo-wt")
	}
	if wts[1].Branch != "feature-1" {
		t.Errorf("wts[1].Branch = %q, want %q", wts[1].Branch, "feature-1")
	}
	if wts[1].Head != "def456" {
		t.Errorf("wts[1].Head = %q, want %q", wts[1].Head, "def456")
	}
}

func TestParseWorktreeList_Empty(t *testing.T) {
	wts := parseWorktreeList("")
	if len(wts) != 0 {
		t.Errorf("len(worktrees) = %d, want 0", len(wts))
	}
}
