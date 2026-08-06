package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

func inspectSQLiteStoreSchemaAtPath(ctx context.Context, databasePath string) (layout sqliteStoreSchemaLayout, returnErr error) {
	db, err := sql.Open("sqlite", sqliteStoreDSN(databasePath, true))
	if err != nil {
		return sqliteStoreSchemaLayout{}, fmt.Errorf("opening sqlite schema preflight %s: %w", databasePath, err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err := db.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing sqlite schema preflight %s: %w", databasePath, err))
		}
	}()
	layout, err = inspectSQLiteStoreSchema(ctx, db)
	if err != nil {
		return sqliteStoreSchemaLayout{}, err
	}
	return layout, nil
}

const (
	sqliteSchemaKVTable = `CREATE TABLE kv (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`
	sqliteSchemaBeadsTable = `CREATE TABLE beads (
			id TEXT PRIMARY KEY,
			tier TEXT NOT NULL CHECK (tier IN ('main','wisp')),
			title TEXT NOT NULL,
			status TEXT NOT NULL,
			issue_type TEXT NOT NULL,
			priority INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			assignee TEXT NOT NULL DEFAULT '',
			from_agent TEXT NOT NULL DEFAULT '',
			parent_id TEXT NOT NULL DEFAULT '',
			ref TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			bead_json TEXT NOT NULL,
			revision INTEGER NOT NULL DEFAULT 0
		)`
	sqliteSchemaPreRevisionBeadsTable = `CREATE TABLE beads (
			id TEXT PRIMARY KEY,
			tier TEXT NOT NULL CHECK (tier IN ('main','wisp')),
			title TEXT NOT NULL,
			status TEXT NOT NULL,
			issue_type TEXT NOT NULL,
			priority INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			assignee TEXT NOT NULL DEFAULT '',
			from_agent TEXT NOT NULL DEFAULT '',
			parent_id TEXT NOT NULL DEFAULT '',
			ref TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			bead_json TEXT NOT NULL
		)`
	sqliteSchemaLabelsTable = `CREATE TABLE labels (
			bead_id TEXT NOT NULL,
			label TEXT NOT NULL,
			PRIMARY KEY(bead_id, label),
			FOREIGN KEY(bead_id) REFERENCES beads(id) ON DELETE CASCADE
		)`
	sqliteSchemaMetadataTable = `CREATE TABLE metadata (
			bead_id TEXT NOT NULL,
			meta_key TEXT NOT NULL,
			meta_value TEXT NOT NULL,
			PRIMARY KEY(bead_id, meta_key),
			FOREIGN KEY(bead_id) REFERENCES beads(id) ON DELETE CASCADE
		)`
	sqliteSchemaDepsTable = `CREATE TABLE deps (
			issue_id TEXT NOT NULL,
			depends_on_id TEXT NOT NULL,
			dep_type TEXT NOT NULL,
			PRIMARY KEY(issue_id, depends_on_id)
		)`
	sqliteSchemaLegacyDepsTable = `CREATE TABLE deps (
			issue_id TEXT NOT NULL,
			depends_on_id TEXT NOT NULL,
			dep_type TEXT NOT NULL,
			PRIMARY KEY(issue_id, depends_on_id, dep_type)
		)`
)

var sqliteSchemaIndexStatements = []string{
	`CREATE INDEX idx_beads_tier_status ON beads(tier, status)`,
	`CREATE INDEX idx_beads_type ON beads(issue_type)`,
	`CREATE INDEX idx_beads_assignee ON beads(assignee)`,
	`CREATE INDEX idx_beads_parent ON beads(parent_id)`,
	`CREATE INDEX idx_beads_created ON beads(created_at)`,
	`CREATE INDEX idx_beads_updated ON beads(updated_at)`,
	`CREATE INDEX idx_labels_label ON labels(label)`,
	`CREATE INDEX idx_metadata_key_value ON metadata(meta_key, meta_value)`,
	`CREATE INDEX idx_deps_issue ON deps(issue_id)`,
	`CREATE INDEX idx_deps_depends ON deps(depends_on_id)`,
}

