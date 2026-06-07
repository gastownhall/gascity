package doctor

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

const defaultBranchFallback = "main"

// RigRootBranchCheck warns when a rig's working tree is on a different branch
// than its configured default branch. SeverityAdvisory; WarmupEligible.
type RigRootBranchCheck struct {
	rig     config.Rig
	gitPath func(name string) (string, error) // injectable for tests
}

// NewRigRootBranchCheck creates a rig root-branch check for the given rig.
func NewRigRootBranchCheck(rig config.Rig) *RigRootBranchCheck {
	return &RigRootBranchCheck{rig: rig, gitPath: exec.LookPath}
}

// Name returns the check identifier.
func (c *RigRootBranchCheck) Name() string { return "rig:" + c.rig.Name + ":root-branch" }

// WarmupEligible returns true so this check runs during gc start warm-up.
func (c *RigRootBranchCheck) WarmupEligible() bool { return true }

// CanFix returns false; branch switches require operator judgment.
func (c *RigRootBranchCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *RigRootBranchCheck) Fix(_ *CheckContext) error { return nil }

// Run checks whether the rig's HEAD is on its configured default branch.
func (c *RigRootBranchCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}

	defaultBranch := c.rig.EffectiveDefaultBranch()
	if defaultBranch == "" {
		defaultBranch = defaultBranchFallback
	}

	gitBin, err := c.gitPath("git")
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("rig %q: unable to determine branch — git unavailable or path is not a git repo", c.rig.Name)
		return r
	}

	branchOut, err := runGitCommand(gitBin, c.rig.Path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("rig %q: unable to determine branch — git unavailable or path is not a git repo", c.rig.Name)
		return r
	}

	currentBranch := strings.TrimSpace(branchOut)
	if currentBranch == defaultBranch {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q HEAD=%s (matches default)", c.rig.Name, currentBranch)
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("rig %q is on %s (expected: %s)", c.rig.Name, currentBranch, defaultBranch)
	r.FixHint = fmt.Sprintf("git -C %q checkout %s", c.rig.Path, defaultBranch)

	if dirty, _ := isGitDirty(gitBin, c.rig.Path); dirty {
		r.Details = []string{"working tree is dirty — git pull will fail"}
	}
	return r
}

// runGitCommand executes a git subcommand in dir and returns trimmed stdout.
func runGitCommand(gitBin, dir string, args ...string) (string, error) {
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// isGitDirty reports whether the working tree has uncommitted changes.
func isGitDirty(gitBin, dir string) (bool, error) {
	cmd := exec.Command(gitBin, "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}
