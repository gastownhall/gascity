package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worktree"
)

// implicitSlotGitOps is the slice of internal/git.Git the slot provisioning
// path uses. An interface so tests can observe or refuse the git
// subprocesses the create/recreate path runs — and prove the reuse path
// runs none.
type implicitSlotGitOps interface {
	WorktreeList() ([]git.Worktree, error)
	WorktreeRemove(path string, force bool) error
	BranchDeleteIfMerged(branch string) error
}

// newImplicitSlotGit is the git seam for slot provisioning; a var so tests
// can count or substitute it.
var newImplicitSlotGit = func(dir string) implicitSlotGitOps { return git.New(dir) }

// implicitSlotWorktreeEnsure is the worktree.Ensure seam; a var so tests
// can prove the reuse path never reaches it.
var implicitSlotWorktreeEnsure = worktree.Ensure

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

// hostLocalSessionProvider reports whether the session runtime executes on
// the controller's own filesystem, where provisioning a host-side slot
// worktree is possible and useful. Remote and container runtimes (k8s,
// hybrid routing, ssh:, exec: transports) and the fake/fail test runtimes
// get no worktree: creating one they cannot use would be pure noise. Fail
// closed — an unrecognized provider name gets no worktree either.
func hostLocalSessionProvider(provider string) bool {
	switch provider {
	case "", "tmux", "subprocess", "acp", "herdr", "t3bridge":
		return true
	default:
		return false
	}
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
// work item's. Three outcomes:
//
//   - an existing valid slot checkout is reused untouched, validated
//     file-only with no git subprocess (reuseImplicitSlotWorktree);
//   - a slot whose directory is absent or empty is re-created after
//     clearing at most its own stale registration
//     (clearStaleSlotRegistration);
//   - anything else fails closed without modifying the slot.
//
// worktree.Ensure's own verify-first semantics cover the final create: it
// never clobbers a path it did not create and rolls back a failed creation.
func ensureImplicitSlotWorktree(slot workdirutil.ImplicitSlot) error {
	reused, err := reuseImplicitSlotWorktree(slot)
	if err != nil {
		return err
	}
	if reused {
		return nil
	}
	if err := clearStaleSlotRegistration(slot); err != nil {
		return err
	}
	spec := worktree.Spec{
		RepoDir: slot.RigRoot,
		Path:    slot.Path,
		Branch:  implicitSlotBranchName(slot.Path),
		Base:    "HEAD",
	}
	if _, err := implicitSlotWorktreeEnsure(spec); err != nil {
		return implicitSlotRefused(slot.Path, err)
	}
	return nil
}

// reuseImplicitSlotWorktree validates an existing slot checkout through the
// filesystem alone — the .git gitfile pointer, the worktree git dir, the
// common dir it shares with the rig, and the registration back-pointer —
// and on success clears the stale marker and reports the slot reusable.
//
// Deliberately no git subprocess and no branch check: this is the hot path
// every session start takes, and a slot holding a dirty tree, local
// commits, or a switched branch is still that instance's checkout; the next
// session reclaims it as-is. A .git entry that is anything other than a
// valid pointer to this rig's worktree registered at exactly this path is
// an error — the slot is not ours to repair.
func reuseImplicitSlotWorktree(slot workdirutil.ImplicitSlot) (bool, error) {
	gitPath := filepath.Join(slot.Path, ".git")
	info, statErr := os.Lstat(gitPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, implicitSlotRefused(slot.Path, fmt.Errorf("inspecting slot .git: %w", statErr))
	}
	if !info.Mode().IsRegular() {
		return false, implicitSlotRefused(slot.Path, fmt.Errorf("slot .git is not a worktree pointer (mode %s); refusing to touch it", info.Mode()))
	}
	gitdir, err := readGitDirPointer(slot.Path)
	if err != nil {
		return false, implicitSlotRefused(slot.Path, err)
	}
	if info, err := os.Stat(gitdir); err != nil || !info.IsDir() {
		return false, implicitSlotRefused(slot.Path, fmt.Errorf("stale worktree pointer: git dir %q does not exist", gitdir))
	}
	repoCommon, err := repoCommonDirFromRoot(slot.RigRoot)
	if err != nil {
		return false, implicitSlotRefused(slot.Path, fmt.Errorf("resolving rig common dir: %w", err))
	}
	slotCommon, err := commonDirFromGitDir(gitdir)
	if err != nil {
		return false, implicitSlotRefused(slot.Path, err)
	}
	if !pathutil.SamePath(slotCommon, repoCommon) {
		return false, implicitSlotRefused(slot.Path, fmt.Errorf("slot belongs to a different repository (common dir %q, want the rig's %q)", slotCommon, repoCommon))
	}
	back, err := os.ReadFile(filepath.Join(gitdir, "gitdir"))
	if err != nil {
		return false, implicitSlotRefused(slot.Path, fmt.Errorf("reading worktree registration back-pointer: %w", err))
	}
	if target := strings.TrimSpace(string(back)); !pathutil.SamePath(target, gitPath) {
		return false, implicitSlotRefused(slot.Path, fmt.Errorf("worktree registration points at %q, not this slot", target))
	}

	// Reuse: clear the stale marker, touch nothing else.
	if err := os.Remove(filepath.Join(slot.Path, worktreeStaleFileName)); err != nil && !os.IsNotExist(err) {
		return false, implicitSlotRefused(slot.Path, fmt.Errorf("removing stale marker: %w", err))
	}
	return true, nil
}

// clearStaleSlotRegistration prepares a slot whose directory is absent or
// empty for re-creation: it removes at most the worktree registration at
// exactly this path (never a repo-wide prune, which could unregister
// worktrees this code did not create), removes an empty leftover directory,
// and deletes the slot branch only when git can prove it fully merged, so
// a recreated merged slot restarts from rig HEAD while an unmerged one
// re-attaches with its work intact.
func clearStaleSlotRegistration(slot workdirutil.ImplicitSlot) error {
	if info, err := os.Lstat(slot.Path); err == nil {
		if !info.IsDir() {
			return implicitSlotRefused(slot.Path, fmt.Errorf("slot path exists and is not a directory"))
		}
		entries, err := os.ReadDir(slot.Path)
		if err != nil {
			return implicitSlotRefused(slot.Path, fmt.Errorf("inspecting slot directory: %w", err))
		}
		if len(entries) > 0 {
			return implicitSlotRefused(slot.Path, fmt.Errorf("slot directory exists and is not empty; refusing to replace it"))
		}
		if err := os.Remove(slot.Path); err != nil {
			return implicitSlotRefused(slot.Path, fmt.Errorf("removing empty slot directory: %w", err))
		}
	} else if !os.IsNotExist(err) {
		return implicitSlotRefused(slot.Path, fmt.Errorf("inspecting slot directory: %w", err))
	}

	repo := newImplicitSlotGit(slot.RigRoot)
	if err := removeSlotWorktreeRegistration(repo, slot.Path); err != nil {
		return err
	}
	// BranchDeleteIfMerged uses `git branch -d`, which refuses anything it
	// cannot prove merged. Every refusal keeps the branch, and Ensure then
	// attaches it as-is — so unmerged work is never destroyed by any
	// failure mode here, including a probe error: worst case the slot
	// re-attaches its old branch instead of restarting from HEAD.
	_ = repo.BranchDeleteIfMerged(implicitSlotBranchName(slot.Path))
	return nil
}

// removeSlotWorktreeRegistration clears at most one registration: the one
// at exactly this slot path. WorktreeRemove fails on a locked registration,
// which surfaces as the refusal it is.
func removeSlotWorktreeRegistration(repo implicitSlotGitOps, slotPath string) error {
	list, err := repo.WorktreeList()
	if err != nil {
		return implicitSlotRefused(slotPath, fmt.Errorf("listing rig worktrees: %w", err))
	}
	registered := false
	for _, wt := range list {
		if pathutil.SamePath(wt.Path, slotPath) {
			registered = true
			break
		}
	}
	if !registered {
		return nil
	}
	if err := repo.WorktreeRemove(slotPath, true); err != nil {
		return implicitSlotRefused(slotPath, fmt.Errorf("clearing stale worktree registration: %w", err))
	}
	return nil
}

// readGitDirPointer reads the "gitdir: <path>" gitfile from <dir>/.git,
// resolving a relative target against dir (Git's gitfile format).
func readGitDirPointer(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return "", fmt.Errorf("reading worktree pointer: %w", err)
	}
	content := strings.TrimSpace(string(data))
	if !strings.HasPrefix(content, "gitdir:") {
		return "", fmt.Errorf("worktree pointer %q does not start with \"gitdir:\"", content)
	}
	target := strings.TrimSpace(strings.TrimPrefix(content, "gitdir:"))
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return filepath.Clean(target), nil
}

// commonDirFromGitDir resolves a worktree git dir's common dir file-only.
// The commondir file holds the shared git dir, relative paths resolved
// against the git dir itself; its absence means the git dir IS the common
// dir (a main repository checkout).
func commonDirFromGitDir(gitdir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		if os.IsNotExist(err) {
			return gitdir, nil
		}
		return "", fmt.Errorf("reading common dir: %w", err)
	}
	common := strings.TrimSpace(string(data))
	if common == "" {
		return gitdir, nil
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	return filepath.Clean(common), nil
}

// repoCommonDirFromRoot resolves a repository root's common git dir
// file-only: a .git directory is its own common dir; a .git gitfile (a
// linked-worktree root) follows the pointer to the worktree git dir and its
// commondir file.
func repoCommonDirFromRoot(root string) (string, error) {
	gitPath := filepath.Join(root, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil {
		return "", fmt.Errorf("inspecting %q: %w", gitPath, err)
	}
	if info.IsDir() {
		return gitPath, nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is neither a directory nor a worktree pointer", gitPath)
	}
	gitdir, err := readGitDirPointer(root)
	if err != nil {
		return "", err
	}
	return commonDirFromGitDir(gitdir)
}
