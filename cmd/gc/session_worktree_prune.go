package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// gitProbe is the slice of internal/git.Git used by the worker-dir
// auto-prune path. Defined as an interface so tests can inject a fake
// without standing up real git worktrees.
type gitProbe interface {
	IsRepo() bool
	HasUncommittedWork() bool
	HasUnpushedCommitsResult() (bool, error)
	HasStashesResult() (bool, error)
	WorktreeRemove(path string, force bool) error
}

// newGitProbe returns a gitProbe scoped to the given directory. Indirected
// through a package-level var so tests can stub the git invocations.
var newGitProbe = func(workDir string) gitProbe { return git.New(workDir) }

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
		return false
	}
	hasUnpushed, err := gp.HasUnpushedCommitsResult()
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: unpushed probe failed: %v\n", workerDir, err) //nolint:errcheck
		return false
	}
	if hasUnpushed {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: has unpushed commits\n", workerDir) //nolint:errcheck
		return false
	}
	hasStashes, err := gp.HasStashesResult()
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: stash probe failed: %v\n", workerDir, err) //nolint:errcheck
		return false
	}
	if hasStashes {
		fmt.Fprintf(stderr, "session reconciler: not pruning worker_dir %s: has stashed work\n", workerDir) //nolint:errcheck
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
