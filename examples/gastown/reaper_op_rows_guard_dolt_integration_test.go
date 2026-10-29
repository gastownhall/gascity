//go:build integration || dolt_integration

package gastown_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReaperOpRowCensusOnRealDolt runs the reaper's op-row census against a
// real Dolt server, which no stub can stand in for: the census is the only
// thing that tells a scope holding op rows from one that holds none. The city
// holds an op row in its issues table only and rig ops one in its wisps table
// only; both are held back, the session prune included. Rig clean holds closed
// rows of other types, one of them "operation", and is purged as before.
func TestReaperOpRowCensusOnRealDolt(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}
	const (
		cols = "(id, title, status, issue_type, priority, created_at, updated_at, closed_at, assignee, metadata)"
		old  = "'2026-01-01 00:00:00', '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{}'"
	)
	seeds := map[string]string{
		"citydb": "INSERT INTO issues " + cols + " VALUES " +
			"('hq-op-aaaa', 'op deliver c1', 'closed', 'op', 2, " + old + "), " +
			"('gm-sess-1', 'Session ended', 'closed', 'session', 2, " + old + ");",
		"opsdb": "INSERT INTO wisps " + cols + " VALUES " +
			"('ops-fact-aaaa', 'fact delivered c1', 'closed', 'op', 2, " + old + ");",
		"cleandb": "INSERT INTO wisps " + cols + " VALUES " +
			"('clean-w1', 'closed step', 'closed', 'task', 2, " + old + "); " +
			"INSERT INTO issues " + cols + " VALUES " +
			"('clean-i1', 'closed notes', 'closed', 'operation', 2, " + old + ");",
	}
	dataDir := filepath.Join(t.TempDir(), "dolt")
	for db, seed := range seeds {
		dbDir := filepath.Join(dataDir, db)
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dbDir, err)
		}
		runDoltForMaintenanceTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
		runDoltSQLForMaintenanceTest(t, doltPath, dbDir, maintenanceReaperSchemaSQL())
		runDoltSQLForMaintenanceTest(t, doltPath, dbDir, seed)
		runDoltForMaintenanceTest(t, doltPath, dbDir, "add", ".")
		runDoltForMaintenanceTest(t, doltPath, dbDir, "commit", "-m", "seed "+db)
	}
	port := startDoltServerForMaintenanceTest(t, doltPath, dataDir)
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "citydb")

	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "citydb")
	binDir := t.TempDir()
	if err := os.Symlink(doltPath, filepath.Join(binDir, "dolt")); err != nil {
		t.Fatalf("Symlink(dolt): %v", err)
	}
	// The gc route sends `gc bd sql` to the real server and every bd verb to
	// the maintenance bd double, which logs it with its scope.
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
case "$1 $2" in
  "session prune")
    printf '{"count":0}\n'
    ;;
esac
exit 0
`)
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	outcomeFile := filepath.Join(t.TempDir(), "outcome.json")
	out, err := runScriptResult(t, coreScriptPath("reaper.sh"), map[string]string{
		"BD_CALL_LOG":           bdLog,
		"BD_PURGE_COUNT":        "1",
		"BD_PRUNE_COUNT":        "1",
		"BD_BACKUP_STATUS_JSON": freshBackupStatusJSON(time.Hour, false),
		"GC_CITY":               cityDir,
		"GC_CITY_PATH":          cityDir,
		"GC_DOLT_HOST":          "127.0.0.1",
		"GC_DOLT_PORT":          fmt.Sprintf("%d", port),
		"GC_DOLT_USER":          "root",
		"GC_DOLT_PASSWORD":      "",
		"GC_ORDER_OUTCOME_FILE": outcomeFile,
		"FAKE_RIG_LIST_JSON":    `{"rigs":[{"name":"ops","hq":false},{"name":"clean","hq":false}]}`,
		"FAKE_SCOPE_DBS":        "city=citydb rig:ops=opsdb rig:clean=cleandb",
		"PATH":                  binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	if err != nil {
		t.Fatalf("reaper.sh failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	bdCalls := string(data)
	purge := " args=purge --wisps-plane --older-than 168h --json --force --limit 500"
	if !strings.Contains(bdCalls, "scope=rig:clean"+purge) {
		t.Fatalf("rig clean holds no op row (only task and operation rows) and must be purged:\n%s\n%s", bdCalls, out)
	}
	for _, held := range []string{"city", "rig:ops"} {
		if strings.Contains(bdCalls, "scope="+held+purge) {
			t.Fatalf("%s holds an op row and must not be purged:\n%s\n%s", held, bdCalls, out)
		}
	}
	if strings.Contains(bdCalls, "args=prune --pattern") {
		t.Fatalf("the city holds an op row and its session prune must not run:\n%s\n%s", bdCalls, out)
	}
	outcome := readReaperOutcome(t, outcomeFile)
	const why = "holds op rows and its bd does not honor types.retained"
	for _, want := range []struct{ scope, reason string }{
		{"city", "purge skipped: " + why},
		{"rig:ops", "purge skipped: " + why},
		{"city", "session prune skipped: " + why},
	} {
		if !outcome.hasScope(want.scope, want.reason) {
			t.Fatalf("outcome scopes = %+v, want %s %q\n%s", outcome.Scopes, want.scope, want.reason, out)
		}
	}
	if outcome.hasScope("rig:clean", "purge skipped: "+why) {
		t.Fatalf("rig clean was held back: %+v", outcome.Scopes)
	}
}

// readReaperOutcome reads the reaper's typed outcome declaration.
func readReaperOutcome(t *testing.T, path string) scriptOutcome {
	t.Helper()
	f := scopeFixture{outcomeFile: path}
	outcome, ok := f.outcome(t)
	if !ok {
		t.Fatalf("the reaper declared no outcome in %s", path)
	}
	return outcome
}