func sqliteStoreCreationSchemaStatements() []string {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA wal_autocheckpoint=1000`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=ON`,
		sqliteSchemaKVTable,
		sqliteSchemaBeadsTable,
		sqliteSchemaLabelsTable,
		sqliteSchemaMetadataTable,
		sqliteSchemaDepsTable,
	}
	return append(statements, sqliteSchemaIndexStatements...)
}

type sqliteStoreSchemaLayout struct {
	hasRevisionColumn    bool
	legacyDepsPrimaryKey bool
}

type sqliteSchemaObject struct {
	kind  string
	name  string
	table string
	sql   string
}

type sqliteSchemaColumn struct {
	position   int
	name       string
	columnType string
	notNull    int
	defaultSQL string
	hasDefault bool
	primaryKey int
	hidden     int
}

type sqliteSchemaIndex struct {
	name    string
	unique  int
	origin  string
	partial int
	columns []string
}

type sqliteSchemaForeignKey struct {
	id       int
	sequence int
	table    string
	from     string
	to       string
	onUpdate string
	onDelete string
	match    string
}

// ValidateSQLiteStoreSchema verifies that db uses one of the four exact
// deployed SQLite bead-store schema layouts.
func ValidateSQLiteStoreSchema(ctx context.Context, db *sql.DB) error {
	_, err := inspectSQLiteStoreSchema(ctx, db)
	return err
}

func inspectSQLiteStoreSchema(ctx context.Context, db *sql.DB) (sqliteStoreSchemaLayout, error) {
	if db == nil {
		return sqliteStoreSchemaLayout{}, fmt.Errorf("unsupported sqlite schema: missing database")
	}
	var userVersion int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		return sqliteStoreSchemaLayout{}, fmt.Errorf("inspecting sqlite schema user_version: %w", err)
	}
	if userVersion != 0 {
		return sqliteStoreSchemaLayout{}, fmt.Errorf("unsupported sqlite schema: user_version=%d, want 0", userVersion)
	}

	objects, err := readSQLiteSchemaObjects(ctx, db)
	if err != nil {
		return sqliteStoreSchemaLayout{}, err
	}
	layout, ok := matchCanonicalSQLiteSchemaObjects(objects)
	if !ok {
		return sqliteStoreSchemaLayout{}, fmt.Errorf("unsupported sqlite schema: sqlite_schema does not match a deployed layout")
	}
	if err := validateCanonicalSQLitePragmas(ctx, db, layout); err != nil {
		return sqliteStoreSchemaLayout{}, err
	}
	return layout, nil
}

func readSQLiteSchemaObjects(ctx context.Context, db *sql.DB) ([]sqliteSchemaObject, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT type, name, tbl_name, COALESCE(sql, '')
		FROM sqlite_schema
		ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("inspecting sqlite_schema: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var objects []sqliteSchemaObject
	for rows.Next() {
		var object sqliteSchemaObject
		if err := rows.Scan(&object.kind, &object.name, &object.table, &object.sql); err != nil {
			return nil, fmt.Errorf("inspecting sqlite_schema: %w", err)
		}
		object.sql = normalizeSQLiteSchemaSQL(object.sql)
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inspecting sqlite_schema: %w", err)
	}
	return objects, nil
}

func matchCanonicalSQLiteSchemaObjects(actual []sqliteSchemaObject) (sqliteStoreSchemaLayout, bool) {
	for _, layout := range []sqliteStoreSchemaLayout{
		{hasRevisionColumn: false, legacyDepsPrimaryKey: false},
		{hasRevisionColumn: false, legacyDepsPrimaryKey: true},
		{hasRevisionColumn: true, legacyDepsPrimaryKey: false},
		{hasRevisionColumn: true, legacyDepsPrimaryKey: true},
	} {
		if reflect.DeepEqual(actual, canonicalSQLiteSchemaObjects(layout)) {
			return layout, true
		}
	}
	return sqliteStoreSchemaLayout{}, false
}

func canonicalSQLiteSchemaObjects(layout sqliteStoreSchemaLayout) []sqliteSchemaObject {
	beadsTable := sqliteSchemaPreRevisionBeadsTable
	if layout.hasRevisionColumn {
		beadsTable = sqliteSchemaBeadsTable
	}
	depsTable := sqliteSchemaDepsTable
	if layout.legacyDepsPrimaryKey {
		depsTable = sqliteSchemaLegacyDepsTable
	}
	objects := []sqliteSchemaObject{
		{kind: "table", name: "kv", table: "kv", sql: sqliteSchemaKVTable},
		{kind: "table", name: "beads", table: "beads", sql: beadsTable},
		{kind: "table", name: "labels", table: "labels", sql: sqliteSchemaLabelsTable},
		{kind: "table", name: "metadata", table: "metadata", sql: sqliteSchemaMetadataTable},
		{kind: "table", name: "deps", table: "deps", sql: depsTable},
		{kind: "index", name: "sqlite_autoindex_kv_1", table: "kv"},
		{kind: "index", name: "sqlite_autoindex_beads_1", table: "beads"},
		{kind: "index", name: "sqlite_autoindex_labels_1", table: "labels"},
		{kind: "index", name: "sqlite_autoindex_metadata_1", table: "metadata"},
		{kind: "index", name: "sqlite_autoindex_deps_1", table: "deps"},
	}
	for _, statement := range sqliteSchemaIndexStatements {
		name, table := sqliteSchemaIndexIdentity(statement)
		objects = append(objects, sqliteSchemaObject{kind: "index", name: name, table: table, sql: statement})
	}
	for index := range objects {
		objects[index].sql = normalizeSQLiteSchemaSQL(objects[index].sql)
	}
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].kind != objects[j].kind {
			return objects[i].kind < objects[j].kind
		}
		return objects[i].name < objects[j].name
	})
	return objects
}

