package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// TestDiscoverWorktreeLiveness_ReportsWorktreeOutsideGcOwnedRoot is the RED
// regression for ga-1xaqgo.1: a live process whose cwd is inside a Git
// worktree registered outside the gc-owned .gc/worktrees root (mirroring a
// worktree Claude Code's own EnterWorktree tool would create under
// .claude/worktrees) was previously omitted entirely, because the only path
// from "list of worktrees" to "is it live" was reapClosedBeadWorktrees's
// pass-1 loop, which discards anything outside .gc/worktrees/<rig>/ before
// liveness is ever computed for it (bead_worktree_reaper.go's
// pathutil.PathWithin scope check). discoverWorktreeLiveness applies no root
// convention, so the same foreign worktree must be visible and correctly
// reported live here even though the reaper will still never act on it.
func TestDiscoverWorktreeLiveness_ReportsWorktreeOutsideGcOwnedRoot(t *testing.T) {
	_, rigRoot := initReapRig(t)

	foreign := filepath.Join(t.TempDir(), ".claude", "worktrees", "some-session")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatalf("mkdir foreign worktree parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "foreign-branch", foreign)

	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(foreign)}}

	results, err := discoverWorktreeLiveness(rigRoot, live, nil)
	if err != nil {
		t.Fatalf("discoverWorktreeLiveness: %v", err)
	}

	var found *worktreeLiveness
	for i := range results {
		if pathutil.SamePath(results[i].Path, foreign) {
			found = &results[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("discoverWorktreeLiveness omitted worktree %s outside the gc-owned root; results=%+v", foreign, results)
	}
	if !found.Live {
		t.Fatalf("discoverWorktreeLiveness reported %s as not live despite a live process cwd inside it (reason=%q)", foreign, found.Reason)
	}
}

// TestDiscoverWorktreeLiveness_IncludesMainCheckout proves discovery is not
// scoped to per-bead worktrees at all: the rig's own main working tree (which
// WorktreeList also returns) is reported too, with the correct liveness for
// its own cwd. Downstream callers (the reaper) are responsible for filtering
// this down to what they may act on; discovery itself filters nothing.
func TestDiscoverWorktreeLiveness_IncludesMainCheckout(t *testing.T) {
	_, rigRoot := initReapRig(t)

	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(rigRoot)}}

	results, err := discoverWorktreeLiveness(rigRoot, live, nil)
	if err != nil {
		t.Fatalf("discoverWorktreeLiveness: %v", err)
	}

	var found *worktreeLiveness
	for i := range results {
		if pathutil.SamePath(results[i].Path, rigRoot) {
			found = &results[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("discoverWorktreeLiveness omitted the main checkout %s; results=%+v", rigRoot, results)
	}
	if !found.Live {
		t.Fatalf("discoverWorktreeLiveness reported main checkout %s as not live despite a live process cwd there", rigRoot)
	}
}

// TestDiscoverWorktreeLiveness_ScanUnavailableReportsNotLiveNoReason pins the
// contract mirrored from worktreeIsLive: discoverWorktreeLiveness does not
// itself fail closed. When live.scanned is false the scan is indeterminate,
// every result reports Live=false with an empty Reason, and the caller —
// exactly as it already must before calling worktreeIsLive directly — is
// responsible for treating an indeterminate scan as fail-closed rather than
// reading a false Live as a confirmed idle worktree.
func TestDiscoverWorktreeLiveness_ScanUnavailableReportsNotLiveNoReason(t *testing.T) {
	_, rigRoot := initReapRig(t)

	results, err := discoverWorktreeLiveness(rigRoot, liveWorktreeState{scanned: false}, nil)
	if err != nil {
		t.Fatalf("discoverWorktreeLiveness: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("discoverWorktreeLiveness returned no results for a repo with a main checkout")
	}
	for _, r := range results {
		if r.Live || r.Reason != "" {
			t.Fatalf("result %+v: want Live=false Reason=\"\" when the liveness scan is indeterminate", r)
		}
	}
}

// TestDiscoverWorktreeLiveness_InvalidRigRootErrors proves a rig whose path
// does not resolve to a Git repository returns an error rather than a silent
// empty result, matching git.Git.WorktreeList's own error contract.
func TestDiscoverWorktreeLiveness_InvalidRigRootErrors(t *testing.T) {
	notARepo := t.TempDir()

	_, err := discoverWorktreeLiveness(notARepo, liveWorktreeState{scanned: true}, nil)
	if err == nil {
		t.Fatal("discoverWorktreeLiveness returned no error for a non-repository rigRoot")
	}
}
