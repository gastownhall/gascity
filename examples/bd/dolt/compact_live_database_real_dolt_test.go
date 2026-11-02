//go:build integration || dolt_integration

package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompactRealDoltLiveGateAbortsWhenDatabaseLeavesDataDir pins the
// live-database gate against a real Dolt server: a full GC whose database
// directory disappears from the data dir while the server keeps running must
// abort the run under quarantine instead of reporting the compaction as a
// success and moving on to the next database.
func TestCompactRealDoltLiveGateAbortsWhenDatabaseLeavesDataDir(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}

	root := repoRoot(t)
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	beadsDir := filepath.Join(dataDir, "beads")
	zedDir := filepath.Join(dataDir, "zed")
	for _, dir := range []string{beadsDir, zedDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir db dir: %v", err)
		}
	}

	// Seed beads with compactable history, opting it into full flattening the
	// way a city-owned store is recognized.
	runDoltForCompactTest(t, doltPath, beadsDir, "init", "--name", "Gas City", "--email", "test@example.com")
	runDoltForCompactTest(t, doltPath, beadsDir, "sql", "-q",
		"CREATE TABLE beads (id int primary key, name varchar(20)); INSERT INTO beads VALUES (1, 'first');")
	runDoltForCompactTest(t, doltPath, beadsDir, "add", ".")
	runDoltForCompactTest(t, doltPath, beadsDir, "commit", "-m", "seed first bead")
	runDoltForCompactTest(t, doltPath, beadsDir, "sql", "-q", "INSERT INTO beads VALUES (2, 'second');")
	runDoltForCompactTest(t, doltPath, beadsDir, "commit", "-Am", "seed second bead")
	if err := os.WriteFile(filepath.Join(beadsDir, ".compact-full-history"), nil, 0o644); err != nil {
		t.Fatalf("write .compact-full-history: %v", err)
	}
	// Seed zed as the healthy peer that must stay untouched.
	runDoltForCompactTest(t, doltPath, zedDir, "init", "--name", "Gas City", "--email", "test@example.com")
	runDoltForCompactTest(t, doltPath, zedDir, "sql", "-q",
		"CREATE TABLE zed (id int primary key); INSERT INTO zed VALUES (1);")
	runDoltForCompactTest(t, doltPath, zedDir, "add", ".")
	runDoltForCompactTest(t, doltPath, zedDir, "commit", "-m", "seed zed")

	// The CLI shim forwards every invocation to the real dolt and, after a
	// successful DOLT_GC call, removes the flattened database's directory from
	// the data dir — the failure mode the gate exists to catch. The gc stub
	// records the compact script's notification attempts.
	shimDir := t.TempDir()
	gcLog := writeDogFakeGC(t, shimDir)
	writeExecutable(t, filepath.Join(shimDir, "dolt"), fmt.Sprintf(`#!/bin/sh
%s "$@"
rc=$?
case "$*" in
  *DOLT_GC*)
    if [ "$rc" -eq 0 ]; then
      rm -rf -- "$GC_DOLT_DATA_DIR/beads"
    fi
    ;;
esac
exit "$rc"
`, shellQuote(doltPath)))

	port, pid := startRealDoltServerForCompactTest(t, doltPath, dataDir)
	writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, pid)
	waitForDoltServerQueryForCompactTest(t, doltPath, port)
	zedHeadBefore := doltServerHeadForDbForCompactTest(t, doltPath, port, "zed")

	out, err := runCompactScriptForRealDoltTest(t, filepath.Join(shimDir, "dolt"), root, cityPath, dataDir, port, nil,
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=1",
		"GC_DOLT_COMPACT_CALL_TIMEOUT_SECS=20",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
	)
	if err == nil {
		t.Fatalf("compact must abort when the flattened database leaves the data dir:\n%s", out)
	}
	if !strings.Contains(out, "live-database gate failed at step post-flatten full GC") {
		t.Fatalf("output missing live-gate failure at the full-GC step:\n%s", out)
	}
	if !strings.Contains(out, "aborting the run before any further database work") {
		t.Fatalf("output missing the run abort:\n%s", out)
	}
	if strings.Contains(out, "db=zed") {
		t.Fatalf("no statement may be issued for the second database after a live-gate failure:\n%s", out)
	}

	marker := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	markerData, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatalf("compact must quarantine the database that left the data dir: %v\n%s", readErr, out)
	}
	for _, want := range []string{
		"reason=live database unavailable after compaction",
		"live_gate_failed_step=post-flatten full GC",
		"live_gate_data_dir_present_before=1",
		"live_gate_data_dir_present_after=0",
	} {
		if !strings.Contains(string(markerData), want) {
			t.Fatalf("quarantine marker missing %q:\n%s", want, markerData)
		}
	}
	if !strings.Contains(string(markerData), "live_gate_pre_compaction_head=") ||
		strings.Contains(string(markerData), "live_gate_pre_compaction_head=\n") {
		t.Fatalf("quarantine marker must record a nonempty pre-compaction HEAD:\n%s", markerData)
	}

	gcCalls, readErr := os.ReadFile(gcLog)
	if readErr != nil {
		t.Fatalf("read gc notification log: %v", readErr)
	}
	if !strings.Contains(string(gcCalls), "event emit dolt.compact.quarantine") {
		t.Fatalf("the quarantine must emit its event:\n%s", gcCalls)
	}
	if !strings.Contains(string(gcCalls), "mail send") ||
		!strings.Contains(string(gcCalls), "live database unavailable after compaction") {
		t.Fatalf("the quarantine must alert through mail with the live-database reason:\n%s", gcCalls)
	}

	// The peer database is untouched: same HEAD, its directory intact, and
	// not even the first-sight tag was created, because the run aborted
	// before reaching it.
	if zedHeadAfter := doltServerHeadForDbForCompactTest(t, doltPath, port, "zed"); zedHeadAfter != zedHeadBefore {
		t.Fatalf("zed HEAD changed across the aborted run: before=%s after=%s", zedHeadBefore, zedHeadAfter)
	}
	if _, statErr := os.Stat(filepath.Join(zedDir, ".dolt")); statErr != nil {
		t.Fatalf("zed data-dir entry must be intact: %v", statErr)
	}
	tags := doltServerQueryForDbForCompactTest(t, doltPath, port, "zed", "SELECT tag_name FROM dolt_tags")
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "gc-compact-base" {
			t.Fatalf("the aborted run must not have tagged the unreached database:\n%s", out)
		}
	}
}

