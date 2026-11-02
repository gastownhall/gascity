package git

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// validateRefArg rejects ref/branch/path arguments that could be parsed as
// git command-line options. All worktree-op helpers pass user-influenced
// strings positionally, so a leading "-" must fail closed.
func validateRefArg(kind, val string) error {
	if strings.TrimSpace(val) == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if strings.HasPrefix(val, "-") {
		return fmt.Errorf("%s %q must not start with '-'", kind, val)
	}
	return nil
}

// BranchExists reports whether a local branch with the given name exists.
// It fails closed: only git's documented "ref not found" exit status is
// reported as absence, and every other probe failure is returned as an
// error. Callers use this to decide whether a branch is theirs to create
// and later delete, so a transient probe failure reported as "absent"
// would authorize deleting a branch that already existed.
func (g *Git) BranchExists(branch string) (bool, error) {
	if err := validateRefArg("branch", branch); err != nil {
		return false, err
	}
	_, err := g.run("show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("probing branch %q: %w", branch, err)
}

// RevParseVerifyCommit resolves ref to a full commit SHA, failing when the
// ref does not exist or does not point at a commit. The ref is resolved
// verbatim against the local repository — no remote fallback — so an
// explicit local base like "main" keeps its local meaning.
func (g *Git) RevParseVerifyCommit(ref string) (string, error) {
	if err := validateRefArg("ref", ref); err != nil {
		return "", err
	}
	// ^{commit} both peels tags and rejects non-commit objects. Append it
	// only for a plain ref; a caller-supplied peel (e.g. ref^{tree}) is
	// passed through verbatim to avoid double-peeling.
	appendedPeel := !strings.Contains(ref, "^{")
	spec := ref
	if appendedPeel {
		spec = ref + "^{commit}"
	}
	out, err := g.run("rev-parse", "--verify", "--quiet", spec)
	if err != nil {
		return "", fmt.Errorf("resolving ref %q: %w", ref, err)
	}
	sha := strings.TrimSpace(out)
	// Object-ID width is repository-dependent: SHA-1 repositories emit 40 hex
	// characters and SHA-256 repositories emit 64. Pinning either width would
	// reject every ref in the other kind of repository, so validate the shape
	// rather than one repository format's length.
	if !isHexObjectID(sha) {
		return "", fmt.Errorf("resolving ref %q: unexpected rev-parse output %q", ref, sha)
	}
	// When we appended ^{commit}, rev-parse already guaranteed a commit.
	// Only a caller-supplied peel can resolve to a ^{tree}/^{blob}, so
	// re-verify the object type only in that case.
	if !appendedPeel {
		typ, err := g.run("cat-file", "-t", sha)
		if err != nil {
			return "", fmt.Errorf("resolving ref %q: %w", ref, err)
		}
		if strings.TrimSpace(typ) != "commit" {
			return "", fmt.Errorf("ref %q resolves to a %s, not a commit", ref, strings.TrimSpace(typ))
		}
	}
	return sha, nil
}

// WorktreeAddNewBranch creates a worktree at path with a NEW branch created
// from base. It never detaches: the new worktree has branch checked out.
// Fails if the branch already exists.
func (g *Git) WorktreeAddNewBranch(path, branch, base string) error {
	if err := validateRefArg("branch", branch); err != nil {
		return err
	}
	if err := validateRefArg("base", base); err != nil {
		return err
	}
	if err := validateRefArg("path", path); err != nil {
		return err
	}
	if _, err := g.run("worktree", "add", "-b", branch, path, base); err != nil {
		return fmt.Errorf("adding worktree %q on new branch %q from %q: %w", path, branch, base, err)
	}
	return nil
}

// WorktreeAddExistingBranch creates a worktree at path with an EXISTING
// branch checked out. It never detaches. Fails if the branch is already
// checked out in another worktree.
func (g *Git) WorktreeAddExistingBranch(path, branch string) error {
	if err := validateRefArg("branch", branch); err != nil {
		return err
	}
	if err := validateRefArg("path", path); err != nil {
		return err
	}
	if _, err := g.run("worktree", "add", path, branch); err != nil {
		return fmt.Errorf("adding worktree %q on branch %q: %w", path, branch, err)
	}
	return nil
}

// BranchDeleteIfMerged deletes a local branch only when git can prove the
// deletion loses nothing, and returns an error when it cannot.
//
// It uses `git branch -d` rather than `-D` deliberately. Rollback paths reach
// here holding a branch an agent may have committed to, and a worktree with
// commits still has a clean status, so a working-tree check cannot see them.
// `-D` would delete the only ref reaching those commits and orphan them; `-d`
// refuses instead, which is the correct outcome for a rollback that would
// otherwise destroy work it did not create.
func (g *Git) BranchDeleteIfMerged(branch string) error {
	if err := validateRefArg("branch", branch); err != nil {
		return err
	}
	if _, err := g.run("branch", "-d", branch); err != nil {
		return fmt.Errorf("deleting branch %q: %w", branch, err)
	}
	return nil
}

// CommonDir returns the absolute path of the repository's common git dir.
// All worktrees of one repository share the same common dir, which makes it
// the repository-identity anchor for worktree verification.
func (g *Git) CommonDir() (string, error) {
	out, err := g.run("rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolving git common dir: %w", err)
	}
	common := strings.TrimSpace(out)
	if !filepath.IsAbs(common) {
		common = filepath.Join(g.workDir, common)
	}
	return filepath.Clean(common), nil
}

// GitDir returns the worktree-specific git directory as an absolute path.
// Unlike CommonDir, this identifies one registered worktree and is therefore
// suitable for per-worktree owner metadata.
func (g *Git) GitDir() (string, error) {
	out, err := g.run("rev-parse", "--git-dir")
	if err != nil {
		return "", fmt.Errorf("resolving git dir: %w", err)
	}
	dir := strings.TrimSpace(out)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(g.workDir, dir)
	}
	return filepath.Clean(dir), nil
}

// TopLevel returns the absolute path of the working-tree root containing
// the scoped directory.
func (g *Git) TopLevel() (string, error) {
	out, err := g.run("rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolving worktree top level: %w", err)
	}
	return filepath.Clean(strings.TrimSpace(out)), nil
}

// HeadSymbolicRef returns the full symbolic ref HEAD points at
// (e.g. "refs/heads/main"). It fails when HEAD is detached.
func (g *Git) HeadSymbolicRef() (string, error) {
	out, err := g.run("symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", fmt.Errorf("HEAD is not a symbolic ref (detached?): %w", err)
	}
	return strings.TrimSpace(out), nil
}

// isHexObjectID reports whether s is a full git object ID: lowercase hex at
// one of git's two object-format widths (SHA-1 is 40, SHA-256 is 64).
func isHexObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
