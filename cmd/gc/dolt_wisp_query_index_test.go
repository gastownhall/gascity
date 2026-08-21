package main

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
)

const wispQueryIndexTestSchema = `
	CREATE TABLE wisps (
		id VARCHAR(64) PRIMARY KEY,
		status VARCHAR(64),
		issue_type VARCHAR(64)
	);
	CREATE TABLE wisp_labels (
		issue_id VARCHAR(64),
		label VARCHAR(64)
	);
	CREATE TABLE unrelated (
		id INT PRIMARY KEY,
		value VARCHAR(64)
	);
	CALL DOLT_ADD('wisps', 'wisp_labels', 'unrelated');
	CALL DOLT_COMMIT('-m', 'test: seed wisp index schema');
`

func TestApplyWispQueryIndexesToDB_ReturnsErrorOnUnreachableServer(t *testing.T) {
	var stderr bytes.Buffer
	err := applyWispQueryIndexesToDB(context.Background(), "19999", "hq", &stderr)
	if err == nil {
		t.Fatal("expected error for unreachable port, got nil")
	}
}

func TestApplyWispQueryIndexesToDB_ContextCancellation(_ *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	// Should fail quickly with context error or connection error, not hang.
	_ = applyWispQueryIndexesToDB(ctx, "19999", "hq", &stderr)
}

func TestWispQueryIndexStatements_AllHaveCreateIndex(t *testing.T) {
	if len(wispQueryIndexes) == 0 {
		t.Fatal("wispQueryIndexes must not be empty")
	}
	for _, index := range wispQueryIndexes {
		if len(index.createStmt) < 20 {
			t.Errorf("statement too short to be a valid DDL: %q", index.createStmt)
		}
	}
}

func TestWispQueryIndexAddStatementsAreTableScoped(t *testing.T) {
	want := map[string]string{
		"wisp_labels": "CALL DOLT_ADD('wisp_labels')",
		"wisps":       "CALL DOLT_ADD('wisps')",
	}
	for _, index := range wispQueryIndexes {
		if index.addStmt != want[index.table] {
			t.Errorf("add statement for %s = %q, want %q", index.table, index.addStmt, want[index.table])
		}
		if strings.Contains(index.addStmt, "'.'") {
			t.Errorf("add statement for %s bulk-stages the database: %q", index.table, index.addStmt)
		}
	}
}

