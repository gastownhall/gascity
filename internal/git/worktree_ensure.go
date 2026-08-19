package git

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnsureAgentWorktree makes workDir an isolated git worktree of rigRoot for
// the given agent identity. It is idempotent: an existing worktree at
// workDir is left alone. When rigRoot is not a git repository, EnsureAgentWorktree
// creates workDir as an ordinary directory so agents still get distinct trees.
//
// Branch names follow the gastown worktree-setup convention:
// gc-<agent>-<12-char-hash-of-workDir> so multiple cities sharing one repo
// do not collide on refs.
func EnsureAgentWorktree(rigRoot, workDir, agentName string) error {
	rigRoot = strings.TrimSpace(rigRoot)
	workDir = strings.TrimSpace(workDir)
	agentName = strings.TrimSpace(agentName)
	if workDir == "" {
		return fmt.Errorf("ensure worktree: work dir is empty")
	}
	if agentName == "" {
		return fmt.Errorf("ensure worktree: agent name is empty")
	}
	if rigRoot == "" {
		return fmt.Errorf("ensure worktree: rig root is empty")
	}
	if samePath(rigRoot, workDir) {
		return fmt.Errorf("ensure worktree: work dir %q must differ from rig root", workDir)
	}

	if isGitWorktree(workDir) {
		return nil
	}

	g := New(rigRoot)
	if !g.IsRepo() {
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return fmt.Errorf("creating non-git work dir %q: %w", workDir, err)
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(workDir), 0o755); err != nil {
		return fmt.Errorf("creating worktree parent for %q: %w", workDir, err)
	}
	if err := prepareEmptyWorkDir(workDir); err != nil {
		return err
	}
	_, _ = g.run("worktree", "prune")

	branch := agentWorktreeBranch(agentName, workDir)
	if refExists(g, "refs/heads/"+branch) {
		if err := g.runWorktreeAdd(workDir, branch, false); err != nil {
			return fmt.Errorf("adding worktree %q on branch %q: %w", workDir, branch, err)
		}
	} else {
		if err := g.runWorktreeAdd(workDir, branch, true); err != nil {
			return fmt.Errorf("adding worktree %q with new branch %q: %w", workDir, branch, err)
		}
	}

	if err := writeBeadsRedirect(rigRoot, workDir); err != nil {
		return err
	}
	_ = New(workDir).SubmoduleInit() // best-effort
	return nil
}

func (g *Git) runWorktreeAdd(workDir, branch string, createBranch bool) error {
	args := []string{"worktree", "add", workDir}
	if createBranch {
		args = append(args, "-b", branch)
	} else {
		args = append(args, branch)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = g.workDir
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && gitEnvBlacklist[k] {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	cmd.Env = append(cmd.Env, "GIT_LFS_SKIP_SMUDGE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

func agentWorktreeBranch(agentName, workDir string) string {
	sum := sha256.Sum256([]byte(workDir))
	return fmt.Sprintf("gc-%s-%s", agentName, hex.EncodeToString(sum[:])[:12])
}

func isGitWorktree(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir() || info.Mode().IsRegular()
}

func prepareEmptyWorkDir(workDir string) error {
	info, err := os.Stat(workDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat work dir %q: %w", workDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("work dir %q exists and is not a directory", workDir)
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return fmt.Errorf("reading work dir %q: %w", workDir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("work dir %q exists and is not empty; refusing to replace with a worktree", workDir)
	}
	if err := os.Remove(workDir); err != nil {
		return fmt.Errorf("removing empty work dir %q before worktree add: %w", workDir, err)
	}
	return nil
}

func writeBeadsRedirect(rigRoot, workDir string) error {
	beadsDir := filepath.Join(workDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		return fmt.Errorf("creating .beads in worktree %q: %w", workDir, err)
	}
	redirect := filepath.Join(rigRoot, ".beads") + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte(redirect), 0o644); err != nil {
		return fmt.Errorf("writing .beads/redirect in %q: %w", workDir, err)
	}
	return nil
}

func refExists(g *Git, ref string) bool {
	_, err := g.run("show-ref", "--verify", "--quiet", ref)
	return err == nil
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	return absA == absB
}
