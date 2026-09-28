//go:build integration || dolt_integration

package gastown_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReaperWorkflowRootCleanupRealDoltSemantics(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}

	cityDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "dolt")
	for _, db := range []string{"citydb", "rigdb"} {
		dbDir := filepath.Join(dataDir, db)
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dbDir, err)
		}
		runDoltForMaintenanceTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
		runDoltSQLForMaintenanceTest(t, doltPath, dbDir, maintenanceReaperSchemaSQL())
	}

	runDoltSQLForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "citydb"), maintenanceReaperCitySeedSQL())
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "citydb"), "add", ".")
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "citydb"), "commit", "-m", "seed city workflow roots")

	runDoltSQLForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "rigdb"), maintenanceReaperRigSeedSQL())
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "rigdb"), "add", ".")
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "rigdb"), "commit", "-m", "seed rig workflow roots")

	port := startDoltServerForMaintenanceTest(t, doltPath, dataDir)
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "citydb")
	writeCityBeadsMetadata(t, cityDir, "citydb")
	rigDir := filepath.Join(cityDir, "rigs", "rig-with-db-alias")
	writeCityBeadsMetadata(t, rigDir, "rigdb")
	writeSiteRigBinding(t, cityDir, "rig-with-db-alias", rigDir)

	binDir := t.TempDir()
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	if err := os.Symlink(doltPath, filepath.Join(binDir, "dolt")); err != nil {
		t.Fatalf("Symlink(dolt): %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
set -e
printf '%s\n' "$*" >> "$BD_CALL_LOG"
case "$1" in
  prune)
    printf '{"pruned_count":0}\n'
    ;;
  close)
    issue_id="$2"
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" dolt --host "$GC_DOLT_HOST" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls --use-db citydb sql \
      -q "UPDATE issues SET status='closed', closed_at=NOW() WHERE id='${issue_id}'; CALL DOLT_COMMIT('-Am', 'test bd close')"
    ;;
esac
exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
case "$1 $2" in
  "session prune")
    printf '{"count":0}\n'
    ;;
esac
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":      bdLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     fmt.Sprintf("%d", port),
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	runScript(t, coreScriptPath("reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(bdData), "close issue-close --reason stale inactive workflow root auto-closed by reaper") {
		t.Fatalf("reaper did not close city workflow issue root through bd close:\n%s", bdData)
	}

	cityWispStatuses := queryMaintenanceStatusByID(t, doltPath, port, "citydb", "wisps")
	requireMaintenanceStatuses(t, cityWispStatuses, map[string]string{
		"wisp-close":               "closed",
		"wisp-city-store-root":     "closed",
		"wisp-cross-store-root":    "open",
		"wisp-held":                "blocked",
		"wisp-non-root-workflow":   "open",
		"wisp-recent-root":         "open",
		"wisp-nested-root":         "open",
		"wisp-subroot":             "closed",
		"wisp-live-grandchild":     "in_progress",
		"wisp-recent-closed-child": "closed",
	})

	cityIssueStatuses := queryMaintenanceStatusByID(t, doltPath, port, "citydb", "issues")
	requireMaintenanceStatuses(t, cityIssueStatuses, map[string]string{
		"issue-city-store-root":   "closed",
		"issue-close":             "closed",
		"issue-cross-store-root":  "open",
		"issue-held":              "blocked",
		"issue-dep-root":          "open",
		"issue-dep-live":          "in_progress",
		"issue-non-root-workflow": "open",
	})

	rigWispStatuses := queryMaintenanceStatusByID(t, doltPath, port, "rigdb", "wisps")
	requireMaintenanceStatuses(t, rigWispStatuses, map[string]string{
		"rig-wisp-close":            "closed",
		"rig-wisp-store-root":       "closed",
		"rig-wisp-other-store-root": "open",
	})

	rigIssueStatuses := queryMaintenanceStatusByID(t, doltPath, port, "rigdb", "issues")
	requireMaintenanceStatuses(t, rigIssueStatuses, map[string]string{
		"rig-issue-preserve": "open",
	})
}

