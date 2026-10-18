//go:build integration

package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// The revision a native list row carries, on real Dolt in server mode (the
// shape the native cities run): the same row_lock token Get publishes, kept by
// a primed cache, and fencing every conditional write, a legacy row's default
// 0 included.

// openServerNativeStoreForTest opens a native store over a throwaway dolt
// sql-server, with a raw handle on its database for the legacy-row setup.
func openServerNativeStoreForTest(tb testing.TB) (*NativeDoltStore, *sql.DB) {
	tb.Helper()
	ctx := context.Background()
	port := startTestDoltServer(tb)
	beadsDir := filepath.Join(tb.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		tb.Fatalf("create .beads directory: %v", err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		tb.Fatalf("write metadata.json: %v", err)
	}
	storage, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		tb.Skipf("upstream native beads storage unavailable: %v", err)
	}
	tb.Cleanup(func() { _ = storage.Close() })
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		tb.Fatalf("set issue prefix: %v", err)
	}
	accessor, ok := storage.(testRawDBGetter)
	if !ok {
		tb.Fatal("server-backed storage exposes no raw DB")
	}
	return newNativeDoltStoreWithStorageAndPrefix(storage, "list-revision", "gc"), accessor.DB()
}

// makeLegacyRow resets id's row_lock to the default migration 0054 backfilled:
// a row no write has touched since.
func makeLegacyRow(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec("UPDATE issues SET row_lock = 0 WHERE id = ?", id); err != nil {
		t.Fatalf("reset row_lock on %s: %v", id, err)
	}
}

func revisionsByID(t *testing.T, rows []Bead) map[string]int64 {
	t.Helper()
	out := make(map[string]int64, len(rows))
	for _, b := range rows {
		out[b.ID] = b.Revision
	}
	return out
}

