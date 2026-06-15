package doctor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// RED tests for RigRootBranchCheck (ga-l0jx0r). They fail to compile until
// the builder-provided check lands in internal/doctor/checks_rig_root_branch.go.

func TestRigRootBranchCheck_HeadMatchesDefaultBranch_OK(t *testing.T) {
	rigPath := initGitRepoOnBranch(t, "main")
	c := NewRigRootBranchCheck(config.Rig{
		Name:          "testrig",
		Path:          rigPath,
		DefaultBranch: "main",
	})

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "matches default") {
		t.Errorf("message = %q, want mention of matching default branch", r.Message)
	}
	if r.FixHint != "" {
		t.Errorf("FixHint = %q, want empty for OK result", r.FixHint)
	}
}

func TestRigRootBranchCheck_HeadDiffersFromDefaultClean_WarnsAdvisory(t *testing.T) {
	rigPath := initGitRepoOnBranch(t, "feature")
	c := NewRigRootBranchCheck(config.Rig{
		Name:          "testrig",
		Path:          rigPath,
		DefaultBranch: "main",
	})

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("severity = %d, want SeverityAdvisory", r.Severity)
	}
	if !strings.Contains(r.Message, "feature") || !strings.Contains(r.Message, "main") {
		t.Errorf("message = %q, want current and default branch names", r.Message)
	}
	if r.FixHint == "" || !strings.Contains(r.FixHint, "checkout main") {
		t.Errorf("FixHint = %q, want checkout hint for default branch", r.FixHint)
	}
	if len(r.Details) != 0 {
		t.Errorf("Details = %v, want none for clean tree", r.Details)
	}
}

func TestRigRootBranchCheck_HeadDiffersFromDefaultDirty_WarnsWithDirtyDetail(t *testing.T) {
	rigPath := initGitRepoOnBranch(t, "feature")
	if err := os.WriteFile(filepath.Join(rigPath, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	c := NewRigRootBranchCheck(config.Rig{
		Name:          "testrig",
		Path:          rigPath,
		DefaultBranch: "main",
	})

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if len(r.Details) == 0 {
		t.Fatalf("Details = %v, want dirty working tree detail", r.Details)
	}
	foundDirty := false
	for _, detail := range r.Details {
		if strings.Contains(detail, "dirty") {
			foundDirty = true
		}
	}
	if !foundDirty {
		t.Errorf("Details = %v, want detail mentioning dirty working tree", r.Details)
	}
}

func TestRigRootBranchCheck_NotGitRepository_WarnsUnableToDetermine(t *testing.T) {
	c := NewRigRootBranchCheck(config.Rig{
		Name:          "testrig",
		Path:          t.TempDir(),
		DefaultBranch: "main",
	})

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("severity = %d, want SeverityAdvisory", r.Severity)
	}
	if !strings.Contains(r.Message, "unable to determine branch") {
		t.Errorf("message = %q, want unable-to-determine warning", r.Message)
	}
}

func TestRigRootBranchCheck_GitUnavailable_WarnsUnableToDetermine(t *testing.T) {
	c := NewRigRootBranchCheck(config.Rig{
		Name:          "testrig",
		Path:          t.TempDir(),
		DefaultBranch: "main",
	})
	c.gitPath = func(string) (string, error) {
		return "", errors.New("git unavailable")
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("severity = %d, want SeverityAdvisory", r.Severity)
	}
	if !strings.Contains(r.Message, "unable to determine branch") {
		t.Errorf("message = %q, want unable-to-determine warning", r.Message)
	}
}

func TestRigRootBranchCheck_DefaultBranchUnsetFallsBackToMain(t *testing.T) {
	rigPath := initGitRepoOnBranch(t, "main")
	c := NewRigRootBranchCheck(config.Rig{
		Name: "testrig",
		Path: rigPath,
	})

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK when unset default falls back to main", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "HEAD=main") {
		t.Errorf("message = %q, want HEAD=main", r.Message)
	}
}

func TestRigRootBranchCheck_NonMainDefaultBranchMatches_OK(t *testing.T) {
	rigPath := initGitRepoOnBranch(t, "develop")
	c := NewRigRootBranchCheck(config.Rig{
		Name:          "testrig",
		Path:          rigPath,
		DefaultBranch: "develop",
	})

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK for non-main default branch", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "HEAD=develop") {
		t.Errorf("message = %q, want HEAD=develop", r.Message)
	}
}

// TestRigRootBranchCheck_IgnoresPoisonedGitEnv proves the rig root-branch check
// resolves the rig's own branch and dirty state even when git-locating
// environment variables point at an unrelated repository. Running gc doctor (or
// gc start warm-up) inside a pre-commit hook or nested worktree exports
// GIT_DIR/GIT_WORK_TREE for the parent repo; without git.SanitizedEnv() the
// leaked vars make rev-parse and status report the poisoned repo, so the check
// would clear (or warn on) the wrong repository.
func TestRigRootBranchCheck_IgnoresPoisonedGitEnv(t *testing.T) {
	// Rig is on a non-default branch with a dirty working tree.
	rigPath := initGitRepoOnBranch(t, "feature")
	if err := os.WriteFile(filepath.Join(rigPath, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	// Unrelated repo on the default branch with a clean tree. Both repos must be
	// created before poisoning so their own git commands are not redirected. If
	// the poison vars leak, runGitCommand reads "main" (== default) and isGitDirty
	// reads a clean tree, wrongly yielding StatusOK with no dirty detail.
	poison := initGitRepoOnBranch(t, "main")
	t.Setenv("GIT_DIR", filepath.Join(poison, ".git"))
	t.Setenv("GIT_WORK_TREE", poison)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(poison, ".git", "index"))

	c := NewRigRootBranchCheck(config.Rig{
		Name:          "testrig",
		Path:          rigPath,
		DefaultBranch: "main",
	})

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning (runGitCommand must read rig branch via cmd.Dir, not poisoned GIT_DIR)", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "feature") {
		t.Errorf("message = %q, want rig branch 'feature'", r.Message)
	}
	foundDirty := false
	for _, detail := range r.Details {
		if strings.Contains(detail, "dirty") {
			foundDirty = true
		}
	}
	if !foundDirty {
		t.Errorf("Details = %v, want dirty detail (isGitDirty must read rig tree, not poisoned GIT_WORK_TREE)", r.Details)
	}
}

func initGitRepoOnBranch(t *testing.T, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir := t.TempDir()
	runGitForRigRootBranchTest(t, dir, "init")
	runGitForRigRootBranchTest(t, dir, "checkout", "-b", branch)
	runGitForRigRootBranchTest(t, dir, "config", "user.name", "Rig Root Branch Test")
	runGitForRigRootBranchTest(t, dir, "config", "user.email", "rig-root-branch@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	runGitForRigRootBranchTest(t, dir, "add", "README.md")
	runGitForRigRootBranchTest(t, dir, "commit", "-m", "initial")
	return dir
}

func runGitForRigRootBranchTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