func sqliteSchemaIndexIdentity(statement string) (name, table string) {
	fields := strings.Fields(statement)
	if len(fields) < 5 {
		panic("invalid canonical SQLite index statement")
	}
	name = fields[2]
	table = strings.SplitN(fields[4], "(", 2)[0]
	return name, table
}

func normalizeSQLiteSchemaSQL(statement string) string {
	return strings.Join(strings.Fields(statement), " ")
}

func validateCanonicalSQLitePragmas(ctx context.Context, db *sql.DB, layout sqliteStoreSchemaLayout) error {
	expectedColumns := canonicalSQLiteSchemaColumns(layout)
	for _, table := range []string{"beads", "deps", "kv", "labels", "metadata"} {
		columns, err := readSQLiteSchemaColumns(ctx, db, table)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(columns, expectedColumns[table]) {
			return fmt.Errorf("unsupported sqlite schema: PRAGMA table_xinfo(%s) drift", table)
		}
	}

	expectedIndexes := canonicalSQLiteSchemaIndexes(layout)
	for _, table := range []string{"beads", "deps", "kv", "labels", "metadata"} {
		indexes, err := readSQLiteSchemaIndexes(ctx, db, table)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(indexes, expectedIndexes[table]) {
			return fmt.Errorf("unsupported sqlite schema: PRAGMA index_list(%s) drift", table)
		}
	}

	expectedForeignKeys := canonicalSQLiteSchemaForeignKeys()
	for _, table := range []string{"beads", "deps", "kv", "labels", "metadata"} {
		foreignKeys, err := readSQLiteSchemaForeignKeys(ctx, db, table)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(foreignKeys, expectedForeignKeys[table]) {
			return fmt.Errorf("unsupported sqlite schema: PRAGMA foreign_key_list(%s) drift", table)
		}
	}
	return nil
}

func readSQLiteSchemaColumns(ctx context.Context, db *sql.DB, table string) ([]sqliteSchemaColumn, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_xinfo(`+quoteSQLiteIdentifier(table)+`)`)
	if err != nil {
		return nil, fmt.Errorf("inspecting sqlite schema table %s: %w", table, err)
	}
	defer rows.Close() //nolint:errcheck
	var columns []sqliteSchemaColumn
	for rows.Next() {
		var column sqliteSchemaColumn
		var defaultSQL sql.NullString
		if err := rows.Scan(
			&column.position,
			&column.name,
			&column.columnType,
			&column.notNull,
			&defaultSQL,
			&column.primaryKey,
			&column.hidden,
		); err != nil {
			return nil, fmt.Errorf("inspecting sqlite schema table %s: %w", table, err)
		}
		column.defaultSQL = defaultSQL.String
		column.hasDefault = defaultSQL.Valid
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inspecting sqlite schema table %s: %w", table, err)
	}
	return columns, nil
}

func readSQLiteSchemaIndexes(ctx context.Context, db *sql.DB, table string) ([]sqliteSchemaIndex, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA index_list(`+quoteSQLiteIdentifier(table)+`)`)
	if err != nil {
		return nil, fmt.Errorf("inspecting sqlite schema indexes for %s: %w", table, err)
	}
	var indexes []sqliteSchemaIndex
	for rows.Next() {
		var sequence int
		var index sqliteSchemaIndex
		if err := rows.Scan(&sequence, &index.name, &index.unique, &index.origin, &index.partial); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("inspecting sqlite schema indexes for %s: %w", table, err)
		}
		indexes = append(indexes, index)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("inspecting sqlite schema indexes for %s: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing sqlite schema index census for %s: %w", table, err)
	}
	for index := range indexes {
		columns, err := readSQLiteSchemaIndexColumns(ctx, db, indexes[index].name)
		if err != nil {
			return nil, err
		}
		indexes[index].columns = columns
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i].name < indexes[j].name })
	return indexes, nil
}

