package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPushOwnershipGuard runs the shell self-test for
// scripts/push-ownership-guard.sh, the pre-push bead ownership/staleness
// guard (ga-fip9ps.1). It exercises assert_bead_still_claimed against real
// temp git repos with a fake `bd` on PATH (allow/block decisions, bead-id
// resolution, fail-closed behavior on an unreachable or slow bd), plus
// .githooks/pre-push wiring end to end against a real bare remote. Hermetic:
// temp git repos and a fake bd only, no network/gh/model calls.
func TestPushOwnershipGuard(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command(filepath.Join(root, "scripts", "test-push-ownership-guard.sh"))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-push-ownership-guard.sh failed: %v\n%s", err, out)
	}
}
