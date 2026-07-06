package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/git"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// worktreeProvisionMuGuard protects worktreeProvisionMus, the per-rig-root
// lock registry that serializes worktree provisioning across the parallel
// start wave: concurrent `git worktree add` invocations against the same
// repository can collide on repository-level lock files. Locking is keyed by
// rig root (rather than one global mutex) so concurrent starts against
// unrelated rigs proceed in parallel.
var (
	worktreeProvisionMuGuard sync.Mutex
	worktreeProvisionMus     = make(map[string]*sync.Mutex)
)

// worktreeProvisionLockFor returns the mutex serializing worktree
// provisioning for rigRoot, creating it on first use.
func worktreeProvisionLockFor(rigRoot string) *sync.Mutex {
	key := filepath.Clean(rigRoot)
	worktreeProvisionMuGuard.Lock()
	defer worktreeProvisionMuGuard.Unlock()
	mu, ok := worktreeProvisionMus[key]
	if !ok {
		mu = &sync.Mutex{}
		worktreeProvisionMus[key] = mu
	}
	return mu
}

// provisionSessionWorktree creates a git worktree of rigRoot at workDir,
// detached at the rig's current HEAD, so a session launched there starts in a
// real checkout instead of a bare staged directory. This is the create half of
// the per-bead worktree lifecycle whose reap half is reapClosedBeadWorktrees;
// both operate on workdirutil.WorktreesRoot(cityPath).
//
// The gate is declarative (the config's WHERE expresses intent — no role or
// bead-type heuristics): provisioning runs only when workDir lies strictly
// under the city worktrees root and a rig root is known. Directories that
// already contain a .git entry are left untouched, which makes the call
// idempotent across restarts. Pre-seeded slot contents (.claude, .gc) are
// preserved by git.WorktreeAdd's evacuate/restore handling.
func provisionSessionWorktree(cityPath, rigRoot, workDir string) error {
	cityPath = strings.TrimSpace(cityPath)
	rigRoot = strings.TrimSpace(rigRoot)
	workDir = strings.TrimSpace(workDir)
	if cityPath == "" || rigRoot == "" || workDir == "" || !filepath.IsAbs(workDir) {
		return nil
	}
	if !isStrictlyUnderDir(workdirutil.WorktreesRoot(cityPath), workDir) {
		return nil
	}

	mu := worktreeProvisionLockFor(rigRoot)
	mu.Lock()
	defer mu.Unlock()

	if _, err := os.Lstat(filepath.Join(workDir, ".git")); err == nil {
		// Already a worktree (or a repository); nothing to provision.
		return nil
	}
	if err := workdirutil.ValidateAncestorWorktreesNotStale(workDir); err != nil {
		return err
	}
	if err := git.New(rigRoot).WorktreeAdd(workDir, "HEAD", true); err != nil {
		return fmt.Errorf("provisioning worktree for %q from rig %q: %w", workDir, rigRoot, err)
	}
	return nil
}
