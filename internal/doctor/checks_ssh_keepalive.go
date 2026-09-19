package doctor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
)

var _ Check = (*RigSSHKeepaliveCheck)(nil)

// longPrePushPattern matches a pre-push hook that runs a test suite (or
// similar heavy work). Those hooks idle the GitHub SSH transport long enough
// for the server to drop it, so git push dies with SIGPIPE/141 after the
// hook passes (ga-2i5).
var longPrePushPattern = regexp.MustCompile(`(?i)(make\s+test|go\s+test|test-fast|pytest|npm\s+test|cargo\s+test)`)

const longPrePushBytes = 2048

// RigSSHKeepaliveCheck warns when a rig's pre-push hook is long-running and
// the clone has no SSH keepalive. SeverityAdvisory; WarmupEligible.
type RigSSHKeepaliveCheck struct {
	rig     config.Rig
	gitPath func(name string) (string, error)
	fixable bool
}

// NewRigSSHKeepaliveCheck creates a per-rig SSH keepalive check.
func NewRigSSHKeepaliveCheck(rig config.Rig) *RigSSHKeepaliveCheck {
	return &RigSSHKeepaliveCheck{rig: rig, gitPath: exec.LookPath}
}

// Name returns the check identifier.
func (c *RigSSHKeepaliveCheck) Name() string { return "rig:" + c.rig.Name + ":ssh-keepalive" }

// WarmupEligible returns true so gc start warns before the next long push.
func (c *RigSSHKeepaliveCheck) WarmupEligible() bool { return true }

// CanFix reports whether Run found a stampable core.sshCommand.
func (c *RigSSHKeepaliveCheck) CanFix() bool { return c.fixable }

// Fix stamps core.sshCommand on the rig clone so every worktree inherits it.
func (c *RigSSHKeepaliveCheck) Fix(_ *CheckContext) error {
	if err := git.ApplySSHKeepaliveConfig(c.rig.Path); err != nil {
		return fmt.Errorf("stamping SSH keepalive on rig %q: %w", c.rig.Name, err)
	}
	return nil
}

// Run checks for a long pre-push hook without an SSH keepalive.
func (c *RigSSHKeepaliveCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}
	c.fixable = false

	gitBin, err := c.gitPath("git")
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("rig %q: git unavailable; cannot check SSH keepalive", c.rig.Name)
		return r
	}

	hook, hookErr := findPrePushHook(gitBin, c.rig.Path)
	if hookErr != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("rig %q: unable to inspect pre-push hook: %v", c.rig.Name, hookErr)
		return r
	}
	if hook == "" || !isLongPrePushHook(hook) {
		r.Status = StatusOK
		if hook == "" {
			r.Message = fmt.Sprintf("rig %q: no pre-push hook", c.rig.Name)
		} else {
			r.Message = fmt.Sprintf("rig %q: pre-push hook is not a long test gate", c.rig.Name)
		}
		return r
	}

	sshCmd, sshErr := runGitCommand(gitBin, c.rig.Path, "config", "--get", "core.sshCommand")
	if sshErr != nil {
		// git config --get exits 1 when the key is unset.
		sshCmd = ""
	}
	sshCmd = strings.TrimSpace(sshCmd)
	if git.HasSSHKeepalive(sshCmd) {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q: long pre-push hook has SSH keepalive", c.rig.Name)
		return r
	}

	r.Status = StatusWarning
	next := git.EnsureSSHKeepaliveCommand(sshCmd)
	if git.HasSSHKeepalive(next) {
		c.fixable = true
		r.FixHint = "gc doctor --fix"
		r.Message = fmt.Sprintf("rig %q has a long pre-push hook and no SSH keepalive; git push can SIGPIPE (exit 141) after the hook", c.rig.Name)
	} else {
		r.FixHint = fmt.Sprintf("add ServerAliveInterval to core.sshCommand (%q is a custom wrapper)", sshCmd)
		r.Message = fmt.Sprintf("rig %q has a long pre-push hook; custom core.sshCommand %q has no SSH keepalive", c.rig.Name, sshCmd)
	}
	r.Details = []string{"hook: " + hook}
	return r
}

func findPrePushHook(gitBin, repoPath string) (string, error) {
	seen := map[string]struct{}{}
	var candidates []string

	if hooksPath, err := runGitCommand(gitBin, repoPath, "config", "--get", "core.hooksPath"); err == nil {
		hooksPath = strings.TrimSpace(hooksPath)
		if hooksPath != "" {
			if !filepath.IsAbs(hooksPath) {
				hooksPath = filepath.Join(repoPath, hooksPath)
			}
			candidates = append(candidates, filepath.Join(hooksPath, "pre-push"))
		}
	}

	if hookDir, err := runGitCommand(gitBin, repoPath, "rev-parse", "--git-path", "hooks"); err == nil {
		hookDir = strings.TrimSpace(hookDir)
		if hookDir != "" {
			if !filepath.IsAbs(hookDir) {
				hookDir = filepath.Join(repoPath, hookDir)
			}
			candidates = append(candidates, filepath.Join(hookDir, "pre-push"))
		}
	}

	candidates = append(candidates, filepath.Join(repoPath, ".githooks", "pre-push"))

	for _, path := range candidates {
		if path == "" {
			continue
		}
		clean, err := filepath.Abs(path)
		if err != nil {
			clean = path
		}
		if _, dup := seen[clean]; dup {
			continue
		}
		seen[clean] = struct{}{}
		st, err := os.Stat(clean)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", err
		}
		if st.IsDir() || strings.HasSuffix(clean, ".sample") {
			continue
		}
		return clean, nil
	}
	return "", nil
}

func isLongPrePushHook(path string) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if longPrePushPattern.Match(content) {
		return true
	}
	return len(content) > longPrePushBytes
}
