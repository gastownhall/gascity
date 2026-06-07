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
