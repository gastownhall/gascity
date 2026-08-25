package workrecord

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The contract rows — which beads Gated covers and what ValidateOnClose calls a
// violation — are pinned by the two planes that run them:
// cmd/gc/work_record_gate_test.go against the bd-argv adapter and
// internal/api/work_record_close_gate_api_test.go against the HTTP handlers.
// CommitReachableOnBranch is the one piece neither plane can pin honestly,
// because both inject a fake oracle to stay off disk — so it is pinned here,
// against a real repository.

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// commitRepo builds a repo with one commit on main and one commit on a side
// branch that main never saw, and returns the repo dir and both SHAs.
func commitRepo(t *testing.T) (repoDir, onMain, offMain string) {
	t.Helper()
	repoDir = t.TempDir()
	runGit(t, repoDir, "init", "--initial-branch=main")
	runGit(t, repoDir, "config", "user.name", "Gas City Test")
	runGit(t, repoDir, "config", "user.email", "gc-test@test.local")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		runGit(t, repoDir, "add", name)
	}
	write("shipped.txt", "on main\n")
	runGit(t, repoDir, "commit", "-m", "test: land the artifact")
	onMain = strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))

	runGit(t, repoDir, "checkout", "-b", "side")
	write("side.txt", "never merged\n")
	runGit(t, repoDir, "commit", "-m", "test: a commit main never saw")
	offMain = strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))
	runGit(t, repoDir, "checkout", "main")

	// Two branches whose local and remote-tracking refs disagree, in both
	// directions — the pair gastownhall/gascity#5037 turns on. remote-ahead is
	// the reported bug (the commit landed via another worktree, so only
	// refs/remotes/origin/remote-ahead advanced); local-ahead is its mirror (a
	// commit made here and never pushed).
	runGit(t, repoDir, "branch", "remote-ahead", onMain)
	runGit(t, repoDir, "update-ref", "refs/remotes/origin/remote-ahead", offMain)
	runGit(t, repoDir, "branch", "local-ahead", offMain)
	runGit(t, repoDir, "update-ref", "refs/remotes/origin/local-ahead", onMain)
	return repoDir, onMain, offMain
}

func TestCommitReachableOnBranch(t *testing.T) {
	repoDir, onMain, offMain := commitRepo(t)
	notARepo := t.TempDir()

	tests := []struct {
		name           string
		dir            string
		commit, branch string
		want           bool
	}{
		{"commit on the branch is reachable", repoDir, onMain, "main", true},
		{"commit on another branch is not reachable", repoDir, offMain, "main", false},
		{"commit reachable on its own branch", repoDir, offMain, "side", true},
		{"commit reachable only from the remote-tracking ref", repoDir, offMain, "remote-ahead", true},
		{"commit reachable only from the local branch", repoDir, offMain, "local-ahead", true},
		{"unknown commit is not reachable", repoDir, "0000000000000000000000000000000000000000", "main", false},
		{"unknown branch is not reachable", repoDir, onMain, "no-such-branch", false},
		{"a directory that is not a repo is not reachable", notARepo, onMain, "main", false},
		{"an empty repo dir is not reachable", "", onMain, "main", false},
		{"an empty commit is not reachable", repoDir, "", "main", false},
		{"an empty branch is not reachable", repoDir, onMain, "", false},
		{"a flag-shaped commit is rejected", repoDir, "--all", "main", false},
		{"a flag-shaped branch is rejected", repoDir, onMain, "--all", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CommitReachableOnBranch(tc.dir, tc.commit, tc.branch); got != tc.want {
				t.Fatalf("CommitReachableOnBranch(%q, %q, %q) = %v, want %v", tc.dir, tc.commit, tc.branch, got, tc.want)
			}
		})
	}
}

// TestPreferredReachabilityRef exercises the ref-selection decision behind
// gastownhall/gascity#5037 via an injected resolver, so the decision itself is
// pinned without spending a git process on each row; the wiring that hands
// those probes to real git is covered by the remote-tracking rows in
// TestCommitReachableOnBranch above. Per the issue's own guidance, this asserts
// the *resolved ref* rather than relying on "no warning" as a proxy — the
// pre-fix code was fail-closed and silently wrong for some bead types, so
// absence of a warning was never sufficient evidence.
func TestPreferredReachabilityRef(t *testing.T) {
	tests := []struct {
		name           string
		branch         string
		remoteResolves map[string]bool
		want           string
	}{
		{
			name:           "prefers the remote-tracking ref when it resolves",
			branch:         "main",
			remoteResolves: map[string]bool{"refs/remotes/origin/main": true},
			want:           "refs/remotes/origin/main",
		},
		{
			name:           "falls back to the bare branch name when no remote-tracking ref exists",
			branch:         "main",
			remoteResolves: map[string]bool{},
			want:           "main",
		},
		{
			name:           "does not confuse a different branch's remote-tracking ref for this one",
			branch:         "main",
			remoteResolves: map[string]bool{"refs/remotes/origin/release": true},
			want:           "main",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PreferredReachabilityRef(tc.branch, func(ref string) bool { return tc.remoteResolves[ref] })
			if got != tc.want {
				t.Fatalf("PreferredReachabilityRef(%q, ...) = %q, want %q", tc.branch, got, tc.want)
			}
		})
	}
}

// TestCommitReachableOnEitherRef pins the union: the remote-tracking ref is
// preferred, but a commit reachable only from the local branch still passes.
func TestCommitReachableOnEitherRef(t *testing.T) {
	tests := []struct {
		name           string
		remoteResolves map[string]bool
		reachable      map[string]bool
		want           bool
	}{
		{"remote resolves and contains the commit", map[string]bool{"refs/remotes/origin/main": true}, map[string]bool{"refs/remotes/origin/main": true}, true},
		{"remote is stale, local branch contains the commit", map[string]bool{"refs/remotes/origin/main": true}, map[string]bool{"main": true}, true},
		{"neither ref contains the commit", map[string]bool{"refs/remotes/origin/main": true}, map[string]bool{}, false},
		{"no remote-tracking ref, local branch contains the commit", map[string]bool{}, map[string]bool{"main": true}, true},
		{"no remote-tracking ref, local branch does not", map[string]bool{}, map[string]bool{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probes := map[string]int{}
			got := CommitReachableOnEitherRef("main",
				func(ref string) bool { return tc.remoteResolves[ref] },
				func(ref string) bool { probes[ref]++; return tc.reachable[ref] })
			if got != tc.want {
				t.Fatalf("CommitReachableOnEitherRef = %v, want %v", got, tc.want)
			}
			if probes["main"] > 1 {
				t.Fatalf("local ref probed %d times, want at most 1", probes["main"])
			}
		})
	}
}
