// Package sqlite provides a SQLite-backed StoreAdapter for the coordination-
// store benchmark suite. It uses modernc.org/sqlite (pure Go, no CGo).
//
// Schema design:
//   - Two physical tables (records, ephemeral) for the two tiers (FR-7).
//   - Secondary indexes on hot filter columns (FR-2, FR-8).
//   - Metadata stored in a separate KV table indexed on (key, value) (FR-11).
//   - Labels in a separate table with cascade on delete (FR-17).
//   - Deps in a separate table (FR-10).
//   - WAL journal mode for concurrent reads (FR-16, throughput target).
//   - PRAGMA synchronous=NORMAL: single-process only; full fsync on each
//     commit is not needed for this benchmark (same trade-off as discovery.md
//     author-own design, which allows sync=normal for single-process use).
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Adapter implements coordstore.StoreAdapter backed by SQLite.
//
// WAL concurrency model:
//   - writeDB: single connection, serializes all writes. WAL mode allows
//     readers to continue during writes without blocking.
//   - readDB: pool of up to 20 connections. Multiple goroutines can read
//     concurrently in WAL mode while a write is in progress.
//
// This matches the canonical SQLite WAL production pattern for in-process use.
type Adapter struct {
	path        string
	writeDB     *sql.DB // single writer connection
	readDB      *sql.DB // reader connection pool
	stmtGetMain *sql.Stmt
	stmtGetEph  *sql.Stmt
	seq         atomic.Int64
}

// New returns an Adapter. Call Open before using it.
func New() *Adapter { return &Adapter{} }

