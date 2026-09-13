package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// addMergedBranchWorktree builds the shape every successfully merged bead
// leaves behind: a per-bead worktree on its own branch, pushed to origin, whose
// remote branch the merge queue then deleted. HEAD is still reachable from the
// local branch, but no remote-tracking ref reaches it anymore.
func addMergedBranchWorktree(t *testing.T, rigRoot, cityPath, agentHome, beadID string) string {
	t.Helper()
	wtPath := filepath.Join(cityPath, ".gc", "worktrees", reapTestRigName, agentHome, beadID)
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		t.Fatalf("mkdir worktree parent: %v", err)
	}
	branch := "polecat/" + beadID
	mustGit(t, rigRoot, "worktree", "add", "-b", branch, wtPath)
	mustGit(t, wtPath, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "work ("+beadID+")")
	mustGit(t, wtPath, "push", "origin", branch)
	mustGit(t, rigRoot, "push", "origin", "--delete", branch)
	mustGit(t, rigRoot, "fetch", "--prune", "origin")
	backdateWorktreeGitFile(t, wtPath, 24*time.Hour)
	return wtPath
}

// TestReapClosedBeadWorktrees_ReapsWorktreeWhoseMergedBranchWasDeleted is the
// regression test for the monotonic worktree leak (ga-uh1m). The git-safety
// gate used to ask "are there commits no remote-tracking ref reaches?", which
// is permanently true once the merge queue deletes the merged branch from
// origin. Every cleanly merged bead therefore latched the gate on forever and
// its worktree could never be reaped, so disk grew without bound.
//
// Removing a worktree deletes the checkout, not refs/heads — the commits stay
// reachable from the local branch — so this tree holds nothing at risk and must
// be reaped.
func TestReapClosedBeadWorktrees_ReapsWorktreeWhoseMergedBranchWasDeleted(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addMergedBranchWorktree(t, rigRoot, cityPath, "builder", "ga-merged1")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-merged1", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 {
		t.Fatalf("Reaped = %+v, want exactly 1: a merged bead's worktree whose branch origin no longer carries must be reaped\nProtected: %+v\nstderr:\n%s",
			report.Reaped, report.Protected, stderr.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree %s still on disk after reap (stat err = %v)", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ProtectsDetachedHEADWithOrphanCommits pins the
// safety half of the same gate. A detached HEAD carrying a commit that no
// branch, tag, or remote reaches WOULD be orphaned by removing the worktree, so
// the reaper must still protect it and say why.
func TestReapClosedBeadWorktrees_ProtectsDetachedHEADWithOrphanCommits(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := filepath.Join(cityPath, ".gc", "worktrees", reapTestRigName, "builder", "ga-orphan1")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatalf("mkdir worktree parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "--detach", wt)
	mustGit(t, wt, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "orphan work")
	backdateWorktreeGitFile(t, wt, 24*time.Hour)

	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-orphan1", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0: a detached HEAD holding commits no ref reaches must be protected", report.Reaped)
	}
	if len(report.Protected) != 1 {
		t.Fatalf("Protected = %+v, want exactly 1 entry", report.Protected)
	}
	if !strings.Contains(report.Protected[0].Reason, "unreachable=true") {
		t.Errorf("Reason = %q, want it to report unreachable=true", report.Protected[0].Reason)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("protected worktree %s was removed: %v", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ReapsWorktreeNestedUnderBeadNamedParent covers
// the second shape that leaked: a worktree whose own directory is a fixed
// "worktree" leaf nested under a bead-named parent. The bead ID was read from
// the leaf name only, so these trees resolved to no bead and were skipped
// before any gate ran — never reaped, never even reported as protected.
func TestReapClosedBeadWorktrees_ReapsWorktreeNestedUnderBeadNamedParent(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := filepath.Join(cityPath, ".gc", "worktrees", reapTestRigName, "builder", "ga-nested1-some-descriptive-slug", "worktree")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatalf("mkdir worktree parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "polecat/ga-nested1", wt)
	backdateWorktreeGitFile(t, wt, 24*time.Hour)

	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-nested1", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 {
		t.Fatalf("Reaped = %+v, want exactly 1: the bead ID must resolve from the parent directory when the leaf carries none\nProtected: %+v\nstderr:\n%s",
			report.Reaped, report.Protected, stderr.String())
	}
	if report.Reaped[0].BeadID != "ga-nested1" {
		t.Errorf("Reaped[0].BeadID = %q, want ga-nested1", report.Reaped[0].BeadID)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree %s still on disk after reap (stat err = %v)", wt, err)
	}
}

// TestReapClosedBeadWorktrees_IgnoresWorktreeWithNoResolvableBead pins the
// bound on that fallback: it climbs exactly one level and still requires a
// bead-shaped name, so a worktree under a non-bead parent resolves to nothing
// and is left entirely alone.
func TestReapClosedBeadWorktrees_IgnoresWorktreeWithNoResolvableBead(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := filepath.Join(cityPath, ".gc", "worktrees", reapTestRigName, "builder", "scratchpad", "worktree")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatalf("mkdir worktree parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "scratch", wt)
	backdateWorktreeGitFile(t, wt, 24*time.Hour)

	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-nested1", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0: a worktree under a non-bead parent resolves to no bead", report.Reaped)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("worktree %s with no resolvable bead was removed: %v", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ProtectsOnGitProbeError pins that a failed git
// probe protects the worktree and names the failure, rather than being
// discarded so an errored probe reads as "clean". The reaper's contract is that
// every gate fails closed; a swallowed probe error would fail open.
func TestReapClosedBeadWorktrees_ProtectsOnGitProbeError(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-broken1")

	// Point the worktree's .git pointer file at a path that does not exist.
	// `git worktree list` still enumerates it from the rig's admin data, and
	// the file still stats (so the freshness gate stays determinate), but every
	// git command run inside the worktree now fails.
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /nonexistent/gc-reaper-probe-test\n"), 0o644); err != nil {
		t.Fatalf("corrupt worktree .git pointer: %v", err)
	}
	backdateWorktreeGitFile(t, wt, 24*time.Hour)

	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-broken1", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0: an errored git probe must not read as a clean tree", report.Reaped)
	}
	if len(report.Protected) != 1 {
		t.Fatalf("Protected = %+v, want exactly 1 entry", report.Protected)
	}
	if !strings.Contains(report.Protected[0].Reason, "git probe failed") {
		t.Errorf("Reason = %q, want it to name the probe failure", report.Protected[0].Reason)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("protected worktree %s was removed: %v", wt, err)
	}
}
