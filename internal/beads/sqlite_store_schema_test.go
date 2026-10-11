package beads

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSQLiteStoreAcceptsOnlyCanonicalDeployedSchemaLayoutsWithoutRewrite(t *testing.T) {
	for _, revision := range []bool{false, true} {
		for _, legacyDeps := range []bool{false, true} {
			name := fmt.Sprintf("revision=%t/legacy-deps=%t", revision, legacyDeps)
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				createSQLiteSchemaFixture(t, dir, revision, legacyDeps, nil)
				before := snapshotSQLiteSchemaSource(t, dir)

				for _, readOnly := range []bool{true, false} {
					options := []SQLiteStoreOption{WithSQLiteStoreIDPrefix(sqliteGraphPrefix)}
					if readOnly {
						options = append(options, WithSQLiteStoreReadOnly())
					}
					opened, err := OpenSQLiteStore(dir, options...)
					if err != nil {
						t.Fatalf("OpenSQLiteStore(readOnly=%t): %v", readOnly, err)
					}
					store := opened.(*SQLiteStore)
					if store.hasRevisionColumn != revision {
						_ = store.CloseStore()
						t.Fatalf("hasRevisionColumn = %t, want %t", store.hasRevisionColumn, revision)
					}
					if store.legacyDepsPrimaryKey != legacyDeps {
						_ = store.CloseStore()
						t.Fatalf("legacyDepsPrimaryKey = %t, want %t", store.legacyDepsPrimaryKey, legacyDeps)
					}
					if err := store.CloseStore(); err != nil {
						t.Fatalf("CloseStore(readOnly=%t): %v", readOnly, err)
					}
					if after := snapshotSQLiteSchemaSource(t, dir); !reflect.DeepEqual(after, before) {
						t.Fatalf("canonical open(readOnly=%t) rewrote source:\n--- before ---\n%#v\n--- after ---\n%#v", readOnly, before, after)
					}
				}
			})
		}
	}
}

func TestSQLiteStoreRejectsSchemaDriftBeforeReadOnlyOrWritableMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]string) []string
	}{
		{
			name: "user_version",
			mutate: func(statements []string) []string {
				return append(statements, `PRAGMA user_version=1`)
			},
		},
		{
			name: "missing_table",
			mutate: func(statements []string) []string {
				return omitSQLiteSchemaStatement(statements, `CREATE TABLE kv`)
			},
		},
		{
			name: "changed_index",
			mutate: func(statements []string) []string {
				return replaceSQLiteSchemaStatement(statements,
					`CREATE INDEX idx_deps_issue ON deps(issue_id)`,
					`CREATE INDEX idx_deps_issue ON deps(dep_type)`)
			},
		},
		{
			name: "changed_constraint",
			mutate: func(statements []string) []string {
				return replaceSQLiteSchemaText(statements,
					`tier TEXT NOT NULL CHECK (tier IN ('main','wisp'))`,
					`tier TEXT NOT NULL CHECK (tier IN ('main','wisp','alien'))`)
			},
		},
		{
			name: "changed_type",
			mutate: func(statements []string) []string {
				return replaceSQLiteSchemaText(statements,
					`title TEXT NOT NULL`,
					`title BLOB NOT NULL`)
			},
		},
		{
			name: "changed_default",
			mutate: func(statements []string) []string {
				return replaceSQLiteSchemaText(statements,
					`assignee TEXT NOT NULL DEFAULT ''`,
					`assignee TEXT NOT NULL DEFAULT 'nobody'`)
			},
		},
		{
			name: "changed_primary_key",
			mutate: func(statements []string) []string {
				return replaceSQLiteSchemaText(statements,
					`PRIMARY KEY(issue_id, depends_on_id)`,
					`PRIMARY KEY(depends_on_id, issue_id)`)
			},
		},
		{
			name: "extra_object",
			mutate: func(statements []string) []string {
				return append(statements, `CREATE VIEW extra_beads AS SELECT id FROM beads`)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			createSQLiteSchemaFixture(t, dir, true, false, test.mutate)
			before := snapshotSQLiteSchemaSource(t, dir)

			for _, readOnly := range []bool{true, false} {
				options := []SQLiteStoreOption{WithSQLiteStoreIDPrefix(sqliteGraphPrefix)}
				if readOnly {
					options = append(options, WithSQLiteStoreReadOnly())
				}
				opened, err := OpenSQLiteStore(dir, options...)
				if opened != nil {
					_ = opened.(*SQLiteStore).CloseStore()
				}
				if err == nil || !strings.Contains(err.Error(), "unsupported sqlite schema") {
					t.Fatalf("OpenSQLiteStore(readOnly=%t) error = %v, want unsupported sqlite schema", readOnly, err)
				}
				if after := snapshotSQLiteSchemaSource(t, dir); !reflect.DeepEqual(after, before) {
					t.Fatalf("rejected open(readOnly=%t) mutated source:\n--- before ---\n%#v\n--- after ---\n%#v", readOnly, before, after)
				}
			}
		})
	}
}

