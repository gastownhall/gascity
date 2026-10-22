package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worktree"
)

// implicitSlotWorktreeBase is the ref a new pool-slot branch starts from. HEAD
// excludes uncommitted rig-root edits and needs no named default branch.
const implicitSlotWorktreeBase = "HEAD"

// ensureImplicitSlotWorktree makes slot.Path a git worktree of the rig on the
// slot's branch. A slot that already is a worktree of the rig is reused as
// it stands, whatever its branch, commits or uncommitted work: only gc's
// stale marker is removed, and no git subprocess runs. A missing slot is
// (re-)created through an unmanaged worktree.Spec (no Root, BeadID or
// Owner), so the slot never carries per-bead ownership provenance; Ensure
// locks the path, refuses to clobber existing state, verifies, and rolls
// back a failed create.
func ensureImplicitSlotWorktree(slot workdirutil.ImplicitSlot) error {
	reused, err := reuseImplicitSlotWorktree(slot)
	if err != nil {
		return fmt.Errorf("provisioning pool-slot worktree %q: %w", slot.Path, err)
	}
	if reused {
		return nil
	}
	if err := prepareMissingImplicitSlot(slot); err != nil {
		return fmt.Errorf("provisioning pool-slot worktree %q: %w", slot.Path, err)
	}
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

// reuseImplicitSlotWorktree reports whether slot.Path already is a worktree
// of the rig, reading only files: its .git file names an admin dir whose
// commondir is the rig's common dir and whose back-pointer names this slot.
// On reuse it removes gc's stale marker. An empty slot dir is removed so the
// slot is re-created.
func reuseImplicitSlotWorktree(slot workdirutil.ImplicitSlot) (bool, error) {
	info, err := os.Lstat(slot.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	entries, err := os.ReadDir(slot.Path)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		return false, os.Remove(slot.Path)
	}
	if !implicitSlotIsRigWorktree(slot) {
		return false, nil
	}
	if err := os.Remove(filepath.Join(slot.Path, worktreeStaleFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("removing stale marker: %w", err)
	}
	return true, nil
}

// implicitSlotIsRigWorktree reports whether slot.Path/.git is a gitdir file
// linking the slot to an admin dir of the rig repository.
func implicitSlotIsRigWorktree(slot workdirutil.ImplicitSlot) bool {
	dotGit := filepath.Join(slot.Path, ".git")
	adminDir, ok := readGitdirFile(dotGit)
	if !ok {
		return false
	}
	rigCommon, ok := gitCommonDirFromFiles(slot.RigRoot)
	if !ok || !pathutil.SamePath(adminCommonDir(adminDir), rigCommon) {
		return false
	}
	backPointer, err := os.ReadFile(filepath.Join(adminDir, "gitdir"))
	return err == nil && pathutil.SamePath(strings.TrimSpace(string(backPointer)), dotGit)
}

// readGitdirFile returns the directory a regular "gitdir: <path>" .git file
// names, resolved against the file's directory, when that directory exists.
func readGitdirFile(dotGit string) (string, bool) {
	info, err := os.Lstat(dotGit)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", false
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(dotGit), target)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		return "", false
	}
	return target, true
}

// adminCommonDir returns the common dir a git dir shares with its sibling
// worktrees: the target of its commondir file, else the git dir itself.
func adminCommonDir(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	common := strings.TrimSpace(string(data))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	return common
}

// gitCommonDirFromFiles returns the common git dir of the repository rooted
// at repoDir, reading only its .git directory or gitdir file.
func gitCommonDirFromFiles(repoDir string) (string, bool) {
	dotGit := filepath.Join(repoDir, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return dotGit, true
	}
	gitDir, ok := readGitdirFile(dotGit)
	if !ok {
		return "", false
	}
	return adminCommonDir(gitDir), true
}

// prepareMissingImplicitSlot readies an absent slot dir for Ensure: it clears
// a registration left at exactly the slot path and deletes the slot branch if
// it is merged, so the slot restarts from HEAD. An unmerged branch is kept
// and Ensure attaches it.
func prepareMissingImplicitSlot(slot workdirutil.ImplicitSlot) error {
	if _, err := os.Lstat(slot.Path); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	repo := git.New(slot.RigRoot)
	registered, err := repo.WorktreeList()
	if err != nil {
		return err
	}
	for _, wt := range registered {
		if pathutil.SamePath(wt.Path, slot.Path) {
			if err := repo.WorktreeRemove(slot.Path, true); err != nil {
				return fmt.Errorf("clearing stale registration: %w", err)
			}
			break
		}
	}
	exists, err := repo.BranchExists(slot.Branch)
	if err != nil || !exists {
		return err
	}
	// git refuses to delete an unmerged branch; the refusal keeps the slot's
	// commits, which Ensure then checks out again.
	_ = repo.BranchDeleteIfMerged(slot.Branch)
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
