package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

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
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-rebase-resolve.sh failed: %v\n%s", err, out)
	}
}
