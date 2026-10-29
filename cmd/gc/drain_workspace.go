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

// sharedDrainWorkspaceOwner is the creator and owner recorded in the
// provenance of every shared drain workspace.
const sharedDrainWorkspaceOwner = "gc-drain"

// sharedDrainWorkspaceProvisioner returns the dispatch hook that gives a
// shared exclusive drain control one managed worktree of its rig. The
// worktree lives at <worktrees root>/<rig>/<workflow root>-<control>, on the
// branch gc/drain/<control> cut from the rig's HEAD, and its provenance names
// the control. Repeat calls re-verify and return the same canonical path. A
// control whose store is not a configured rig that is a git repository has no
// workspace to provision and gets ("", nil).
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
		repo := rig.Path
		if !filepath.IsAbs(repo) {
			repo = filepath.Join(cityPath, repo)
		}
		if !git.New(repo).IsRepo() {
			return "", nil
		}
		rootID := strings.TrimSpace(control.Metadata[beadmeta.RootBeadIDMetadataKey])
		if rootID == "" {
			return "", fmt.Errorf("drain %s has no %s", control.ID, beadmeta.RootBeadIDMetadataKey)
		}
		worktreesRoot := workdirutil.WorktreesRoot(cityPath)
		root := filepath.Join(worktreesRoot, rig.Name)
		spec := worktree.Spec{
			RepoDir:    repo,
			Path:       filepath.Join(root, rootID+"-"+control.ID),
			Root:       root,
			Branch:     "gc/drain/" + control.ID,
			Base:       "HEAD",
			BeadID:     control.ID,
			StoreRef:   storeRef,
			Creator:    sharedDrainWorkspaceOwner,
			Owner:      sharedDrainWorkspaceOwner,
			Generation: control.ID,
			Lifecycle:  worktree.LifecycleActive,
		}
		rep, err := worktree.Ensure(spec)
		if err != nil {
			return "", fmt.Errorf("ensuring shared workspace for drain %s in rig %q under %s (the worktrees root is GC_WORKTREES_DIR, default <city>/.gc/worktrees, and must lie outside the rig checkout): %w",
				control.ID, rig.Name, worktreesRoot, err)
		}
		if rep.Provenance == nil {
			return "", fmt.Errorf("ensuring shared workspace for drain %s in rig %q: worktree %s has no provenance", control.ID, rig.Name, rep.Path)
		}
		return rep.Provenance.Path, nil
	}
}
