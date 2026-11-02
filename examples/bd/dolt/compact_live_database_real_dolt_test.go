//go:build integration || dolt_integration

// Real-Dolt counterpart of the fake-dolt live-database gate tests: a full GC
// whose storage rewrite drops the database out of the data dir of a running
// sql-server must fail the pass, quarantine the database, and abort the run
// before the next database, not report the pass as success.

package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// doltServerHeadInDBForCompactTest returns HASHOF('HEAD') for db on the test
// sql-server, so a test can pin a database's history across a compaction
// run. Unlike doltServerHeadForCompactTest it takes the database name.
func doltServerHeadInDBForCompactTest(t *testing.T, doltPath string, port int, db string) string {
	t.Helper()
	ctx, cancel := doltCallContext(t)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--user", "root",
		"--no-tls",
		"--use-db", db,
		"sql", "-r", "csv", "-q", "SELECT HASHOF('HEAD')",
	)
	cmd.Env = append(filteredEnv("DOLT_CLI_PASSWORD"), "DOLT_CLI_PASSWORD=", "NO_COLOR=1")
	cmd.WaitDelay = doltCallWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("query %s HEAD on server: %v\n%s", db, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[1]) == "" {
		t.Fatalf("unexpected %s HEAD output: %q", db, out)
	}
	return strings.TrimSpace(lines[1])
}

// TestCompactRealDoltAbortsWhenDatabaseLeavesDataDirAfterGC runs compact
// against a real sql-server serving two databases and a dolt shim that drops
// the beads data-dir entry after a successful --full DOLT_GC on beads. The
// pass must be quarantined and the run aborted before zed is touched.
func TestCompactRealDoltAbortsWhenDatabaseLeavesDataDirAfterGC(t *testing.T) {
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
	}
	beadsDir := filepath.Join(dataDir, "beads")
	runDoltForCompactTest(t, doltPath, beadsDir, "sql", "-q",
		"CREATE TABLE beads (id int primary key, name varchar(20)); INSERT INTO beads VALUES (1, 'first');")
	runDoltForCompactTest(t, doltPath, beadsDir, "add", ".")
	runDoltForCompactTest(t, doltPath, beadsDir, "commit", "-m", "seed first bead")
	runDoltForCompactTest(t, doltPath, beadsDir, "sql", "-q", "INSERT INTO beads VALUES (2, 'second');")
	runDoltForCompactTest(t, doltPath, beadsDir, "commit", "-Am", "seed second bead")
	// This history is the city's own (not adopted), so opt beads into full
	// flattening; zed stays on the default first-sight watermark.
	if err := os.WriteFile(filepath.Join(beadsDir, ".compact-full-history"), nil, 0o644); err != nil {
		t.Fatalf("write .compact-full-history: %v", err)
	}

	port, pid := startRealDoltServerForCompactTest(t, doltPath, dataDir)
	writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, pid)
	waitForDoltServerQueryForCompactTest(t, doltPath, port)
	zedHeadBefore := doltServerHeadInDBForCompactTest(t, doltPath, port, "zed")

	// Shim dolt: after a successful --full DOLT_GC on beads, drop the beads
	// database directory out of the data dir — the storage rewrite that
	// leaves the server without the database it just compacted.
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "dolt")
	shim := fmt.Sprintf(`#!/bin/sh
REAL="%s"
"$REAL" "$@"
rc=$?
if [ "$rc" -eq 0 ]; then
  prev=""
  for a in "$@"; do
    case "$prev" in --use-db) case "$a" in beads) use_beads=1 ;; esac ;; esac
    case "$a" in *DOLT_GC*) gc=1 ;; esac
    case "$a" in *--full*) full=1 ;; esac
    prev="$a"
  done
  if [ "${use_beads:-0}" = 1 ] && [ "${gc:-0}" = 1 ] && [ "${full:-0}" = 1 ]; then
    rm -rf -- "${GC_DOLT_DATA_DIR:-}/beads"
  fi
fi
exit "$rc"
`, doltPath)
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		t.Fatalf("write dolt shim: %v", err)
	}

	out, err := runCompactScriptForRealDoltTest(t, shimPath, root, cityPath, dataDir, port, nil,
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=1",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
	)
	if err == nil {
		t.Fatalf("compact succeeded although beads left the data dir after its full GC:\n%s", out)
	}
	marker := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	body, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatalf("compact must quarantine a database that left the data dir: %v\n%s", readErr, out)
	}
	for _, want := range []string{
		"reason=live database unavailable after compaction",
		"live_gate_failed_step=post-flatten full GC",
		"live_gate_dir_present_before=1",
		"live_gate_dir_present_after=0",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("quarantine marker missing %q:\n%s\noutput:\n%s", want, body, out)
		}
	}
	if !strings.Contains(out, "not reached after the abort: zed") {
		t.Fatalf("compact abort diagnostic must name the database not reached:\n%s", out)
	}
	if zedHeadAfter := doltServerHeadInDBForCompactTest(t, doltPath, port, "zed"); zedHeadAfter != zedHeadBefore {
		t.Fatalf("zed HEAD changed %s -> %s although the run aborted on beads", zedHeadBefore, zedHeadAfter)
	}
}
