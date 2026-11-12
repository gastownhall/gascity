package main

import (
	"os/exec"
	"testing"
)

// TestCLIBranchResolverBaseRefMissing pins the CLI adapter of the sling-time
// base_branch check against a real repository: a local branch, a bare name
// found as origin/<name> and a full remote-tracking ref all resolve, a name
// that is none of them is missing, and an empty dir cannot prove anything.
func TestCLIBranchResolverBaseRefMissing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in PATH")
	}
	repo := t.TempDir()
	runGitInTest(t, repo, "init", "-b", "main")
	runGitInTest(t, repo, "config", "user.email", "test@test.com")
	runGitInTest(t, repo, "config", "user.name", "Test")
	runGitInTest(t, repo, "commit", "--allow-empty", "-m", "init")
	runGitInTest(t, repo, "update-ref", "refs/remotes/origin/only-on-origin", "HEAD")
	runGitInTest(t, repo, "update-ref", "refs/remotes/headfork/restack/foo", "HEAD")

	r := cliBranchResolver{}
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
	if r.BaseRefMissing("", "ghost/base") {
		t.Error("BaseRefMissing with an empty dir = true, want false (nothing to probe)")
	}
}
