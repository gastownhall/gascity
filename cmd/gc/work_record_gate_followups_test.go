package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// --- finding 2: bug and feature beads are as worker-claimable as tasks ---

// TestIsWorkRecordGatedBeadGatesWorkerClaimableTypes pins finding 2 of
// gastownhall/gascity#5037: the gate keyed on Type != "task", so bug beads —
// claimed, worked, and closed by workers exactly like tasks — bypassed the
// work-record contract silently. Both silent rows in that issue's six-close
// table were type bug.
//
// feature is gated for the same reason (raised in the #5120 review): it is a bd
// built-in claimed, worked, and closed with an outcome exactly as task and bug
// are, and internal/runproj/summary.go already groups it with them as an
// engineering type. A feature closed as shipped with no commit is the very
// defect this gate exists to catch, one type over.
//
// The exclusions are equally load-bearing: epic, story, and milestone are
// containers whose closes summarize child work rather than report an artifact,
// and decision records a choice, not delivered work. None of them is a unit a
// worker claims and ships, so the commit contract does not apply.
func TestIsWorkRecordGatedBeadGatesWorkerClaimableTypes(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want bool
	}{
		{name: "bug bead is gated", bead: beads.Bead{Type: "bug"}, want: true},
		{name: "feature bead is gated", bead: beads.Bead{Type: "feature"}, want: true},
		{name: "task bead stays gated", bead: beads.Bead{Type: "task"}, want: true},
		{name: "empty type stays gated", bead: beads.Bead{}, want: true},
		{
			name: "control-kind bug bead stays exempt",
			bead: beads.Bead{Type: "bug", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRun}},
			want: false,
		},
		{
			name: "control-kind feature bead stays exempt",
			bead: beads.Bead{Type: "feature", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRun}},
			want: false,
		},
		{name: "convoy bead stays exempt", bead: beads.Bead{Type: "convoy"}, want: false},
		{name: "message bead stays exempt", bead: beads.Bead{Type: "message"}, want: false},
		{name: "epic bead stays exempt", bead: beads.Bead{Type: "epic"}, want: false},
		{name: "story bead stays exempt", bead: beads.Bead{Type: "story"}, want: false},
		{name: "milestone bead stays exempt", bead: beads.Bead{Type: "milestone"}, want: false},
		{name: "decision bead stays exempt", bead: beads.Bead{Type: "decision"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWorkRecordGatedBead(tc.bead); got != tc.want {
				t.Fatalf("isWorkRecordGatedBead = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEvaluateWorkRecordCloseGateBlocksShippedWithoutCommit proves the widened
// gate actually arms for each newly gated bead class end to end, not just in
// the predicate: a bead closed as shipped with no commit must be blocked under
// enforcement, exactly as the same task bead already was.
func TestEvaluateWorkRecordCloseGateBlocksShippedWithoutCommit(t *testing.T) {
	for _, beadType := range []string{"bug", "feature"} {
		t.Run(beadType, func(t *testing.T) {
			id := "wr-" + beadType + "-shipped"
			preFetched := map[string]beads.Bead{
				id: {ID: id, Type: beadType, Status: "in_progress", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped}},
			}
			var stderr strings.Builder
			block := evaluateWorkRecordCloseGate([]string{"close", id}, panicOnGetStore{}, preFetched, t.TempDir(), true, &stderr)
			if !block {
				t.Fatalf("expected block=true for a shipped %s bead with no commit, got false; stderr=%s", beadType, stderr.String())
			}
		})
	}
}

// --- finding 3: reachability must resolve against a stable repo root ---

// TestWorkRecordRepoDirPrefersStableScopeRoot pins finding 3: gc.work_dir is a
// polecat worktree that is deleted at cleanup, so preferring it made every
// post-cleanup reachability probe fail closed on a missing directory — a false
// negative that blocks a legitimately delivered close. The rig root is stable,
// and worktrees share one object store, so it answers the same question.
func TestWorkRecordRepoDirPrefersStableScopeRoot(t *testing.T) {
	rigRoot := t.TempDir()
	bead := beads.Bead{Metadata: map[string]string{beadmeta.WorkDirMetadataKey: t.TempDir()}}
	if got := workRecordRepoDir(bead, rigRoot); got != rigRoot {
		t.Errorf("workRecordRepoDir = %q, want the stable scope root %q", got, rigRoot)
	}
}

// TestWorkRecordRepoDirFallsBackToWorkDir keeps the worktree usable when no
// scope root is known — the gate should still answer rather than fail closed.
func TestWorkRecordRepoDirFallsBackToWorkDir(t *testing.T) {
	workDir := t.TempDir()
	bead := beads.Bead{Metadata: map[string]string{beadmeta.WorkDirMetadataKey: workDir}}
	if got := workRecordRepoDir(bead, "  "); got != workDir {
		t.Errorf("workRecordRepoDir = %q, want the work_dir fallback %q", got, workDir)
	}
}

// TestWorkRecordRepoDirFallsBackWhenScopeRootIsNotOnDisk is the case sjarmak's
// #5120 review identified: preferring the scope root on non-emptiness alone
// relocates the false block rather than closing it.
//
// scopeRoot is never blank in production — resolveStoreScopeRoot
// (cmd/gc/main.go) falls back to the city path when the store path is blank —
// so an unconditional preference makes the gc.work_dir branch unreachable, a
// priority inversion rather than an added fallback. And nothing upstream proves
// the root is on disk: resolveBdScopeTarget (cmd/gc/cmd_bd.go) rejects a rig
// whose registered Path is empty, never one whose directory has been moved or
// removed. A stale registration therefore yields a non-empty root, the probe
// runs in a repo that does not contain the commit, and the gate calls a
// delivered close unreachable — the same failure this PR closes, moved from
// "worktree reaped" to "rig registration stale". Only an existence check keeps
// the preference honest.
func TestWorkRecordRepoDirFallsBackWhenScopeRootIsNotOnDisk(t *testing.T) {
	tests := []struct {
		name      string
		scopeRoot func(t *testing.T) string
	}{
		{
			name:      "rig directory moved or removed",
			scopeRoot: func(t *testing.T) string { return filepath.Join(t.TempDir(), "rig-moved-away") },
		},
		{
			name: "rig path is a file, not a directory",
			scopeRoot: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "rig-replaced-by-file")
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatalf("writing stand-in file: %v", err)
				}
				return path
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			bead := beads.Bead{Metadata: map[string]string{beadmeta.WorkDirMetadataKey: workDir}}
			if got := workRecordRepoDir(bead, tc.scopeRoot(t)); got != workDir {
				t.Errorf("workRecordRepoDir = %q, want the work_dir fallback %q", got, workDir)
			}
		})
	}
}