// Open initializes the SQLite database at cfg.DataDir/store.db.
func (a *Adapter) Open(ctx context.Context, cfg coordstore.Config) error {
	path := cfg.DataDir + "/store.db"

	// Write connection: single connection, serializes all mutations.
	writeDB, err := sql.Open("sqlite", path+"?_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("sqlite: open write db %s: %w", path, err)
	}
	writeDB.SetMaxOpenConns(1)

	if _, err := writeDB.ExecContext(ctx, pragmas); err != nil {
		writeDB.Close() //nolint:errcheck
		return fmt.Errorf("sqlite: set pragmas: %w", err)
	}
	if _, err := writeDB.ExecContext(ctx, schema); err != nil {
		writeDB.Close() //nolint:errcheck
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	if err := a.recoverSequence(ctx, writeDB); err != nil {
		writeDB.Close() //nolint:errcheck
		return err
	}

	// Read connection pool: WAL allows concurrent reads even during writes.
	// Opened without mode=ro so WAL wal-index is fully shared; the Go code
	// never issues writes through readDB — that's enforced structurally.
	readDB, err := sql.Open("sqlite", path+"?_busy_timeout=5000")
	if err != nil {
		writeDB.Close() //nolint:errcheck
		return fmt.Errorf("sqlite: open read db %s: %w", path, err)
	}
	// Keep all connections warm — connection churn is the dominant read latency
	// when MaxIdleConns < MaxOpenConns causes constant open/close on every op.
	readDB.SetMaxOpenConns(20)
	readDB.SetMaxIdleConns(20)
	readDB.SetConnMaxIdleTime(5 * time.Minute)

	writeDB.SetMaxIdleConns(1)
	writeDB.SetConnMaxIdleTime(5 * time.Minute)

	// Pre-compile hot-path statements so repeated SQL parsing overhead is paid
	// once at Open time, not on every call to Get.
	stmtGetMain, err := readDB.PrepareContext(ctx, sqlGetMain)
	if err != nil {
		readDB.Close()  //nolint:errcheck
		writeDB.Close() //nolint:errcheck
		return fmt.Errorf("sqlite: prepare getMain: %w", err)
	}
	stmtGetEph, err := readDB.PrepareContext(ctx, sqlGetEphemeral)
	if err != nil {
		stmtGetMain.Close() //nolint:errcheck
		readDB.Close()      //nolint:errcheck
		writeDB.Close()     //nolint:errcheck
		return fmt.Errorf("sqlite: prepare getEphemeral: %w", err)
	}

	a.writeDB = writeDB
	a.readDB = readDB
	a.stmtGetMain = stmtGetMain
	a.stmtGetEph = stmtGetEph
	a.path = path
	return nil
}

// Close releases all database connections and prepared statements.
func (a *Adapter) Close() error {
	var errs []string
	if a.stmtGetMain != nil {
		if err := a.stmtGetMain.Close(); err != nil {
			errs = append(errs, "stmtGetMain: "+err.Error())
		}
	}
	if a.stmtGetEph != nil {
		if err := a.stmtGetEph.Close(); err != nil {
			errs = append(errs, "stmtGetEph: "+err.Error())
		}
	}
	if a.readDB != nil {
		if err := a.readDB.Close(); err != nil {
			errs = append(errs, "readDB: "+err.Error())
		}
	}
	if a.writeDB != nil {
		if err := a.writeDB.Close(); err != nil {
			errs = append(errs, "writeDB: "+err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("sqlite Close: %s", strings.Join(errs, "; "))
	}
	a.path = ""
	return nil
}

// Reset wipes all data. The schema remains intact.
func (a *Adapter) Reset(ctx context.Context) error {
	tables := []string{
		"deps", "labels", "metadata", "ephemeral_labels",
		"ephemeral_metadata", "ephemeral", "records",
	}
	for _, t := range tables {
		if _, err := a.writeDB.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("sqlite: reset %s: %w", t, err)
		}
	}
	a.seq.Store(0)
	return nil
}

// nextID generates a unique record ID.
func (a *Adapter) nextID() string {
	return fmt.Sprintf("sq-%d", a.seq.Add(1))
}

func (a *Adapter) recoverSequence(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
SELECT id FROM records WHERE id LIKE ?
UNION ALL
SELECT id FROM ephemeral WHERE id LIKE ?`, "sq-%", "sq-%")
	if err != nil {
		return fmt.Errorf("sqlite: recover id sequence: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var maxSeq int64
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("sqlite: recover id sequence scan: %w", err)
		}
		raw := strings.TrimPrefix(id, "sq-")
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		if value > maxSeq {
			maxSeq = value
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: recover id sequence rows: %w", err)
	}
	a.seq.Store(maxSeq)
	return nil
}

// --- FR-1: CRUD ---

// Create persists a new record. Routes to the appropriate tier based on r.Ephemeral.
func (a *Adapter) Create(ctx context.Context, r coordstore.Record) (coordstore.Record, error) {
	if r.ID == "" {
		r.ID = a.nextID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	if r.Status == "" {
		r.Status = "open"
	}
	if r.Type == "" {
		r.Type = "task"
	}

	tx, err := a.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return coordstore.Record{}, fmt.Errorf("sqlite Create: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if r.Ephemeral {
		expiresNs := int64(0)
		if !r.ExpiresAt.IsZero() {
			expiresNs = r.ExpiresAt.UnixNano()
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO ephemeral(id,title,status,type,created_at,updated_at,assignee,parent_id,expires_at)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			r.ID, r.Title, r.Status, r.Type,
			r.CreatedAt.UnixNano(), r.UpdatedAt.UnixNano(), r.Assignee, r.ParentID, expiresNs)
		if err != nil {
			return coordstore.Record{}, fmt.Errorf("sqlite Create ephemeral: %w", err)
		}
		if err := insertLabels(ctx, tx, r.ID, r.Labels, true); err != nil {
			return coordstore.Record{}, err
		}
		if err := insertMetadata(ctx, tx, r.ID, r.Metadata, true); err != nil {
			return coordstore.Record{}, err
		}
	} else {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO records(id,title,status,type,priority,created_at,updated_at,assignee,parent_id,description)
			 VALUES(?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.Title, r.Status, r.Type,
			r.Priority, r.CreatedAt.UnixNano(), r.UpdatedAt.UnixNano(), r.Assignee, r.ParentID, "")
		if err != nil {
			return coordstore.Record{}, fmt.Errorf("sqlite Create main: %w", err)
		}
		if err := insertLabels(ctx, tx, r.ID, r.Labels, false); err != nil {
			return coordstore.Record{}, err
		}
		if err := insertMetadata(ctx, tx, r.ID, r.Metadata, false); err != nil {
			return coordstore.Record{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return coordstore.Record{}, fmt.Errorf("sqlite Create: commit: %w", err)
	}
	return r, nil
}

// Get retrieves a record from either tier by ID.
func (a *Adapter) Get(ctx context.Context, id string) (coordstore.Record, error) {
	// Try main tier first.
	r, err := a.getMain(ctx, id)
	if err == nil {
		return r, nil
	}
	if !isNotFound(err) {
		return coordstore.Record{}, err
	}
	// Try ephemeral tier.
	r, err = a.getEphemeral(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return coordstore.Record{}, coordstore.ErrNotFound
		}
		return coordstore.Record{}, err
	}
	return r, nil
}

func (a *Adapter) getMain(ctx context.Context, id string) (coordstore.Record, error) {
	// Uses pre-compiled stmtGetMain: single query with correlated subqueries
	// for labels and metadata. char(30)=RS, char(31)=US as separators.
	var r coordstore.Record
	var createdNs, updatedNs int64
	var labelsStr, metaStr sql.NullString
	err := a.stmtGetMain.QueryRowContext(ctx, id).Scan(
		&r.ID, &r.Title, &r.Status, &r.Type, &r.Priority, &createdNs, &updatedNs, &r.Assignee, &r.ParentID,
		&labelsStr, &metaStr,
	)
	if err == sql.ErrNoRows {
		return coordstore.Record{}, errNotFound("main record not found")
	}
	if err != nil {
		return coordstore.Record{}, err
	}
	r.CreatedAt = time.Unix(0, createdNs)
	r.UpdatedAt = r.CreatedAt
	if updatedNs > 0 {
		r.UpdatedAt = time.Unix(0, updatedNs)
	}
	r.Labels = splitConcat(labelsStr)
	r.Metadata = splitKVConcat(metaStr)
	return r, nil
}

func (a *Adapter) getEphemeral(ctx context.Context, id string) (coordstore.Record, error) {
	var r coordstore.Record
	var createdNs, updatedNs, expiresNs int64
	var labelsStr, metaStr sql.NullString
	err := a.stmtGetEph.QueryRowContext(ctx, id).Scan(
		&r.ID, &r.Title, &r.Status, &r.Type, &createdNs, &updatedNs, &r.Assignee, &r.ParentID, &expiresNs,
		&labelsStr, &metaStr,
	)
	if err == sql.ErrNoRows {
		return coordstore.Record{}, errNotFound("ephemeral record not found")
	}
	if err != nil {
		return coordstore.Record{}, err
	}
	r.CreatedAt = time.Unix(0, createdNs)
	r.UpdatedAt = r.CreatedAt
	if updatedNs > 0 {
		r.UpdatedAt = time.Unix(0, updatedNs)
	}
	r.Ephemeral = true
	if expiresNs > 0 {
		r.ExpiresAt = time.Unix(0, expiresNs)
	}
	r.Labels = splitConcat(labelsStr)
	r.Metadata = splitKVConcat(metaStr)
	return r, nil
}

// Update modifies fields of an existing main-tier record.
func (a *Adapter) Update(ctx context.Context, id string, u coordstore.Update) error {
	var setClauses []string
	var args []any
	if u.Status != "" {
		setClauses = append(setClauses, "status=?")
		args = append(args, u.Status)
	}
	if u.Assignee != "" {
		setClauses = append(setClauses, "assignee=?")
		args = append(args, u.Assignee)
	}
	if len(setClauses) == 0 && len(u.Metadata) == 0 {
		return nil
	}
	setClauses = append(setClauses, "updated_at=?")
	args = append(args, time.Now().UnixNano())

	tx, err := a.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite Update: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	q := "UPDATE records SET " + strings.Join(setClauses, ",") + " WHERE id=?"
	args = append(args, id)
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("sqlite Update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return coordstore.ErrNotFound
	}
	if len(u.Metadata) > 0 {
		if err := upsertMetadata(ctx, tx, id, u.Metadata, false); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Delete removes a record (and its labels, metadata, deps) from both tiers.
func (a *Adapter) Delete(ctx context.Context, id string) error {
	tx, err := a.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite Delete: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Delete from main tier (cascades handled manually — SQLite FK cascade
	// requires PRAGMA foreign_keys=ON; we manage it explicitly for clarity).
	res, err := tx.ExecContext(ctx, "DELETE FROM records WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("sqlite Delete main: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Try ephemeral tier.
		res2, err := tx.ExecContext(ctx, "DELETE FROM ephemeral WHERE id=?", id)
		if err != nil {
			return fmt.Errorf("sqlite Delete ephemeral: %w", err)
		}
		n2, _ := res2.RowsAffected()
		if n2 == 0 {
			return coordstore.ErrNotFound
		}
		tx.ExecContext(ctx, "DELETE FROM ephemeral_labels WHERE record_id=?", id)   //nolint:errcheck
		tx.ExecContext(ctx, "DELETE FROM ephemeral_metadata WHERE record_id=?", id) //nolint:errcheck
	} else {
		// FR-17: cascade on main-tier delete.
		tx.ExecContext(ctx, "DELETE FROM labels WHERE record_id=?", id)                     //nolint:errcheck
		tx.ExecContext(ctx, "DELETE FROM metadata WHERE record_id=?", id)                   //nolint:errcheck
		tx.ExecContext(ctx, "DELETE FROM deps WHERE issue_id=? OR depends_on_id=?", id, id) //nolint:errcheck
	}
	return tx.Commit()
}

// --- FR-2 + FR-8: Filter scan ---

// FilterScan returns records matching q from the selected tier.
func (a *Adapter) FilterScan(ctx context.Context, q coordstore.Query) ([]coordstore.Record, error) {
	switch q.Tier {
	case coordstore.TierEphemeral:
		return a.ephemeralFilterScan(ctx, q)
	case coordstore.TierBoth:
		main, err := a.mainFilterScan(ctx, q)
		if err != nil {
			return nil, err
		}
		eph, err := a.ephemeralFilterScan(ctx, q)
		if err != nil {
			return nil, err
		}
		return append(main, eph...), nil
	default:
		return a.mainFilterScan(ctx, q)
	}
}

func (a *Adapter) mainFilterScan(ctx context.Context, q coordstore.Query) ([]coordstore.Record, error) {
	query, args := buildMainQuery(q)
	rows, err := a.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite FilterScan main: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	return scanMainRows(rows)
}

func (a *Adapter) ephemeralFilterScan(ctx context.Context, q coordstore.Query) ([]coordstore.Record, error) {
	query, args := buildEphemeralQuery(q)
	rows, err := a.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite FilterScan ephemeral: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	return scanEphemeralRows(rows)
}

// --- FR-4: Batch fetch ---

// BatchGet retrieves multiple records by ID. Missing IDs are silently omitted.
func (a *Adapter) BatchGet(ctx context.Context, ids []string) ([]coordstore.Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	rows, err := a.readDB.QueryContext(ctx,
		"SELECT id,title,status,type,priority,created_at,updated_at,assignee,parent_id FROM records WHERE id IN ("+placeholders+")",
		args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite BatchGet: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	return scanMainRows(rows)
}

// --- FR-5: Intra-record multi-key atomic write ---

// SetMetadataBatch atomically sets multiple metadata keys within a single transaction.
func (a *Adapter) SetMetadataBatch(ctx context.Context, id string, kvs map[string]string) error {
	tx, err := a.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite SetMetadataBatch: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Verify the record exists.
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM records WHERE id=? LIMIT 1", id).Scan(&exists); err != nil {
		return coordstore.ErrNotFound
	}

	if err := upsertMetadata(ctx, tx, id, kvs, false); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE records SET updated_at=? WHERE id=?", time.Now().UnixNano(), id); err != nil {
		return fmt.Errorf("sqlite SetMetadataBatch updated_at: %w", err)
	}
	return tx.Commit()
}

// --- FR-9: Ready semantics ---

// Ready returns open, unblocked main-tier records that are not excluded types.
func (a *Adapter) Ready(ctx context.Context, q coordstore.ReadyQuery) ([]coordstore.Record, error) {
	// Excluded types from beads.go readyExcludeTypes.
	excluded := []string{"merge-request", "gate", "molecule", "step", "message", "session", "agent", "role", "rig"}
	excl := make([]any, len(excluded))
	for i, t := range excluded {
		excl[i] = t
	}
	placeholders := strings.Repeat("?,", len(excluded))
	placeholders = placeholders[:len(placeholders)-1]

	// CTE materializes the blocked-ID set once; avoids correlated re-execution
	// of the subquery for each outer row (which is O(n×deps) without CTE).
	query := `WITH blocked AS (
	              SELECT DISTINCT d.issue_id
	              FROM deps d
	              JOIN records b ON b.id = d.depends_on_id
	              WHERE b.status IN ('open','in_progress')
	          )
	          SELECT r.id,r.title,r.status,r.type,r.priority,r.created_at,r.updated_at,r.assignee,r.parent_id
	          FROM records r
	          WHERE r.status IN ('open','in_progress')
	            AND r.type NOT IN (` + placeholders + `)
	            AND r.id NOT IN (SELECT issue_id FROM blocked)`
	args := excl
	if q.Assignee != "" {
		query += " AND r.assignee=?"
		args = append(args, q.Assignee)
	}
	if q.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", q.Limit)
	}

	rows, err := a.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite Ready: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	return scanMainRows(rows)
}

// --- FR-10: Dependency graph ---

// DepAdd records a dependency: fromID depends on (is blocked by) toID.
func (a *Adapter) DepAdd(ctx context.Context, fromID, toID, depType string) error {
	_, err := a.writeDB.ExecContext(ctx,
		`INSERT OR REPLACE INTO deps(issue_id,depends_on_id,dep_type) VALUES(?,?,?)`,
		fromID, toID, depType)
	return err
}

// DepRemove removes the dependency between fromID and toID.
func (a *Adapter) DepRemove(ctx context.Context, fromID, toID string) error {
	_, err := a.writeDB.ExecContext(ctx,
		"DELETE FROM deps WHERE issue_id=? AND depends_on_id=?", fromID, toID)
	return err
}

// DepList returns dependencies for a record.
func (a *Adapter) DepList(ctx context.Context, id, direction string) ([]coordstore.Dep, error) {
	var q string
	if direction == "up" {
		q = "SELECT issue_id,depends_on_id,dep_type FROM deps WHERE depends_on_id=?"
	} else {
		q = "SELECT issue_id,depends_on_id,dep_type FROM deps WHERE issue_id=?"
	}
	rows, err := a.readDB.QueryContext(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("sqlite DepList: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var deps []coordstore.Dep
	for rows.Next() {
		var d coordstore.Dep
		if err := rows.Scan(&d.FromID, &d.ToID, &d.DepType); err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

// --- FR-12: TTL expiry ---

// PurgeExpired removes expired ephemeral records.
func (a *Adapter) PurgeExpired(ctx context.Context) (int, error) {
	now := time.Now().UnixNano()
	tx, err := a.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx,
		"SELECT id FROM ephemeral WHERE expires_at > 0 AND expires_at < ?", now)
	if err != nil {
		return 0, fmt.Errorf("sqlite PurgeExpired: query: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, id := range ids {
		tx.ExecContext(ctx, "DELETE FROM ephemeral WHERE id=?", id)                 //nolint:errcheck
		tx.ExecContext(ctx, "DELETE FROM ephemeral_labels WHERE record_id=?", id)   //nolint:errcheck
		tx.ExecContext(ctx, "DELETE FROM ephemeral_metadata WHERE record_id=?", id) //nolint:errcheck
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// PurgeTerminal removes old terminal main-tier records.
func (a *Adapter) PurgeTerminal(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-olderThan).UnixNano()
	tx, err := a.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlite PurgeTerminal: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM records
		 WHERE status IN ('closed','cancelled','canceled','expired')
		   AND COALESCE(NULLIF(updated_at, 0), created_at) < ?
		 LIMIT ?`, cutoff, coordstore.TerminalPurgeBatchSize)
	if err != nil {
		return 0, fmt.Errorf("sqlite PurgeTerminal: query: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close() //nolint:errcheck
			return 0, fmt.Errorf("sqlite PurgeTerminal: scan: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sqlite PurgeTerminal: rows: %w", err)
	}

	for _, id := range ids {
		for _, stmt := range []string{
			"DELETE FROM labels WHERE record_id=?",
			"DELETE FROM metadata WHERE record_id=?",
			"DELETE FROM deps WHERE issue_id=? OR depends_on_id=?",
			"DELETE FROM records WHERE id=?",
		} {
			if strings.Count(stmt, "?") == 2 {
				if _, err := tx.ExecContext(ctx, stmt, id, id); err != nil {
					return 0, fmt.Errorf("sqlite PurgeTerminal delete %q: %w", id, err)
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
				return 0, fmt.Errorf("sqlite PurgeTerminal delete %q: %w", id, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlite PurgeTerminal: commit: %w", err)
	}
	return len(ids), nil
}

// --- FR-15: Prime scan ---

// PrimeScan loads all open records and returns the count.
func (a *Adapter) PrimeScan(ctx context.Context) (int, error) {
	rows, err := a.readDB.QueryContext(ctx,
		"SELECT id FROM records WHERE status != 'closed'")
	if err != nil {
		return 0, fmt.Errorf("sqlite PrimeScan: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	n := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// --- FR-18: Range scan by recency ---

// RecentScan returns the most recently created records, newest first.
func (a *Adapter) RecentScan(ctx context.Context, limit int) ([]coordstore.Record, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := a.readDB.QueryContext(ctx,
		`SELECT id,title,status,type,priority,created_at,updated_at,assignee,parent_id
		 FROM records ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite RecentScan: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	return scanMainRows(rows)
}

// Stats returns SQLite-level diagnostics.
func (a *Adapter) Stats(_ context.Context) map[string]int64 {
	stats := a.readDB.Stats()
	out := map[string]int64{
		"open_connections": int64(stats.OpenConnections),
		"in_use":           int64(stats.InUse),
		"idle":             int64(stats.Idle),
	}
	if a.path != "" {
		out["db_bytes"] = fileSize(a.path)
		out["wal_bytes"] = fileSize(a.path + "-wal")
	}
	return out
}

// --- helpers ---

func buildMainQuery(q coordstore.Query) (string, []any) {
	where := []string{"1=1"}
	var args []any
	if q.Status != "" {
		where = append(where, "r.status=?")
		args = append(args, q.Status)
	} else {
		// Use IN so the (status,assignee) composite index can serve two tight
		// range scans (~10 rows) instead of scanning all assignee rows (~1k+).
		where = append(where, "r.status IN ('open','in_progress')")
	}
	if q.Type != "" {
		where = append(where, "r.type=?")
		args = append(args, q.Type)
	}
	if q.Assignee != "" {
		where = append(where, "r.assignee=?")
		args = append(args, q.Assignee)
	}
	if q.ParentID != "" {
		where = append(where, "r.parent_id=?")
		args = append(args, q.ParentID)
	}
	if q.Label != "" {
		where = append(where, "EXISTS(SELECT 1 FROM labels l WHERE l.record_id=r.id AND l.label=?)")
		args = append(args, q.Label)
	}
	for k, v := range q.Metadata {
		where = append(where, "EXISTS(SELECT 1 FROM metadata m WHERE m.record_id=r.id AND m.key=? AND m.value=?)")
		args = append(args, k, v)
	}
	query := `SELECT r.id,r.title,r.status,r.type,r.priority,r.created_at,r.updated_at,r.assignee,r.parent_id
	          FROM records r WHERE ` + strings.Join(where, " AND ")
	if q.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	return query, args
}

func buildEphemeralQuery(q coordstore.Query) (string, []any) {
	where := []string{"1=1"}
	var args []any
	if q.Status != "" {
		where = append(where, "e.status=?")
		args = append(args, q.Status)
	} else {
		where = append(where, "e.status != 'closed'")
	}
	if q.Type != "" {
		where = append(where, "e.type=?")
		args = append(args, q.Type)
	}
	if q.Assignee != "" {
		where = append(where, "e.assignee=?")
		args = append(args, q.Assignee)
	}
	if q.Label != "" {
		where = append(where, "EXISTS(SELECT 1 FROM ephemeral_labels el WHERE el.record_id=e.id AND el.label=?)")
		args = append(args, q.Label)
	}
	query := `SELECT e.id,e.title,e.status,e.type,e.created_at,e.updated_at,e.assignee,e.parent_id,e.expires_at
	          FROM ephemeral e WHERE ` + strings.Join(where, " AND ")
	if q.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	return query, args
}

func scanMainRows(rows *sql.Rows) ([]coordstore.Record, error) {
	var records []coordstore.Record
	for rows.Next() {
		var r coordstore.Record
		var createdNs, updatedNs int64
		if err := rows.Scan(&r.ID, &r.Title, &r.Status, &r.Type, &r.Priority, &createdNs, &updatedNs, &r.Assignee, &r.ParentID); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(0, createdNs)
		r.UpdatedAt = r.CreatedAt
		if updatedNs > 0 {
			r.UpdatedAt = time.Unix(0, updatedNs)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func scanEphemeralRows(rows *sql.Rows) ([]coordstore.Record, error) {
	var records []coordstore.Record
	for rows.Next() {
		var r coordstore.Record
		var createdNs, updatedNs, expiresNs int64
		if err := rows.Scan(&r.ID, &r.Title, &r.Status, &r.Type, &createdNs, &updatedNs, &r.Assignee, &r.ParentID, &expiresNs); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(0, createdNs)
		r.UpdatedAt = r.CreatedAt
		if updatedNs > 0 {
			r.UpdatedAt = time.Unix(0, updatedNs)
		}
		r.Ephemeral = true
		if expiresNs > 0 {
			r.ExpiresAt = time.Unix(0, expiresNs)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func insertLabels(ctx context.Context, tx *sql.Tx, id string, labels []string, ephemeral bool) error {
	table := "labels"
	if ephemeral {
		table = "ephemeral_labels"
	}
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO "+table+"(record_id,label) VALUES(?,?)", id, label); err != nil {
			return fmt.Errorf("sqlite insertLabels: %w", err)
		}
	}
	return nil
}

func insertMetadata(ctx context.Context, tx *sql.Tx, id string, meta map[string]string, ephemeral bool) error {
	if len(meta) == 0 {
		return nil
	}
	table := "metadata"
	if ephemeral {
		table = "ephemeral_metadata"
	}
	for k, v := range meta {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO "+table+"(record_id,key,value) VALUES(?,?,?)", id, k, v); err != nil {
			return fmt.Errorf("sqlite insertMetadata: %w", err)
		}
	}
	return nil
}

func upsertMetadata(ctx context.Context, tx *sql.Tx, id string, kvs map[string]string, ephemeral bool) error {
	table := "metadata"
	if ephemeral {
		table = "ephemeral_metadata"
	}
	for k, v := range kvs {
		if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO "+table+"(record_id,key,value) VALUES(?,?,?)", id, k, v); err != nil {
			return fmt.Errorf("sqlite upsertMetadata: %w", err)
		}
	}
	return nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// splitConcat parses the GROUP_CONCAT(col, char(30)) output from getMain/getEphemeral.
func splitConcat(s sql.NullString) []string {
	if !s.Valid || s.String == "" {
		return nil
	}
	return strings.Split(s.String, "\x1e")
}

// splitKVConcat parses GROUP_CONCAT(key||char(31)||value, char(30)) output.
func splitKVConcat(s sql.NullString) map[string]string {
	if !s.Valid || s.String == "" {
		return nil
	}
	m := make(map[string]string)
	for _, pair := range strings.Split(s.String, "\x1e") {
		if idx := strings.IndexByte(pair, '\x1f'); idx >= 0 {
			m[pair[:idx]] = pair[idx+1:]
		}
	}
	return m
}

// sqlGetMain and sqlGetEphemeral are pre-compiled at Open time to avoid
// repeated SQL parsing overhead on the hot Get path.
const sqlGetMain = `
	SELECT r.id, r.title, r.status, r.type, r.priority, r.created_at, r.updated_at, r.assignee, r.parent_id,
	    (SELECT GROUP_CONCAT(label, char(30)) FROM labels WHERE record_id = r.id),
	    (SELECT GROUP_CONCAT(key || char(31) || value, char(30)) FROM metadata WHERE record_id = r.id)
	FROM records r WHERE r.id = ?`

const sqlGetEphemeral = `
	SELECT e.id, e.title, e.status, e.type, e.created_at, e.updated_at, e.assignee, e.parent_id, e.expires_at,
	    (SELECT GROUP_CONCAT(label, char(30)) FROM ephemeral_labels WHERE record_id = e.id),
	    (SELECT GROUP_CONCAT(key || char(31) || value, char(30)) FROM ephemeral_metadata WHERE record_id = e.id)
	FROM ephemeral e WHERE e.id = ?`

type errNotFound string

func (e errNotFound) Error() string { return string(e) }

func isNotFound(err error) bool {
	var errNotFound errNotFound
	ok := errors.As(err, &errNotFound)
	return ok
}

// pragmas are applied once after opening the database.
const pragmas = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA cache_size=-65536;
PRAGMA temp_store=MEMORY;
PRAGMA mmap_size=268435456;
PRAGMA wal_autocheckpoint=1000;
`

// schema defines all tables and indexes. Applied once at Open.
const schema = `
CREATE TABLE IF NOT EXISTS records (
    id          TEXT PRIMARY KEY,
    title       TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'open',
    type        TEXT NOT NULL DEFAULT 'task',
    priority    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL DEFAULT 0,
    assignee    TEXT NOT NULL DEFAULT '',
    parent_id   TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_records_status    ON records(status);
CREATE INDEX IF NOT EXISTS idx_records_assignee  ON records(assignee);
CREATE INDEX IF NOT EXISTS idx_records_type      ON records(type);
CREATE INDEX IF NOT EXISTS idx_records_parent_id ON records(parent_id);
CREATE INDEX IF NOT EXISTS idx_records_created   ON records(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_records_terminal_retention ON records(status, updated_at)
    WHERE status IN ('closed','cancelled','canceled','expired');
CREATE INDEX IF NOT EXISTS idx_records_status_assignee ON records(status, assignee);
-- Partial index covering only non-closed rows: makes "status != 'closed' AND assignee=?"
-- scan ~10 rows instead of all 1k+ rows per assignee in a large closed-record dataset.
CREATE INDEX IF NOT EXISTS idx_records_open ON records(assignee, status) WHERE status != 'closed';

CREATE TABLE IF NOT EXISTS ephemeral (
    id          TEXT PRIMARY KEY,
    title       TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'open',
    type        TEXT NOT NULL DEFAULT 'message',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL DEFAULT 0,
    assignee    TEXT NOT NULL DEFAULT '',
    parent_id   TEXT NOT NULL DEFAULT '',
    expires_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_eph_status          ON ephemeral(status);
CREATE INDEX IF NOT EXISTS idx_eph_assignee        ON ephemeral(assignee);
CREATE INDEX IF NOT EXISTS idx_eph_type            ON ephemeral(type);
CREATE INDEX IF NOT EXISTS idx_eph_expires_at      ON ephemeral(expires_at);
CREATE INDEX IF NOT EXISTS idx_eph_type_status_assignee ON ephemeral(type, status, assignee);
-- Covering index for the mail-poll hot path: all SELECT columns are in the index,
-- so SQLite answers the query without touching the heap table.
CREATE INDEX IF NOT EXISTS idx_eph_mailpoll
ON ephemeral(type, status, assignee, id, title, created_at, updated_at, parent_id, expires_at)
WHERE status = 'open';

CREATE TABLE IF NOT EXISTS labels (
    record_id TEXT NOT NULL,
    label     TEXT NOT NULL,
    PRIMARY KEY (record_id, label)
);
CREATE INDEX IF NOT EXISTS idx_labels_label ON labels(label);

CREATE TABLE IF NOT EXISTS metadata (
    record_id TEXT NOT NULL,
    key       TEXT NOT NULL,
    value     TEXT NOT NULL,
    PRIMARY KEY (record_id, key)
);
CREATE INDEX IF NOT EXISTS idx_metadata_kv ON metadata(key, value);

CREATE TABLE IF NOT EXISTS ephemeral_labels (
    record_id TEXT NOT NULL,
    label     TEXT NOT NULL,
    PRIMARY KEY (record_id, label)
);
CREATE INDEX IF NOT EXISTS idx_eph_labels_label ON ephemeral_labels(label);

CREATE TABLE IF NOT EXISTS ephemeral_metadata (
    record_id TEXT NOT NULL,
    key       TEXT NOT NULL,
    value     TEXT NOT NULL,
    PRIMARY KEY (record_id, key)
);
CREATE INDEX IF NOT EXISTS idx_eph_meta_kv ON ephemeral_metadata(key, value);

CREATE TABLE IF NOT EXISTS deps (
    issue_id      TEXT NOT NULL,
    depends_on_id TEXT NOT NULL,
    dep_type      TEXT NOT NULL DEFAULT 'blocks',
    PRIMARY KEY (issue_id, depends_on_id)
);
CREATE INDEX IF NOT EXISTS idx_deps_depends_on ON deps(depends_on_id);
`
