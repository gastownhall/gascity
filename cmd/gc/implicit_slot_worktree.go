package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
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

// implicitSlotRuntimeIsHostLocal reports whether sessions on the session
// provider run on this host, where a pool-slot worktree on local disk is the
// session's filesystem. Remote, routed and test runtimes are not.
func implicitSlotRuntimeIsHostLocal(provider string) bool {
	switch provider {
	case "", "tmux", "subprocess", "acp", "herdr", "t3bridge":
		return true
	}
	return false
}

// implicitSlotAgentForRuntime returns a, or a copy of it without the
// pool-slot marker when the agent's session runtime (its own session, else
// sessionRuntime, the city runtime after the GC_SESSION override) is not
// host-local, so such a session keeps today's rig-root work dir.
func implicitSlotAgentForRuntime(a *config.Agent, sessionRuntime string) *config.Agent {
	if a == nil || !a.PoolSlotWorktrees {
		return a
	}
	if implicitSlotRuntimeIsHostLocal(effectiveSessionProvider(a.Session, sessionRuntime)) {
		return a
	}
	unmarked := *a
	unmarked.PoolSlotWorktrees = false
	return &unmarked
}
