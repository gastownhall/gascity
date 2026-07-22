package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

type wispQueryIndexSpec struct {
	table      string
	name       string
	columns    []string
	createStmt string
	addStmt    string
}

// wispQueryIndexes lists the indexes applied by applyWispQueryIndexes. The
// CREATE statements are idempotent, and each add statement names exactly one
// table. Never replace the named adds with DOLT_ADD('.'): a managed database
// may legitimately contain unrelated WORKING changes.
//
// Background: the beads library's SearchIssuesWithCountsInTx generates SQL
// with full-table subquery aggregations for wisp_labels and wisp_dependencies
// (JSON_ARRAYAGG labels, dep/rdep/comment counts), all materialized across the
// entire table before the outer WHERE filter is applied. Two indexes are
// missing from the beads schema migrations that would significantly reduce the
// cost of these scans on a busy server:
//
//   - idx_wisp_labels_issue_id: without it, the GROUP BY issue_id scan in the
//     labels subquery does a full wisp_labels scan + sort on every bd query call.
//   - idx_wisps_status_type: composite covering the most common hot filter
//     (status='open' AND issue_type='message') so the outer WHERE can use a
//     single range scan instead of filtering two separate index rows.
//
// These belong upstream in the beads schema migrations; this gc-side guard
// applies them immediately without waiting for a beads version bump.
var wispQueryIndexes = []wispQueryIndexSpec{
	{
		table:      "wisp_labels",
		name:       "idx_wisp_labels_issue_id",
		columns:    []string{"issue_id"},
		createStmt: "CREATE INDEX IF NOT EXISTS idx_wisp_labels_issue_id ON wisp_labels(issue_id)",
		addStmt:    "CALL DOLT_ADD('wisp_labels')",
	},
	{
		table:      "wisps",
		name:       "idx_wisps_status_type",
		columns:    []string{"status", "issue_type"},
		createStmt: "CREATE INDEX IF NOT EXISTS idx_wisps_status_type ON wisps(status, issue_type)",
		addStmt:    "CALL DOLT_ADD('wisps')",
	},
}

const (
	wispQueryIndexBeginStatement  = "START TRANSACTION"
	wispQueryIndexCommitStatement = "CALL DOLT_COMMIT('-m', 'gc: add wisp-query performance indexes (gcy-0m1)', '--author', 'gascity-builder <builder@gascity.local>', '--skip-empty')"
)

