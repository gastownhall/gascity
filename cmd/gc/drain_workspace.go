package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worktree"
)

// sharedDrainWorkspaceOwner is the creator and owner recorded in the
// provenance of every shared drain workspace.
const sharedDrainWorkspaceOwner = "gc-drain"

// sharedDrainWorkspaceTimeout bounds one provisioning call, checkout
// included. The serve loop works its controls one at a time, so a checkout
// that never ends would stall every control of the rig; this is the bound an
// exec check has in the same loop (convergence.DefaultGateTimeout).
const sharedDrainWorkspaceTimeout = 5 * time.Minute

// sharedDrainWorkspaceProvisioner returns the dispatch hook that gives a
// shared exclusive drain control one managed worktree of its rig. The
// worktree lives at <worktrees root>/<rig>/<workflow root>-<control>, on the
// branch gc/drain/<control> cut from the rig's origin default branch
// (sharedDrainWorkspaceBase) with no upstream, so a bare push from it never
// targets the default branch, and its provenance names the control. git
// checks it out with the rig's hooks disabled, within
// sharedDrainWorkspaceTimeout. A repeat call re-verifies and returns the same
// canonical path. A control whose store is not a configured rig, or whose rig
// path git answers is not the top level of a repository (none at all, or a
// directory inside another repository's checkout), has no workspace to
// provision and gets ("", nil).
func sharedDrainWorkspaceProvisioner(cityPath, storePath string, cfg *config.City) func(beads.Bead) (string, error) {
	return func(control beads.Bead) (string, error) {
		storeRef := workflowStoreRefForDir(storePath, cityPath, loadedCityName(cfg, cityPath), cfg)
		rigName, ok := strings.CutPrefix(storeRef, "rig:")
		if !ok {
			return "", nil
		}
		rig, ok := rigByName(cfg, rigName)
		if !ok {
			return "", nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), sharedDrainWorkspaceTimeout)
		defer cancel()
		return provisionSharedDrainWorkspace(ctx, cityPath, storeRef, rig, control)
	}
}

// provisionSharedDrainWorkspace ensures the drain worktree of control in rig
// and returns its canonical path, or "" when the rig path is not the top
// level of a git repository.
func provisionSharedDrainWorkspace(ctx context.Context, cityPath, storeRef string, rig config.Rig, control beads.Bead) (string, error) {
	repo := rig.Path
	if !filepath.IsAbs(repo) {
		repo = filepath.Join(cityPath, repo)
	}
	isRepo, err := git.New(repo).ProbeRepoRootCtx(ctx)
	if err != nil {
		return "", fmt.Errorf("drain %s: rig %q: %w", control.ID, rig.Name, err)
	}
	if !isRepo {
		return "", nil
	}
	rootID := strings.TrimSpace(control.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" {
		return "", fmt.Errorf("drain %s has no %s", control.ID, beadmeta.RootBeadIDMetadataKey)
	}
	base, err := sharedDrainWorkspaceBase(ctx, repo, rig)
	if err != nil {
		return "", fmt.Errorf("drain %s: %w", control.ID, err)
	}
	worktreesRoot := workdirutil.WorktreesRoot(cityPath)
	root := filepath.Join(worktreesRoot, rig.Name)
	spec := worktree.Spec{
		RepoDir:    repo,
		Path:       filepath.Join(root, rootID+"-"+control.ID),
		Root:       root,
		Branch:     "gc/drain/" + control.ID,
		Base:       base,
		BeadID:     control.ID,
		StoreRef:   storeRef,
		Creator:    sharedDrainWorkspaceOwner,
		Owner:      sharedDrainWorkspaceOwner,
		Generation: control.ID,
		Lifecycle:  worktree.LifecycleActive,
		NoHooks:    true,
		NoTrack:    true,
	}
	rep, err := worktree.EnsureContext(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("ensuring shared workspace for drain %s in rig %q under %s (the worktrees root is GC_WORKTREES_DIR, else T3CODE_WORKTREES_DIR, else $T3CODE_HOME/worktrees, else <city>/.gc/worktrees, and must lie outside the rig checkout): %w",
			control.ID, rig.Name, worktreesRoot, err)
	}
	if rep.Provenance == nil {
		return "", fmt.Errorf("ensuring shared workspace for drain %s in rig %q: worktree %s has no provenance", control.ID, rig.Name, rep.Path)
	}
	return rep.Provenance.Path, nil
}

// sharedDrainWorkspaceBase returns the ref a shared drain's branch is cut
// from: refs/remotes/origin/<branch>, where branch is the rig's
// default_branch, else the one refs/remotes/origin/HEAD names (then
// origin/main, then origin/master; git.DefaultBranchCtx). That is the base
// the single-lane sibling do-work cuts its worktree from, read as last
// fetched: the controller does not fetch. A ref that does not resolve is an
// error, never a fallback to the rig checkout's HEAD, which can be any branch
// (a deploy branch far behind origin, say).
func sharedDrainWorkspaceBase(ctx context.Context, repo string, rig config.Rig) (string, error) {
	branch := rig.EffectiveDefaultBranch()
	if branch == "" {
		probed, err := git.New(repo).DefaultBranchCtx(ctx)
		if err != nil {
			return "", fmt.Errorf("probing rig %q for its default branch: %w", rig.Name, err)
		}
		branch = probed
	}
	base := "refs/remotes/origin/" + branch
	if _, err := git.New(repo).RevParseVerifyCommitCtx(ctx, base); err != nil {
		return "", fmt.Errorf("rig %q has no %s to cut the drain worktree from (set the rig's default_branch, or fetch its origin): %w", rig.Name, base, err)
	}
	return base, nil
}
