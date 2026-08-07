package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// gitProbe is the slice of internal/git.Git used by the worker-dir
// auto-prune path. Defined as an interface so tests can inject a fake
// without standing up real git worktrees.
type gitProbe interface {
	IsRepo() bool
	CurrentBranch() (string, error)
	HasUncommittedWork() bool
	HasUnpushedCommitsResult() (bool, error)
	HasStashesResult() (bool, error)
	StatusPorcelain() (string, error)
	WorktreeRemove(path string, force bool) error
}

// newGitProbe returns a gitProbe scoped to the given directory. Indirected
// through a package-level var so tests can stub the git invocations.
var newGitProbe = func(workDir string) gitProbe { return git.New(workDir) }

// Reason codes recorded in a .worktree-stale marker's reason= header field
// by writeWorktreeStaleMarker. These are the worker_dir-reclaim gates; they
// are disjoint from the session-start gates in
// packs/gastown/scripts/worktree-setup.sh (rebase-onto-main-conflicted and
// friends), which fire on a different scenario and write their own marker.
const (
	worktreeStaleReasonUncommittedWork = "uncommitted-work"
	worktreeStaleReasonUnpushedCommits = "unpushed-commits"
	worktreeStaleReasonStashedWork     = "stashed-work"
)

// writeWorktreeStaleMarker records why workerDir was left in place instead of
// pruned, so cleanupClosedBeadAgentHomeWorktrees (agent_home_worktree_cleanup.go)
// can later detect when it's safe to reclaim. Best-effort: write failures are
// logged but never alter the caller's control flow.
//
// The marker follows the same contract as
// packs/gastown/scripts/worktree-setup.sh's write_stale_marker: a
// machine-readable header (branch=, worktree=, reason=, blocking=, at=)
// census/recovery tooling can grep, followed by a blank line and
// self-contained prose for whoever finds the marker next.
func writeWorktreeStaleMarker(gp gitProbe, workerDir, reason string, stderr io.Writer) {
	branch, err := gp.CurrentBranch()
	if err != nil {
		branch = ""
	}
	var dirty string
	if reason == worktreeStaleReasonUncommittedWork {
		if status, statusErr := gp.StatusPorcelain(); statusErr == nil {
			dirty = status
		}
	}
	// blocking=no: the reconciler declined to prune an already-closed
	// worktree. It does not stop the live agent still working in it.
	header := fmt.Sprintf("branch=%s\nworktree=%s\nreason=%s\nblocking=no\nat=%s\n\n",
		branch, workerDir, reason, time.Now().UTC().Format(time.RFC3339))
	content := header + worktreeStaleGuidance(reason, dirty)
	if err := os.WriteFile(filepath.Join(workerDir, worktreeStaleFileName), []byte(content), 0o644); err != nil {
		fmt.Fprintf(stderr, "session reconciler: writing %s marker for %s: %v\n", worktreeStaleFileName, workerDir, err) //nolint:errcheck
	}
}

// worktreeStaleGuidance returns the self-contained WHAT HAPPENED / WHAT TO
// DO / NEVER prose for a .worktree-stale marker. Mirrors the contract
// packs/gastown/scripts/worktree-setup.sh's stale_marker_guidance
// establishes for its own (disjoint) reason codes: an agent recovering
// from a marker reads this text and nothing else, so every reason code —
// including one this function doesn't recognize — must produce guidance
// rather than leave the reader with nothing. Commands are written bare
// (no "git -C"), since the reader is standing in the worktree the marker
// lives in. Unlike the shell writer's markers, none of these three
// conditions require the reader to `rm .worktree-stale` by hand: the
// session reconciler retries the prune automatically on its next pass
// once the blocking condition clears.
func worktreeStaleGuidance(reason, dirtyPaths string) string {
	switch reason {
	case worktreeStaleReasonUncommittedWork:
		var dirtyBlock string
		if trimmed := strings.TrimRight(dirtyPaths, "\n"); trimmed != "" {
			var b strings.Builder
			b.WriteString("\nDirty paths:\n")
			for _, line := range strings.Split(trimmed, "\n") {
				b.WriteString("  ")
				b.WriteString(strings.TrimSpace(line))
				b.WriteString("\n")
			}
			dirtyBlock = b.String()
		}
		return fmt.Sprintf(`WHAT HAPPENED
  This worktree still has uncommitted changes, so the session reconciler
  left it in place instead of removing it. Nothing was discarded.
%s
WHAT TO DO
  1. Inspect what's dirty:
       git status --porcelain
  2. Commit what you want to keep, or discard what you don't:
       git add -A && git commit -m "..."
     or
       git restore .
  3. No further action needed after that: the next reconciler pass
     retries automatically and removes this worktree once it's clean.

NEVER
  Do not delete this worktree by hand with rm -rf. That destroys
  uncommitted work before you've had a chance to look at it.
`, dirtyBlock)

	case worktreeStaleReasonUnpushedCommits:
		return `WHAT HAPPENED
  This worktree has commits that haven't been pushed to origin, so the
  session reconciler left it in place instead of removing it. Nothing
  was discarded.

WHAT TO DO
  1. See what hasn't been pushed:
       git log --oneline @{u}..HEAD
  2. Push the branch so the commits are safe on the remote:
       git push -u origin HEAD
  3. No further action needed after that: the next reconciler pass
     retries automatically and removes this worktree once it's pushed.

NEVER
  Do not delete this worktree by hand with rm -rf. That destroys commits
  that exist nowhere else.
`

	case worktreeStaleReasonStashedWork:
		return `WHAT HAPPENED
  This worktree has stashed changes, so the session reconciler left it
  in place instead of removing it. Nothing was discarded.

WHAT TO DO
  1. See what's stashed:
       git stash list
  2. Restore it if you still need it:
       git stash pop
     or drop it if you don't:
       git stash drop
  3. No further action needed after that: the next reconciler pass
     retries automatically and removes this worktree once the stash is
     gone.

NEVER
  Do not delete this worktree by hand with rm -rf. That destroys stashed
  work that exists nowhere else.
`

	default:
		return fmt.Sprintf(`WHAT HAPPENED
  The session reconciler left this worktree marked as needing attention,
  with reason=%s. Nothing was discarded -- these markers never remove
  work on their own.

WHAT TO DO
  1. Inspect the state before changing anything:
       git status
       git log --oneline -5
  2. Once you understand what's blocking cleanup, resolve it. The next
     reconciler pass retries automatically and removes this worktree
     once nothing is blocking it.

NEVER
  Do not delete this worktree by hand with rm -rf, and do not run
  git reset --hard to force past this without understanding why it's
  here.
`, reason)
	}
}

