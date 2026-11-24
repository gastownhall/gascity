package beads

import (
	"errors"
	"testing"
)

// The tests in this file pin the close a conditional write's refetch finds:
// the write evicts the row it holds and installs the refetched one, and when
// the evicted row was open and the refetched one is closed, an out-of-band
// close landed in between. The install is the close of a row the cache held
// open, so it is queued and announced as a read that saw it would announce it
// (gastownhall/gascity#6860), with or without a ledger feed.

func TestCachingStoreConditionalWriteQueuesTheCloseOfARowHeldOpen(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		write func(cache *CachingStore, backing Store, id string) error
	}{
		{name: "compare_and_set_metadata_key", write: func(cache *CachingStore, _ Store, id string) error {
			swapped, err := cache.CompareAndSetMetadataKey(id, "gc.note", "", "x")
			if err == nil && !swapped {
				return errors.New("CompareAndSetMetadataKey lost")
			}
			return err
		}},
		{name: "update_if_match", write: func(cache *CachingStore, backing Store, id string) error {
			// The caller holds the row's current revision, read live.
			row, err := backing.Get(id)
			if err != nil {
				return err
			}
			return cache.UpdateIfMatch(id, row.Revision, UpdateOpts{Metadata: map[string]string{"gc.note": "x"}})
		}},
		{name: "set_metadata_control", write: func(cache *CachingStore, _ Store, id string) error {
			return cache.SetMetadata(id, "gc.note", "x")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, rec := newReportCacheForTest(t, backing)
			id := mustCreateCached(t, cache).ID
			rec.take()
			if err := backing.Close(id); err != nil {
				t.Fatalf("backing Close: %v", err)
			}
			if err := tc.write(cache, backing, id); err != nil {
				t.Fatalf("write: %v", err)
			}
			if row, held, _ := cachedRowState(cache, id); !held || row.Status != "closed" {
				t.Fatalf("cached row = %+v held %v, want the refetched closed row", row, held)
			}
			afterWrite := rec.take()
			if n := closedCount(afterWrite, id); n != 1 {
				t.Errorf("bead.closed for %s after the write = %d (%q), want 1", id, n, afterWrite)
			}
			// Neither the feed's refresh nor a re-scan announces it again.
			if _, err := cache.RefreshRowReport(id, feedRefreshOpts); err != nil {
				t.Fatalf("RefreshRowReport: %v", err)
			}
			cache.ReconcileNowForTest()
			if n := closedCount(rec.take(), id); n != 0 {
				t.Errorf("bead.closed for %s after the refresh and re-scan = %d, want 0", id, n)
			}
		})
	}
}

// A stale conditional write fails and evicts the held open row; the caller's
// re-read installs the closed row over no cached row, and its retry evicts and
// refetches it again. No install there saw the open row, so the close is left
// to the ledger feed's refresh (silentCloses), which announces it once.
func TestCachingStoreStaleConditionalRetryLeavesTheCloseToTheRefresh(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache, rec := newReportCacheForTest(t, backing)
	row := mustCreateCached(t, cache)
	id := row.ID
	rec.take()
	if err := backing.Close(id); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	err := cache.UpdateIfMatch(id, row.Revision, UpdateOpts{Metadata: map[string]string{"gc.note": "x"}})
	if !IsPreconditionFailed(err) {
		t.Fatalf("UpdateIfMatch on the stale revision = %v, want a precondition failure", err)
	}
	reread, err := cache.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := cache.UpdateIfMatch(id, reread.Revision, UpdateOpts{Metadata: map[string]string{"gc.note": "x"}}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := cache.RefreshRowReport(id, feedRefreshOpts); err != nil {
		t.Fatalf("RefreshRowReport: %v", err)
	}
	cache.ReconcileNowForTest()
	if n := closedCount(rec.take(), id); n != 1 {
		t.Errorf("bead.closed for %s = %d, want 1", id, n)
	}
}

// A closing conditional write claims its own close before it evicts, so the
// row its refetch installs is not a silently installed close, even when the
// cache never held the row.
func TestCachingStoreOwnConditionalCloseIsNotASilentClose(t *testing.T) {
	t.Parallel()

	closed := "closed"
	for _, tc := range []struct {
		name  string
		close func(cache *CachingStore, id string, revision int64) error
	}{
		{name: "close_if_match", close: func(cache *CachingStore, id string, revision int64) error {
			return cache.CloseIfMatch(id, revision)
		}},
		{name: "update_if_match_closed", close: func(cache *CachingStore, id string, revision int64) error {
			return cache.UpdateIfMatch(id, revision, UpdateOpts{Status: &closed})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, rec := newReportCacheForTest(t, backing)
			row, err := backing.Create(Bead{Title: "never cached"})
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.close(cache, row.ID, row.Revision); err != nil {
				t.Fatalf("close: %v", err)
			}
			if got, held, _ := cachedRowState(cache, row.ID); !held || got.Status != "closed" {
				t.Fatalf("cached row = %+v held %v, want the refetched closed row", got, held)
			}
			if n := silentCloseCount(cache); n != 0 {
				t.Errorf("silent closes = %d, want 0: the write announced its close", n)
			}
			if _, err := cache.RefreshRowReport(row.ID, feedRefreshOpts); err != nil {
				t.Fatalf("RefreshRowReport: %v", err)
			}
			if n := closedCount(rec.take(), row.ID); n != 1 {
				t.Errorf("bead.closed for %s = %d, want 1", row.ID, n)
			}
		})
	}
}
