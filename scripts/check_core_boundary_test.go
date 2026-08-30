package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCheckCoreBoundary runs the shell self-test for
// scripts/check-core-boundary.sh, the structural open-core boundary guard
// (gascity#4479). It exercises the tracked-file scan against real temp git
// repos: an untracked in-tree Go build cache is invisible, tracked
// vendor/testdata stay excluded, a `// boundary:allow org_` annotation still
// suppresses, and a genuine tracked `org_` violation still blocks — including
// one under a path containing whitespace. It also covers the fail-closed path
// outside a git work tree and the lone-tracked-_test.go case. Hermetic: temp
// git repos and real grep only, no network or bd calls. HOME is overridden so
// a developer's global core.excludesFile cannot reach the temp repos.
func TestCheckCoreBoundary(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command(filepath.Join(root, "scripts", "test-check-core-boundary.sh"))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-check-core-boundary.sh failed: %v\n%s", err, out)
	}
}
