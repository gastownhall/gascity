// Package sqlstore provides a portable SQL StoreAdapter core for external
// database candidates in the coordination-store benchmark sweep.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
)

// Dialect captures SQL differences needed by the benchmark adapter.
type Dialect struct {
	Name             string
	Driver           string
	Schema           []string
	Placeholder      func(int) string
	InsertLabel      string
	InsertMetadata   string
	UpsertMetadata   string
	UpsertDependency string
}

// Adapter implements coordstore.StoreAdapter for a SQL database.
type Adapter struct {
	dialect Dialect
	dsn     string
	prefix  string
	db      *sql.DB
	seq     atomic.Int64
}

// New returns a SQL adapter for the supplied dialect and DSN.
func New(dialect Dialect, dsn, prefix string) *Adapter {
	return &Adapter{dialect: dialect, dsn: dsn, prefix: prefix}
}

// Open initializes the database schema.
func (a *Adapter) Open(ctx context.Context, _ coordstore.Config) error {
	db, err := sql.Open(a.dialect.Driver, a.dsn)
	if err != nil {
		return fmt.Errorf("%s: open: %w", a.dialect.Name, err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close() //nolint:errcheck
		return fmt.Errorf("%s: ping: %w", a.dialect.Name, err)
	}
	for _, stmt := range a.dialect.Schema {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			db.Close() //nolint:errcheck
			return fmt.Errorf("%s: apply schema: %w\nstatement: %s", a.dialect.Name, err, stmt)
		}
	}
	a.db = db
	if err := a.Reset(ctx); err != nil {
		db.Close() //nolint:errcheck
		a.db = nil
		return fmt.Errorf("%s: reset at open: %w", a.dialect.Name, err)
	}
	return nil
}

// Close releases database connections.
func (a *Adapter) Close() error {
	if a.db == nil {
		return nil
	}
	err := a.db.Close()
	a.db = nil
	return err
}

// Reset wipes stored data while preserving schema.
func (a *Adapter) Reset(ctx context.Context) error {
	for _, table := range []string{
		"deps", "labels", "metadata", "ephemeral_labels",
		"ephemeral_metadata", "ephemeral", "records",
	} {
		if _, err := a.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("%s: reset %s: %w", a.dialect.Name, table, err)
		}
	}
	a.seq.Store(0)
	return nil
}

