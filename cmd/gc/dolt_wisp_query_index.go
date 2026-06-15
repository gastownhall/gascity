package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// wispQueryIndexes lists the CREATE INDEX statements applied by
// applyWispQueryIndexes. Each entry is idempotent (IF NOT EXISTS).
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
var wispQueryIndexStatements = []string{
	"CREATE INDEX IF NOT EXISTS idx_wisp_labels_issue_id ON wisp_labels(issue_id)",
	"CREATE INDEX IF NOT EXISTS idx_wisps_status_type ON wisps(status, issue_type)",
}

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

// applyWispQueryIndexesToDB is the testable core of applyWispQueryIndexes.
// It opens a short-lived MySQL connection, applies each index statement, and
// closes. Errors from individual statements are logged but do not abort the
// loop so a partial success still improves query performance.
func applyWispQueryIndexesToDB(ctx context.Context, port, database string, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	db, err := managedDoltOpenDatabase("127.0.0.1", port, "root", database)
	if err != nil {
		return fmt.Errorf("open dolt connection: %w", err)
	}
	defer db.Close() //nolint:errcheck

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping dolt: %w", err)
	}

	for _, stmt := range wispQueryIndexStatements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			// Non-fatal: log and continue. Some statements may fail if the
			// table doesn't exist yet (fresh install before first bd init).
			fmt.Fprintf(stderr, "wisp-query-index: %s: %v\n", stmt, err) //nolint:errcheck // best-effort stderr
		}
	}

	// Persist the schema change so the indexes survive reset/sync, matching
	// the schemas/wisps-composite-index migration convention. Fail-open:
	// "nothing to commit" (idempotent re-run) and any other commit error are
	// logged, never returned.
	for _, stmt := range []string{
		"CALL DOLT_ADD('.')",
		"CALL DOLT_COMMIT('-m', 'gc: add wisp-query performance indexes (gcy-0m1)', '--author', 'gascity-builder <builder@gascity.local>')",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "nothing to commit") {
				break
			}
			fmt.Fprintf(stderr, "wisp-query-index commit: %s: %v\n", stmt, err) //nolint:errcheck
		}
	}
	return nil
}
