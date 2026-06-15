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

// DefaultBranch returns the default branch name via the origin HEAD symref,
// with a candidate-ref fallback when origin/HEAD is unset.
//
// Resolution order:
//  1. refs/remotes/origin/HEAD symref (the configured default)
//  2. refs/remotes/origin/main when it exists locally
//  3. refs/remotes/origin/master when it exists locally
//  4. "main" as a last resort
//
// The candidate-ref pass at step 2-3 prevents master-default rigs from
// silently inheriting "main" when origin/HEAD has not been wired by the
// clone (e.g., rigs added before gc rig add auto-detected the default
// branch). See gc-8cowk / gc-ao9t.
func (g *Git) DefaultBranch() (string, error) {
	if out, err := g.run("symbolic-ref", "refs/remotes/origin/HEAD"); err == nil {
		ref := strings.TrimSpace(out)
		if branch := strings.TrimPrefix(ref, "refs/remotes/origin/"); branch != "" {
			return branch, nil
		}
	}
	for _, candidate := range []string{"main", "master"} {
		if _, err := g.run("show-ref", "--verify", "--quiet", "refs/remotes/origin/"+candidate); err == nil {
			return candidate, nil
		}
	}
	return "main", nil
}

// ProbeDefaultBranch returns the repo's mainline branch name with a richer
// fallback chain than DefaultBranch:
//  1. refs/remotes/origin/HEAD symref (the configured default)
//  2. the currently checked-out branch (when origin/HEAD is unset, the
//     first branch is usually the mainline)
//  3. empty string (caller decides)
//
// Use this at registration time (gc rig add) where we want to record the
// repo's actual mainline rather than a generic "main" placeholder.
func (g *Git) ProbeDefaultBranch() string {
	if out, err := g.run("symbolic-ref", "refs/remotes/origin/HEAD"); err == nil {
		ref := strings.TrimSpace(out)
		if branch := strings.TrimPrefix(ref, "refs/remotes/origin/"); branch != "" {
			return branch
		}
	}
	if branch, err := g.CurrentBranch(); err == nil {
		branch = strings.TrimSpace(branch)
		if branch != "" && branch != "HEAD" {
			return branch
		}
	}
	return ""
}

// CheckoutDetach switches the working tree to a detached HEAD at ref.
func (g *Git) CheckoutDetach(ref string) error {
	if _, err := g.run("checkout", "--detach", ref); err != nil {
		return fmt.Errorf("checkout --detach %s: %w", ref, err)
	}
	return nil
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

// hermeticGitEnvExtra lists git environment variables stripped by HermeticEnv
// in addition to gitEnvBlacklist. These are repository-discovery,
// config-location, and pager/exec-path variables that a hermetic cache clone
// must not inherit from the parent process. They are kept separate from
// gitEnvBlacklist because SanitizedEnv deliberately preserves some of them: for
// example GIT_CEILING_DIRECTORIES is required by ordinary repo-discovery checks
// such as IsRepo, which would climb out of a non-repo directory if it were
// stripped. Cache clones, by contrast, want maximum isolation.
var hermeticGitEnvExtra = map[string]bool{
	"GIT_CEILING_DIRECTORIES":         true,
	"GIT_DISCOVERY_ACROSS_FILESYSTEM": true,
	"GIT_NAMESPACE":                   true,
	"GIT_CONFIG_SYSTEM":               true,
	"GIT_CONFIG_GLOBAL":               true,
	"GIT_CONFIG_NOSYSTEM":             true,
	"GIT_EXEC_PATH":                   true,
	"GIT_PAGER":                       true,
}

// SanitizedEnv returns a copy of the current process environment with
// git-specific variables removed. Subprocess git invocations should run with
// this environment so they operate on their own working directory instead of a
// parent repository leaked through GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, and
// related variables (for example when gc runs inside a pre-commit hook or
// nested worktree tooling). Callers outside this package that shell out to git
// directly should assign this to cmd.Env.
func SanitizedEnv() []string {
	return sanitizeGitEnv(os.Environ())
}

// HermeticEnv returns a process environment for git subprocesses that must run
// hermetically against a cached clone, isolated from ambient system, global,
// and parent-repository git state. It strips everything SanitizedEnv removes
// plus the repository-discovery, config-location, and pager/exec-path variables
// in hermeticGitEnvExtra, then pins GIT_CONFIG_NOSYSTEM=1 and
// GIT_CONFIG_GLOBAL=/dev/null so the clone reads no system or user git config.
// Cache and fetch runners that previously maintained their own duplicate
// blacklists should assign this to cmd.Env instead.
func HermeticEnv() []string {
	environ := os.Environ()
	cleaned := make([]string, 0, len(environ)+2)
	for _, e := range environ {
		if k, _, ok := strings.Cut(e, "="); ok && (gitEnvBlacklist[k] || hermeticGitEnvExtra[k]) {
			continue
		}
		cleaned = append(cleaned, e)
	}
	cleaned = append(cleaned, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	return cleaned
}

// sanitizeGitEnv returns environ with git-specific variables removed. It is the
// single filtering implementation shared by SanitizedEnv and runCtx so the
// blacklist has exactly one enforcement path.
func sanitizeGitEnv(environ []string) []string {
	cleaned := make([]string, 0, len(environ))
	for _, e := range environ {
		if k, _, ok := strings.Cut(e, "="); ok && gitEnvBlacklist[k] {
			continue
		}
		cleaned = append(cleaned, e)
	}
	return cleaned
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
	cmd.Env = sanitizeGitEnv(os.Environ())
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
