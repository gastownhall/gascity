// Package git provides minimal Git worktree operations for agent sandboxing.
package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Worktree represents a single git worktree entry.
type Worktree struct {
	Path   string
	Head   string
	Branch string
}

// Git wraps git operations scoped to a working directory.
type Git struct {
	workDir string
}

// New returns a Git instance scoped to the given directory.
func New(workDir string) *Git {
	return &Git{workDir: workDir}
}

// IsRepo reports whether workDir is inside a git repository.
func (g *Git) IsRepo() bool {
	return g.IsRepoCtx(context.Background())
}

// IsRepoCtx is like IsRepo but accepts a context for cancellation.
func (g *Git) IsRepoCtx(ctx context.Context) bool {
	_, err := g.runCtx(ctx, "rev-parse", "--git-dir")
	return err == nil
}

// CurrentBranch returns the current branch name. Returns "HEAD" if detached.
func (g *Git) CurrentBranch() (string, error) {
	return g.CurrentBranchCtx(context.Background())
}

// CurrentBranchCtx is like CurrentBranch but accepts a context.
func (g *Git) CurrentBranchCtx(ctx context.Context) (string, error) {
	out, err := g.runCtx(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("getting current branch: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// DefaultBranch returns the default branch name via the origin HEAD symref.
// Falls back to "main" if no remote is configured.
func (g *Git) DefaultBranch() (string, error) {
	out, err := g.run("symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return "main", nil
	}
	// Output is like "refs/remotes/origin/main" or
	// "refs/remotes/origin/user/feature". Strip the full prefix so branch
	// names containing slashes are preserved.
	ref := strings.TrimSpace(out)
	return strings.TrimPrefix(ref, "refs/remotes/origin/"), nil
}

// WorktreeRemove removes a worktree. If force is true, removes even with
// uncommitted changes.
func (g *Git) WorktreeRemove(path string, force bool) error {
	args := []string{"worktree", "remove", path}
	if force {
		args = append(args, "--force")
	}
	_, err := g.run(args...)
	if err != nil {
		return fmt.Errorf("removing worktree %q: %w", path, err)
	}
	return nil
}

// WorktreeList returns all worktrees in porcelain format.
func (g *Git) WorktreeList() ([]Worktree, error) {
	out, err := g.run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("listing worktrees: %w", err)
	}
	return parseWorktreeList(out), nil
}

// HasUncommittedWork reports whether the working directory has uncommitted
// changes (staged or unstaged) or untracked files. Used as a safety check
// before removing a worktree to avoid losing in-progress work.
func (g *Git) HasUncommittedWork() bool {
	out, err := g.run("status", "--porcelain")
	if err != nil {
		return true // assume dirty on error (safe default)
	}
	return strings.TrimSpace(out) != ""
}

// HasUnpushedCommits reports whether HEAD has commits not reachable from
// any remote tracking branch. Used as a safety check before removing a
// worktree — unpushed commits represent completed work that would be lost.
// If the probe fails, it returns true to fail closed.
func (g *Git) HasUnpushedCommits() bool {
	has, err := g.HasUnpushedCommitsResult()
	if err != nil {
		return true
	}
	return has
}

// HasUnpushedCommitsResult is like HasUnpushedCommits but preserves git
// probe errors for callers that need to expose the precise failure reason.
func (g *Git) HasUnpushedCommitsResult() (bool, error) {
	out, err := g.run("log", "HEAD", "--oneline", "--not", "--remotes")
	if err != nil {
		return false, fmt.Errorf("checking unpushed commits: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// HasStashes reports whether the repository has stashed work.
// If the probe fails, it returns true to fail closed.
func (g *Git) HasStashes() bool {
	has, err := g.HasStashesResult()
	if err != nil {
		return true
	}
	return has
}

// HasStashesResult is like HasStashes but preserves git probe errors for
// callers that need to expose the precise failure reason.
func (g *Git) HasStashesResult() (bool, error) {
	out, err := g.run("stash", "list")
	if err != nil {
		return false, fmt.Errorf("checking stashes: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// SubmoduleInit initializes and updates submodules recursively.
// No-op if the repo has no submodules. Best-effort — errors are returned
// but callers may choose to ignore them.
func (g *Git) SubmoduleInit() error {
	_, err := g.run("submodule", "update", "--init", "--recursive")
	if err != nil {
		return fmt.Errorf("initializing submodules: %w", err)
	}
	return nil
}

// WorktreePrune removes stale worktree entries.
func (g *Git) WorktreePrune() error {
	_, err := g.run("worktree", "prune")
	if err != nil {
		return fmt.Errorf("pruning worktrees: %w", err)
	}
	return nil
}

// Fetch runs git fetch origin to update remote tracking branches.
func (g *Git) Fetch() error {
	_, err := g.run("fetch", "origin")
	if err != nil {
		return fmt.Errorf("fetching origin: %w", err)
	}
	return nil
}

// Stash pushes uncommitted changes (including untracked files) onto the stash.
func (g *Git) Stash(message string) error {
	_, err := g.run("stash", "push", "-u", "-m", message)
	if err != nil {
		return fmt.Errorf("stashing changes: %w", err)
	}
	return nil
}

// StashPop restores the most recent stash entry and removes it from the stash.
func (g *Git) StashPop() error {
	_, err := g.run("stash", "pop")
	if err != nil {
		return fmt.Errorf("popping stash: %w", err)
	}
	return nil
}

// PullRebase runs git pull --rebase from the specified remote and branch.
func (g *Git) PullRebase(remote, branch string) error {
	_, err := g.run("pull", "--rebase", remote, branch)
	if err != nil {
		return fmt.Errorf("pulling with rebase from %s/%s: %w", remote, branch, err)
	}
	return nil
}

// StatusPorcelain returns the porcelain status output showing changed files.
// Each non-empty line represents one changed/untracked file.
func (g *Git) StatusPorcelain() (string, error) {
	return g.StatusPorcelainCtx(context.Background())
}

// StatusPorcelainCtx is like StatusPorcelain but accepts a context.
func (g *Git) StatusPorcelainCtx(ctx context.Context) (string, error) {
	out, err := g.runCtx(ctx, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("getting status: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// AheadBehind returns the number of commits ahead and behind the upstream
// tracking branch. Returns (0, 0, err) if no upstream is configured.
func (g *Git) AheadBehind() (ahead, behind int, err error) {
	return g.AheadBehindCtx(context.Background())
}

// AheadBehindCtx is like AheadBehind but accepts a context.
func (g *Git) AheadBehindCtx(ctx context.Context) (ahead, behind int, err error) {
	out, err := g.runCtx(ctx, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(strings.TrimSpace(out))
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output: %q", out)
	}
	a, err := fmt.Sscanf(parts[0], "%d", &ahead)
	if err != nil || a != 1 {
		return 0, 0, fmt.Errorf("parsing ahead count: %w", err)
	}
	b, err := fmt.Sscanf(parts[1], "%d", &behind)
	if err != nil || b != 1 {
		return 0, 0, fmt.Errorf("parsing behind count: %w", err)
	}
	return ahead, behind, nil
}

// gitEnvBlacklist lists git environment variables that must be stripped
// so subprocess git commands use the intended workDir, not a parent repo.
// This prevents leakage from pre-commit hooks or other git tooling.
var gitEnvBlacklist = map[string]bool{
	"GIT_COMMON_DIR":                   true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_DIR":                          true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_PREFIX":                       true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_SHALLOW_FILE":                 true,
}

// run executes a git command in the working directory. Git environment
// variables from the parent process are stripped to prevent interference
// (e.g., when called from a pre-commit hook context).
func (g *Git) run(args ...string) (string, error) {
	return g.runCtx(context.Background(), args...)
}

// runCtx executes a git command with a context for cancellation/timeout.
func (g *Git) runCtx(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.workDir
	// Build clean env: inherit everything except git-specific vars.
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && gitEnvBlacklist[k] {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

// parseWorktreeList parses git worktree list --porcelain output.
// Each worktree block is separated by a blank line and contains
// "worktree <path>", "HEAD <sha>", "branch refs/heads/<name>".
func parseWorktreeList(output string) []Worktree {
	var worktrees []Worktree
	var current Worktree

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if current.Path != "" {
				worktrees = append(worktrees, current)
				current = Worktree{}
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			current.Path = canonicalWorktreePath(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "HEAD "):
			current.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			// Strip refs/heads/ prefix.
			current.Branch = strings.TrimPrefix(ref, "refs/heads/")
		}
	}
	// Handle last block if output doesn't end with blank line.
	if current.Path != "" {
		worktrees = append(worktrees, current)
	}
	return worktrees
}

func canonicalWorktreePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}
