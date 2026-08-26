//go:build integration

package beads

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// wantNativeSchemaVersion is the migration ceiling at the exact Beads commit
// pinned by go.mod and deps.env. It must move in lockstep with that pin.
const (
	wantNativeSchemaVersion        = 66
	wantNativeIgnoredSchemaVersion = 25
)

func currentSchemaMigrationVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	return version
}

func currentIgnoredSchemaMigrationVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM ignored_schema_migrations").Scan(&version); err != nil {
		t.Fatalf("query ignored_schema_migrations: %v", err)
	}
	return version
}

func assertSchemaV66IgnoredV25Shape(t *testing.T, db *sql.DB) {
	t.Helper()
	if got := currentSchemaMigrationVersion(t, db); got != wantNativeSchemaVersion {
		t.Fatalf("schema_migrations MAX(version) = %d, want %d", got, wantNativeSchemaVersion)
	}
	if got := currentIgnoredSchemaMigrationVersion(t, db); got != wantNativeIgnoredSchemaVersion {
		t.Fatalf("ignored_schema_migrations MAX(version) = %d, want %d", got, wantNativeIgnoredSchemaVersion)
	}
	var dataType, nullable string
	var defaultValue sql.NullString
	if err := db.QueryRow(`
SELECT DATA_TYPE, IS_NULLABLE, COLUMN_DEFAULT
FROM INFORMATION_SCHEMA.COLUMNS
WHERE TABLE_SCHEMA = DATABASE()
  AND TABLE_NAME = 'bd_events_journal'
  AND COLUMN_NAME = 'actor'`).Scan(&dataType, &nullable, &defaultValue); err != nil {
		t.Fatalf("query bd_events_journal.actor shape: %v", err)
	}
	if dataType != "varchar" || nullable != "NO" || !defaultValue.Valid || defaultValue.String != "" {
		t.Fatalf("bd_events_journal.actor shape = type %q nullable %q default %#v, want varchar NOT NULL DEFAULT empty", dataType, nullable, defaultValue)
	}
	var tempTables int
	if err := db.QueryRow(`
SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES
WHERE TABLE_SCHEMA = DATABASE() AND LEFT(TABLE_NAME, 8) = '__temp__'`).Scan(&tempTables); err != nil {
		t.Fatalf("query temporary migration tables: %v", err)
	}
	if tempTables != 0 {
		t.Fatalf("temporary migration tables remaining = %d, want 0", tempTables)
	}
}

func openSchemaCompatibilityStorage(t *testing.T, ctx context.Context, scopeRoot string, port int) beadslib.Storage {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("create .beads directory: %v", err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	storage, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Fatalf("open exact pinned upstream native Beads storage: %v", err)
	}
	return storage
}

// TestNativeDoltStoreFreshOpenReachesSchemaV66 proves a fresh native store
// initializes at the exact pinned migration ceiling and is immediately
// writable without a subprocess fallback.
func TestNativeDoltStoreFreshOpenReachesSchemaV66(t *testing.T) {
	ctx := context.Background()
	scopeRoot := t.TempDir()
	port := startTestDoltServer(t)
	storage := openSchemaCompatibilityStorage(t, ctx, scopeRoot, port)
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}

	accessor, ok := storage.(testRawDBGetter)
	if !ok {
		t.Fatal("exact pinned native storage does not expose the required raw DB test contract")
	}
	assertSchemaV66IgnoredV25Shape(t, accessor.DB())

	store := newNativeDoltStoreWithStorageAndPrefix(storage, "schema-v66-fresh", "gc")
	created, err := store.Create(Bead{Title: "schema v66 fresh-open bead"})
	if err != nil {
		t.Fatalf("Create bead on freshly-opened v66 store: %v", err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get bead on freshly-opened v66 store: %v", err)
	}
	if got.Title != "schema v66 fresh-open bead" {
		t.Fatalf("Title = %q, want schema v66 fresh-open bead", got.Title)
	}
}

// TestNativeDoltStoreReopenExistingSchemaV66NoSkewFallback proves an
// already-current store reopens without moving its migration cursor or losing
// native read/write behavior.
func TestNativeDoltStoreReopenExistingSchemaV66NoSkewFallback(t *testing.T) {
	ctx := context.Background()
	scopeRoot := t.TempDir()
	port := startTestDoltServer(t)

	first := openSchemaCompatibilityStorage(t, ctx, scopeRoot, port)
	if err := first.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		_ = first.Close()
		t.Fatalf("set issue prefix: %v", err)
	}
	firstStore := newNativeDoltStoreWithStorageAndPrefix(first, "schema-v66-reopen-seed", "gc")
	seeded, err := firstStore.Create(Bead{Title: "schema v66 pre-reopen bead"})
	if err != nil {
		_ = first.Close()
		t.Fatalf("Create seed bead before reopen: %v", err)
	}
	accessor, ok := first.(testRawDBGetter)
	if !ok {
		_ = first.Close()
		t.Fatal("exact pinned native storage does not expose the required raw DB test contract")
	}
	assertSchemaV66IgnoredV25Shape(t, accessor.DB())
	if err := first.Close(); err != nil {
		t.Fatalf("close first open: %v", err)
	}

	second := openSchemaCompatibilityStorage(t, ctx, scopeRoot, port)
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("close second open: %v", err)
		}
	})
	secondAccessor, ok := second.(testRawDBGetter)
	if !ok {
		t.Fatal("exact pinned native storage does not expose the required raw DB test contract")
	}
	assertSchemaV66IgnoredV25Shape(t, secondAccessor.DB())

	secondStore := newNativeDoltStoreWithStorageAndPrefix(second, "schema-v66-reopen-verify", "gc")
	got, err := secondStore.Get(seeded.ID)
	if err != nil {
		t.Fatalf("Get bead %s created before reopen: %v", seeded.ID, err)
	}
	if got.Title != "schema v66 pre-reopen bead" {
		t.Fatalf("Title after reopen = %q, want schema v66 pre-reopen bead", got.Title)
	}
	if _, err := secondStore.Create(Bead{Title: "schema v66 post-reopen bead"}); err != nil {
		t.Fatalf("Create bead after reopen: %v", err)
	}
}