func TestSQLiteStorePrivateRecoveryRequiresExistingCanonicalDatabase(t *testing.T) {
	t.Run("missing database", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "missing")
		opened, err := OpenSQLiteStore(dir, WithSQLiteStorePrivateRecovery())
		if opened != nil {
			_ = opened.(*SQLiteStore).CloseStore()
		}
		if err == nil || !strings.Contains(err.Error(), "requires an existing database") {
			t.Fatalf("OpenSQLiteStore(private recovery) error = %v, want existing database requirement", err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("private recovery created missing directory: %v", err)
		}
	})

	t.Run("schema drift", func(t *testing.T) {
		dir := t.TempDir()
		opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
		if err != nil {
			t.Fatalf("creating canonical recovery fixture: %v", err)
		}
		if err := opened.(*SQLiteStore).CloseStore(); err != nil {
			t.Fatalf("closing canonical recovery fixture: %v", err)
		}
		databasePath := filepath.Join(dir, sqliteStoreFilename)
		db, err := sql.Open("sqlite", sqliteStoreDSN(databasePath, false))
		if err != nil {
			t.Fatalf("opening schema-drift fixture: %v", err)
		}
		if _, err := db.Exec(`CREATE VIEW extra_beads AS SELECT id FROM beads`); err != nil {
			_ = db.Close()
			t.Fatalf("creating schema drift: %v", err)
		}
		var journalMode string
		if err := db.QueryRow(`PRAGMA journal_mode=DELETE`).Scan(&journalMode); err != nil {
			_ = db.Close()
			t.Fatalf("returning schema-drift fixture to rollback mode: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("closing schema-drift fixture: %v", err)
		}
		before := snapshotSQLiteSchemaSource(t, dir)

		opened, err = OpenSQLiteStore(dir, WithSQLiteStorePrivateRecovery(), WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
		if opened != nil {
			_ = opened.(*SQLiteStore).CloseStore()
		}
		if err == nil || !strings.Contains(err.Error(), "unsupported sqlite schema") {
			t.Fatalf("OpenSQLiteStore(private recovery) error = %v, want unsupported sqlite schema", err)
		}
		if after := snapshotSQLiteSchemaSource(t, dir); !reflect.DeepEqual(after, before) {
			t.Fatalf("private recovery repaired or mutated schema drift:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
		}
	})
}

func createSQLiteSchemaFixture(t *testing.T, dir string, revision, legacyDeps bool, mutate func([]string) []string) {
	t.Helper()
	statements := canonicalSQLiteSchemaStatementsForTest(revision, legacyDeps)
	if mutate != nil {
		statements = mutate(statements)
	}
	databasePath := filepath.Join(dir, sqliteStoreFilename)
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("opening schema fixture: %v", err)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("applying schema fixture statement %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing schema fixture: %v", err)
	}
}

func canonicalSQLiteSchemaStatementsForTest(revision, legacyDeps bool) []string {
	beadsColumns := `
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
			bead_json TEXT NOT NULL`
	if revision {
		beadsColumns += `,
			revision INTEGER NOT NULL DEFAULT 0`
	}
	depsPrimaryKey := `PRIMARY KEY(issue_id, depends_on_id)`
	if legacyDeps {
		depsPrimaryKey = `PRIMARY KEY(issue_id, depends_on_id, dep_type)`
	}
	return []string{
		`PRAGMA user_version=0`,
		`CREATE TABLE kv (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE beads (` + beadsColumns + `
		)`,
		`CREATE TABLE labels (
			bead_id TEXT NOT NULL,
			label TEXT NOT NULL,
			PRIMARY KEY(bead_id, label),
			FOREIGN KEY(bead_id) REFERENCES beads(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE metadata (
			bead_id TEXT NOT NULL,
			meta_key TEXT NOT NULL,
			meta_value TEXT NOT NULL,
			PRIMARY KEY(bead_id, meta_key),
			FOREIGN KEY(bead_id) REFERENCES beads(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE deps (
			issue_id TEXT NOT NULL,
			depends_on_id TEXT NOT NULL,
			dep_type TEXT NOT NULL,
			` + depsPrimaryKey + `
		)`,
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
}

func omitSQLiteSchemaStatement(statements []string, prefix string) []string {
	result := make([]string, 0, len(statements))
	for _, statement := range statements {
		if strings.HasPrefix(statement, prefix) {
			continue
		}
		result = append(result, statement)
	}
	return result
}

func replaceSQLiteSchemaStatement(statements []string, old, replacement string) []string {
	result := append([]string(nil), statements...)
	for index, statement := range result {
		if statement == old {
			result[index] = replacement
			return result
		}
	}
	panic("schema test statement not found: " + old)
}

func replaceSQLiteSchemaText(statements []string, old, replacement string) []string {
	result := append([]string(nil), statements...)
	for index, statement := range result {
		if strings.Contains(statement, old) {
			result[index] = strings.Replace(statement, old, replacement, 1)
			return result
		}
	}
	panic("schema test text not found: " + old)
}

type sqliteSchemaSourceEntry struct {
	name    string
	mode    os.FileMode
	size    int64
	modTime time.Time
	hash    string
}

func snapshotSQLiteSchemaSource(t *testing.T, dir string) []sqliteSchemaSourceEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading sqlite schema source: %v", err)
	}
	result := make([]sqliteSchemaSourceEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("stating sqlite schema source entry %q: %v", entry.Name(), err)
		}
		item := sqliteSchemaSourceEntry{
			name:    entry.Name(),
			mode:    info.Mode(),
			size:    info.Size(),
			modTime: info.ModTime(),
		}
		if info.Mode().IsRegular() {
			item.hash = fileSHA256(t, filepath.Join(dir, entry.Name()))
		}
		result = append(result, item)
	}
	return result
}