// Create persists a new record.
func (a *Adapter) Create(ctx context.Context, r coordstore.Record) (coordstore.Record, error) {
	r = a.normalize(r)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return coordstore.Record{}, fmt.Errorf("%s Create: begin tx: %w", a.dialect.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck

	if r.Ephemeral {
		expiresNs := int64(0)
		if !r.ExpiresAt.IsZero() {
			expiresNs = r.ExpiresAt.UnixNano()
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO ephemeral(id,title,status,type,created_at,updated_at,assignee,parent_id,expires_at)
			 VALUES(`+a.placeholders(9)+`)`,
			r.ID, r.Title, r.Status, r.Type, r.CreatedAt.UnixNano(), r.UpdatedAt.UnixNano(), r.Assignee, r.ParentID, expiresNs)
		if err != nil {
			return coordstore.Record{}, fmt.Errorf("%s Create ephemeral: %w", a.dialect.Name, err)
		}
		if err := a.insertLabels(ctx, tx, r.ID, r.Labels, true); err != nil {
			return coordstore.Record{}, err
		}
		if err := a.insertMetadata(ctx, tx, r.ID, r.Metadata, true); err != nil {
			return coordstore.Record{}, err
		}
	} else {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO records(id,title,status,type,priority,created_at,updated_at,assignee,parent_id,description)
			 VALUES(`+a.placeholders(10)+`)`,
			r.ID, r.Title, r.Status, r.Type, r.Priority, r.CreatedAt.UnixNano(), r.UpdatedAt.UnixNano(), r.Assignee, r.ParentID, "")
		if err != nil {
			return coordstore.Record{}, fmt.Errorf("%s Create main: %w", a.dialect.Name, err)
		}
		if err := a.insertLabels(ctx, tx, r.ID, r.Labels, false); err != nil {
			return coordstore.Record{}, err
		}
		if err := a.insertMetadata(ctx, tx, r.ID, r.Metadata, false); err != nil {
			return coordstore.Record{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return coordstore.Record{}, fmt.Errorf("%s Create: commit: %w", a.dialect.Name, err)
	}
	return r, nil
}

// Get retrieves a record by ID.
func (a *Adapter) Get(ctx context.Context, id string) (coordstore.Record, error) {
	r, err := a.getFrom(ctx, "records", id, false)
	if err == nil {
		return r, nil
	}
	if !coordstore.IsNotFound(err) {
		return coordstore.Record{}, err
	}
	r, err = a.getFrom(ctx, "ephemeral", id, true)
	if err != nil {
		return coordstore.Record{}, err
	}
	return r, nil
}

// Update modifies a record in either tier.
func (a *Adapter) Update(ctx context.Context, id string, u coordstore.Update) error {
	table, ephemeral, err := a.locate(ctx, id)
	if err != nil {
		return err
	}
	if u.Status == "" && u.Assignee == "" && len(u.Metadata) == 0 {
		return nil
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s Update: begin tx: %w", a.dialect.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck

	var clauses []string
	var args []any
	if u.Status != "" {
		clauses = append(clauses, "status="+a.dialect.Placeholder(len(args)+1))
		args = append(args, u.Status)
	}
	if u.Assignee != "" {
		clauses = append(clauses, "assignee="+a.dialect.Placeholder(len(args)+1))
		args = append(args, u.Assignee)
	}
	clauses = append(clauses, "updated_at="+a.dialect.Placeholder(len(args)+1))
	args = append(args, time.Now().UnixNano())
	args = append(args, id)
	q := "UPDATE " + table + " SET " + strings.Join(clauses, ",") +
		" WHERE id=" + a.dialect.Placeholder(len(args))
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("%s Update: %w", a.dialect.Name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return coordstore.ErrNotFound
	}
	if len(u.Metadata) > 0 {
		if err := a.upsertMetadata(ctx, tx, id, u.Metadata, ephemeral); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Delete removes a record and cascade-deletes dependent rows.
func (a *Adapter) Delete(ctx context.Context, id string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s Delete: begin tx: %w", a.dialect.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck

	deleted := false
	for _, table := range []string{"records", "ephemeral"} {
		res, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE id="+a.dialect.Placeholder(1), id)
		if err != nil {
			return fmt.Errorf("%s Delete %s: %w", a.dialect.Name, table, err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			deleted = true
		}
	}
	if !deleted {
		return coordstore.ErrNotFound
	}
	for _, del := range []struct {
		stmt    string
		twoArgs bool
	}{
		{"DELETE FROM labels WHERE record_id=" + a.dialect.Placeholder(1), false},
		{"DELETE FROM metadata WHERE record_id=" + a.dialect.Placeholder(1), false},
		{"DELETE FROM ephemeral_labels WHERE record_id=" + a.dialect.Placeholder(1), false},
		{"DELETE FROM ephemeral_metadata WHERE record_id=" + a.dialect.Placeholder(1), false},
		{"DELETE FROM deps WHERE issue_id=" + a.dialect.Placeholder(1) + " OR depends_on_id=" + a.dialect.Placeholder(2), true},
	} {
		if del.twoArgs {
			_, err = tx.ExecContext(ctx, del.stmt, id, id)
		} else {
			_, err = tx.ExecContext(ctx, del.stmt, id)
		}
		if err != nil {
			return fmt.Errorf("%s Delete cascade: %w", a.dialect.Name, err)
		}
	}
	return tx.Commit()
}

// FilterScan returns records matching q from the selected tier.
func (a *Adapter) FilterScan(ctx context.Context, q coordstore.Query) ([]coordstore.Record, error) {
	var out []coordstore.Record
	if q.Tier == coordstore.TierMain || q.Tier == coordstore.TierBoth {
		main, err := a.filter(ctx, q, false)
		if err != nil {
			return nil, err
		}
		out = append(out, main...)
	}
	if q.Tier == coordstore.TierEphemeral || q.Tier == coordstore.TierBoth {
		eph, err := a.filter(ctx, q, true)
		if err != nil {
			return nil, err
		}
		out = append(out, eph...)
	}
	if q.Tier == 0 {
		return a.filter(ctx, q, false)
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// BatchGet retrieves multiple records by ID.
func (a *Adapter) BatchGet(ctx context.Context, ids []string) ([]coordstore.Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := a.db.QueryContext(ctx,
		"SELECT id,title,status,type,priority,created_at,updated_at,assignee,parent_id FROM records WHERE id IN ("+a.placeholders(len(ids))+")",
		args...)
	if err != nil {
		return nil, fmt.Errorf("%s BatchGet: %w", a.dialect.Name, err)
	}
	defer rows.Close() //nolint:errcheck
	return scanMainRows(rows)
}

// SetMetadataBatch atomically sets multiple metadata keys.
func (a *Adapter) SetMetadataBatch(ctx context.Context, id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	table, ephemeral, err := a.locate(ctx, id)
	if err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s SetMetadataBatch: begin tx: %w", a.dialect.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := a.upsertMetadata(ctx, tx, id, kvs, ephemeral); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE "+table+" SET updated_at="+a.dialect.Placeholder(1)+" WHERE id="+a.dialect.Placeholder(2),
		time.Now().UnixNano(), id); err != nil {
		return fmt.Errorf("%s SetMetadataBatch updated_at: %w", a.dialect.Name, err)
	}
	return tx.Commit()
}

// Ready returns unblocked open records from the main tier.
func (a *Adapter) Ready(ctx context.Context, q coordstore.ReadyQuery) ([]coordstore.Record, error) {
	excluded := []string{"merge-request", "gate", "molecule", "step", "message", "session", "agent", "role", "rig"}
	args := make([]any, 0, len(excluded)+2)
	for _, t := range excluded {
		args = append(args, t)
	}
	query := `SELECT r.id,r.title,r.status,r.type,r.priority,r.created_at,r.updated_at,r.assignee,r.parent_id
	          FROM records r
	          WHERE r.status IN ('open','in_progress')
	            AND r.type NOT IN (` + a.placeholders(len(excluded)) + `)
	            AND NOT EXISTS (
	                SELECT 1 FROM deps d JOIN records b ON b.id = d.depends_on_id
	                WHERE d.issue_id = r.id AND b.status IN ('open','in_progress')
	            )`
	if q.Assignee != "" {
		args = append(args, q.Assignee)
		query += " AND r.assignee=" + a.dialect.Placeholder(len(args))
	}
	if q.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s Ready: %w", a.dialect.Name, err)
	}
	defer rows.Close() //nolint:errcheck
	return scanMainRows(rows)
}

// DepAdd records a dependency edge.
func (a *Adapter) DepAdd(ctx context.Context, fromID, toID, depType string) error {
	if depType == "" {
		depType = "blocks"
	}
	_, err := a.db.ExecContext(ctx, a.dialect.UpsertDependency, fromID, toID, depType)
	if err != nil {
		return fmt.Errorf("%s DepAdd: %w", a.dialect.Name, err)
	}
	return nil
}

// DepRemove removes a dependency edge.
func (a *Adapter) DepRemove(ctx context.Context, fromID, toID string) error {
	_, err := a.db.ExecContext(ctx,
		"DELETE FROM deps WHERE issue_id="+a.dialect.Placeholder(1)+" AND depends_on_id="+a.dialect.Placeholder(2),
		fromID, toID)
	return err
}

// DepList returns dependency edges by direction.
func (a *Adapter) DepList(ctx context.Context, id, direction string) ([]coordstore.Dep, error) {
	col := "issue_id"
	if direction == "up" {
		col = "depends_on_id"
	}
	rows, err := a.db.QueryContext(ctx,
		"SELECT issue_id,depends_on_id,dep_type FROM deps WHERE "+col+"="+a.dialect.Placeholder(1),
		id)
	if err != nil {
		return nil, fmt.Errorf("%s DepList: %w", a.dialect.Name, err)
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

// PurgeExpired removes expired ephemeral records.
func (a *Adapter) PurgeExpired(ctx context.Context) (int, error) {
	now := time.Now().UnixNano()
	rows, err := a.db.QueryContext(ctx,
		"SELECT id FROM ephemeral WHERE expires_at > 0 AND expires_at < "+a.dialect.Placeholder(1), now)
	if err != nil {
		return 0, fmt.Errorf("%s PurgeExpired query: %w", a.dialect.Name, err)
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
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%s PurgeExpired begin tx: %w", a.dialect.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, id := range ids {
		for _, stmt := range []string{
			"DELETE FROM ephemeral WHERE id=" + a.dialect.Placeholder(1),
			"DELETE FROM ephemeral_labels WHERE record_id=" + a.dialect.Placeholder(1),
			"DELETE FROM ephemeral_metadata WHERE record_id=" + a.dialect.Placeholder(1),
		} {
			if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
				return 0, err
			}
		}
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
	rows, err := a.db.QueryContext(ctx,
		`SELECT id FROM records
		 WHERE status IN ('closed','cancelled','canceled','expired')
		   AND COALESCE(NULLIF(updated_at, 0), created_at) < `+a.dialect.Placeholder(1)+
			fmt.Sprintf(" LIMIT %d", coordstore.TerminalPurgeBatchSize), cutoff)
	if err != nil {
		return 0, fmt.Errorf("%s PurgeTerminal query: %w", a.dialect.Name, err)
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

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%s PurgeTerminal begin tx: %w", a.dialect.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, id := range ids {
		deletes := []struct {
			stmt string
			args []any
		}{
			{"DELETE FROM labels WHERE record_id=" + a.dialect.Placeholder(1), []any{id}},
			{"DELETE FROM metadata WHERE record_id=" + a.dialect.Placeholder(1), []any{id}},
			{"DELETE FROM deps WHERE issue_id=" + a.dialect.Placeholder(1) + " OR depends_on_id=" + a.dialect.Placeholder(2), []any{id, id}},
			{"DELETE FROM records WHERE id=" + a.dialect.Placeholder(1), []any{id}},
		}
		for _, del := range deletes {
			if _, err := tx.ExecContext(ctx, del.stmt, del.args...); err != nil {
				return 0, fmt.Errorf("%s PurgeTerminal delete %q: %w", a.dialect.Name, id, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// PrimeScan scans open main-tier records.
func (a *Adapter) PrimeScan(ctx context.Context) (int, error) {
	rows, err := a.db.QueryContext(ctx,
		"SELECT id FROM records WHERE status <> 'closed'")
	if err != nil {
		return 0, fmt.Errorf("%s PrimeScan: %w", a.dialect.Name, err)
	}
	defer rows.Close() //nolint:errcheck
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// RecentScan returns recently-created records from both tiers.
func (a *Adapter) RecentScan(ctx context.Context, limit int) ([]coordstore.Record, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id,title,status,type,priority,created_at,updated_at,assignee,parent_id,expires_at,ephemeral
	          FROM (
	              SELECT id,title,status,type,priority,created_at,updated_at,assignee,parent_id,0 AS expires_at,0 AS ephemeral FROM records
	              UNION ALL
	              SELECT id,title,status,type,0 AS priority,created_at,updated_at,assignee,parent_id,expires_at,1 AS ephemeral FROM ephemeral
	          ) recent ORDER BY created_at DESC LIMIT ` + fmt.Sprintf("%d", limit)
	rows, err := a.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%s RecentScan: %w", a.dialect.Name, err)
	}
	defer rows.Close() //nolint:errcheck
	var out []coordstore.Record
	for rows.Next() {
		var r coordstore.Record
		var createdNs, updatedNs, expiresNs int64
		var ephemeral int
		if err := rows.Scan(&r.ID, &r.Title, &r.Status, &r.Type, &r.Priority, &createdNs, &updatedNs, &r.Assignee, &r.ParentID, &expiresNs, &ephemeral); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(0, createdNs)
		r.UpdatedAt = r.CreatedAt
		if updatedNs > 0 {
			r.UpdatedAt = time.Unix(0, updatedNs)
		}
		r.Ephemeral = ephemeral == 1
		if expiresNs > 0 {
			r.ExpiresAt = time.Unix(0, expiresNs)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Stats returns database pool stats.
func (a *Adapter) Stats(context.Context) map[string]int64 {
	if a.db == nil {
		return nil
	}
	stats := a.db.Stats()
	return map[string]int64{
		"open_connections": int64(stats.OpenConnections),
		"in_use":           int64(stats.InUse),
		"idle":             int64(stats.Idle),
	}
}

func (a *Adapter) normalize(r coordstore.Record) coordstore.Record {
	if r.ID == "" {
		r.ID = fmt.Sprintf("%s-%d", a.prefix, a.seq.Add(1))
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
		if r.Ephemeral {
			r.Type = "message"
		} else {
			r.Type = "task"
		}
	}
	return r
}

func (a *Adapter) getFrom(ctx context.Context, table, id string, ephemeral bool) (coordstore.Record, error) {
	var r coordstore.Record
	var createdNs, updatedNs, expiresNs int64
	query := "SELECT id,title,status,type,"
	if ephemeral {
		query += "0 AS priority,"
	} else {
		query += "priority,"
	}
	query += "created_at,updated_at,assignee,parent_id"
	if ephemeral {
		query += ",expires_at"
	}
	query += " FROM " + table + " WHERE id=" + a.dialect.Placeholder(1)
	row := a.db.QueryRowContext(ctx, query, id)
	var err error
	if ephemeral {
		err = row.Scan(&r.ID, &r.Title, &r.Status, &r.Type, &r.Priority, &createdNs, &updatedNs, &r.Assignee, &r.ParentID, &expiresNs)
	} else {
		err = row.Scan(&r.ID, &r.Title, &r.Status, &r.Type, &r.Priority, &createdNs, &updatedNs, &r.Assignee, &r.ParentID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return coordstore.Record{}, coordstore.ErrNotFound
	}
	if err != nil {
		return coordstore.Record{}, err
	}
	r.CreatedAt = time.Unix(0, createdNs)
	r.UpdatedAt = r.CreatedAt
	if updatedNs > 0 {
		r.UpdatedAt = time.Unix(0, updatedNs)
	}
	r.Ephemeral = ephemeral
	if expiresNs > 0 {
		r.ExpiresAt = time.Unix(0, expiresNs)
	}
	labels, err := a.loadLabels(ctx, r.ID, ephemeral)
	if err != nil {
		return coordstore.Record{}, err
	}
	meta, err := a.loadMetadata(ctx, r.ID, ephemeral)
	if err != nil {
		return coordstore.Record{}, err
	}
	r.Labels = labels
	r.Metadata = meta
	return r, nil
}

func (a *Adapter) locate(ctx context.Context, id string) (string, bool, error) {
	var found int
	err := a.db.QueryRowContext(ctx,
		"SELECT 1 FROM records WHERE id="+a.dialect.Placeholder(1), id).Scan(&found)
	if err == nil {
		return "records", false, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	err = a.db.QueryRowContext(ctx,
		"SELECT 1 FROM ephemeral WHERE id="+a.dialect.Placeholder(1), id).Scan(&found)
	if err == nil {
		return "ephemeral", true, nil
	}
	if err == sql.ErrNoRows {
		return "", false, coordstore.ErrNotFound
	}
	return "", false, err
}

func (a *Adapter) filter(ctx context.Context, q coordstore.Query, ephemeral bool) ([]coordstore.Record, error) {
	table := "records"
	alias := "r"
	labelTable := "labels"
	metaTable := "metadata"
	selectCols := "r.id,r.title,r.status,r.type,r.priority,r.created_at,r.updated_at,r.assignee,r.parent_id"
	if ephemeral {
		table = "ephemeral"
		alias = "e"
		labelTable = "ephemeral_labels"
		metaTable = "ephemeral_metadata"
		selectCols = "e.id,e.title,e.status,e.type,e.created_at,e.updated_at,e.assignee,e.parent_id,e.expires_at"
	}
	var where []string
	var args []any
	switch {
	case q.Status != "":
		args = append(args, q.Status)
		where = append(where, alias+".status="+a.dialect.Placeholder(len(args)))
	case ephemeral:
		where = append(where, alias+".status <> 'closed'")
	default:
		where = append(where, alias+".status IN ('open','in_progress')")
	}
	if q.Type != "" {
		args = append(args, q.Type)
		where = append(where, alias+".type="+a.dialect.Placeholder(len(args)))
	}
	if q.Assignee != "" {
		args = append(args, q.Assignee)
		where = append(where, alias+".assignee="+a.dialect.Placeholder(len(args)))
	}
	if q.ParentID != "" {
		args = append(args, q.ParentID)
		where = append(where, alias+".parent_id="+a.dialect.Placeholder(len(args)))
	}
	if q.Label != "" {
		args = append(args, q.Label)
		where = append(where, "EXISTS(SELECT 1 FROM "+labelTable+" l WHERE l.record_id="+alias+".id AND l.label="+a.dialect.Placeholder(len(args))+")")
	}
	for k, v := range q.Metadata {
		args = append(args, k, v)
		where = append(where, "EXISTS(SELECT 1 FROM "+metaTable+" m WHERE m.record_id="+alias+".id AND m.meta_key="+a.dialect.Placeholder(len(args)-1)+" AND m.meta_value="+a.dialect.Placeholder(len(args))+")")
	}
	query := "SELECT " + selectCols + " FROM " + table + " " + alias
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if q.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s FilterScan %s: %w", a.dialect.Name, table, err)
	}
	defer rows.Close() //nolint:errcheck
	if ephemeral {
		return scanEphemeralRows(rows)
	}
	return scanMainRows(rows)
}

func (a *Adapter) insertLabels(ctx context.Context, tx *sql.Tx, id string, labels []string, ephemeral bool) error {
	table := "labels"
	if ephemeral {
		table = "ephemeral_labels"
	}
	stmt := strings.Replace(a.dialect.InsertLabel, "{{table}}", table, 1)
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx, stmt, id, label); err != nil {
			return fmt.Errorf("%s insertLabels: %w", a.dialect.Name, err)
		}
	}
	return nil
}

func (a *Adapter) insertMetadata(ctx context.Context, tx *sql.Tx, id string, meta map[string]string, ephemeral bool) error {
	table := "metadata"
	if ephemeral {
		table = "ephemeral_metadata"
	}
	stmt := strings.Replace(a.dialect.InsertMetadata, "{{table}}", table, 1)
	for k, v := range meta {
		if _, err := tx.ExecContext(ctx, stmt, id, k, v); err != nil {
			return fmt.Errorf("%s insertMetadata: %w", a.dialect.Name, err)
		}
	}
	return nil
}

func (a *Adapter) upsertMetadata(ctx context.Context, tx *sql.Tx, id string, meta map[string]string, ephemeral bool) error {
	table := "metadata"
	if ephemeral {
		table = "ephemeral_metadata"
	}
	stmt := strings.Replace(a.dialect.UpsertMetadata, "{{table}}", table, 1)
	for k, v := range meta {
		if _, err := tx.ExecContext(ctx, stmt, id, k, v); err != nil {
			return fmt.Errorf("%s upsertMetadata: %w", a.dialect.Name, err)
		}
	}
	return nil
}

func (a *Adapter) loadLabels(ctx context.Context, id string, ephemeral bool) ([]string, error) {
	table := "labels"
	if ephemeral {
		table = "ephemeral_labels"
	}
	rows, err := a.db.QueryContext(ctx,
		"SELECT label FROM "+table+" WHERE record_id="+a.dialect.Placeholder(1), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func (a *Adapter) loadMetadata(ctx context.Context, id string, ephemeral bool) (map[string]string, error) {
	table := "metadata"
	if ephemeral {
		table = "ephemeral_metadata"
	}
	rows, err := a.db.QueryContext(ctx,
		"SELECT meta_key,meta_value FROM "+table+" WHERE record_id="+a.dialect.Placeholder(1), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	meta := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		meta[k] = v
	}
	return meta, rows.Err()
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

func (a *Adapter) placeholders(n int) string {
	values := make([]string, n)
	for i := range n {
		values[i] = a.dialect.Placeholder(i + 1)
	}
	return strings.Join(values, ",")
}
