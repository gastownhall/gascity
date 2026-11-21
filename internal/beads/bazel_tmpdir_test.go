package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerdictSQLiteStoreLivesUnderTestTmpdir pins where the SQLite store that
// TestEchoedWritesLeaveIdleReconcileQuiet and TestCacheReadinessNeverAheadOfBacking
// open lives under `bazel test`. The repo roots TEST_TMPDIR at a short tmpfs
// path with `--test_tmpdir` (.bazelrc), but t.TempDir() follows TMPDIR, which
// Bazel's linux-sandbox forces to /tmp, a hermetic directory under the output
// base on a real disk. Those two tests commit with synchronous=FULL, so on a
// disk they pay an fsync per commit, and under host load that stretches until
// a shard's 300 s budget is gone (gate tracker ga-0auozqq0). The store must
// therefore sit under TEST_TMPDIR.
func TestVerdictSQLiteStoreLivesUnderTestTmpdir(t *testing.T) {
	testTmp := os.Getenv("TEST_TMPDIR")
	if testTmp == "" {
		t.Skip("TEST_TMPDIR is unset: not running under bazel test")
	}
	engine, _ := newVerdictSQLite(t, 0)
	rel, err := filepath.Rel(testTmp, engine.path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("store path %q is not under TEST_TMPDIR %q: it sits on the sandbox's disk-backed /tmp, not the tmpfs root", engine.path, testTmp)
	}
}
