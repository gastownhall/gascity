// Package dolt provides a Dolt SQL adapter for the coordination-store
// benchmark. It uses Dolt's MySQL-compatible wire protocol.
package dolt

import (
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/internal/sqlstore"
	_ "github.com/go-sql-driver/mysql" // MySQL/Dolt database/sql driver
)

// New returns a Dolt/MySQL-wire adapter using dsn.
func New(dsn string) *sqlstore.Adapter {
	return sqlstore.New(dialect, dsn, "dt")
}

var dialect = sqlstore.Dialect{
	Name:        "dolt",
	Driver:      "mysql",
	Placeholder: func(int) string { return "?" },
	Schema: []string{
		`CREATE TABLE IF NOT EXISTS records (
		    id VARCHAR(191) PRIMARY KEY,
		    title TEXT NOT NULL,
		    status VARCHAR(64) NOT NULL DEFAULT 'open',
		    type VARCHAR(64) NOT NULL DEFAULT 'task',
		    priority BIGINT NOT NULL DEFAULT 0,
		    created_at BIGINT NOT NULL,
		    assignee VARCHAR(191) NOT NULL DEFAULT '',
		    parent_id VARCHAR(191) NOT NULL DEFAULT '',
		    description TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_status ON records(status)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_assignee ON records(assignee)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_type ON records(type)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_parent_id ON records(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_created ON records(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_records_status_assignee ON records(status, assignee)`,
		`CREATE TABLE IF NOT EXISTS ephemeral (
		    id VARCHAR(191) PRIMARY KEY,
		    title TEXT NOT NULL,
		    status VARCHAR(64) NOT NULL DEFAULT 'open',
		    type VARCHAR(64) NOT NULL DEFAULT 'message',
		    created_at BIGINT NOT NULL,
		    assignee VARCHAR(191) NOT NULL DEFAULT '',
		    parent_id VARCHAR(191) NOT NULL DEFAULT '',
		    expires_at BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_status ON ephemeral(status)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_assignee ON ephemeral(assignee)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_type ON ephemeral(type)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_expires_at ON ephemeral(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_mailpoll ON ephemeral(type, status, assignee, id, created_at, parent_id, expires_at)`,
		`CREATE TABLE IF NOT EXISTS labels (
		    record_id VARCHAR(191) NOT NULL,
		    label VARCHAR(191) NOT NULL,
		    PRIMARY KEY(record_id, label)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_labels_label ON labels(label)`,
		`CREATE TABLE IF NOT EXISTS metadata (
		    record_id VARCHAR(191) NOT NULL,
		    meta_key VARCHAR(191) NOT NULL,
		    meta_value VARCHAR(191) NOT NULL,
		    PRIMARY KEY(record_id, meta_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_metadata_kv ON metadata(meta_key, meta_value)`,
		`CREATE TABLE IF NOT EXISTS ephemeral_labels (
		    record_id VARCHAR(191) NOT NULL,
		    label VARCHAR(191) NOT NULL,
		    PRIMARY KEY(record_id, label)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_labels_label ON ephemeral_labels(label)`,
		`CREATE TABLE IF NOT EXISTS ephemeral_metadata (
		    record_id VARCHAR(191) NOT NULL,
		    meta_key VARCHAR(191) NOT NULL,
		    meta_value VARCHAR(191) NOT NULL,
		    PRIMARY KEY(record_id, meta_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_eph_metadata_kv ON ephemeral_metadata(meta_key, meta_value)`,
		`CREATE TABLE IF NOT EXISTS deps (
		    issue_id VARCHAR(191) NOT NULL,
		    depends_on_id VARCHAR(191) NOT NULL,
		    dep_type VARCHAR(64) NOT NULL DEFAULT 'blocks',
		    PRIMARY KEY(issue_id, depends_on_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coord_deps_depends_on ON deps(depends_on_id)`,
	},
	InsertLabel:      `INSERT IGNORE INTO {{table}}(record_id,label) VALUES(?,?)`,
	InsertMetadata:   `INSERT IGNORE INTO {{table}}(record_id,meta_key,meta_value) VALUES(?,?,?)`,
	UpsertMetadata:   `INSERT INTO {{table}}(record_id,meta_key,meta_value) VALUES(?,?,?) ON DUPLICATE KEY UPDATE meta_value=VALUES(meta_value)`,
	UpsertDependency: `INSERT INTO deps(issue_id,depends_on_id,dep_type) VALUES(?,?,?) ON DUPLICATE KEY UPDATE dep_type=VALUES(dep_type)`,
}
