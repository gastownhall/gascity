//go:build integration || dolt_integration

package dolt_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompactRealDoltLiveGateAbortsWhenDatabaseLeavesDataDir reproduces the
// #7909 outcome against a real sql-server: a dolt CLI shim drops the beads
// database out of the data dir right after its full GC succeeds, while the
// server keeps answering SQL. The live-database gate must quarantine beads,
// alert through the quarantine path, abort the run before the second
// database is touched, and leave that database's history unchanged.
func TestCompactRealDoltLiveGateAbortsWhenDatabaseLeavesDataDir(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}

	root := repoRoot(t)
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	for _, db := range []string{"beads", "zed"} {
		dbDir := filepath.Join(dataDir, db)
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			t.Fatalf("mkdir %s db dir: %v", db, err)
		}
		runDoltForCompactTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
		runDoltForCompactTest(t, doltPath, dbDir, "sql", "-q",
			"CREATE TABLE items (id int primary key, name varchar(20)); INSERT INTO items VALUES (1, 'first');")
		runDoltForCompactTest(t, doltPath, dbDir, "add", ".")
		runDoltForCompactTest(t, doltPath, dbDir, "commit", "-m", "seed first item")
		runDoltForCompactTest(t, doltPath, dbDir, "sql", "-q", "INSERT INTO items VALUES (2, 'second');")
		runDoltForCompactTest(t, doltPath, dbDir, "commit", "-Am", "seed second item")
		// This history is the city's own (not adopted), so opt it into full
		// flattening; no remotes, so nothing holds the history back.
		if err := os.WriteFile(filepath.Join(dbDir, ".compact-full-history"), nil, 0o644); err != nil {
			t.Fatalf("write .compact-full-history: %v", err)
		}
	}
	zedHeadBefore := doltHeadForCompactTest(t, doltPath, filepath.Join(dataDir, "zed"))

	// Shim the dolt CLI: after the beads database's full GC succeeds, drop
	// beads out of the data dir while the sql-server keeps running — the
	// #7909 failure mode. Only the test's own data dir is ever touched.
	shimDir := t.TempDir()
	shim := `#!/bin/sh
"` + doltPath + `" "$@"
rc=$?
if [ "$rc" -eq 0 ] && [ -n "$GC_DOLT_DATA_DIR" ]; then
  case "$*" in
    *"--use-db beads"*)
      case "$*" in
        *"DOLT_GC('--full'"*)
          rm -rf -- "$GC_DOLT_DATA_DIR/beads"
          ;;
      esac
      ;;
  esac
fi
exit "$rc"
`
	if err := os.WriteFile(filepath.Join(shimDir, "dolt"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write dolt shim: %v", err)
	}

	port, pid := startRealDoltServerForCompactTest(t, doltPath, dataDir)
	writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, pid)
	waitForDoltServerQueryForCompactTest(t, doltPath, port)

	out, err := runCompactScriptForRealDoltTest(t, filepath.Join(shimDir, "dolt"), root, cityPath, dataDir, port, nil,
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=1",
		"GC_DOLT_COMPACT_CALL_TIMEOUT_SECS=60",
		"GC_DOLT_COMPACT_PUSH_TIMEOUT_SECS=60",
	)
	if err == nil {
		t.Fatalf("compact succeeded although beads left the data dir after its full GC:\n%s", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("live-gate abort must exit 1, got %v:\n%s", err, out)
	}
	if !strings.Contains(out, "live gate failed") || !strings.Contains(out, "not reached: zed") {
		t.Fatalf("abort output missing the gate failure and the not-reached database naming:\n%s", out)
	}
	marker := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	assertCompactMarkerHasEvidence(t, marker,
		"reason=live database unavailable after compaction",
		"live_gate_dir_after=absent",
		"live_gate_failed_step=post-flatten full GC",
		"decision=preserve_marker_manual_review_required",
	)
	// The run never reached the second database: no statements, no
	// compaction output, and its history is exactly what it was before.
	if strings.Contains(out, "db=zed") {
		t.Fatalf("the aborted run must not touch zed:\n%s", out)
	}
	if got := doltHeadForCompactTest(t, doltPath, filepath.Join(dataDir, "zed")); got != zedHeadBefore {
		t.Fatalf("zed HEAD changed across the aborted run: before=%s after=%s", zedHeadBefore, got)
	}
}