// applyWispQueryIndexes creates the missing wisp query performance indexes on
// the managed Dolt server. It is idempotent and fail-open: any error is logged
// to stderr but never returned, so a degraded Dolt connection at startup does
// not block the controller. The function picks up the database name from beads
// metadata (falling back to "hq") and the port from cr.managedDoltPort.
func (cr *CityRuntime) applyWispQueryIndexes(ctx context.Context) {
	if !cityUsesManagedDoltBeadsLifecycle(cr.cityPath) {
		return
	}
	portFn := cr.managedDoltPort
	if portFn == nil {
		portFn = currentResolvableManagedDoltPort
	}
	port := portFn(cr.cityPath)
	if port == "" {
		return
	}
	database := canonicalScopeDoltDatabase(cr.cityPath, cr.cityPath, "hq")
	if database == "" {
		database = "hq"
	}
	if err := applyWispQueryIndexesToDB(ctx, port, database, cr.stderr); err != nil {
		fmt.Fprintf(cr.stderr, "%s: wisp-query-index migration: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
	}
}

type wispQueryDoltStatus struct {
	table  string
	staged bool
	status string
}

// applyWispQueryIndexesToDB is the testable core of applyWispQueryIndexes.
//
// Safety contract:
//   - pre-existing staged changes anywhere cause the migration to skip;
//   - pre-existing WORKING changes to either target table cause it to skip;
//   - unrelated WORKING changes are allowed and are never staged;
//   - inspection, index creation, staging, and commit share one explicit SQL
//     transaction, isolating this migration from concurrent client writes; and
//   - immediately before commit, the staged set must contain only schema-only
//     changes to target tables whose expected indexes have exact definitions.
//
// GET_LOCK serializes cooperating gc controllers. The SQL transaction is the
// Dolt-supported boundary that keeps a non-cooperating client's concurrent
// WORKING changes out of this client's DOLT_COMMIT.
func applyWispQueryIndexesToDB(ctx context.Context, port, database string, stderr io.Writer) error {
	return applyWispQueryIndexesToDBWithHook(ctx, port, database, stderr, nil)
}

func applyWispQueryIndexesToDBWithHook(
	ctx context.Context,
	port, database string,
	stderr io.Writer,
	beforeCommit func(),
) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	db, err := managedDoltOpenDatabase("127.0.0.1", port, "root", database)
	if err != nil {
		return fmt.Errorf("open dolt connection: %w", err)
	}
	defer db.Close() //nolint:errcheck

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve dolt connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck
	if err := conn.PingContext(ctx); err != nil {
		return fmt.Errorf("ping dolt: %w", err)
	}

	lockDigest := sha256.Sum256([]byte(database))
	lockName := fmt.Sprintf("gc-wisp-query-index:%x", lockDigest[:12])
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", lockName).Scan(&acquired); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		fmt.Fprintln(stderr, "wisp-query-index: migration already active; skipping") //nolint:errcheck
		return nil
	}
	defer func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer releaseCancel()
		if _, releaseErr := conn.ExecContext(releaseCtx, "SELECT RELEASE_LOCK(?)", lockName); releaseErr != nil {
			fmt.Fprintf(stderr, "wisp-query-index: release advisory lock: %v\n", releaseErr) //nolint:errcheck
		}
	}()

	if _, err := conn.ExecContext(ctx, wispQueryIndexBeginStatement); err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	transactionActive := true
	defer func() {
		if !transactionActive {
			return
		}
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer rollbackCancel()
		if _, rollbackErr := conn.ExecContext(rollbackCtx, "ROLLBACK"); rollbackErr != nil {
			fmt.Fprintf(stderr, "wisp-query-index: rollback migration transaction: %v\n", rollbackErr) //nolint:errcheck
		}
	}()

	var targetTableCount int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT table_name)
		FROM information_schema.tables
		WHERE table_schema = DATABASE()
		  AND table_name IN ('wisp_labels', 'wisps')`,
	).Scan(&targetTableCount); err != nil {
		return fmt.Errorf("inspect target tables: %w", err)
	}
	if targetTableCount != len(wispQueryIndexes) {
		fmt.Fprintln(stderr, "wisp-query-index: target tables are not initialized; skipping") //nolint:errcheck
		return nil
	}

	status, err := readWispQueryDoltStatus(ctx, conn)
	if err != nil {
		return fmt.Errorf("inspect pre-migration status: %w", err)
	}
	if reason := wispQueryIndexUnsafeBaseline(status); reason != "" {
		fmt.Fprintf(stderr, "wisp-query-index: %s; skipping\n", reason) //nolint:errcheck
		return nil
	}

	for _, index := range wispQueryIndexes {
		if _, err := conn.ExecContext(ctx, index.createStmt); err != nil {
			// A missing or not-yet-migrated table is non-fatal. Exact index
			// verification below prevents a partial or conflicting definition
			// from being staged.
			fmt.Fprintf(stderr, "wisp-query-index: %s: %v\n", index.createStmt, err) //nolint:errcheck
		}
	}

	exactIndexes := make(map[string]bool, len(wispQueryIndexes))
	for _, index := range wispQueryIndexes {
		exact, err := wispQueryIndexDefinitionIsExact(ctx, conn, index)
		if err != nil {
			return fmt.Errorf("verify index %s: %w", index.name, err)
		}
		exactIndexes[index.table] = exact
		if !exact {
			fmt.Fprintf(stderr, "wisp-query-index: index %s is absent or has an unexpected definition\n", index.name) //nolint:errcheck
		}
	}

	status, err = readWispQueryDoltStatus(ctx, conn)
	if err != nil {
		return fmt.Errorf("inspect post-create status: %w", err)
	}
	changedTargets, reason := wispQueryIndexChangesReadyToStage(status, exactIndexes)
	if reason != "" {
		fmt.Fprintf(stderr, "wisp-query-index: %s; leaving intended schema changes unstaged\n", reason) //nolint:errcheck
		return nil
	}
	if len(changedTargets) == 0 {
		return nil
	}
	if err := validateWispQueryIndexDiff(ctx, conn, "WORKING", changedTargets); err != nil {
		fmt.Fprintf(stderr, "wisp-query-index: %v; leaving intended schema changes unstaged\n", err) //nolint:errcheck
		return nil
	}

	for _, index := range wispQueryIndexes {
		if !changedTargets[index.table] {
			continue
		}
		if _, err := conn.ExecContext(ctx, index.addStmt); err != nil {
			fmt.Fprintf(stderr, "wisp-query-index stage: %s: %v\n", index.addStmt, err) //nolint:errcheck
			return nil
		}
	}

	status, err = readWispQueryDoltStatus(ctx, conn)
	if err != nil {
		return fmt.Errorf("inspect staged status: %w", err)
	}
	if reason := wispQueryIndexUnsafeStagedSet(status, changedTargets); reason != "" {
		fmt.Fprintf(stderr, "wisp-query-index: %s; refusing to commit\n", reason) //nolint:errcheck
		return nil
	}
	if err := validateWispQueryIndexDiff(ctx, conn, "STAGED", changedTargets); err != nil {
		fmt.Fprintf(stderr, "wisp-query-index: %v; refusing to commit\n", err) //nolint:errcheck
		return nil
	}
	if beforeCommit != nil {
		beforeCommit()
	}

	// DOLT_COMMIT implicitly commits the surrounding SQL transaction. Keep it
	// adjacent to the final staged-set validation.
	if _, err := conn.ExecContext(ctx, wispQueryIndexCommitStatement); err != nil {
		fmt.Fprintf(stderr, "wisp-query-index commit: %v\n", err) //nolint:errcheck
		return nil
	}
	transactionActive = false
	return nil
}

func readWispQueryDoltStatus(ctx context.Context, conn *sql.Conn) ([]wispQueryDoltStatus, error) {
	rows, err := conn.QueryContext(ctx, "SELECT table_name, staged, status FROM dolt_status ORDER BY table_name, staged")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var status []wispQueryDoltStatus
	for rows.Next() {
		var entry wispQueryDoltStatus
		if err := rows.Scan(&entry.table, &entry.staged, &entry.status); err != nil {
			return nil, err
		}
		status = append(status, entry)
	}
	return status, rows.Err()
}

func wispQueryIndexUnsafeBaseline(status []wispQueryDoltStatus) string {
	for _, entry := range status {
		if entry.staged {
			return fmt.Sprintf("pre-existing staged change on %s", entry.table)
		}
		if wispQueryIndexTarget(entry.table) {
			return fmt.Sprintf("pre-existing WORKING change on target table %s", entry.table)
		}
	}
	return ""
}

func wispQueryIndexChangesReadyToStage(status []wispQueryDoltStatus, exactIndexes map[string]bool) (map[string]bool, string) {
	changed := make(map[string]bool, len(wispQueryIndexes))
	for _, entry := range status {
		if entry.staged {
			return nil, fmt.Sprintf("a staged change appeared on %s", entry.table)
		}
		if !wispQueryIndexTarget(entry.table) {
			continue
		}
		if entry.status != "modified" {
			return nil, fmt.Sprintf("unexpected target status %s=%s", entry.table, entry.status)
		}
		if !exactIndexes[entry.table] {
			return nil, fmt.Sprintf("target %s changed without its exact expected index", entry.table)
		}
		changed[entry.table] = true
	}
	return changed, ""
}

func wispQueryIndexUnsafeStagedSet(status []wispQueryDoltStatus, expected map[string]bool) string {
	seen := make(map[string]bool, len(expected))
	for _, entry := range status {
		if entry.staged {
			if !expected[entry.table] {
				return fmt.Sprintf("unexpected staged change on %s", entry.table)
			}
			seen[entry.table] = true
			continue
		}
		if expected[entry.table] {
			return fmt.Sprintf("target %s still has an unstaged change", entry.table)
		}
	}
	for table := range expected {
		if !seen[table] {
			return fmt.Sprintf("target %s is missing from the staged set", table)
		}
	}
	return ""
}

func wispQueryIndexTarget(table string) bool {
	for _, index := range wispQueryIndexes {
		if table == index.table {
			return true
		}
	}
	return false
}

func wispQueryIndexDefinitionIsExact(ctx context.Context, conn *sql.Conn, index wispQueryIndexSpec) (bool, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT column_name, non_unique, index_type, sub_part
		FROM information_schema.statistics
		WHERE table_schema = DATABASE()
		  AND table_name = ?
		  AND index_name = ?
		ORDER BY seq_in_index`, index.table, index.name)
	if err != nil {
		return false, err
	}
	defer rows.Close() //nolint:errcheck

	var columns []string
	for rows.Next() {
		var column, indexType string
		var nonUnique int
		var subPart sql.NullInt64
		if err := rows.Scan(&column, &nonUnique, &indexType, &subPart); err != nil {
			return false, err
		}
		if nonUnique != 1 || !strings.EqualFold(indexType, "BTREE") || subPart.Valid {
			return false, nil
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(columns) != len(index.columns) {
		return false, nil
	}
	for i := range columns {
		if columns[i] != index.columns[i] {
			return false, nil
		}
	}
	return true, nil
}

func validateWispQueryIndexDiff(ctx context.Context, conn *sql.Conn, revision string, expected map[string]bool) error {
	if revision != "WORKING" && revision != "STAGED" {
		return fmt.Errorf("unsupported diff revision %q", revision)
	}
	query := fmt.Sprintf(`
		SELECT COALESCE(to_table_name, from_table_name), data_change, schema_change
		FROM dolt_diff_summary('HEAD', '%s')`, revision)
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("inspect %s diff: %w", strings.ToLower(revision), err)
	}
	defer rows.Close() //nolint:errcheck

	seen := make(map[string]bool, len(expected))
	for rows.Next() {
		var table string
		var dataChange, schemaChange bool
		if err := rows.Scan(&table, &dataChange, &schemaChange); err != nil {
			return fmt.Errorf("scan %s diff: %w", strings.ToLower(revision), err)
		}
		if !expected[table] {
			continue
		}
		if dataChange || !schemaChange {
			return fmt.Errorf("target %s has a non-schema-only %s diff", table, strings.ToLower(revision))
		}
		seen[table] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s diff: %w", strings.ToLower(revision), err)
	}

	var missing []string
	for table := range expected {
		if !seen[table] {
			missing = append(missing, table)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return fmt.Errorf("expected schema diff missing for %s in %s", strings.Join(missing, ", "), revision)
	}
	return nil
}
