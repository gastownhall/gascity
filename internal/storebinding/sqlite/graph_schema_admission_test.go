package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/storebinding"
)

type graphSchemaMutationTestCase struct {
	name  string
	apply func(*testing.T, *sql.DB)
}

func TestGraphInspectorAdmitsOnlyCanonicalDeployedSchemaLayouts(t *testing.T) {
	for _, revision := range []bool{false, true} {
		for _, legacyDeps := range []bool{false, true} {
			name := fmt.Sprintf("revision=%t/legacy-deps=%t", revision, legacyDeps)
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				prepareGraphSchemaFixture(t, root, revision, legacyDeps)

				inspection, err := InspectGraph(context.Background(), graphBindingSpec(root))
				if err != nil {
					t.Fatalf("InspectGraph(): %v", err)
				}
				if !inspection.Complete() {
					t.Fatal("InspectGraph() returned incomplete for a canonical deployed schema")
				}
			})
		}
	}
}

func TestGraphInspectorRejectsSchemaDriftWithoutSourceMutation(t *testing.T) {
	for _, test := range graphSchemaDriftTestCases() {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			graphDir := prepareGraphSchemaFixture(t, root, true, false)
			mutateGraphSchemaFixture(t, graphDir, test.apply)
			before := snapshotGraphSource(t, graphDir)

			inspection, err := InspectGraph(context.Background(), graphBindingSpec(root))
			if err == nil || !strings.Contains(err.Error(), "unsupported sqlite schema") {
				t.Fatalf("InspectGraph() error = %v, inspection = %#v; want unsupported sqlite schema", err, inspection)
			}
			if inspection.Complete() {
				t.Fatal("InspectGraph() returned a complete descriptor for schema drift")
			}

			after := snapshotGraphSource(t, graphDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected Graph inspection mutated source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
			}
		})
	}
}

func graphBindingSpec(root string) storebinding.BindingSpec {
	return storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	}
}

func prepareGraphSchemaFixture(t *testing.T, root string, revision, legacyDeps bool) string {
	t.Helper()
	graphDir := filepath.Join(root, graphDirectoryName)
	writer := openGraphSource(t, graphDir)
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing canonical Graph fixture: %v", err)
	}
	mutateGraphSchemaFixture(t, graphDir, func(t *testing.T, db *sql.DB) {
		if !revision {
			setGraphSchemaSQLForTest(t, db, "beads", graphPreRevisionSchemaForTest)
		}
		if legacyDeps {
			rewriteGraphSchemaSQLForTest(t, db, "deps",
				"PRIMARY KEY(issue_id, depends_on_id)",
				"PRIMARY KEY(issue_id, depends_on_id, dep_type)")
		}
	})
	return graphDir
}

const graphPreRevisionSchemaForTest = `CREATE TABLE beads (
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

func mutateGraphSchemaFixture(t *testing.T, graphDir string, mutate func(*testing.T, *sql.DB)) {
	t.Helper()
	databasePath := filepath.Join(graphDir, graphFilename)
	db, err := sql.Open("sqlite", graphPrivateSnapshotDSN(databasePath))
	if err != nil {
		t.Fatalf("opening Graph schema mutation fixture: %v", err)
	}
	db.SetMaxOpenConns(1)
	mutate(t, db)
	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode=DELETE`).Scan(&journalMode); err != nil {
		_ = db.Close()
		t.Fatalf("returning Graph schema fixture to rollback mode: %v", err)
	}
	if journalMode != "delete" {
		_ = db.Close()
		t.Fatalf("Graph schema fixture journal mode = %q, want delete", journalMode)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing Graph schema mutation fixture: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(databasePath + suffix); !os.IsNotExist(err) {
			t.Fatalf("Graph schema fixture retained SQLite sidecar %q: %v", suffix, err)
		}
	}
}

func setGraphSchemaSQLForTest(t *testing.T, db *sql.DB, table, schema string) {
	t.Helper()
	if _, err := db.Exec(`PRAGMA writable_schema=ON`); err != nil {
		t.Fatalf("enabling writable_schema: %v", err)
	}
	result, err := db.Exec(
		`UPDATE sqlite_schema SET sql = ? WHERE type = 'table' AND name = ?`,
		schema,
		table,
	)
	if err != nil {
		t.Fatalf("setting %s schema: %v", table, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		t.Fatalf("reading set %s schema row count: %v", table, err)
	}
	if changed != 1 {
		t.Fatalf("set %s schema rows = %d, want 1", table, changed)
	}
	if _, err := db.Exec(`PRAGMA writable_schema=OFF`); err != nil {
		t.Fatalf("disabling writable_schema: %v", err)
	}
}

func rewriteGraphSchemaSQLForTest(t *testing.T, db *sql.DB, table, old, replacement string) {
	t.Helper()
	if _, err := db.Exec(`PRAGMA writable_schema=ON`); err != nil {
		t.Fatalf("enabling writable_schema: %v", err)
	}
	result, err := db.Exec(
		`UPDATE sqlite_schema SET sql = replace(sql, ?, ?) WHERE type = 'table' AND name = ?`,
		old,
		replacement,
		table,
	)
	if err != nil {
		t.Fatalf("rewriting %s schema: %v", table, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		t.Fatalf("reading rewritten %s schema row count: %v", table, err)
	}
	if changed != 1 {
		t.Fatalf("rewritten %s schema rows = %d, want 1", table, changed)
	}
	if _, err := db.Exec(`PRAGMA writable_schema=OFF`); err != nil {
		t.Fatalf("disabling writable_schema: %v", err)
	}
}

func graphSchemaDriftTestCases() []graphSchemaMutationTestCase {
	return []graphSchemaMutationTestCase{
		{
			name: "changed constraint",
			apply: func(t *testing.T, db *sql.DB) {
				rewriteGraphSchemaSQLForTest(t, db, "beads",
					"CHECK (tier IN ('main','wisp'))",
					"CHECK (tier IN ('main','wisp','alien'))")
			},
		},
		{
			name: "changed default",
			apply: func(t *testing.T, db *sql.DB) {
				rewriteGraphSchemaSQLForTest(t, db, "beads",
					"assignee TEXT NOT NULL DEFAULT ''",
					"assignee TEXT NOT NULL DEFAULT 'nobody'")
			},
		},
		{
			name: "changed index",
			apply: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`DROP INDEX idx_deps_issue`); err != nil {
					t.Fatalf("dropping canonical dependency index: %v", err)
				}
				if _, err := db.Exec(`CREATE INDEX idx_deps_issue ON deps(dep_type)`); err != nil {
					t.Fatalf("creating drifted dependency index: %v", err)
				}
			},
		},
		{
			name: "changed primary key",
			apply: func(t *testing.T, db *sql.DB) {
				rewriteGraphSchemaSQLForTest(t, db, "deps",
					"PRIMARY KEY(issue_id, depends_on_id)",
					"PRIMARY KEY(depends_on_id, issue_id)")
			},
		},
		{
			name: "extra object",
			apply: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`CREATE VIEW extra_beads AS SELECT id FROM beads`); err != nil {
					t.Fatalf("creating extra schema object: %v", err)
				}
			},
		},
		{
			name: "extra column",
			apply: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`ALTER TABLE kv ADD COLUMN extra TEXT`); err != nil {
					t.Fatalf("adding extra schema column: %v", err)
				}
			},
		},
		{
			name: "user version",
			apply: func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec(`PRAGMA user_version=1`); err != nil {
					t.Fatalf("changing user_version: %v", err)
				}
			},
		},
	}
}