func TestWispQueryIndexesCommitOnlyIndexesAndPreserveUnrelatedWorkingChange(t *testing.T) {
	port, cleanup := startProjectIDTestServer(t, wispQueryIndexTestSchema)
	defer cleanup()
	db := openWispQueryIndexTestDB(t, port)
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("INSERT INTO unrelated VALUES (1, 'keep working')"); err != nil {
		t.Fatalf("insert unrelated WORKING row: %v", err)
	}
	headBefore := wispQueryIndexTestHead(t, db)

	var stderr bytes.Buffer
	if err := applyWispQueryIndexesToDB(context.Background(), port, "hq", &stderr); err != nil {
		t.Fatalf("applyWispQueryIndexesToDB: %v", err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("migration stderr = %q, want empty", got)
	}
	headAfter := wispQueryIndexTestHead(t, db)
	if headAfter == headBefore {
		t.Fatal("HEAD did not advance for index migration")
	}

	assertWispQueryIndexTestSchema(t, db, "wisps", "HEAD", "KEY `idx_wisps_status_type` (`status`,`issue_type`)", true)
	assertWispQueryIndexTestSchema(t, db, "wisp_labels", "HEAD", "KEY `idx_wisp_labels_issue_id` (`issue_id`)", true)
	assertWispQueryIndexTestRowCount(t, db, "SELECT COUNT(*) FROM unrelated AS OF 'HEAD'", 0)
	assertWispQueryIndexTestRowCount(t, db, "SELECT COUNT(*) FROM unrelated", 1)
	assertWispQueryIndexTestModifiedStatus(t, db, "unrelated", false)

	// Idempotence must not create another commit, even while the unrelated
	// WORKING row remains present.
	if err := applyWispQueryIndexesToDB(context.Background(), port, "hq", &stderr); err != nil {
		t.Fatalf("second applyWispQueryIndexesToDB: %v", err)
	}
	if got := wispQueryIndexTestHead(t, db); got != headAfter {
		t.Fatalf("idempotent migration moved HEAD from %s to %s", headAfter, got)
	}
}

func TestWispQueryIndexesTransactionPreservesConcurrentTargetWorkingChange(t *testing.T) {
	port, cleanup := startProjectIDTestServer(t, wispQueryIndexTestSchema)
	defer cleanup()
	db := openWispQueryIndexTestDB(t, port)
	defer func() { _ = db.Close() }()

	headBefore := wispQueryIndexTestHead(t, db)
	var stderr bytes.Buffer
	err := applyWispQueryIndexesToDBWithHook(context.Background(), port, "hq", &stderr, func() {
		if _, err := db.Exec("INSERT INTO wisps VALUES ('concurrent', 'open', 'message')"); err != nil {
			t.Fatalf("insert concurrent target WORKING row: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("applyWispQueryIndexesToDBWithHook: %v", err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("migration stderr = %q, want empty", got)
	}
	headAfter := wispQueryIndexTestHead(t, db)
	if headAfter == headBefore {
		t.Fatal("HEAD did not advance for index migration")
	}

	assertWispQueryIndexTestSchema(t, db, "wisps", "HEAD", "KEY `idx_wisps_status_type` (`status`,`issue_type`)", true)
	assertWispQueryIndexTestSchema(t, db, "wisp_labels", "HEAD", "KEY `idx_wisp_labels_issue_id` (`issue_id`)", true)
	assertWispQueryIndexTestRowCount(t, db, "SELECT COUNT(*) FROM wisps AS OF 'HEAD' WHERE id = 'concurrent'", 0)
	assertWispQueryIndexTestRowCount(t, db, "SELECT COUNT(*) FROM wisps WHERE id = 'concurrent'", 1)
	assertWispQueryIndexTestModifiedStatus(t, db, "wisps", false)
}

func TestWispQueryIndexesSkipPreexistingStagedChange(t *testing.T) {
	port, cleanup := startProjectIDTestServer(t, wispQueryIndexTestSchema)
	defer cleanup()
	db := openWispQueryIndexTestDB(t, port)
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("INSERT INTO unrelated VALUES (1, 'already staged')"); err != nil {
		t.Fatalf("insert unrelated row: %v", err)
	}
	if _, err := db.Exec("CALL DOLT_ADD('unrelated')"); err != nil {
		t.Fatalf("stage unrelated row: %v", err)
	}
	headBefore := wispQueryIndexTestHead(t, db)

	var stderr bytes.Buffer
	if err := applyWispQueryIndexesToDB(context.Background(), port, "hq", &stderr); err != nil {
		t.Fatalf("applyWispQueryIndexesToDB: %v", err)
	}
	if !strings.Contains(stderr.String(), "pre-existing staged change on unrelated; skipping") {
		t.Fatalf("migration stderr = %q, want staged-change skip", stderr.String())
	}
	if got := wispQueryIndexTestHead(t, db); got != headBefore {
		t.Fatalf("skipped migration moved HEAD from %s to %s", headBefore, got)
	}
	assertWispQueryIndexTestSchema(t, db, "wisps", "WORKING", "idx_wisps_status_type", false)
	assertWispQueryIndexTestSchema(t, db, "wisp_labels", "WORKING", "idx_wisp_labels_issue_id", false)
	assertWispQueryIndexTestModifiedStatus(t, db, "unrelated", true)
}

func TestWispQueryIndexesSkipDirtyTargetTable(t *testing.T) {
	port, cleanup := startProjectIDTestServer(t, wispQueryIndexTestSchema)
	defer cleanup()
	db := openWispQueryIndexTestDB(t, port)
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("INSERT INTO wisps VALUES ('w1', 'open', 'message')"); err != nil {
		t.Fatalf("insert target WORKING row: %v", err)
	}
	headBefore := wispQueryIndexTestHead(t, db)

	var stderr bytes.Buffer
	if err := applyWispQueryIndexesToDB(context.Background(), port, "hq", &stderr); err != nil {
		t.Fatalf("applyWispQueryIndexesToDB: %v", err)
	}
	if !strings.Contains(stderr.String(), "pre-existing WORKING change on target table wisps; skipping") {
		t.Fatalf("migration stderr = %q, want dirty-target skip", stderr.String())
	}
	if got := wispQueryIndexTestHead(t, db); got != headBefore {
		t.Fatalf("skipped migration moved HEAD from %s to %s", headBefore, got)
	}
	assertWispQueryIndexTestSchema(t, db, "wisps", "WORKING", "idx_wisps_status_type", false)
	assertWispQueryIndexTestModifiedStatus(t, db, "wisps", false)
}

func openWispQueryIndexTestDB(t *testing.T, port string) *sql.DB {
	t.Helper()
	db, err := managedDoltOpenDatabase("127.0.0.1", port, "root", "hq")
	if err != nil {
		t.Fatalf("managedDoltOpenDatabase: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping test Dolt: %v", err)
	}
	return db
}

func wispQueryIndexTestHead(t *testing.T, db *sql.DB) string {
	t.Helper()
	var head string
	if err := db.QueryRow("SELECT DOLT_HASHOF('HEAD')").Scan(&head); err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	return head
}

func assertWispQueryIndexTestSchema(t *testing.T, db *sql.DB, table, revision, fragment string, present bool) {
	t.Helper()
	var gotTable, createStmt string
	query := "SHOW CREATE TABLE " + table + " AS OF '" + revision + "'"
	if err := db.QueryRow(query).Scan(&gotTable, &createStmt); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if got := strings.Contains(createStmt, fragment); got != present {
		t.Fatalf("%s schema contains %q = %t, want %t\n%s", table, fragment, got, present, createStmt)
	}
}

func assertWispQueryIndexTestRowCount(t *testing.T, db *sql.DB, query string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if got != want {
		t.Fatalf("%s = %d, want %d", query, got, want)
	}
}

func assertWispQueryIndexTestModifiedStatus(t *testing.T, db *sql.DB, table string, wantStaged bool) {
	t.Helper()
	var staged bool
	var status string
	if err := db.QueryRow("SELECT staged, status FROM dolt_status WHERE table_name = ?", table).Scan(&staged, &status); err != nil {
		t.Fatalf("read dolt_status for %s: %v", table, err)
	}
	if staged != wantStaged || status != "modified" {
		t.Fatalf("dolt_status[%s] = staged:%t status:%s, want staged:%t status:modified", table, staged, status, wantStaged)
	}
}