// pruneAgentHomeWorktreeIfSafe removes the worktree at the closed session's
// worker_dir, after applying the same safety gates as doctor's
// NestedWorktreePruneCheck. Returns true when the removal actually
// happened.
//
// The decision is mechanical, never role-coupled: any pool-managed agent
// worktree that lives under the city's .gc/worktrees/ tree, is a git
// worktree, and probes clean is safe to reclaim. Pool sessions are
// transient by design — their worktrees were never meant to outlive the
// session bead.
//
// No-op when:
//   - cfg.Daemon.AutoPruneWorkerDir is false
//   - the session bead has no worker_dir metadata
//   - the worker_dir does not live under cityPath/.gc/worktrees/
//   - the worker_dir is missing on disk or has no .git pointer
//   - the worktree has uncommitted changes, unpushed commits, or stashes
//   - the rig that owns the session cannot be resolved to a filesystem path
//
// Removal failures are logged but never surfaced — an orphaned worktree
// still shows up via `gc doctor` later, which is the operator's existing
// reclaim path.
func pruneAgentHomeWorktreeIfSafe(session beads.Bead, cityPath string, cfg *config.City, stderr io.Writer) bool {
	if cfg == nil || !cfg.Daemon.AutoPruneWorkerDirEnabled() {
		return false
	}
	workerDir := strings.TrimSpace(contract.WorkerDirFromMetadata(session.Metadata))
	if workerDir == "" {
		return false
	}
	if !filepath.IsAbs(workerDir) {
		return false
	}

	wtRoot := filepath.Join(cityPath, ".gc", "worktrees")
	if !pathutil.PathWithin(wtRoot, workerDir) || pathutil.SamePath(wtRoot, workerDir) {
		return false
	}

	if _, err := os.Stat(filepath.Join(workerDir, ".git")); err != nil {
		// Already gone, or never a worktree — nothing to do.
		return false
	}

	gp := newGitProbe(workerDir)
	if !gp.IsRepo() {
		return false
	}
	if gp.HasUncommittedWork() {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: has uncommitted changes\n", workerDir) //nolint:errcheck
		writeWorktreeStaleMarker(gp, workerDir, worktreeStaleReasonUncommittedWork, stderr)
		return false
	}
	hasUnpushed, err := gp.HasUnpushedCommitsResult()
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: unpushed probe failed: %v\n", workerDir, err) //nolint:errcheck
		return false
	}
	if hasUnpushed {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: has unpushed commits\n", workerDir) //nolint:errcheck
		writeWorktreeStaleMarker(gp, workerDir, worktreeStaleReasonUnpushedCommits, stderr)
		return false
	}
	hasStashes, err := gp.HasStashesResult()
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: stash probe failed: %v\n", workerDir, err) //nolint:errcheck
		return false
	}
	if hasStashes {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: has stashed work\n", workerDir) //nolint:errcheck
		writeWorktreeStaleMarker(gp, workerDir, worktreeStaleReasonStashedWork, stderr)
		return false
	}

	// Run `git worktree remove` from the rig root rather than from the
	// worktree being removed: git refuses to remove a worktree whose path
	// equals cwd in some configurations, and operating from cwd of a
	// directory we are about to delete is fragile in general.
	rigRoot := lookupRigRootForSession(session, cfg)
	if rigRoot == "" {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: rig path unresolved\n", workerDir) //nolint:errcheck
		return false
	}
	if err := newGitProbe(rigRoot).WorktreeRemove(workerDir, true); err != nil {
		fmt.Fprintf(stderr, "session reconciler: pruning worker_dir %s: %v\n", workerDir, err) //nolint:errcheck
		return false
	}
	fmt.Fprintf(stderr, "session reconciler: pruned worker_dir %s (session %s)\n", workerDir, session.Metadata["session_name"]) //nolint:errcheck
	return true
}

