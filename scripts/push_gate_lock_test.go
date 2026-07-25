package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPushGateLock runs the shell self-test for
// scripts/push-gate-lock-lib.sh, the pre-push gate's cross-invocation
// concurrency bound (ga-owh20p). It exercises push_gate_acquire_slot
// directly against a temp slot dir (acquire/hold/deny/timeout/release/
// escape-hatch), plus static wiring assertions against
// scripts/test-local-parallel. Hermetic: temp dirs and flock(1) only, no
// network/gh/model calls, no dependency on a real city.
func TestPushGateLock(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command(filepath.Join(root, "scripts", "test-push-gate-lock.sh"))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-push-gate-lock.sh failed: %v\n%s", err, out)
	}
}
