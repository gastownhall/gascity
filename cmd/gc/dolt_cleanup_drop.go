package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// CleanupDoltClient is the SQL surface the cleanup engine needs. The
// production implementation wraps a *sql.DB; tests inject a fake.
//
// Methods are scoped to the operations the engine actually performs:
// ListDatabases for the scan/plan phase, DropDatabase per stale name,
// PurgeDroppedDatabases per rig DB after drops complete. Close is for
// resource hygiene.
type CleanupDoltClient interface {
	ListDatabases(ctx context.Context) ([]string, error)
	DropDatabase(ctx context.Context, name string) error
	// PurgeDroppedDatabases issues CALL DOLT_PURGE_DROPPED_DATABASES()
	// against the given rig database. The dolt server's purge routine is
	// per-database — caller iterates over each rig DB it wants reclaimed.
	PurgeDroppedDatabases(ctx context.Context, rigDB string) error
	// ProbeLiveSessions returns the set of databases with at least one
	// active session at probe time, keyed on database name with the
	// thread count as the value. The probe issues
	// cleanupLiveSessionProbeQuery against the same Dolt server the
	// drop stage targets. Errors (timeout, auth, network, malformed
	// result) propagate verbatim — the runner converts them into a
	// FAIL-CLOSED force-blocker (architect FR-05).
	ProbeLiveSessions(ctx context.Context) (map[string]int, error)
	Close() error
}

// cleanupDropTimeout caps each individual DROP DATABASE call. Dolt drops
// can be slow (the server walks the database directory), so a generous
// timeout avoids spurious failures while still bounding hangs.
const cleanupDropTimeout = 30 * time.Second

// cleanupListTimeout caps SHOW DATABASES.
const cleanupListTimeout = 30 * time.Second

// cleanupLiveSessionProbeQuery is the verbatim SQL the probe issues.
// It scans the live processlist on the same Dolt server cleanup is
// targeting and aggregates one row per database with at least one
// active session. The schema is stable across all Dolt versions
// gascity targets (information_schema.processlist is MySQL-compat).
const cleanupLiveSessionProbeQuery = "SELECT db, COUNT(*) FROM information_schema.processlist WHERE db IS NOT NULL AND db != '' GROUP BY db"

// cleanupLiveSessionProbeTimeout caps SHOW PROCESSLIST. NFR-01 of
// ga-nw4z6: the query is expected to complete in well under 1 second
// on a healthy Dolt server; 2 s is the FAIL-CLOSED threshold.
//
// Declared as var (not const) so TestProbeLiveSessions_TimesOut can
// shrink it to keep CI wall time under 250 ms while still exercising
// the timeout wiring. Production never mutates it; the assertion in
// TestProbeLiveSessions_FailClosed pins the live value at 2*time.Second.
var cleanupLiveSessionProbeTimeout = 2 * time.Second

