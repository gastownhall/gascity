package core

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// worktreeSetupScriptPath is the on-disk copy of the pre_start script that
// workspace.implicit_agent_isolation = "worktree" wires into rig-scoped
// implicit agents.
const worktreeSetupScriptPath = "assets/scripts/worktree-setup.sh"

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRigRepo creates a git repo with one tracked file and returns its path.
func newRigRepo(t *testing.T) string {
	t.Helper()
	rig := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, rig, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(rig, "README.md"), []byte("rig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, rig, "add", "README.md")
	gitCmd(t, rig, "commit", "-q", "-m", "initial")
	return rig
}

func runWorktreeSetup(t *testing.T, rigRoot, target, agentBase string) {
	t.Helper()
	cmd := exec.Command("sh", worktreeSetupScriptPath, rigRoot, target, agentBase, "--sync")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree-setup.sh %s %s %s: %v\n%s", rigRoot, target, agentBase, err, out)
	}
}

// AC-7: two implicit instances get their own worktrees on per-slot branches,
// with a bead redirect to the rig, and the rig root's HEAD, index and
// working tree stay untouched while each instance commits.
func TestWorktreeSetupIsolatesConcurrentImplicitInstances(t *testing.T) {
	rig := newRigRepo(t)
	headBefore := gitCmd(t, rig, "rev-parse", "HEAD")
	branchBefore := gitCmd(t, rig, "symbolic-ref", "HEAD")

	worktrees := filepath.Join(t.TempDir(), ".gc", "worktrees", "demo")
	slots := map[string]string{
		"claude-1": filepath.Join(worktrees, "claude-1"),
		"claude-2": filepath.Join(worktrees, "claude-2"),
	}
	for base, wt := range slots {
		runWorktreeSetup(t, rig, wt, base)
	}

	branches := map[string]bool{}
	for base, wt := range slots {
		top := gitCmd(t, wt, "rev-parse", "--show-toplevel")
		if resolved, _ := filepath.EvalSymlinks(wt); top != resolved && top != wt {
			t.Fatalf("%s: show-toplevel = %q, want its own worktree %q", base, top, wt)
		}
		branch := gitCmd(t, wt, "branch", "--show-current")
		if !regexp.MustCompile(`^gc-` + regexp.QuoteMeta(base) + `-[0-9a-f]{12}$`).MatchString(branch) {
			t.Fatalf("%s: branch %q, want gc-%s-<hash12>", base, branch, base)
		}
		branches[branch] = true
		redirect, err := os.ReadFile(filepath.Join(wt, ".beads", "redirect"))
		if err != nil {
			t.Fatalf("%s: reading .beads/redirect: %v", base, err)
		}
		if got, want := strings.TrimSpace(string(redirect)), filepath.Join(rig, ".beads"); got != want {
			t.Fatalf("%s: .beads/redirect = %q, want %q", base, got, want)
		}
		// Each instance edits, switches branches and commits in its own tree.
		if err := os.WriteFile(filepath.Join(wt, base+".txt"), []byte(base+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, wt, "checkout", "-q", "-b", "work-"+base)
		gitCmd(t, wt, "add", base+".txt")
		gitCmd(t, wt, "commit", "-q", "-m", base)
		if status := gitCmd(t, wt, "status", "--porcelain"); status != "" {
			t.Fatalf("%s: runtime files leak into git status:\n%s", base, status)
		}
	}
	if len(branches) != 2 {
		t.Fatalf("instances share a branch: %v", branches)
	}

	if got := gitCmd(t, rig, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("rig root HEAD moved from %s to %s", headBefore, got)
	}
	if got := gitCmd(t, rig, "symbolic-ref", "HEAD"); got != branchBefore {
		t.Fatalf("rig root branch changed from %s to %s", branchBefore, got)
	}
	if status := gitCmd(t, rig, "status", "--porcelain"); status != "" {
		t.Fatalf("rig root index or working tree changed:\n%s", status)
	}
	if gitignore, err := os.ReadFile(filepath.Join(rig, ".gitignore")); err == nil {
		t.Fatalf("rig root .gitignore was written:\n%s", gitignore)
	}
}

// AC-7: the core pack embeds the script; materialization makes it
// executable by its .sh suffix (builtinpacks.MaterializedFileMode).
func TestWorktreeSetupScriptIsEmbedded(t *testing.T) {
	data, err := fs.ReadFile(PackFS, worktreeSetupScriptPath)
	if err != nil {
		t.Fatalf("core PackFS lacks %s: %v", worktreeSetupScriptPath, err)
	}
	if !strings.HasPrefix(string(data), "#!/bin/sh") {
		t.Fatalf("%s has no #!/bin/sh shebang", worktreeSetupScriptPath)
	}
}
