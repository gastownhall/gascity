package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests in this file pin the close of a row the cache never held, closed
// out of band and then installed by a path that announces no close: a write
// that only refreshes the row, a conditional write's refetch, an applied event
// for an id the cache does not hold, or a list read that includes closed rows.
// The install holds the row closed, but nothing announced its close, so a
// refresh with evidence of a recent close (AnnounceUnheldClose, SeenOpen)
// treats the row as unheld and announces it, as it would had the row stayed
// uncached.

// feedRefreshOpts are the options the controller's ledger feed passes for a
// row its journal page reads as closing.
var feedRefreshOpts = RowRefreshOptions{AnnounceUnheldClose: true, YieldToLocalWrites: true}

// closedCount counts the bead.closed notifications for id in events.
func closedCount(events []string, id string) int {
	n := 0
	for _, e := range events {
		if strings.HasSuffix(e, " bead.closed "+id) {
			n++
		}
	}
	return n
}

// outOfBandClosedRow creates and closes a row behind the cache's back.
func outOfBandClosedRow(t *testing.T, backing Store, parentID string) Bead {
	t.Helper()
	row, err := backing.Create(Bead{Title: "out of process", ParentID: parentID})
	if err != nil {
		t.Fatalf("backing Create: %v", err)
	}
	if err := backing.Close(row.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	return row
}

func TestCachingStoreWriteOverANeverHeldCloseLeavesItToTheRefresh(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		write func(cache *CachingStore, id string) error
	}{
		{name: "set_metadata", write: func(cache *CachingStore, id string) error {
			return cache.SetMetadata(id, "gc.note", "x")
		}},
		{name: "set_metadata_batch", write: func(cache *CachingStore, id string) error {
			return cache.SetMetadataBatch(id, map[string]string{"gc.note": "x", "gc.n2": "y"})
		}},
		{name: "update_title", write: func(cache *CachingStore, id string) error {
			title := "renamed"
			return cache.Update(id, UpdateOpts{Title: &title})
		}},
		{name: "update_metadata", write: func(cache *CachingStore, id string) error {
			return cache.Update(id, UpdateOpts{Metadata: map[string]string{"gc.note": "x"}})
		}},
		{name: "compare_and_set_metadata_key", write: func(cache *CachingStore, id string) error {
			swapped, err := cache.CompareAndSetMetadataKey(id, "gc.note", "", "x")
			if err == nil && !swapped {
				return errors.New("CompareAndSetMetadataKey lost")
			}
			return err
		}},
		{name: "update_if_match", write: func(cache *CachingStore, id string) error {
			row, err := cache.Get(id)
			if err != nil {
				return err
			}
			return cache.UpdateIfMatch(id, row.Revision, UpdateOpts{Metadata: map[string]string{"gc.note": "x"}})
		}},
		{name: "close_all", write: func(cache *CachingStore, id string) error {
			_, err := cache.CloseAll([]string{id}, map[string]string{"gc.closed_by": "sweep"})
			return err
		}},
		{name: "set_metadata_then_close_all", write: func(cache *CachingStore, id string) error {
			if err := cache.SetMetadata(id, "gc.note", "x"); err != nil {
				return err
			}
			_, err := cache.CloseAll([]string{id}, map[string]string{"gc.closed_by": "sweep"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, rec := newReportCacheForTest(t, backing)
			id := outOfBandClosedRow(t, backing, "").ID
			if err := tc.write(cache, id); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, held, _ := cachedRowState(cache, id); !held {
				t.Fatal("the write did not install the closed row; the case needs it held")
			}
			rep, err := cache.RefreshRowReport(id, feedRefreshOpts)
			if err != nil {
				t.Fatalf("RefreshRowReport: %v", err)
			}
			if rep.Event != "bead.closed" || rep.Held || rep.Reclosed {
				t.Errorf("refresh = event %q held %v reclosed %v, want bead.closed of a row reported unheld, not reclosed", rep.Event, rep.Held, rep.Reclosed)
			}
			if n := closedCount(rec.take(), id); n != 1 {
				t.Errorf("bead.closed for %s = %d across the write and the refresh, want 1", id, n)
			}
			// The announced close is not announced again.
			if rep, err := cache.RefreshRowReport(id, feedRefreshOpts); err != nil || rep.Event != "" {
				t.Errorf("second refresh = %q, %v; want nothing", rep.Event, err)
			}
		})
	}
}

// CloseAll's own close of a row the cache never held is not announced by the
// write (its backing reports a count, not which ids it closed), so the closed
// row it installs stays marked silently installed and the feed's refresh
// announces it once.
func TestCachingStoreCloseAllOfANeverHeldOpenRowLeavesItToTheRefresh(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache, rec := newReportCacheForTest(t, backing)
	row, err := backing.Create(Bead{Title: "out of process, open"})
	if err != nil {
		t.Fatalf("backing Create: %v", err)
	}
	if _, err := cache.CloseAll([]string{row.ID}, nil); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if n := closedCount(rec.take(), row.ID); n != 0 {
		t.Fatalf("CloseAll announced %d bead.closed for a row it never held, want 0", n)
	}
	rep, err := cache.RefreshRowReport(row.ID, feedRefreshOpts)
	if err != nil {
		t.Fatalf("RefreshRowReport: %v", err)
	}
	if rep.Event != "bead.closed" || rep.Held {
		t.Errorf("refresh = event %q held %v, want bead.closed of a row reported unheld", rep.Event, rep.Held)
	}
	if n := closedCount(rec.take(), row.ID); n != 1 {
		t.Errorf("bead.closed for %s after the refresh = %d, want 1", row.ID, n)
	}
	if rep, err := cache.RefreshRowReport(row.ID, feedRefreshOpts); err != nil || rep.Event != "" {
		t.Errorf("second refresh = %q, %v; want nothing", rep.Event, err)
	}
}

func TestCachingStoreReadOverANeverHeldCloseLeavesItToTheRefresh(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		install func(t *testing.T, cache *CachingStore, row Bead)
	}{
		{name: "apply_event_created", install: func(t *testing.T, cache *CachingStore, row Bead) {
			open := row
			open.Status = "open"
			payload, err := json.Marshal(open)
			if err != nil {
				t.Fatal(err)
			}
			cache.ApplyEvent("bead.created", payload)
		}},
		{name: "list_children_include_closed", install: func(t *testing.T, cache *CachingStore, row Bead) {
			if _, err := cache.List(ListQuery{ParentID: row.ParentID, IncludeClosed: true}); err != nil {
				t.Fatalf("List: %v", err)
			}
		}},
		{name: "refresh_row", install: func(t *testing.T, cache *CachingStore, row Bead) {
			if _, err := cache.RefreshRow(row.ID); err != nil {
				t.Fatalf("RefreshRow: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, rec := newReportCacheForTest(t, backing)
			parent := mustCreateCached(t, cache)
			row := outOfBandClosedRow(t, backing, parent.ID)
			rec.take()
			tc.install(t, cache, row)
			if _, held, _ := cachedRowState(cache, row.ID); !held {
				t.Fatal("the install did not hold the closed row; the case needs it held")
			}
			rep, err := cache.RefreshRowReport(row.ID, feedRefreshOpts)
			if err != nil {
				t.Fatalf("RefreshRowReport: %v", err)
			}
			if rep.Event != "bead.closed" || rep.Held {
				t.Errorf("refresh = event %q held %v, want bead.closed of a row reported unheld", rep.Event, rep.Held)
			}
			if n := closedCount(rec.take(), row.ID); n != 1 {
				t.Errorf("bead.closed for %s = %d, want 1", row.ID, n)
			}
		})
	}
}

// A silently installed close stays the refresh's only with evidence of a
// recent close: without it a long-closed row a read installed is not
// announced, as an unheld one is not.
func TestCachingStoreSilentlyInstalledCloseNeedsCloseEvidence(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache, rec := newReportCacheForTest(t, backing)
	id := outOfBandClosedRow(t, backing, "").ID
	if err := cache.SetMetadata(id, "gc.note", "x"); err != nil {
		t.Fatal(err)
	}
	rec.take()
	rep, err := cache.RefreshRowReport(id, RowRefreshOptions{YieldToLocalWrites: true})
	if err != nil || rep.Event != "" || !rep.Held {
		t.Fatalf("refresh without close evidence = event %q held %v err %v, want nothing of a held row", rep.Event, rep.Held, err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("notified %q, want nothing", got)
	}
}

// A close the cache announced itself, or queued over a row it held open, is
// not left to the refresh: the install that announced it clears nothing the
// refresh could read as unannounced.
func TestCachingStoreAnnouncedCloseIsNotLeftToTheRefresh(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, cache *CachingStore, backing Store) string
	}{
		{name: "own_close", setup: func(t *testing.T, cache *CachingStore, _ Store) string {
			id := mustCreateCached(t, cache).ID
			if err := cache.Close(id); err != nil {
				t.Fatal(err)
			}
			if err := cache.SetMetadata(id, "gc.note", "x"); err != nil {
				t.Fatal(err)
			}
			return id
		}},
		{name: "queued_over_a_held_open_row", setup: func(t *testing.T, cache *CachingStore, backing Store) string {
			id := mustCreateCached(t, cache).ID
			if err := backing.Close(id); err != nil {
				t.Fatal(err)
			}
			if err := cache.SetMetadata(id, "gc.note", "x"); err != nil {
				t.Fatal(err)
			}
			return id
		}},
		{name: "announced_by_the_refresh_then_written", setup: func(t *testing.T, cache *CachingStore, backing Store) string {
			id := outOfBandClosedRow(t, backing, "").ID
			if _, err := cache.RefreshRowReport(id, feedRefreshOpts); err != nil {
				t.Fatal(err)
			}
			if err := cache.SetMetadata(id, "gc.note", "x"); err != nil {
				t.Fatal(err)
			}
			return id
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, rec := newReportCacheForTest(t, backing)
			id := tc.setup(t, cache, backing)
			rep, err := cache.RefreshRowReport(id, feedRefreshOpts)
			if err != nil {
				t.Fatalf("RefreshRowReport: %v", err)
			}
			if rep.Event != "" {
				t.Errorf("refresh announced %q, want nothing", rep.Event)
			}
			if n := closedCount(rec.take(), id); n != 1 {
				t.Errorf("bead.closed for %s = %d, want 1", id, n)
			}
		})
	}
}

// gatedSetMetadataStore holds SetMetadata after it commits until released,
// so a refresh can land while the write is in flight.
type gatedSetMetadataStore struct {
	*MemStore
	committed chan struct{}
	release   chan struct{}
}

func (s *gatedSetMetadataStore) SetMetadata(id, key, value string) error {
	err := s.MemStore.SetMetadata(id, key, value)
	if s.committed != nil {
		close(s.committed)
		<-s.release
	}
	return err
}

// The feed's shape: its refresh of the closing row lands while a metadata
// write of it is in flight and yields; its retry after the write finds the
// row the write installed and still announces the close.
func TestCachingStoreYieldedRefreshStillAnnouncesANeverHeldClose(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	backing := &gatedSetMetadataStore{MemStore: mem}
	cache, rec := newReportCacheForTest(t, backing)
	id := outOfBandClosedRow(t, mem, "").ID
	backing.committed, backing.release = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- cache.SetMetadata(id, "gc.note", "x") }()
	<-backing.committed
	_, first := cache.RefreshRowReport(id, feedRefreshOpts)
	close(backing.release)
	if err := <-done; err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if !errors.Is(first, ErrRowRefreshYielded) {
		t.Fatalf("refresh during the write = %v, want ErrRowRefreshYielded", first)
	}
	rep, err := cache.RefreshRowReport(id, feedRefreshOpts)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if rep.Event != "bead.closed" || rep.Held {
		t.Errorf("retry = event %q held %v, want bead.closed of a row reported unheld", rep.Event, rep.Held)
	}
	if n := closedCount(rec.take(), id); n != 1 {
		t.Errorf("bead.closed for %s = %d, want 1", id, n)
	}
}

// The record of silently installed closes holds only rows the cache holds
// closed: a re-scan, which holds no closed rows, leaves it empty.
func TestCachingStoreReconcileDropsSilentCloseMarks(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache, _ := newReportCacheForTest(t, backing)
	id := outOfBandClosedRow(t, backing, "").ID
	if err := cache.SetMetadata(id, "gc.note", "x"); err != nil {
		t.Fatal(err)
	}
	if n := silentCloseCount(cache); n != 1 {
		t.Fatalf("silent closes after the write = %d, want 1", n)
	}
	cache.ReconcileNowForTest()
	if n := silentCloseCount(cache); n != 0 {
		t.Fatalf("silent closes after a re-scan = %d, want 0", n)
	}
}

func silentCloseCount(cache *CachingStore) int {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return len(cache.silentCloses)
}

// A close of this cache's own that finds the row closed already, with the
// close announced and the row evicted since, announces nothing more: the
// backing close was a no-op, and announcedCloses shows the close went out.
func TestCachingStoreOwnCloseOfAnAnnouncedEvictedCloseIsQuiet(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		first func(t *testing.T, cache *CachingStore, id string)
	}{
		{name: "scan_then_close", first: func(_ *testing.T, cache *CachingStore, _ string) {
			cache.ReconcileNowForTest()
		}},
		{name: "refresh_then_scan_then_close", first: func(t *testing.T, cache *CachingStore, id string) {
			if _, err := cache.RefreshRowReport(id, feedRefreshOpts); err != nil {
				t.Fatal(err)
			}
			cache.ReconcileNowForTest()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, rec := newReportCacheForTest(t, backing)
			id := mustCreateCached(t, cache).ID
			rec.take()
			if err := backing.Close(id); err != nil {
				t.Fatal(err)
			}
			tc.first(t, cache, id)
			if err := cache.Close(id); err != nil {
				t.Fatal(err)
			}
			got := rec.take()
			if n := closedCount(got, id); n != 1 {
				t.Errorf("bead.closed for %s = %d (%q), want 1 for one close", id, n, got)
			}
		})
	}
}

