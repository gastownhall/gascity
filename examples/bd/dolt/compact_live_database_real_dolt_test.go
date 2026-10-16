//go:build integration || dolt_integration

package dolt_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Real-Dolt regression for a lost live database: a database that
// leaves the data dir during compaction must stop the run, not be reported as
// compacted while the next database is rewritten.
func TestCompactRealDoltAbortsWhenDatabaseLeavesDataDirDuringCompaction(t *testing.T) {
	c := newHistoryProtectionCity(t)
	seedCommits := historyProtectionThreshold + 5
	for _, db := range []string{"beads", "zed"} {
		dir := filepath.Join(c.dataDir, db)
		seedTeamHistory(t, c.dolt, dir, seedCommits)
		if err := os.WriteFile(filepath.Join(dir, ".compact-full-history"), nil, 0o644); err != nil {
			t.Fatalf("write .compact-full-history for %s: %v", db, err)
		}
	}
	c.start()
	zedCommitsBefore := realDoltCommitCount(t, c, "zed")

	// The shim runs the real dolt, then drops beads right after its full GC
	// succeeds: the database directory leaves the data dir and the server
	// stops serving the name, the signature of a lost live database.
	shimDir := t.TempDir()
	gcLog, _ := writeCompactFakeGC(t, shimDir)
	shim := filepath.Join(shimDir, "dolt")
	writeExecutable(t, shim, fmt.Sprintf(`#!/bin/sh
%[1]s "$@"
rc=$?
case "$*" in
  *"--use-db beads"*"DOLT_GC('--full')"*)
    if [ "$rc" -eq 0 ]; then
      %[1]s --host 127.0.0.1 --port %[2]d --user root --no-tls --use-db zed sql -q "DROP DATABASE beads" >&2 || exit 99
    fi
    ;;
esac
exit "$rc"
`, shellQuote(c.dolt), c.port))

	out, err := runCompactScriptForRealDoltTest(t, shim, c.root, c.city, c.dataDir, c.port, nil,
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS="+strconv.Itoa(historyProtectionThreshold),
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_DOLT_COMPACT_ALERT_TO=dolt-ops")
	t.Logf("compact (err=%v):\n%s", err, out)
	if err == nil {
		t.Fatalf("compact succeeded although beads left the data dir after its full GC")
	}
	for _, want := range []string{
		"db=beads is no longer served after full GC",
		"data_dir_entry_missing",
		"aborting this run so no other database is compacted (untouched: zed)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q", want)
		}
	}
	if strings.Contains(out, "db=zed") {
		t.Fatalf("compact must not touch zed after losing beads")
	}
	if got := realDoltCommitCount(t, c, "zed"); got != zedCommitsBefore {
		t.Fatalf("zed commits = %d after the aborted run, want untouched %d", got, zedCommitsBefore)
	}

	marker := filepath.Join(c.city, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if got := compactMarkerValue(t, marker, "reason"); got != "live database unavailable after compaction" {
		t.Fatalf("quarantine reason = %q", got)
	}
	assertCompactMarkerHasEvidence(t, marker, "compaction_step=full GC", "data_dir_entry_before=present")
	if head := compactMarkerValue(t, marker, "pre_compaction_head"); head == "" {
		t.Fatalf("marker must record the pre-compaction HEAD for the restore")
	}
	gcCalls, readErr := os.ReadFile(gcLog)
	if readErr != nil {
		t.Fatalf("read fake gc log: %v", readErr)
	}
	if !strings.Contains(string(gcCalls), "gc event emit dolt.compact.quarantine") ||
		!strings.Contains(string(gcCalls), "gc mail send dolt-ops") {
		t.Fatalf("lost database must emit the quarantine event and mail the alert recipient:\n%s", gcCalls)
	}
}

func realDoltCommitCount(t *testing.T, c *historyProtectionCity, db string) int {
	t.Helper()
	rows := doltServerQueryDBForCompactTest(t, c.dolt, c.port, db, "SELECT COUNT(*) FROM dolt_log")
	if len(rows) == 0 {
		t.Fatalf("count %s commits: no rows", db)
	}
	n, err := strconv.Atoi(strings.TrimSpace(rows[len(rows)-1]))
	if err != nil {
		t.Fatalf("parse %s commit count from %q: %v", db, rows, err)
	}
	return n
}