func maintenanceReaperSchemaSQL() string {
	return `
CREATE TABLE wisps (
  id VARCHAR(64) PRIMARY KEY,
  title VARCHAR(255),
  status VARCHAR(32),
  issue_type VARCHAR(32),
  priority BIGINT,
  created_at DATETIME(6),
  updated_at DATETIME(6),
  closed_at DATETIME(6),
  assignee VARCHAR(255),
  description LONGTEXT,
  metadata JSON
);
CREATE TABLE issues (
  id VARCHAR(64) PRIMARY KEY,
  title VARCHAR(255),
  status VARCHAR(32),
  issue_type VARCHAR(32),
  priority BIGINT,
  created_at DATETIME(6),
  updated_at DATETIME(6),
  closed_at DATETIME(6),
  assignee VARCHAR(255),
  description LONGTEXT,
  metadata JSON
);
CREATE TABLE dependencies (
  issue_id VARCHAR(64),
  depends_on_issue_id VARCHAR(64),
  depends_on_wisp_id VARCHAR(64),
  depends_on_external VARCHAR(64),
  type VARCHAR(32)
);
CREATE TABLE wisp_dependencies (
  issue_id VARCHAR(64),
  depends_on_issue_id VARCHAR(64),
  depends_on_wisp_id VARCHAR(64),
  depends_on_external VARCHAR(64),
  type VARCHAR(32)
);
CREATE TABLE labels (
  issue_id VARCHAR(64),
  label VARCHAR(255)
);
CREATE TABLE wisp_labels (
  issue_id VARCHAR(64),
  label VARCHAR(255)
);
`
}

func maintenanceReaperCitySeedSQL() string {
	return `
INSERT INTO wisps (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('wisp-close', 'closeable root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-city-store-root', 'closeable city-store root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"city:test-city"}'),
  ('wisp-cross-store-root', 'cross-store root preserved', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:other"}'),
  ('wisp-held', 'held root', 'blocked', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-non-root-workflow', 'non-root topology bead preserved', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_bead_id":"wisp-nested-root"}'),
  ('wisp-recent-root', 'recent descendant root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-recent-closed-child', 'recent closed child', 'closed', 'task', 2, '2026-01-01 00:00:00', NOW(), '', '{"gc.root_bead_id":"wisp-recent-root"}'),
  ('wisp-nested-root', 'nested root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-subroot', 'nested subroot', 'closed', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.root_bead_id":"wisp-nested-root"}'),
  ('wisp-live-grandchild', 'live nested child', 'in_progress', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{}');
INSERT INTO wisp_dependencies (issue_id, depends_on_wisp_id, type) VALUES
  ('wisp-subroot', 'wisp-nested-root', 'tracks'),
  ('wisp-live-grandchild', 'wisp-subroot', 'tracks');
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('issue-close', 'closeable city issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('issue-city-store-root', 'closeable city-store issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"city:test-city"}'),
  ('issue-cross-store-root', 'cross-store issue root preserved', 'open', 'task', 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:other"}'),
  ('issue-held', 'held city issue root', 'blocked', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('issue-non-root-workflow', 'non-root issue topology bead preserved', 'open', 'task', 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_bead_id":"issue-dep-root"}'),
  ('issue-dep-root', 'dependency-protected issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('issue-dep-live', 'live issue dependency child', 'in_progress', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{}');
INSERT INTO dependencies (issue_id, depends_on_issue_id, type) VALUES
  ('issue-dep-live', 'issue-dep-root', 'blocks');
`
}

func maintenanceReaperRigSeedSQL() string {
	return `
INSERT INTO wisps (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('rig-wisp-close', 'closeable non-city wisp root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('rig-wisp-store-root', 'closeable rig-store wisp root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:rig-with-db-alias"}'),
  ('rig-wisp-other-store-root', 'other rig-store root preserved', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:other"}');
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('rig-issue-preserve', 'non-city issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}');
`
}

func runDoltForMaintenanceTest(t *testing.T, doltPath, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt %s failed in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func runDoltSQLForMaintenanceTest(t *testing.T, doltPath, dir, query string) string {
	t.Helper()
	return runDoltForMaintenanceTest(t, doltPath, dir, "sql", "-q", query)
}

func startDoltServerForMaintenanceTest(t *testing.T, doltPath, dataDir string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("Close listener: %v", err)
	}

	logPath := filepath.Join(dataDir, "sql-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("Create(%s): %v", logPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, doltPath, "sql-server",
		"-H", "127.0.0.1",
		"-P", fmt.Sprintf("%d", port),
		"--data-dir", dataDir,
		"--loglevel", "warning",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("Start dolt sql-server: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-waitCh:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
		}
		_ = logFile.Close()
	})
	return port
}

