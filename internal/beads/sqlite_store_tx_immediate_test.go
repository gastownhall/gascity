package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// openSQLiteProbe opens an independent handle on the store file standing in
// for another gc process. busy_timeout is zero so a held write lock is observed
// immediately instead of waited out, which keeps the tests below deterministic.
func openSQLiteProbe(t *testing.T, path string, txlock string) *sql.DB {
	t.Helper()
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(0)")
	if txlock != "" {
		query.Set("_txlock", txlock)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("opening probe handle: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func probeWrite(ctx context.Context, db *sql.DB, key string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `INSERT INTO kv(key,value) VALUES(?, 'x')`, key); err != nil {
		return err
	}
	return tx.Commit()
}

func openSQLiteStoreForTest(t *testing.T, dir string) *SQLiteStore {
	t.Helper()
	opened, err := OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

// TestSQLiteDeferredWriteUpgradeFailsWithBusySnapshot pins the root cause the
// write handle's BEGIN IMMEDIATE exists for, on the real driver: a deferred
// transaction that has read, and then loses a commit race to another
// connection, cannot upgrade to a writer. SQLite reports SQLITE_BUSY_SNAPSHOT
// (517) at once, busy_timeout notwithstanding, and isSQLiteBusy must classify
// the driver's actual error as retryable.
func TestSQLiteDeferredWriteUpgradeFailsWithBusySnapshot(t *testing.T) {
	dir := t.TempDir()
	store := openSQLiteStoreForTest(t, dir)
	ctx := context.Background()

	// Both handles use the read-pool (deferred) DSN, with the production
	// busy_timeout, to show the timeout does not rescue the upgrade.
	stale, err := sql.Open("sqlite", sqliteStoreDSN(store.path, false))
	if err != nil {
		t.Fatalf("opening stale handle: %v", err)
	}
	defer stale.Close() //nolint:errcheck
	other := openSQLiteProbe(t, store.path, "")

	tx, err := stale.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin deferred tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM beads`).Scan(&n); err != nil {
		t.Fatalf("read establishing snapshot: %v", err)
	}
	if err := probeWrite(ctx, other, "other-process-write"); err != nil {
		t.Fatalf("concurrent writer commit: %v", err)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM kv WHERE key=?`, sqliteClaimFenceKey("gc-1"))
	if err == nil {
		t.Fatal("stale deferred transaction upgraded to a writer; expected SQLITE_BUSY_SNAPSHOT")
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_BUSY_SNAPSHOT {
		t.Fatalf("upgrade error = %v, want SQLITE_BUSY_SNAPSHOT (517)", err)
	}
	if !isSQLiteBusy(err) {
		t.Fatalf("isSQLiteBusy(%q) = false, want true", err)
	}
	if !isSQLiteBusy(fmt.Errorf("clearing claim fence for %q: %w", "gcg-1", err)) {
		t.Fatalf("isSQLiteBusy does not see through wrapping of %q", err)
	}
}

// TestSQLiteStoreTxTakesWriteLockAtBegin is the regression test for mc-u5vhb.
// Another process commits a write after Tx's callback has read but before it
// writes — the interleaving that failed in production with "clearing claim
// fence ...: database is locked (517)" during concurrent `gc sling`. With a
// deferred BEGIN the other write lands and the callback's Create then fails
// with SQLITE_BUSY_SNAPSHOT. With BEGIN IMMEDIATE the other process cannot
// write at all until Tx commits, so the callback's reads stay current and the
// transaction commits.
func TestSQLiteStoreTxTakesWriteLockAtBegin(t *testing.T) {
	dir := t.TempDir()
	store := openSQLiteStoreForTest(t, dir)
	peer := openSQLiteStoreForTest(t, dir) // an independent handle, as a second gc process has
	otherProcess := openSQLiteProbe(t, store.path, "immediate")
	ctx := context.Background()

	calls := 0
	var probeErr error
	var created Bead
	err := store.Tx("regression: mc-u5vhb", func(tx Tx) error {
		calls++
		// Read first. On a deferred transaction this opens the read snapshot.
		if err := tx.Update("gc-does-not-exist", UpdateOpts{}); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("priming read: got %w, want ErrNotFound", err)
		}
		if calls == 1 {
			probeErr = probeWrite(ctx, otherProcess, "other-process-write")
		}
		var err error
		created, err = tx.Create(Bead{Title: "created under contention"})
		return err
	})
	if err != nil {
		t.Fatalf("Tx failed under cross-process write contention: %v", err)
	}
	if calls != 1 {
		t.Errorf("callback ran %d times, want 1: the write lock should be held from BEGIN", calls)
	}
	if probeErr == nil {
		t.Fatal("another process committed a write inside Tx: the write handle is not beginning transactions IMMEDIATE")
	}
	if !isSQLiteBusy(probeErr) {
		t.Fatalf("other process write error = %v, want SQLITE_BUSY", probeErr)
	}

	// The lock is released at commit: the other process can write now, and
	// sees the committed bead.
	if err := probeWrite(ctx, otherProcess, "other-process-write"); err != nil {
		t.Fatalf("other process write after Tx committed: %v", err)
	}
	if _, err := peer.Get(created.ID); err != nil {
		t.Fatalf("peer handle Get(%s) after commit: %v", created.ID, err)
	}
}

// TestSQLiteStoreTxRetriesWholeTransactionOnBusy covers the retry wrapper: a
// busy failure after the callback has written rolls the attempt back and re-runs
// the callback in a fresh transaction. The rolled-back attempt's auto-minted id
// is not reused and does not exist.
func TestSQLiteStoreTxRetriesWholeTransactionOnBusy(t *testing.T) {
	store := openSQLiteStoreForTest(t, t.TempDir())

	calls := 0
	var ids []string
	err := store.Tx("retry", func(tx Tx) error {
		calls++
		b, err := tx.Create(Bead{Title: fmt.Sprintf("attempt %d", calls)})
		if err != nil {
			return err
		}
		ids = append(ids, b.ID)
		if calls == 1 {
			return fmt.Errorf("clearing claim fence for %q: database is locked (517)", b.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if calls != 2 || len(ids) != 2 {
		t.Fatalf("callback ran %d times minting %v, want 2", calls, ids)
	}
	if ids[0] == ids[1] {
		t.Fatalf("retry reused rolled-back id %s", ids[0])
	}
	if _, err := store.Get(ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(%s) from the rolled-back attempt = %v, want ErrNotFound", ids[0], err)
	}
	if got, err := store.Get(ids[1]); err != nil || got.Title != "attempt 2" {
		t.Fatalf("Get(%s) = %+v, %v; want the committed attempt", ids[1], got, err)
	}
}

// TestCachingStoreTxRefreshesOnlyCommittedAttempt covers CachingStore.Tx over
// a backing store that re-runs the callback after a busy failure: touched-id
// tracking restarts each attempt, so only the committed attempt's ids are
// refreshed into the cache. Refreshing the rolled-back attempt's id would find
// nothing in the backing store and mark it dirty as a cache problem.
func TestCachingStoreTxRefreshesOnlyCommittedAttempt(t *testing.T) {
	store := openSQLiteStoreForTest(t, t.TempDir())
	cache := NewCachingStoreForTest(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	calls := 0
	var ids []string
	err := cache.Tx("retry through cache", func(tx Tx) error {
		calls++
		b, err := tx.Create(Bead{Title: fmt.Sprintf("attempt %d", calls)})
		if err != nil {
			return err
		}
		ids = append(ids, b.ID)
		if calls == 1 {
			return fmt.Errorf("clearing claim fence for %q: database is locked (517)", b.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if calls != 2 || len(ids) != 2 {
		t.Fatalf("callback ran %d times minting %v, want 2", calls, ids)
	}

	if got, err := cache.cachedGetOnly(ids[1]); err != nil || got.Title != "attempt 2" {
		t.Fatalf("cachedGetOnly(%s) = %+v, %v; want the committed attempt", ids[1], got, err)
	}
	if _, err := cache.cachedGetOnly(ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cachedGetOnly(%s) from the rolled-back attempt = %v, want ErrNotFound", ids[0], err)
	}
	if _, err := cache.Get(ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(%s) from the rolled-back attempt = %v, want ErrNotFound", ids[0], err)
	}
	if stats := cache.Stats(); stats.ProblemCount != 0 {
		t.Fatalf("cache recorded %d problems (last: %s), want 0", stats.ProblemCount, stats.LastProblem)
	}
}

func TestSQLiteStoreWriterDSNBeginsImmediate(t *testing.T) {
	path := filepath.Join("/city/.gc/store/graph", sqliteStoreFilename)
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{"writer", sqliteStoreDSNWithMode(path, "", true), "immediate"},
		{"private recovery writer", sqliteStoreDSNWithMode(path, sqliteStoreDSNPrivateRecoveryMode, true), "immediate"},
		{"read-only writer", sqliteStoreDSNWithMode(path, sqliteStoreDSNReadOnlyMode, true), ""},
		{"read pool", sqliteStoreDSN(path, false), ""},
		{"read-only read pool", sqliteStoreDSN(path, true), ""},
		{"private recovery read pool", sqliteStorePrivateRecoveryDSN(path), ""},
	}
	for _, tc := range cases {
		parsed, err := url.Parse(tc.dsn)
		if err != nil {
			t.Fatalf("%s: parsing %q: %v", tc.name, tc.dsn, err)
		}
		if got := parsed.Query().Get("_txlock"); got != tc.want {
			t.Errorf("%s: _txlock = %q, want %q (DSN %s)", tc.name, got, tc.want, tc.dsn)
		}
	}
}
