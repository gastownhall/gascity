package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRebaseResolveLibUsesBash3Syntax(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "scripts", "rebase-resolve-lib.sh")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rebase-resolve-lib.sh: %v", err)
	}

	bash4LowercaseExpansion := regexp.MustCompile(`\$\{[^}\n]*,,[^}\n]*\}`)
	if match := bash4LowercaseExpansion.Find(contents); match != nil {
		t.Fatalf("rebase-resolve-lib.sh must run under macOS Bash 3; found Bash 4 lowercase expansion %q", match)
	}
}

// TestRebaseResolveLibHoldsNoColumnOneConflictMarkers guards the lib and its
// self-test script against tracked lines that look like conflict markers. The
// lib's repo-wide `git grep` marker nets (the final check of every bounded
// self-rebase) match such a line in either tracked file and answer rc=12 for
// every rebase, so the self-test script must build its marker fixtures at run
// time instead of holding them in heredocs.
func TestRebaseResolveLibHoldsNoColumnOneConflictMarkers(t *testing.T) {
	root := repoRoot(t)
	marker := regexp.MustCompile(`^(<<<<<<< |=======$|>>>>>>> |\|\|\|\|\|\|\| )`)
	for _, name := range []string{"rebase-resolve-lib.sh", "test-rebase-resolve.sh"} {
		contents, err := os.ReadFile(filepath.Join(root, "scripts", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(contents), "\n") {
			if marker.MatchString(line) {
				t.Errorf("scripts/%s:%d holds a column-1 conflict marker %q; build it at run time", name, i+1, line)
			}
		}
	}
}

// TestRebaseResolveLib runs the shell self-test for
// scripts/rebase-resolve-lib.sh, the deployer's bounded self-rebase
// trivial-conflict classifier. It exercises the classifier against real
// temp git repos (identical/one-side-empty/additive-both hunks, real
// conflicts, structural conflicts) plus attempt_bounded_self_rebase's guard
// rails and --force-with-lease push behavior. Hermetic: temp git repos only,
// no network/gh/model calls.
func TestRebaseResolveLib(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command(filepath.Join(root, "scripts", "test-rebase-resolve.sh"))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		// internal/testenv's hermetic git template, so the temp repos the script
		// makes have git auto-maintenance off (ga-zoe1wr).
		"GIT_TEMPLATE_DIR=" + os.Getenv("GIT_TEMPLATE_DIR"),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-rebase-resolve.sh failed: %v\n%s", err, out)
	}
}