func waitForDoltServerForMaintenanceTest(t *testing.T, doltPath string, port int, db string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastOut []byte
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, doltPath,
			"--host", "127.0.0.1",
			"--port", fmt.Sprintf("%d", port),
			"--user", "root",
			"--no-tls",
			"--use-db", db,
			"sql", "-q", "SELECT 1",
		)
		cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
		lastOut, lastErr = cmd.CombinedOutput()
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("dolt sql-server did not become ready on port %d: %v\n%s", port, lastErr, lastOut)
}

func queryMaintenanceStatusByID(t *testing.T, doltPath string, port int, db string, table string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--user", "root",
		"--no-tls",
		"--use-db", db,
		"sql", "-r", "csv", "-q", fmt.Sprintf("SELECT id,status FROM %s ORDER BY id", table),
	)
	cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("query %s.%s statuses: %v\n%s", db, table, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "id,status" {
		t.Fatalf("unexpected status output for %s.%s:\n%s", db, table, out)
	}
	statuses := make(map[string]string)
	for _, line := range lines[1:] {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			t.Fatalf("unexpected status row for %s.%s: %q\nfull output:\n%s", db, table, line, out)
		}
		statuses[fields[0]] = fields[1]
	}
	return statuses
}

func requireMaintenanceStatuses(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	for id, wantStatus := range want {
		if got[id] != wantStatus {
			t.Fatalf("status[%s] = %q, want %q\nall statuses: %#v", id, got[id], wantStatus, got)
		}
	}
}

// TestReaperStaleIssueCloseSkipsDurableExtmsgRecordsRealDolt runs the real
// reaper.sh Step 5 stale-issue query against a real Dolt sql-server. Durable
// extmsg protocol records (group roots, participants, bindings, memberships,
// transcript state and entries) must survive the stale sweep while an ordinary
// stale task is still closed, and the query itself must not fail (#6380).
//
// A hyphenated rig database (my-rig) pins that the correlated label probe
// binds to each database's own labels table: decoy labels in the other
// database must not change which rows are stale candidates, and the query
// must not fail for rig databases either.
func TestReaperStaleIssueCloseSkipsDurableExtmsgRecordsRealDolt(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}

	durableLabels := map[string]string{
		"ext-group":            "gc:extmsg-group",
		"ext-participant":      "gc:extmsg-participant",
		"ext-binding":          "gc:extmsg-binding",
		"ext-membership":       "gc:extmsg-membership",
		"ext-transcript-state": "gc:extmsg-transcript-state",
		"ext-transcript":       "gc:extmsg-transcript",
	}

	var seed strings.Builder
	seed.WriteString("INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES\n")
	seed.WriteString("  ('ord-stale', 'ordinary stale task', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}')")
	for id := range durableLabels {
		fmt.Fprintf(&seed, ",\n  ('%s', 'durable extmsg record', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}')", id)
	}
	seed.WriteString(";\nINSERT INTO labels (issue_id, label) VALUES\n  ('ord-stale', 'unrelated-label')")
	for id, label := range durableLabels {
		fmt.Fprintf(&seed, ",\n  ('%s', '%s')", id, label)
	}
	// Decoy: a durable label for the rig's ordinary row lives only in citydb.
	seed.WriteString(",\n  ('rig-ord', 'gc:extmsg-group');\n")

	// my-rig: rig-ext and rig-ext2 are durable via my-rig's own labels; rig-ord
	// is ordinary there. The decoy label for the city's ord-stale lives only in
	// my-rig. The asymmetric counts (1 ordinary vs 2 durable) make a probe
	// against the wrong database's labels table visible in the candidate count.
	rigSeed := `
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('rig-ord', 'ordinary stale rig task', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}'),
  ('rig-ext', 'durable extmsg rig record', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}'),
  ('rig-ext2', 'durable extmsg rig record', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}');
INSERT INTO labels (issue_id, label) VALUES
  ('rig-ext', 'gc:extmsg-group'),
  ('rig-ext2', 'gc:extmsg-binding'),
  ('ord-stale', 'gc:extmsg-group');
`

	cityDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "dolt")
	for db, dbSeed := range map[string]string{"citydb": seed.String(), "my-rig": rigSeed} {
		dbDir := filepath.Join(dataDir, db)
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dbDir, err)
		}
		runDoltForMaintenanceTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
		runDoltSQLForMaintenanceTest(t, doltPath, dbDir, maintenanceReaperSchemaSQL())
		runDoltSQLForMaintenanceTest(t, doltPath, dbDir, dbSeed)
		runDoltForMaintenanceTest(t, doltPath, dbDir, "add", ".")
		runDoltForMaintenanceTest(t, doltPath, dbDir, "commit", "-m", "seed stale extmsg records")
	}

	port := startDoltServerForMaintenanceTest(t, doltPath, dataDir)
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "citydb")
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "my-rig")
	writeCityBeadsMetadata(t, cityDir, "citydb")
	rigDir := filepath.Join(cityDir, "rigs", "my-rig")
	writeCityBeadsMetadata(t, rigDir, "my-rig")
	writeSiteRigBinding(t, cityDir, "my-rig", rigDir)

	binDir := t.TempDir()
	logDir := t.TempDir()
	bdLog := filepath.Join(logDir, "bd.log")
	escalateLog := filepath.Join(logDir, "escalate.log")
	if err := os.Symlink(doltPath, filepath.Join(binDir, "dolt")); err != nil {
		t.Fatalf("Symlink(dolt): %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
set -e
printf '%s\n' "$*" >> "$BD_CALL_LOG"
case "$1" in
  prune)
    printf '{"pruned_count":0}\n'
    ;;
  close)
    issue_id="$2"
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" dolt --host "$GC_DOLT_HOST" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls --use-db citydb sql \
      -q "UPDATE issues SET status='closed', closed_at=NOW() WHERE id='${issue_id}'; CALL DOLT_COMMIT('-Am', 'test bd close')"
    ;;