func readSQLiteSchemaIndexColumns(ctx context.Context, db *sql.DB, indexName string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA index_info(`+quoteSQLiteIdentifier(indexName)+`)`)
	if err != nil {
		return nil, fmt.Errorf("inspecting sqlite schema index %s: %w", indexName, err)
	}
	defer rows.Close() //nolint:errcheck
	var columns []string
	for rows.Next() {
		var sequence, columnID int
		var name string
		if err := rows.Scan(&sequence, &columnID, &name); err != nil {
			return nil, fmt.Errorf("inspecting sqlite schema index %s: %w", indexName, err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inspecting sqlite schema index %s: %w", indexName, err)
	}
	return columns, nil
}

func readSQLiteSchemaForeignKeys(ctx context.Context, db *sql.DB, table string) ([]sqliteSchemaForeignKey, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_list(`+quoteSQLiteIdentifier(table)+`)`)
	if err != nil {
		return nil, fmt.Errorf("inspecting sqlite schema foreign keys for %s: %w", table, err)
	}
	defer rows.Close() //nolint:errcheck
	var foreignKeys []sqliteSchemaForeignKey
	for rows.Next() {
		var foreignKey sqliteSchemaForeignKey
		if err := rows.Scan(
			&foreignKey.id,
			&foreignKey.sequence,
			&foreignKey.table,
			&foreignKey.from,
			&foreignKey.to,
			&foreignKey.onUpdate,
			&foreignKey.onDelete,
			&foreignKey.match,
		); err != nil {
			return nil, fmt.Errorf("inspecting sqlite schema foreign keys for %s: %w", table, err)
		}
		foreignKeys = append(foreignKeys, foreignKey)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inspecting sqlite schema foreign keys for %s: %w", table, err)
	}
	return foreignKeys, nil
}

func quoteSQLiteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func canonicalSQLiteSchemaColumns(layout sqliteStoreSchemaLayout) map[string][]sqliteSchemaColumn {
	columns := map[string][]sqliteSchemaColumn{
		"kv": {
			{position: 0, name: "key", columnType: "TEXT", primaryKey: 1},
			{position: 1, name: "value", columnType: "TEXT", notNull: 1},
		},
		"beads": {
			{position: 0, name: "id", columnType: "TEXT", primaryKey: 1},
			{position: 1, name: "tier", columnType: "TEXT", notNull: 1},
			{position: 2, name: "title", columnType: "TEXT", notNull: 1},
			{position: 3, name: "status", columnType: "TEXT", notNull: 1},
			{position: 4, name: "issue_type", columnType: "TEXT", notNull: 1},
			{position: 5, name: "priority", columnType: "INTEGER"},
			{position: 6, name: "created_at", columnType: "INTEGER", notNull: 1},
			{position: 7, name: "updated_at", columnType: "INTEGER", notNull: 1},
			{position: 8, name: "assignee", columnType: "TEXT", notNull: 1, defaultSQL: "''", hasDefault: true},
			{position: 9, name: "from_agent", columnType: "TEXT", notNull: 1, defaultSQL: "''", hasDefault: true},
			{position: 10, name: "parent_id", columnType: "TEXT", notNull: 1, defaultSQL: "''", hasDefault: true},
			{position: 11, name: "ref", columnType: "TEXT", notNull: 1, defaultSQL: "''", hasDefault: true},
			{position: 12, name: "description", columnType: "TEXT", notNull: 1, defaultSQL: "''", hasDefault: true},
			{position: 13, name: "bead_json", columnType: "TEXT", notNull: 1},
		},
		"labels": {
			{position: 0, name: "bead_id", columnType: "TEXT", notNull: 1, primaryKey: 1},
			{position: 1, name: "label", columnType: "TEXT", notNull: 1, primaryKey: 2},
		},
		"metadata": {
			{position: 0, name: "bead_id", columnType: "TEXT", notNull: 1, primaryKey: 1},
			{position: 1, name: "meta_key", columnType: "TEXT", notNull: 1, primaryKey: 2},
			{position: 2, name: "meta_value", columnType: "TEXT", notNull: 1},
		},
		"deps": {
			{position: 0, name: "issue_id", columnType: "TEXT", notNull: 1, primaryKey: 1},
			{position: 1, name: "depends_on_id", columnType: "TEXT", notNull: 1, primaryKey: 2},
			{position: 2, name: "dep_type", columnType: "TEXT", notNull: 1},
		},
	}
	if layout.hasRevisionColumn {
		columns["beads"] = append(columns["beads"], sqliteSchemaColumn{
			position: 14, name: "revision", columnType: "INTEGER", notNull: 1, defaultSQL: "0", hasDefault: true,
		})
	}
	if layout.legacyDepsPrimaryKey {
		deps := append([]sqliteSchemaColumn(nil), columns["deps"]...)
		deps[2].primaryKey = 3
		columns["deps"] = deps
	}
	return columns
}

func canonicalSQLiteSchemaIndexes(layout sqliteStoreSchemaLayout) map[string][]sqliteSchemaIndex {
	indexes := map[string][]sqliteSchemaIndex{
		"kv": {
			{name: "sqlite_autoindex_kv_1", unique: 1, origin: "pk", columns: []string{"key"}},
		},
		"beads": {
			{name: "idx_beads_assignee", origin: "c", columns: []string{"assignee"}},
			{name: "idx_beads_created", origin: "c", columns: []string{"created_at"}},
			{name: "idx_beads_parent", origin: "c", columns: []string{"parent_id"}},
			{name: "idx_beads_tier_status", origin: "c", columns: []string{"tier", "status"}},
			{name: "idx_beads_type", origin: "c", columns: []string{"issue_type"}},
			{name: "idx_beads_updated", origin: "c", columns: []string{"updated_at"}},
			{name: "sqlite_autoindex_beads_1", unique: 1, origin: "pk", columns: []string{"id"}},
		},
		"labels": {
			{name: "idx_labels_label", origin: "c", columns: []string{"label"}},
			{name: "sqlite_autoindex_labels_1", unique: 1, origin: "pk", columns: []string{"bead_id", "label"}},
		},
		"metadata": {
			{name: "idx_metadata_key_value", origin: "c", columns: []string{"meta_key", "meta_value"}},
			{name: "sqlite_autoindex_metadata_1", unique: 1, origin: "pk", columns: []string{"bead_id", "meta_key"}},
		},
		"deps": {
			{name: "idx_deps_depends", origin: "c", columns: []string{"depends_on_id"}},
			{name: "idx_deps_issue", origin: "c", columns: []string{"issue_id"}},
			{name: "sqlite_autoindex_deps_1", unique: 1, origin: "pk", columns: []string{"issue_id", "depends_on_id"}},
		},
	}
	if layout.legacyDepsPrimaryKey {
		indexes["deps"][2].columns = []string{"issue_id", "depends_on_id", "dep_type"}
	}
	return indexes
}

func canonicalSQLiteSchemaForeignKeys() map[string][]sqliteSchemaForeignKey {
	return map[string][]sqliteSchemaForeignKey{
		"beads": nil,
		"deps":  nil,
		"kv":    nil,
		"labels": {
			{table: "beads", from: "bead_id", to: "id", onUpdate: "NO ACTION", onDelete: "CASCADE", match: "NONE"},
		},
		"metadata": {
			{table: "beads", from: "bead_id", to: "id", onUpdate: "NO ACTION", onDelete: "CASCADE", match: "NONE"},
		},
	}
}
