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

// wantNativeSchemaVersion is the beads main-schema migration cursor this
// gascity release is pinned to (deps.env BD_CURRENT_REF /
// github.com/steveyegge/beads in go.mod). It must equal the highest
// numbered file under beads' internal/storage/schema/migrations at that
// pinned commit (0065_widen_wisp_comments_text). Bump this in lockstep with
// the next deliberate BD_CURRENT_REF promotion — see
// gascity#5349/bd_version_pin_test.go for the anchor-consistency contract
// this test complements at the schema-migration layer.
const wantNativeSchemaVersion = 65

// currentSchemaMigrationVersion reads beads' own migration cursor
// (SELECT COALESCE(MAX(version), 0) FROM schema_migrations), mirroring
// upstream's internal/storage/schema.CurrentVersion query. gascity cannot
// import that internal package, so this restates the same query against the
// raw *sql.DB the native store already exposes for tests
// (testRawDBGetter.DB(), used by TestNativeDoltStoreOpenPreservesMissingIDDefaults).
func currentSchemaMigrationVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	return version
}

// TestNativeDoltStoreFreshOpenReachesSchemaV65 proves gascity#5349's
// "fresh-store" acceptance gate: a brand-new native store, opened through the
// pinned beads module, initializes at exactly the pinned schema version (65 —
// main-schema migrations 0060 through 0065) and supports a representative
// native read/write immediately, with no subprocess/CLI fallback involved.
func TestNativeDoltStoreFreshOpenReachesSchemaV65(t *testing.T) {
	ctx := context.Background()
	scopeRoot := t.TempDir()
	port := startTestDoltServer(t)
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
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
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
		t.Skip("storage does not expose a raw DB")
	}
	if got := currentSchemaMigrationVersion(t, accessor.DB()); got != wantNativeSchemaVersion {
		t.Fatalf("schema_migrations MAX(version) = %d after fresh open, want %d (pinned beads commit's main-schema migration ceiling)", got, wantNativeSchemaVersion)
	}

	store := newNativeDoltStoreWithStorageAndPrefix(storage, "schema-v65-fresh", "gc")
	bead, err := store.Create(Bead{Title: "schema v65 fresh-open bead"})
	if err != nil {
		t.Fatalf("Create bead on freshly-opened v65 store: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get bead on freshly-opened v65 store: %v", err)
	}
	if got.Title != "schema v65 fresh-open bead" {
		t.Fatalf("Title = %q, want %q", got.Title, "schema v65 fresh-open bead")
	}
}

// TestNativeDoltStoreReopenExistingSchemaV65NoSkewFallback proves
// gascity#5349's "existing-store open" acceptance gate: opening a store that
// already sits at schema v65 (a second, independent open against the same
// Dolt data, e.g. after a process restart) succeeds through the native path,
// does not re-run or roll back any migration, and preserves previously
// written data — i.e. no schema-skew fallback or downgrade is triggered by an
// already-current store.
func TestNativeDoltStoreReopenExistingSchemaV65NoSkewFallback(t *testing.T) {
	ctx := context.Background()
	scopeRoot := t.TempDir()
	port := startTestDoltServer(t)
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("create .beads directory: %v", err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	// First open: creates the store and migrates it to the pinned ceiling.
	first, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	if err := first.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		_ = first.Close()
		t.Fatalf("set issue prefix: %v", err)
	}
	firstStore := newNativeDoltStoreWithStorageAndPrefix(first, "schema-v65-reopen-seed", "gc")
	seeded, err := firstStore.Create(Bead{Title: "schema v65 pre-reopen bead"})
	if err != nil {
		_ = first.Close()
		t.Fatalf("Create seed bead before reopen: %v", err)
	}
	accessor, ok := first.(testRawDBGetter)
	if !ok {
		_ = first.Close()
		t.Skip("storage does not expose a raw DB")
	}
	beforeVersion := currentSchemaMigrationVersion(t, accessor.DB())
	if beforeVersion != wantNativeSchemaVersion {
		_ = first.Close()
		t.Fatalf("schema_migrations MAX(version) = %d before reopen, want %d", beforeVersion, wantNativeSchemaVersion)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first open: %v", err)
	}

	// Second, independent open against the SAME Dolt data (same scopeRoot,
	// same running server): the exact shape of an existing-store open the
	// issue's acceptance gate describes. It must succeed natively, must not
	// move the migration cursor, and must still see the first open's write.
	second, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Fatalf("reopen existing schema-v65 store: %v (this is the schema-skew-fallback failure mode the acceptance gate forbids)", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("close second open: %v", err)
		}
	})
	secondAccessor, ok := second.(testRawDBGetter)
	if !ok {
		t.Skip("storage does not expose a raw DB")
	}
	afterVersion := currentSchemaMigrationVersion(t, secondAccessor.DB())
	if afterVersion != wantNativeSchemaVersion {
		t.Fatalf("schema_migrations MAX(version) = %d after reopen, want unchanged %d (a moving cursor means the reopen re-migrated or skewed instead of recognizing an already-current store)", afterVersion, wantNativeSchemaVersion)
	}

	secondStore := newNativeDoltStoreWithStorageAndPrefix(second, "schema-v65-reopen-verify", "gc")
	got, err := secondStore.Get(seeded.ID)
	if err != nil {
		t.Fatalf("Get bead %s created before reopen: %v", seeded.ID, err)
	}
	if got.Title != "schema v65 pre-reopen bead" {
		t.Fatalf("Title after reopen = %q, want %q", got.Title, "schema v65 pre-reopen bead")
	}
	// A representative write through the reopened native path, proving the
	// second open is fully live (not a read-only or degraded fallback path).
	if _, err := secondStore.Create(Bead{Title: "schema v65 post-reopen bead"}); err != nil {
		t.Fatalf("Create bead after reopen: %v", err)
	}
}
