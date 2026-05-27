// Package postgres provides a PostgreSQL-backed StoreAdapter.
package postgres

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/internal/sqlstore"
	_ "github.com/lib/pq" // PostgreSQL database/sql driver
)

// New returns a PostgreSQL adapter using dsn.
func New(dsn string) *sqlstore.Adapter {
	return sqlstore.New(dialect, dsn, "pg")
}

var dialect = sqlstore.Dialect{
	Name:   "postgres",
	Driver: "postgres",
	Placeholder: func(i int) string {
		return fmt.Sprintf("$%d", i)
	},
	Schema: []string{
		`CREATE TABLE IF NOT EXISTS records (
		    id TEXT PRIMARY KEY,
		    title TEXT NOT NULL DEFAULT '',
		    status TEXT NOT NULL DEFAULT 'open',
		    type TEXT NOT NULL DEFAULT 'task',
		    priority BIGINT NOT NULL DEFAULT 0,
		    created_at BIGINT NOT NULL,
		    updated_at BIGINT NOT NULL DEFAULT 0,
		    assignee TEXT NOT NULL DEFAULT '',
		    parent_id TEXT NOT NULL DEFAULT '',
		    description TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_status ON records(status)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_assignee ON records(assignee)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_type ON records(type)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_parent_id ON records(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_created ON records(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_terminal_retention ON records(status, updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_status_assignee ON records(status, assignee)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_open ON records(assignee, status) WHERE status <> 'closed'`,
		`CREATE TABLE IF NOT EXISTS ephemeral (
		    id TEXT PRIMARY KEY,
		    title TEXT NOT NULL DEFAULT '',
		    status TEXT NOT NULL DEFAULT 'open',
		    type TEXT NOT NULL DEFAULT 'message',
		    created_at BIGINT NOT NULL,
		    updated_at BIGINT NOT NULL DEFAULT 0,
		    assignee TEXT NOT NULL DEFAULT '',
		    parent_id TEXT NOT NULL DEFAULT '',
		    expires_at BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_status ON ephemeral(status)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_assignee ON ephemeral(assignee)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_type ON ephemeral(type)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_expires_at ON ephemeral(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_mailpoll ON ephemeral(type, status, assignee, id, title, created_at, parent_id, expires_at) WHERE status = 'open'`,
		`CREATE TABLE IF NOT EXISTS labels (
		    record_id TEXT NOT NULL,
		    label TEXT NOT NULL,
		    PRIMARY KEY(record_id, label)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_labels_label ON labels(label)`,
		`CREATE TABLE IF NOT EXISTS metadata (
		    record_id TEXT NOT NULL,
		    meta_key TEXT NOT NULL,
		    meta_value TEXT NOT NULL,
		    PRIMARY KEY(record_id, meta_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_metadata_kv ON metadata(meta_key, meta_value)`,
		`CREATE TABLE IF NOT EXISTS ephemeral_labels (
		    record_id TEXT NOT NULL,
		    label TEXT NOT NULL,
		    PRIMARY KEY(record_id, label)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_labels_label ON ephemeral_labels(label)`,
		`CREATE TABLE IF NOT EXISTS ephemeral_metadata (
		    record_id TEXT NOT NULL,
		    meta_key TEXT NOT NULL,
		    meta_value TEXT NOT NULL,
		    PRIMARY KEY(record_id, meta_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_metadata_kv ON ephemeral_metadata(meta_key, meta_value)`,
		`CREATE TABLE IF NOT EXISTS deps (
		    issue_id TEXT NOT NULL,
		    depends_on_id TEXT NOT NULL,
		    dep_type TEXT NOT NULL DEFAULT 'blocks',
		    PRIMARY KEY(issue_id, depends_on_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_deps_depends_on ON deps(depends_on_id)`,
	},
	InsertLabel:      `INSERT INTO {{table}}(record_id,label) VALUES($1,$2) ON CONFLICT DO NOTHING`,
	InsertMetadata:   `INSERT INTO {{table}}(record_id,meta_key,meta_value) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`,
	UpsertMetadata:   `INSERT INTO {{table}}(record_id,meta_key,meta_value) VALUES($1,$2,$3) ON CONFLICT(record_id,meta_key) DO UPDATE SET meta_value=EXCLUDED.meta_value`,
	UpsertDependency: `INSERT INTO deps(issue_id,depends_on_id,dep_type) VALUES($1,$2,$3) ON CONFLICT(issue_id,depends_on_id) DO UPDATE SET dep_type=EXCLUDED.dep_type`,
}