// TestWorkRecordRepoDirEmptyWhenNeitherKnown pins the fail-closed direction:
// with no repo to consult, gitCommitReachableOnBranch reads "" as not
// reachable rather than probing the process's cwd.
func TestWorkRecordRepoDirEmptyWhenNeitherKnown(t *testing.T) {
	if got := workRecordRepoDir(beads.Bead{}, ""); got != "" {
		t.Errorf("workRecordRepoDir = %q, want empty", got)
	}
}

// TestWorkRecordRepoDirEmptyWhenNeitherIsOnDisk extends that fail-closed
// direction to the both-stale case: a stale rig registration and a reaped
// worktree are both named but neither exists, so there is still no repo to
// consult. Returning either path would hand gitCommitReachableOnBranch a
// directory git cannot open; "" says so honestly.
func TestWorkRecordRepoDirEmptyWhenNeitherIsOnDisk(t *testing.T) {
	gone := t.TempDir()
	bead := beads.Bead{Metadata: map[string]string{beadmeta.WorkDirMetadataKey: filepath.Join(gone, "reaped-worktree")}}
	if got := workRecordRepoDir(bead, filepath.Join(gone, "rig-moved-away")); got != "" {
		t.Errorf("workRecordRepoDir = %q, want empty when neither root is on disk", got)
	}
}