// Kills beadFromNativeIssueRow clearing the revision: every listed row, a
// written one and a legacy one included, carries the revision Get returns.
func TestNativeDoltStoreListCarriesTheGetRevision(t *testing.T) {
	store, db := openServerNativeStoreForTest(t)
	var ids []string
	for i := 0; i < 3; i++ {
		b, err := store.Create(Bead{Title: fmt.Sprintf("row %d", i)})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, b.ID)
	}
	if err := store.Update(ids[1], UpdateOpts{Assignee: stringPtrForListRevisionTest("worker")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	makeLegacyRow(t, db, ids[2])

	rows, err := store.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	listed := revisionsByID(t, rows)
	for i, id := range ids {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if listed[id] != got.Revision {
			t.Errorf("row %d (%s): List revision %d, Get revision %d", i, id, listed[id], got.Revision)
		}
		if legacy := i == 2; legacy != (got.Revision == 0) {
			t.Errorf("row %d (%s): Get revision %d; only the legacy row is at 0", i, id, got.Revision)
		}
	}
}

// Kills a cache that drops the revision it primed with: after Prime, the
// cached List and the cached Get carry the backing's revision.
func TestNativeDoltStorePrimedCacheKeepsTheRevision(t *testing.T) {
	store, _ := openServerNativeStoreForTest(t)
	created, err := store.Create(Bead{Title: "primed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want, err := store.Get(created.ID)
	if err != nil || want.Revision == 0 {
		t.Fatalf("backing Get = (%d, %v), want a real revision", want.Revision, err)
	}
	cache := NewCachingStoreForTest(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	rows, err := cache.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("cached List: %v", err)
	}
	if got := revisionsByID(t, rows)[created.ID]; got != want.Revision {
		t.Errorf("cached List revision %d, want %d", got, want.Revision)
	}
	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("cached Get: %v", err)
	}
	if got.Revision != want.Revision {
		t.Errorf("cached Get revision %d, want %d", got.Revision, want.Revision)
	}
}

// Kills a native conditional write that does not compare: a write fenced on
// a listed revision another writer has since moved is refused and changes
// nothing.
func TestNativeDoltStoreConditionalWriteRefusesAStaleListedRevision(t *testing.T) {
	store, _ := openServerNativeStoreForTest(t)
	created, err := store.Create(Bead{Title: "contended"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rows, err := store.List(ListQuery{IDs: []string{created.ID}})
	if err != nil || len(rows) != 1 || rows[0].Revision == 0 {
		t.Fatalf("List = (%+v, %v), want the row at a real revision", rows, err)
	}
	stale := rows[0].Revision
	if err := store.Update(created.ID, UpdateOpts{Assignee: stringPtrForListRevisionTest("winner")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	err = store.UpdateIfMatch(created.ID, stale, UpdateOpts{Assignee: stringPtrForListRevisionTest("loser")})
	var pfe *PreconditionFailedError
	if !errors.As(err, &pfe) {
		t.Fatalf("UpdateIfMatch at the stale listed revision = %v, want a precondition failure", err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Assignee != "winner" {
		t.Fatalf("assignee = %q after the refused write, want winner", got.Assignee)
	}
}

// Kills a 0 that fences nothing: a legacy row's default 0 is a real revision.
// A write fenced on it lands once and moves the row off 0, so a second write
// at 0 is refused, and so is a write at 0 after another writer touched it.
func TestNativeDoltStoreLegacyZeroRevisionIsAFence(t *testing.T) {
	store, db := openServerNativeStoreForTest(t)
	first, err := store.Create(Bead{Title: "legacy, first touch"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	makeLegacyRow(t, db, first.ID)
	rows, err := store.List(ListQuery{IDs: []string{first.ID}})
	if err != nil || len(rows) != 1 || rows[0].Revision != 0 {
		t.Fatalf("List = (%+v, %v), want the legacy row at 0", rows, err)
	}
	if err := store.UpdateIfMatch(first.ID, 0, UpdateOpts{Assignee: stringPtrForListRevisionTest("a")}); err != nil {
		t.Fatalf("first UpdateIfMatch at 0: %v", err)
	}
	after, err := store.Get(first.ID)
	if err != nil || after.Revision == 0 {
		t.Fatalf("Get after the fenced write = (%d, %v), want the row off 0", after.Revision, err)
	}
	var pfe *PreconditionFailedError
	if err := store.UpdateIfMatch(first.ID, 0, UpdateOpts{Assignee: stringPtrForListRevisionTest("b")}); !errors.As(err, &pfe) {
		t.Fatalf("second UpdateIfMatch at 0 = %v, want a precondition failure", err)
	}

	moved, err := store.Create(Bead{Title: "legacy, moved"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	makeLegacyRow(t, db, moved.ID)
	if err := store.Update(moved.ID, UpdateOpts{Assignee: stringPtrForListRevisionTest("other")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := store.CloseIfMatch(moved.ID, 0); !errors.As(err, &pfe) {
		t.Fatalf("CloseIfMatch at 0 after another write = %v, want a precondition failure", err)
	}
	got, err := store.Get(moved.ID)
	if err != nil || got.Status == "closed" {
		t.Fatalf("Get = (%s, %v), want the moved row left open", got.Status, err)
	}
}

// C0.7's revision-fidelity check admits real native Dolt, bare and under the
// controller's cache, with a written and a legacy row present.
func TestListRevisionFidelityAdmitsRealNativeDolt(t *testing.T) {
	store, db := openServerNativeStoreForTest(t)
	written, err := store.Create(Bead{Title: "written"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	legacy, err := store.Create(Bead{Title: "legacy"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	makeLegacyRow(t, db, legacy.ID)
	if err := store.Update(written.ID, UpdateOpts{Assignee: stringPtrForListRevisionTest("worker")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	cache := NewCachingStoreForTest(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	for name, s := range map[string]Store{"native": store, "cached native": cache} {
		if err := CheckListRevisionFidelity(s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// An id held in both the issues and the wisps table is two rows: the native
// List reads the wisps one and Get the issues one, so a legacy wisps twin at
// 0 lists at 0 while it reads live at a real revision. That is the row's own
// anomaly, not a read path that drops revisions, so beside a row that lists
// its revision it is no refusal. Kills a check that refuses on one row.
func TestListRevisionFidelityAdmitsAnIDInBothTables(t *testing.T) {
	store, db := openServerNativeStoreForTest(t)
	if _, err := store.Create(Bead{Title: "plain"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	written, err := store.Create(Bead{Title: "twin"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cols := sharedColumnsForTest(t, db, "issues", "wisps")
	if _, err := db.Exec("INSERT INTO wisps ("+cols+") SELECT "+cols+" FROM issues WHERE id = ?", written.ID); err != nil {
		t.Fatalf("plant the wisps twin: %v", err)
	}
	if _, err := db.Exec("UPDATE wisps SET row_lock = 0 WHERE id = ?", written.ID); err != nil {
		t.Fatalf("reset the twin's row_lock: %v", err)
	}
	got, err := store.Get(written.ID)
	if err != nil || got.Revision == 0 {
		t.Fatalf("Get = (%d, %v), want the issues row at a real revision", got.Revision, err)
	}
	both, err := store.List(ListQuery{IDs: []string{written.ID}, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List both tiers: %v", err)
	}
	if len(both) != 1 || both[0].Revision != 0 {
		t.Fatalf("List = %+v, want the one wisps twin at 0; the fixture no longer shows the anomaly", revisionsByID(t, both))
	}
	cache := NewCachingStoreForTest(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	for name, s := range map[string]Store{"native": store, "cached native": cache} {
		if err := CheckListRevisionFidelity(s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// sharedColumnsForTest lists, quoted and comma-joined, the columns tables a
// and b both have.
func sharedColumnsForTest(t *testing.T, db *sql.DB, a, b string) string {
	t.Helper()
	rows, err := db.Query(`SELECT c1.column_name FROM information_schema.columns c1
		JOIN information_schema.columns c2 ON c2.table_schema = c1.table_schema AND c2.column_name = c1.column_name AND c2.table_name = ?
		WHERE c1.table_schema = DATABASE() AND c1.table_name = ? ORDER BY c1.ordinal_position`, b, a)
	if err != nil {
		t.Fatalf("read the shared columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan a column: %v", err)
		}
		cols = append(cols, "`"+c+"`")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the shared columns: %v", err)
	}
	return strings.Join(cols, ", ")
}

func stringPtrForListRevisionTest(s string) *string { return &s }

// BenchmarkNativeDoltStoreListRevision measures a full-scan List on real Dolt,
// for the read cost of carrying the row's revision on every list row.
func BenchmarkNativeDoltStoreListRevision(b *testing.B) {
	store, _ := openServerNativeStoreForTest(b)
	for i := 0; i < 500; i++ {
		if _, err := store.Create(Bead{Title: fmt.Sprintf("bench row %d", i)}); err != nil {
			b.Fatalf("Create: %v", err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		rows, err := store.List(ListQuery{AllowScan: true})
		if err != nil || len(rows) != 500 {
			b.Fatalf("List = (%d rows, %v)", len(rows), err)
		}
	}
}