esac
exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
case "$1 $2" in
  "session prune")
    printf '{"count":0}\n'
    ;;
esac
exit 0
`)
	escalateScript := filepath.Join(binDir, "escalate.sh")
	writeExecutable(t, escalateScript, `#!/bin/sh
printf '%s\n' "$*" >> "$ESCALATE_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":        bdLog,
		"ESCALATE_CALL_LOG":  escalateLog,
		"GC_ESCALATE_SCRIPT": escalateScript,
		"GC_CITY":            cityDir,
		"GC_CITY_PATH":       cityDir,
		"GC_DOLT_HOST":       "127.0.0.1",
		"GC_DOLT_PORT":       fmt.Sprintf("%d", port),
		"GC_DOLT_USER":       "root",
		"GC_DOLT_PASSWORD":   "",
		"PATH":               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	reaperOut, err := runScriptResult(t, coreScriptPath("reaper.sh"), env)
	if err != nil {
		t.Fatalf("reaper.sh failed: %v\n%s", err, reaperOut)
	}

	escalateData, err := os.ReadFile(escalateLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(escalate log): %v", err)
	}
	if strings.Contains(string(escalateData), "stale issue query failed") {
		t.Fatalf("reaper stale issue query failed on real Dolt:\n%s", escalateData)
	}
	// Rig rows are never closed by the city reaper; they are counted as skipped
	// candidates. Exactly one (rig-ord) must be a candidate: rig-ext/rig-ext2
	// are durable via my-rig's labels, and the citydb decoy must not hide
	// rig-ord. Probing citydb's labels instead would count 2; no filter, 3.
	if !strings.Contains(string(reaperOut), "skipped_non_city_issues:1,") {
		t.Fatalf("reaper did not report exactly one my-rig stale candidate:\n%s\nescalations:\n%s", reaperOut, escalateData)
	}

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v\nescalations:\n%s", err, escalateData)
	}
	if !strings.Contains(string(bdData), "close ord-stale --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not close the ordinary stale issue:\nbd calls:\n%s\nescalations:\n%s", bdData, escalateData)
	}
	for id := range durableLabels {
		if strings.Contains(string(bdData), "close "+id+" ") {
			t.Fatalf("reaper closed durable extmsg record %s:\n%s", id, bdData)
		}
	}

	want := map[string]string{"ord-stale": "closed"}
	for id := range durableLabels {
		want[id] = "open"
	}
	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, doltPath, port, "citydb", "issues"), want)
	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, doltPath, port, "my-rig", "issues"), map[string]string{
		"rig-ord":  "open",
		"rig-ext":  "open",
		"rig-ext2": "open",
	})
}
