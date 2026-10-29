package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worktree"
)

// sharedDrainWorkspaceOwner names the single provisioner — and lifecycle
// owner — of the worktree a shared drain works in. worktree ownership keys
// carry it as both Creator and Owner, so cleanup paths can distinguish a
// drain workspace from a session or pool one.
const sharedDrainWorkspaceOwner = "gc-drain"

// sharedDrainWorkspaceProvisioner returns the dispatch.SharedDrainWorkspace
// hook for control dispatch at a given scope: it ensures ONE managed worktree
// per shared drain control, rooted in the rig the control's store lives in,
// and returns its canonical path.
//
// The hook answers for the scope storePath names. A control dispatched from a
// rig store (storePath inside a registered rig's path) gets a worktree under
// the configured worktrees root, one directory per rig, named
// <workflow root id>-<drain control id> on the attached branch gc/drain/<drain
// control id>, created from the rig checkout's local HEAD. A control
// dispatched from the city, or from a directory that is not a git repository,
// gets ("", nil): there is no single checkout to root a workspace in, and the
// drain decides (traces) rather than failing.
//
// The workspace is intentionally NOT torn down with the drain: downstream
// summarize, review, finalize and publish steps read the source anchors'
// worktrees after the drain closes. Removal is owner-mediated through the
// normal worktree verify/cleanup commands, which this provenance binds to the
// drain control that owns it.
func sharedDrainWorkspaceProvisioner(cityPath, storePath string, cfg *config.City) func(beads.Bead) (string, error) {
	return func(control beads.Bead) (string, error) {
		storeRef := workflowStoreRefForDir(storePath, cityPath, loadedCityName(cfg, cityPath), cfg)
		if !strings.HasPrefix(storeRef, "rig:") {
			// A city-scoped control, or one dispatched from an unregistered
			// directory: no rig checkout backs this scope.
			return "", nil
		}
		rigName := strings.TrimPrefix(storeRef, "rig:")
		rig, ok := rigByName(cfg, rigName)
		if !ok {
			// Unreachable for a ref workflowStoreRefForDir derived from the
			// same config; kept so a nil cfg cannot panic.
			return "", nil
		}
		repo := rig.Path
		if !filepath.IsAbs(repo) {
			repo = filepath.Join(cityPath, repo)
		}
		if !git.New(repo).IsRepo() {
			// A rig registered against a plain directory has no checkout to
			// branch from; its packs prepare their own workspaces.
			return "", nil
		}
		rootID := strings.TrimSpace(control.Metadata[beadmeta.RootBeadIDMetadataKey])
		if rootID == "" {
			return "", fmt.Errorf("shared drain %s: missing gc.root_bead_id", control.ID)
		}
		root := filepath.Join(workdirutil.WorktreesRoot(cityPath), rig.Name)
		spec := worktree.Spec{
			RepoDir: repo,
			Root:    root,
			Path:    filepath.Join(root, rootID+"-"+control.ID),
			Branch:  "gc/drain/" + control.ID,
			// The registered rig checkout is the city's authority; fetching a
			// remote default belongs in an operator step with a network
			// budget, not in ProcessControl. worktree.Ensure records the
			// resolved SHA, so a moving rig HEAD does not break re-verification.
			Base:     "HEAD",
			BeadID:   control.ID,
			StoreRef: storeRef,
			// The drain control is the workspace's owner; only it names a
			// stable generation, so every reservation re-verifies the same slot.
			Creator:    sharedDrainWorkspaceOwner,
			Owner:      sharedDrainWorkspaceOwner,
			Generation: control.ID,
			Lifecycle:  worktree.LifecycleActive,
		}
		rep, err := worktree.Ensure(spec)
		if err != nil {
			return "", fmt.Errorf("ensuring shared workspace for drain %s in rig %q under %q (worktrees root: GC_WORKTREES_DIR, default %s; it must lie outside the rig checkout): %w",
				control.ID, rig.Name, root, filepath.Join(cityPath, ".gc", "worktrees"), err)
		}
		if rep.Provenance == nil {
			return "", fmt.Errorf("ensuring shared workspace for drain %s in rig %q: no provenance recorded at %q", control.ID, rig.Name, rep.Path)
		}
		return rep.Provenance.Path, nil
	}
}