// doltServerHeadForDbForCompactTest returns the served HEAD of the named
// database on the test sql-server.
func doltServerHeadForDbForCompactTest(t *testing.T, doltPath string, port int, db string) string {
	t.Helper()
	rows := doltServerQueryForDbForCompactTest(t, doltPath, port, db, "SELECT HASHOF('HEAD')")
	if len(rows) == 0 || strings.TrimSpace(rows[0]) == "" {
		t.Fatalf("unexpected %s HEAD output", db)
	}
	return strings.TrimSpace(rows[0])
}

// doltServerQueryForDbForCompactTest runs query against the named database on
// the test sql-server and returns the CSV rows without the header.
func doltServerQueryForDbForCompactTest(t *testing.T, doltPath string, port int, db, query string) []string {
	t.Helper()
	ctx, cancel := doltCallContext(t)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--user", "root",
		"--no-tls",
		"--use-db", db,
		"sql", "-r", "csv", "-q", query,
	)
	cmd.Env = append(filteredEnv("DOLT_CLI_PASSWORD"), "DOLT_CLI_PASSWORD=", "NO_COLOR=1")
	cmd.WaitDelay = doltCallWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("query %s %q: %v\n%s", db, query, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) <= 1 {
		return nil
	}
	return lines[1:]
}
