//go:build gascity_native_beads

package beads

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestDoltliteReadStoreProjectsPlainColumns pins that bd's plain columns reach
// the domain Bead from a doltlite snapshot that has them.
//
// await_type/await_id are the gate condition this store did not carry, so a
// gate bead read here reported no condition at all — and carrying only the type
// would say a gate waits on a PR without saying which one, since bd renders the
// pair. created_by, owner and notes are the plain-column members of the fields
// bd's list/show output carries that Bead did not model; comment_count,
// dependency_count and dependent_count are computed over relations and
// comments, not columns.
func TestDoltliteReadStoreProjectsPlainColumns(t *testing.T) {
	store, closeStore := newPlainColumnDoltliteStore(t, true)
	defer closeStore()

	rows, err := store.List(ListQuery{Type: "gate"})
	if err != nil {
		t.Fatalf("List gate beads: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("gate rows = %d, want 1", len(rows))
	}
	got := rows[0]
	for _, tc := range []struct{ field, got, want string }{
		{"AwaitType", got.AwaitType, "gh:pr"},
		{"AwaitID", got.AwaitID, "4912"},
		{"CreatedBy", got.CreatedBy, "seeder"},
		{"Owner", got.Owner, "owner@example.com"},
		{"Notes", got.Notes, "waiting on a PR"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
}

// TestDoltliteReadStoreToleratesMissingPlainColumns pins the schema-presence
// probe, which is what keeps this change from being a breaking one.
//
// These columns land in the MAIN list SELECT, not in an opt-in projection. A
// snapshot written before bd added them would therefore fail EVERY list read
// rather than merely omitting one field, so the probe resolves each to the
// empty-string literal exactly as the ephemeral/no_history flags already
// degrade. The fixture omits await_type, created_by and owner; notes predates
// them in bd's schema and is present in every generation this store can open,
// and all five resolve through the same loop.
func TestDoltliteReadStoreToleratesMissingPlainColumns(t *testing.T) {
	store, closeStore := newPlainColumnDoltliteStore(t, false)
	defer closeStore()

	rows, err := store.List(ListQuery{Type: "gate"})
	if err != nil {
		t.Fatalf("List on a snapshot without bd's plain columns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("gate rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.AwaitType != "" || got.AwaitID != "" || got.CreatedBy != "" || got.Owner != "" {
		t.Fatalf("AwaitType=%q AwaitID=%q CreatedBy=%q Owner=%q, want empty when the columns are absent",
			got.AwaitType, got.AwaitID, got.CreatedBy, got.Owner)
	}
	// The rest of the row must still project, or the degraded path is not a
	// degradation but a different bug.
	if got.ID != "gate-1" || got.Title != "Gate: gh:pr 4912" {
		t.Fatalf("degraded row = %+v, want the fixture bead intact", got)
	}
}

// newPlainColumnDoltliteStore builds a doltlite fixture holding one gate bead,
// with or without bd's await_type/await_id/created_by/owner columns.
func newPlainColumnDoltliteStore(t *testing.T, withPlainColumns bool) (*DoltliteReadStore, func()) {
	t.Helper()
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(filepath.Join(beadsDir, "doltlite"), 0o755); err != nil {
		t.Fatalf("mkdir doltlite dir: %v", err)
	}
	meta := []byte(`{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), meta, 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	dbPath := filepath.Join(beadsDir, "doltlite", "hq.db")
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=10000")
	if err != nil {
		t.Fatalf("open doltlite fixture db: %v", err)
	}
	cols := `,
			ephemeral INTEGER DEFAULT 0,
			no_history INTEGER DEFAULT 0`
	insertCols, insertVals := "", ""
	if withPlainColumns {
		cols += `,
			await_type TEXT DEFAULT '',
			await_id TEXT DEFAULT '',
			created_by TEXT DEFAULT '',
			owner TEXT DEFAULT ''`
		insertCols = ", await_type, await_id, created_by, owner"
		insertVals = ", 'gh:pr', '4912', 'seeder', 'owner@example.com'"
	}
	createTestDoltliteSchemaWithRowColumns(t, db, cols)
	if _, err := db.Exec(`INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, metadata, notes` +
		insertCols + `) VALUES ('gate-1', 'Gate: gh:pr 4912', 'open', 'gate', 2, '2026-08-01T12:00:00Z', '2026-08-01T12:00:00Z', '{}', 'waiting on a PR'` +
		insertVals + `)`); err != nil {
		t.Fatalf("insert gate fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture db: %v", err)
	}

	backing := NewBdStore(dir, func(string, string, ...string) ([]byte, error) {
		t.Fatal("backing bd runner should not be called by doltlite read tests")
		return nil, nil
	})
	store, err := NewDoltliteReadStore(dir, backing)
	if err != nil {
		t.Fatalf("NewDoltliteReadStore: %v", err)
	}
	return store, func() { _ = store.CloseStore() }
}
