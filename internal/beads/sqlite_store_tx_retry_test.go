package beads

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

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

// holdSQLiteWriteLock takes store's write lock from a second connection, as a
// writer in another process would, and returns the function that releases it.
// It also zeroes the store's own busy_timeout, so a contended BEGIN reports
// SQLITE_BUSY at once instead of after five seconds.
func holdSQLiteWriteLock(t *testing.T, store *SQLiteStore) (release func()) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		t.Fatalf("zeroing the store's busy_timeout: %v", err)
	}
	other, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: store.path, RawQuery: "_pragma=busy_timeout(0)&_txlock=immediate"}).String())
	if err != nil {
		t.Fatalf("open competing connection: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	held, err := other.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("competing BEGIN IMMEDIATE: %v", err)
	}
	release = func() { _ = held.Rollback() }
	t.Cleanup(release)
	return release
}

// stubSQLiteBusySleep replaces the busy-retry backoff for one test, running
// onSleep (when non-nil) in place of each sleep, and returns the sleep count.
func stubSQLiteBusySleep(t *testing.T, onSleep func()) *int {
	t.Helper()
	sleeps := 0
	prev := sqliteBusySleep
	sqliteBusySleep = func(time.Duration) {
		sleeps++
		if onSleep != nil {
			onSleep()
		}
	}
	t.Cleanup(func() { sqliteBusySleep = prev })
	return &sleeps
}

// TestSQLiteStoreTxRetriesAContendedBegin covers the contention Tx retries.
// Another writer holds the write lock, so BEGIN IMMEDIATE reports SQLITE_BUSY
// before fn runs; Tx backs off and begins again, like every other write path,
// and fn runs once, in the transaction that commits.
func TestSQLiteStoreTxRetriesAContendedBegin(t *testing.T) {
	store := openSQLiteStoreForTest(t, t.TempDir())
	release := holdSQLiteWriteLock(t, store)
	sleeps := stubSQLiteBusySleep(t, release)

	calls := 0
	var created Bead
	err := store.Tx("contended begin", func(tx Tx) error {
		calls++
		var err error
		created, err = tx.Create(Bead{Title: "written after the wait"})
		return err
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if calls != 1 || *sleeps != 1 {
		t.Fatalf("callback ran %d times after %d backoff sleeps, want 1 and 1", calls, *sleeps)
	}
	if _, err := store.Get(created.ID); err != nil {
		t.Fatalf("Get(%s) after the retried Tx: %v", created.ID, err)
	}
}

// TestSQLiteStoreTxReportsAnExhaustedBeginAsBusyExhausted covers a write lock
// held through every retry: fn never runs, and the error matches
// ErrSQLiteBusyExhausted, the mark of a write that committed nothing.
func TestSQLiteStoreTxReportsAnExhaustedBeginAsBusyExhausted(t *testing.T) {
	store := openSQLiteStoreForTest(t, t.TempDir())
	holdSQLiteWriteLock(t, store)
	sleeps := stubSQLiteBusySleep(t, nil)

	calls := 0
	err := store.Tx("held lock", func(Tx) error {
		calls++
		return nil
	})
	if !errors.Is(err, ErrSQLiteBusyExhausted) || !strings.Contains(err.Error(), "sqlite tx: begin") {
		t.Fatalf("Tx under a held write lock = %v, want ErrSQLiteBusyExhausted from the begin", err)
	}
	if calls != 0 || *sleeps != sqliteBusyRetryAttempts {
		t.Fatalf("callback ran %d times after %d backoff sleeps, want 0 and %d", calls, *sleeps, sqliteBusyRetryAttempts)
	}
}

// TestSQLiteStoreTxRunsTheCallbackAtMostOnce covers the other half of the
// retry: only a contended begin, which fails before fn runs, is retried. An
// error from fn, even one that reads as SQLITE_BUSY, rolls the transaction back
// and is returned as fn's own, without running fn again: fn may have acted
// outside tx, or decided on a read taken before Tx was called.
func TestSQLiteStoreTxRunsTheCallbackAtMostOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		busy bool
	}{
		{name: "plain error", err: errors.New("callback refused")},
		{name: "busy error", err: errors.New("clearing claim fence: database is locked (5) (SQLITE_BUSY)"), busy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if isSQLiteBusy(tc.err) != tc.busy {
				t.Fatalf("isSQLiteBusy(%v) = %v, want %v", tc.err, !tc.busy, tc.busy)
			}
			store := openSQLiteStoreForTest(t, t.TempDir())
			sleeps := stubSQLiteBusySleep(t, nil)

			calls := 0
			var created Bead
			err := store.Tx("failing callback", func(tx Tx) error {
				calls++
				var err error
				if created, err = tx.Create(Bead{Title: "rolled back"}); err != nil {
					return err
				}
				return tc.err
			})
			if calls != 1 || *sleeps != 0 {
				t.Fatalf("callback ran %d times after %d backoff sleeps, want 1 and 0", calls, *sleeps)
			}
			if !errors.Is(err, tc.err) || errors.Is(err, ErrSQLiteBusyExhausted) {
				t.Fatalf("Tx = %v, want the callback's own error, unmarked", err)
			}
			if _, err := store.Get(created.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get(%s) after the failed Tx = %v, want ErrNotFound", created.ID, err)
			}
		})
	}
}
