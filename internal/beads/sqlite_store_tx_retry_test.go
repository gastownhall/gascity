package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
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

// TestCachingStoreTxRefreshesOnlyTheCommittedAttempt covers CachingStore.Tx
// over a backing store that re-runs the callback after a busy rollback. The
// rolled-back attempt's id never existed in the backing store, so refreshing
// it after commit is wrong: it reports a spurious "tx refresh" not-found
// problem. Only the committed attempt's ids may be refreshed.
func TestCachingStoreTxRefreshesOnlyTheCommittedAttempt(t *testing.T) {
	store := openSQLiteStoreForTest(t, t.TempDir())
	cache := NewCachingStore(store, func(ChangeSource, string, string, string, string, string, *[]string, json.RawMessage) {})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	calls := 0
	var ids []string
	err := cache.Tx("retry", func(tx Tx) error {
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
	if calls != 2 {
		t.Fatalf("callback ran %d times, want 2", calls)
	}
	if stats := cache.Stats(); stats.ProblemCount != 0 {
		t.Fatalf("cache recorded %d problem(s) after a retried Tx, last %q; want none", stats.ProblemCount, stats.LastProblem)
	}
	if got, err := cache.Get(ids[1]); err != nil || got.Title != "attempt 2" {
		t.Fatalf("cache.Get(%s) = %+v, %v; want the committed attempt", ids[1], got, err)
	}
	if _, err := cache.Get(ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cache.Get(%s) from the rolled-back attempt = %v, want ErrNotFound", ids[0], err)
	}
}
