package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worktree"
)

// implicitSlotBranchName derives the deterministic per-slot branch name:
// gc-<instance>-<12 hex of sha256(slot path)>. The hash suffix binds the
// branch to the slot's absolute path, so two cities — or two rigs with the
// same template name — never collide on one branch, while the name stays
// stable across sessions so slot reuse and the session pruner can find the
// same branch again.
func implicitSlotBranchName(slotPath string) string {
	clean := filepath.Clean(slotPath)
	sum := sha256.Sum256([]byte(clean))
	return fmt.Sprintf("gc-%s-%s", filepath.Base(clean), hex.EncodeToString(sum[:])[:12])
}

// implicitSlotRefused wraps a slot-provisioning failure so the operator sees
// the exact slot path and the escape hatch together: the session did not
// start, nothing was modified, and [workspace] implicit_isolation = "none"
// restores the shared rig-root behavior.
func implicitSlotRefused(slotPath string, err error) error {
	return fmt.Errorf("implicit pool slot worktree %q: %w; set [workspace] implicit_isolation = \"none\" to keep pool sessions on the rig root", slotPath, err)
}

// ensureImplicitSlotWorktree provisions the slot's git worktree from the rig
// root at session start. The spec is unmanaged — no bead, owner, or
// generation, because the slot's lifecycle is the session's, not a single
// work item's — so worktree.Ensure's verify-first semantics apply: an
// already-valid slot worktree is reused untouched, an absent path is created
// on a deterministic branch from rig HEAD, and any path existing in another
// state fails closed without modifying anything.
func ensureImplicitSlotWorktree(slot workdirutil.ImplicitSlot) error {
	spec := worktree.Spec{
		RepoDir: slot.RigRoot,
		Path:    slot.Path,
		Branch:  implicitSlotBranchName(slot.Path),
		Base:    "HEAD",
	}
	if _, err := worktree.Ensure(spec); err != nil {
		return implicitSlotRefused(slot.Path, err)
	}
	return nil
}