// pruneAgentHomeWorktreeIfSafeInfo is the session.Info form of
// pruneAgentHomeWorktreeIfSafe: the worker_dir read routes through
// session.WorkerDirFromInfo (the canonical→legacy Info fallback equivalent to
// contract.WorkerDirFromMetadata), the rig-root lookup reads Info.Template via
// lookupRigRootForSessionInfo, and the log line reads Info.SessionNameMetadata —
// every safety gate and the removal itself are unchanged. Byte-identical to the
// raw form, which survives for its test callers.
func pruneAgentHomeWorktreeIfSafeInfo(info sessionpkg.Info, cityPath string, cfg *config.City, stderr io.Writer) {
	if cfg == nil || !cfg.Daemon.AutoPruneWorkerDirEnabled() {
		return
	}
	workerDir := strings.TrimSpace(sessionpkg.WorkerDirFromInfo(info))
	if workerDir == "" {
		return
	}
	if !filepath.IsAbs(workerDir) {
		return
	}

	wtRoot := filepath.Join(cityPath, ".gc", "worktrees")
	if !pathutil.PathWithin(wtRoot, workerDir) || pathutil.SamePath(wtRoot, workerDir) {
		return
	}

	if _, err := os.Stat(filepath.Join(workerDir, ".git")); err != nil {
		return
	}

	gp := newGitProbe(workerDir)
	if !gp.IsRepo() {
		return
	}
	if gp.HasUncommittedWork() {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: has uncommitted changes\n", workerDir) //nolint:errcheck
		writeWorktreeStaleMarker(gp, workerDir, worktreeStaleReasonUncommittedWork, stderr)
		return
	}
	hasUnpushed, err := gp.HasUnpushedCommitsResult()
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: unpushed probe failed: %v\n", workerDir, err) //nolint:errcheck
		return
	}
	if hasUnpushed {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: has unpushed commits\n", workerDir) //nolint:errcheck
		writeWorktreeStaleMarker(gp, workerDir, worktreeStaleReasonUnpushedCommits, stderr)
		return
	}
	hasStashes, err := gp.HasStashesResult()
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: stash probe failed: %v\n", workerDir, err) //nolint:errcheck
		return
	}
	if hasStashes {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: has stashed work\n", workerDir) //nolint:errcheck
		writeWorktreeStaleMarker(gp, workerDir, worktreeStaleReasonStashedWork, stderr)
		return
	}

	rigRoot := lookupRigRootForSessionInfo(info, cfg)
	if rigRoot == "" {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: rig path unresolved\n", workerDir) //nolint:errcheck
		return
	}
	if err := newGitProbe(rigRoot).WorktreeRemove(workerDir, true); err != nil {
		fmt.Fprintf(stderr, "session reconciler: pruning worker_dir %s: %v\n", workerDir, err) //nolint:errcheck
		return
	}
	fmt.Fprintf(stderr, "session reconciler: pruned worker_dir %s (session %s)\n", workerDir, info.SessionNameMetadata) //nolint:errcheck
}

// lookupRigRootForSession returns the filesystem path of the rig that owns
// the given session bead, derived from the qualified template metadata
// ("<rig>/<template>"). Returns "" when the rig cannot be identified or
// has no configured path.
func lookupRigRootForSession(session beads.Bead, cfg *config.City) string {
	qt := strings.TrimSpace(session.Metadata["template"])
	slash := strings.IndexByte(qt, '/')
	if slash <= 0 {
		return ""
	}
	rigName := qt[:slash]
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName {
			return strings.TrimSpace(cfg.Rigs[i].Path)
		}
	}
	return ""
}

// lookupRigRootForSessionInfo is the session.Info form of
// lookupRigRootForSession: it reads the qualified template off Info.Template (the
// verbatim raw mirror of b.Metadata["template"]), so the rig resolution is
// byte-identical to the raw form.
func lookupRigRootForSessionInfo(info sessionpkg.Info, cfg *config.City) string {
	qt := strings.TrimSpace(info.Template)
	slash := strings.IndexByte(qt, '/')
	if slash <= 0 {
		return ""
	}
	rigName := qt[:slash]
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName {
			return strings.TrimSpace(cfg.Rigs[i].Path)
		}
	}
	return ""
}
