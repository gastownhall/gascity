package main

import (
	"fmt"

	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worktree"
)

// implicitSlotWorktreeBase is the ref a new pool-slot branch starts from. HEAD
// excludes uncommitted rig-root edits and needs no named default branch.
const implicitSlotWorktreeBase = "HEAD"

// ensureImplicitSlotWorktree makes slot.Path a git worktree of the rig on the
// slot's branch. The spec is unmanaged (no Root, BeadID or Owner), so the
// slot never carries per-bead ownership provenance; Ensure locks the path,
// refuses to clobber existing state, verifies, and rolls back a failed create.
func ensureImplicitSlotWorktree(slot workdirutil.ImplicitSlot) error {
	if _, err := worktree.Ensure(worktree.Spec{
		RepoDir: slot.RigRoot,
		Path:    slot.Path,
		Branch:  slot.Branch,
		Base:    implicitSlotWorktreeBase,
	}); err != nil {
		return fmt.Errorf("provisioning pool-slot worktree %q: %w", slot.Path, err)
	}
	return nil
}