// A closing write of a row the cache holds closed only silently owns its
// close, as it would an unheld row's: nobody announced it.
func TestCachingStoreClosingWriteOwnsASilentlyHeldClose(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache, rec := newReportCacheForTest(t, backing)
	id := outOfBandClosedRow(t, backing, "").ID
	if err := cache.SetMetadata(id, "gc.note", "x"); err != nil {
		t.Fatal(err)
	}
	row, err := backing.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.CloseIfMatch(id, row.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}
	if _, err := cache.RefreshRowReport(id, feedRefreshOpts); err != nil {
		t.Fatalf("RefreshRowReport: %v", err)
	}
	if n := closedCount(rec.take(), id); n != 1 {
		t.Errorf("bead.closed for %s = %d, want 1", id, n)
	}
}

// A close announced once stays announced after its announced-close record
// expires: a feed refresh past recentWriteVerifyWindow of a held closed row
// does not announce it again. Each case announces the close a different way,
// and each must clear the row's silent-install mark or claim the install.
func TestCachingStoreAnnouncedCloseStaysQuietPastTheWindow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, backing Store, cache *CachingStore) string
	}{
		{name: "refresh_announced_silently_held_row", setup: func(t *testing.T, backing Store, cache *CachingStore) string {
			id := outOfBandClosedRow(t, backing, "").ID
			if err := cache.SetMetadata(id, "gc.note", "x"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			if _, err := cache.RefreshRowReport(id, feedRefreshOpts); err != nil {
				t.Fatalf("RefreshRowReport: %v", err)
			}
			return id
		}},
		{name: "own_close_of_silently_held_row", setup: func(t *testing.T, backing Store, cache *CachingStore) string {
			id := outOfBandClosedRow(t, backing, "").ID
			if err := cache.SetMetadata(id, "gc.note", "x"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			if err := cache.Close(id); err != nil {
				t.Fatalf("Close: %v", err)
			}
			return id
		}},
		{name: "own_close_of_unheld_announced_row", setup: func(t *testing.T, backing Store, cache *CachingStore) string {
			// Announced by the feed while unheld, evicted by a re-scan, then
			// closed again by the cache.
			id := outOfBandClosedRow(t, backing, "").ID
			if _, err := cache.RefreshRowReport(id, feedRefreshOpts); err != nil {
				t.Fatalf("RefreshRowReport: %v", err)
			}
			cache.ReconcileNowForTest()
			if err := cache.Close(id); err != nil {
				t.Fatalf("Close: %v", err)
			}
			return id
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			now := time.Unix(1_800_000_000, 0)
			clock := func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return now
			}
			backing := &casBackingStore{Store: NewMemStore()}
			rec := &sourcedRecorder{}
			cache := NewCachingStore(backing, rec.record, WithClock(clock))
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			id := tc.setup(t, backing, cache)
			if n := closedCount(rec.take(), id); n != 1 {
				t.Fatalf("setup announced bead.closed for %s %d times, want 1", id, n)
			}
			mu.Lock()
			now = now.Add(recentWriteVerifyWindow + time.Second)
			mu.Unlock()
			rep, err := cache.RefreshRowReport(id, feedRefreshOpts)
			if err != nil {
				t.Fatalf("RefreshRowReport past the window: %v", err)
			}
			if n := closedCount(rec.take(), id); n != 0 || rep.Event != "" {
				t.Errorf("refresh past the window = event %q held %v, %d bead.closed; want nothing", rep.Event, rep.Held, n)
			}
		})
	}
}
