package api

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestApiBranchResolverIgnoresPoisonedGitEnv proves DefaultBranch resolves the
// origin/HEAD of the targeted directory's repository even when the process
// environment leaks GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE from a parent repo or
// a pre-commit hook. Without gitpkg.SanitizedEnv() on the symbolic-ref
// subprocess, the leaked GIT_DIR redirects git away from dir and DefaultBranch
// returns "". Regression guard for gastownhall/gascity#3343 (review attempt-8
// blocker on the unsanitized API branch resolver).
func TestApiBranchResolverIgnoresPoisonedGitEnv(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in PATH")
	}
	repo := t.TempDir()
	runResolverGit(t, repo, "init", "-b", "main")
	// Point origin/HEAD at origin/main without needing a real remote; the
	// symbolic ref's target need not exist for symbolic-ref --short to read it.
	runResolverGit(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	// Poison the environment after setup so the setup commands stay clean.
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "poison.git"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "poison.index"))

	r := apiBranchResolver{}
	if got := r.DefaultBranch(repo); got != "main" {
		t.Fatalf("DefaultBranch under poisoned GIT_* env = %q, want %q", got, "main")
	}
}

func runResolverGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestApiBranchResolverBaseRefMissing pins the API adapter of the sling-time
// base_branch check: a local branch, a bare name found as origin/<name> and a
// full remote-tracking ref all resolve; a name that is none of them is missing;
// and an empty dir falls back to the city path, as DefaultBranch does.
func TestApiBranchResolverBaseRefMissing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in PATH")
	}
	repo := t.TempDir()
	runResolverGit(t, repo, "init", "-b", "main")
	runResolverGit(t, repo, "config", "user.email", "test@test.com")
	runResolverGit(t, repo, "config", "user.name", "Test")
	runResolverGit(t, repo, "commit", "--allow-empty", "-m", "init")
	runResolverGit(t, repo, "update-ref", "refs/remotes/origin/only-on-origin", "HEAD")
	runResolverGit(t, repo, "update-ref", "refs/remotes/headfork/restack/foo", "HEAD")

	r := apiBranchResolver{cityPath: repo}
	for _, tt := range []struct {
		ref  string
		want bool
	}{
		{"main", false},
		{"only-on-origin", false},
		{"headfork/restack/foo", false},
		{"ghost/base", true},
	} {
		if got := r.BaseRefMissing(repo, tt.ref); got != tt.want {
			t.Errorf("BaseRefMissing(%q) = %v, want %v", tt.ref, got, tt.want)
		}
	}
	if got := r.BaseRefMissing("", "ghost/base"); !got {
		t.Error("BaseRefMissing with an empty dir must probe the city path and report ghost/base missing")
	}
}