// TestNativeDoltStoreMigratesPopulatedPreActorStateToSchemaV66 exercises both
// migration doors that add bd_events_journal.actor. It starts with populated
// current storage, rewinds only the exact 0066/ignored-0025 shape and cursors,
// then proves reopen restores 66/25 without losing the issue or journal row.
// Full live 53/59 migration remains a deployment gate against physical clones;
// this focused test makes the pinned module's immediate compatibility boundary
// deterministic in ordinary integration CI.
func TestNativeDoltStoreMigratesPopulatedPreActorStateToSchemaV66(t *testing.T) {
	ctx := context.Background()
	scopeRoot := t.TempDir()
	port := startTestDoltServer(t)

	first := openSchemaCompatibilityStorage(t, ctx, scopeRoot, port)
	if err := first.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		_ = first.Close()
		t.Fatalf("set issue prefix: %v", err)
	}
	firstStore := newNativeDoltStoreWithStorageAndPrefix(first, "schema-v66-migration-seed", "gc")
	seeded, err := firstStore.Create(Bead{Title: "preserved across actor migration"})
	if err != nil {
		_ = first.Close()
		t.Fatalf("create populated pre-actor seed: %v", err)
	}
	accessor, ok := first.(testRawDBGetter)
	if !ok {
		_ = first.Close()
		t.Fatal("exact pinned native storage does not expose the required raw DB test contract")
	}
	db := accessor.DB()
	assertSchemaV66IgnoredV25Shape(t, db)
	const journalSeq = 660025
	if _, err := db.Exec(`
INSERT INTO bd_events_journal (seq, ts, op, issue_id, actor, issue_json)
VALUES (?, CURRENT_TIMESTAMP, 'update', ?, 'pre-actor', '{}')`, journalSeq, seeded.ID); err != nil {
		_ = first.Close()
		t.Fatalf("seed pre-actor journal row: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE bd_events_journal DROP COLUMN actor"); err != nil {
		_ = first.Close()
		t.Fatalf("rewind bd_events_journal.actor: %v", err)
	}
	if _, err := db.Exec("DELETE FROM schema_migrations WHERE version = ?", wantNativeSchemaVersion); err != nil {
		_ = first.Close()
		t.Fatalf("rewind schema migration cursor: %v", err)
	}
	if _, err := db.Exec("DELETE FROM ignored_schema_migrations WHERE version = ?", wantNativeIgnoredSchemaVersion); err != nil {
		_ = first.Close()
		t.Fatalf("rewind ignored migration cursor: %v", err)
	}
	if got := currentSchemaMigrationVersion(t, db); got != wantNativeSchemaVersion-1 {
		_ = first.Close()
		t.Fatalf("rewound schema cursor = %d, want %d", got, wantNativeSchemaVersion-1)
	}
	if got := currentIgnoredSchemaMigrationVersion(t, db); got != wantNativeIgnoredSchemaVersion-1 {
		_ = first.Close()
		t.Fatalf("rewound ignored cursor = %d, want %d", got, wantNativeIgnoredSchemaVersion-1)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close pre-actor storage: %v", err)
	}

	second := openSchemaCompatibilityStorage(t, ctx, scopeRoot, port)
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("close migrated storage: %v", err)
		}
	})
	secondAccessor, ok := second.(testRawDBGetter)
	if !ok {
		t.Fatal("exact pinned native storage does not expose the required raw DB test contract")
	}
	assertSchemaV66IgnoredV25Shape(t, secondAccessor.DB())

	secondStore := newNativeDoltStoreWithStorageAndPrefix(second, "schema-v66-migration-verify", "gc")
	got, err := secondStore.Get(seeded.ID)
	if err != nil {
		t.Fatalf("get issue after actor migration: %v", err)
	}
	if got.Title != "preserved across actor migration" {
		t.Fatalf("title after migration = %q, want preserved", got.Title)
	}
	var actor string
	if err := secondAccessor.DB().QueryRow("SELECT actor FROM bd_events_journal WHERE seq = ?", journalSeq).Scan(&actor); err != nil {
		t.Fatalf("read preserved journal row after migration: %v", err)
	}
	if actor != "" {
		t.Fatalf("backfilled actor = %q, want empty attribution for pre-column row", actor)
	}
}