// runDropStage discovers all databases on the resolved Dolt server,
// classifies them with planDoltDrops against the protection list, and (when
// --force is set) drops each stale name. Errors are recorded into the
// report. It returns false only when a force-mode safety guard refuses cleanup
// and the caller must skip the remaining destructive stages.
func runDropStage(report *CleanupReport, opts cleanupOptions) bool {
	if opts.DoltClient == nil {
		if opts.DoltClientOpenErr != nil {
			recordCleanupError(report, "drop", "", opts.DoltClientOpenErr)
		}
		return true
	}
	if opts.Force && hasRigProtectionError(report) {
		return true
	}

	listCtx, listCancel := context.WithTimeout(context.Background(), cleanupListTimeout)
	defer listCancel()

	all, err := opts.DoltClient.ListDatabases(listCtx)
	if err != nil {
		report.Errors = append(report.Errors, CleanupError{Stage: "drop", Error: err.Error()})
		report.Summary.ErrorsTotal++
		return true
	}

	stalePrefixes := opts.StalePrefixes
	if len(stalePrefixes) == 0 {
		stalePrefixes = defaultStaleDatabasePrefixes
	}
	protected := make([]string, 0, len(report.RigsProtected))
	for _, rp := range report.RigsProtected {
		protected = append(protected, rp.DB)
	}

	plan := planDoltDrops(all, stalePrefixes, protected)

	probeCtx, probeCancel := context.WithTimeout(context.Background(), cleanupLiveSessionProbeTimeout)
	liveSessions, probeErr := opts.DoltClient.ProbeLiveSessions(probeCtx)
	probeCancel()

	if probeErr != nil {
		recordCleanupForceBlocker(report, cleanupErrorKindLiveSessionProbeFailed, "", probeErr)
		if opts.Force {
			recordCleanupErrorKind(report, "drop", cleanupErrorKindLiveSessionProbeFailed, "", probeErr)
			report.Dropped.Skipped = append([]DoltDropSkip{}, plan.Skipped...)
			for _, skipped := range plan.Skipped {
				if skipped.Reason == DropSkipReasonInvalidIdentifier {
					recordCleanupError(report, "drop", skipped.Name, fmt.Errorf("invalid database identifier %q", skipped.Name))
				}
			}
			return false
		}
		// Dry-run continues; report renders the planned ToDrop as if the
		// probe had returned empty — operator sees what WOULD have been
		// dropped, plus the visible ForceBlocker.
	} else {
		plan = applyLiveSessionsToPlan(plan, liveSessions)
	}

	report.Dropped.Skipped = append([]DoltDropSkip{}, plan.Skipped...)
	for _, skipped := range plan.Skipped {
		if skipped.Reason == DropSkipReasonInvalidIdentifier {
			recordCleanupError(report, "drop", skipped.Name, fmt.Errorf("invalid database identifier %q", skipped.Name))
		}
	}

	if !opts.Force {
		report.Dropped.Count = len(plan.ToDrop)
		report.Dropped.Names = append([]string{}, plan.ToDrop...)
		return true
	}
	if opts.MaxOrphanDBs > 0 && len(plan.ToDrop) > opts.MaxOrphanDBs {
		report.Dropped.Count = len(plan.ToDrop)
		report.Dropped.Names = append([]string{}, plan.ToDrop...)
		recordCleanupErrorKind(
			report,
			"drop",
			cleanupErrorKindMaxOrphanRefusal,
			"",
			fmt.Errorf("apply-time stale database count %d exceeds --max-orphan-dbs=%d; refusing forced cleanup", len(plan.ToDrop), opts.MaxOrphanDBs),
		)
		return false
	}

	droppedNames := make([]string, 0, len(plan.ToDrop))
	for _, name := range plan.ToDrop {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), cleanupDropTimeout)
		err := opts.DoltClient.DropDatabase(dropCtx, name)
		dropCancel()
		if err != nil {
			report.Dropped.Failed = append(report.Dropped.Failed, CleanupDropFailure{
				Name:  name,
				Error: err.Error(),
			})
			report.Errors = append(report.Errors, CleanupError{
				Stage: "drop",
				Name:  name,
				Error: err.Error(),
			})
			report.Summary.ErrorsTotal++
			continue
		}
		droppedNames = append(droppedNames, name)
	}
	// Update the count to the actually-dropped tally so the summary
	// matches the live world rather than the planned set.
	report.Dropped.Names = droppedNames
	report.Dropped.Count = len(droppedNames)
	return true
}

// sqlCleanupDoltClient wraps a *sql.DB to satisfy CleanupDoltClient.
type sqlCleanupDoltClient struct {
	db *sql.DB
}

// newSQLCleanupDoltClient opens a connection to the resolved Dolt server.
// Caller must Close() when done.
func newSQLCleanupDoltClient(host, port string) (CleanupDoltClient, error) {
	db, err := managedDoltOpenDB(host, port, "root")
	if err != nil {
		return nil, fmt.Errorf("open dolt connection: %w", err)
	}
	return &sqlCleanupDoltClient{db: db}, nil
}

func (c *sqlCleanupDoltClient) ListDatabases(ctx context.Context) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *sqlCleanupDoltClient) DropDatabase(ctx context.Context, name string) error {
	if !validDoltDatabaseIdentifier(name) {
		return fmt.Errorf("invalid database identifier %q", name)
	}
	// Escape backticks in identifiers to prevent injection (` → ``).
	safe := strings.ReplaceAll(name, "`", "``")
	// IF EXISTS keeps the drop idempotent: teardown/rollback is documented as
	// "best-effort and idempotent (a re-crash mid-sweep re-runs cleanly)", so a
	// drop that already succeeded before a marker write failed must not error the
	// next sweep and wedge the record — a database-not-found is success here.
	_, err := c.db.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", safe)) //nolint:gosec // G201: identifier-escaped
	return err
}

func (c *sqlCleanupDoltClient) PurgeDroppedDatabases(ctx context.Context, rigDB string) error {
	if !validDoltDatabaseIdentifier(rigDB) {
		return fmt.Errorf("invalid database identifier %q", rigDB)
	}
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	safe := strings.ReplaceAll(rigDB, "`", "``")
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("USE `%s`", safe)); err != nil { //nolint:gosec // G201: identifier-escaped
		return fmt.Errorf("USE %q: %w", rigDB, err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_PURGE_DROPPED_DATABASES()"); err != nil {
		return err
	}
	return nil
}

func (c *sqlCleanupDoltClient) ProbeLiveSessions(ctx context.Context) (map[string]int, error) {
	rows, err := c.db.QueryContext(ctx, cleanupLiveSessionProbeQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	out := map[string]int{}
	for rows.Next() {
		var (
			name    string
			threads int
		)
		if err := rows.Scan(&name, &threads); err != nil {
			return nil, err
		}
		out[name] = threads
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *sqlCleanupDoltClient) Close() error {
	return c.db.Close()
}