// TestEvaluateWorkRecordCloseGateAllowsShippedCloseAfterWorktreePruned is the
// regression the gas-dr5 review confirmed with a concrete deputy-close
// scenario: the work landed, the commit is reachable on its branch in the rig
// repo, but the polecat worktree named by gc.work_dir has been reaped. Before
// the fix the git probe ran in a directory that no longer exists, errored, and
// the fail-closed reading blocked a delivered close.
func TestEvaluateWorkRecordCloseGateAllowsShippedCloseAfterWorktreePruned(t *testing.T) {
	repo, sha := newWorkRecordDeliveredRepo(t)
	prunedWorktree := filepath.Join(t.TempDir(), "reaped-polecat-worktree")

	var stderr strings.Builder
	block := evaluateWorkRecordCloseGate(
		[]string{"close", "wr-delivered"}, panicOnGetStore{},
		workRecordDeliveredBead(sha, prunedWorktree), repo, true, &stderr)
	if block {
		t.Fatalf("delivered close blocked after its worktree was pruned; the probe must resolve against the stable rig root. stderr=%s", stderr.String())
	}
}

// TestEvaluateWorkRecordCloseGateAllowsShippedCloseAfterRigMoved is the mirror
// regression sjarmak's #5120 review called for, and the reason the preference
// needs an existence check rather than an emptiness check. Here the roles are
// swapped: the rig registration is stale (its directory was moved away) while
// the worktree named by gc.work_dir is alive and holds the commit. Preferring a
// scope root that is merely non-empty ran the probe in a directory git cannot
// open, and the fail-closed reading blocked a delivered close — the same defect
// as the pruned-worktree case, one root over.
func TestEvaluateWorkRecordCloseGateAllowsShippedCloseAfterRigMoved(t *testing.T) {
	liveWorktree, sha := newWorkRecordDeliveredRepo(t)
	staleRigRoot := filepath.Join(t.TempDir(), "rig-moved-away")

	var stderr strings.Builder
	block := evaluateWorkRecordCloseGate(
		[]string{"close", "wr-delivered"}, panicOnGetStore{},
		workRecordDeliveredBead(sha, liveWorktree), staleRigRoot, true, &stderr)
	if block {
		t.Fatalf("delivered close blocked after its rig directory moved; the probe must fall through to the live work_dir. stderr=%s", stderr.String())
	}
}

// newWorkRecordDeliveredRepo builds a git repo holding one commit on
// "work-branch" and returns the repo path and that commit's SHA.
func newWorkRecordDeliveredRepo(t *testing.T) (repo, sha string) {
	t.Helper()
	repo = t.TempDir()
	mustGit(t, repo, "init", "-b", "work-branch")
	mustGit(t, repo, "config", "user.email", "test@test.com")
	mustGit(t, repo, "config", "user.name", "Test")
	mustGit(t, repo, "commit", "--allow-empty", "-m", "delivered work")
	// Read the SHA from the loose ref instead of shelling out for rev-parse:
	// the repository resource census budgets subprocess call sites, and a
	// fresh single-commit repo has not packed its refs.
	shaBytes, err := os.ReadFile(filepath.Join(repo, ".git", "refs", "heads", "work-branch"))
	if err != nil {
		t.Fatalf("reading work-branch ref: %v", err)
	}
	return repo, strings.TrimSpace(string(shaBytes))
}

// workRecordDeliveredBead is the pre-fetched bead both reachability regressions
// close: work shipped as sha on "work-branch", stamped with workDir.
func workRecordDeliveredBead(sha, workDir string) map[string]beads.Bead {
	return map[string]beads.Bead{
		"wr-delivered": {
			ID: "wr-delivered", Type: "task", Status: "in_progress",
			Metadata: map[string]string{
				beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
				beadmeta.WorkCommitMetadataKey:  sha,
				beadmeta.WorkBranchMetadataKey:  "work-branch",
				beadmeta.WorkDirMetadataKey:     workDir,
			},
		},
	}
}
