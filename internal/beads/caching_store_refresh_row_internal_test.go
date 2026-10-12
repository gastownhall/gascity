package beads

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The tests in this file pin RefreshRow, the per-row live refresh v2's lag
// repair needs (CONTRACT C5.15 as amended 2026-10-03 per P3-4 review): one
// backing read of a row, installed under the refetch fences whether the cached
// copy is clean, dirty or absent, with closed, not found and fenced reported
// apart.

// refreshRecorder collects the change notifications a cache emits, and the
// status the last one's payload carried.
type refreshRecorder struct {
	mu         sync.Mutex
	events     []string
	lastStatus string
}

func (r *refreshRecorder) record(eventType, beadID string, payload json.RawMessage) {
	var b struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(payload, &b)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, eventType+" "+beadID)
	r.lastStatus = b.Status
}

func (r *refreshRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

func newRefreshCacheForTest(t *testing.T, backing Store) (*CachingStore, *refreshRecorder) {
	t.Helper()
	rec := &refreshRecorder{}
	cache := NewCachingStoreForTest(backing, rec.record)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache, rec
}

// cachedRowState reads id's cached row and dirty mark.
func cachedRowState(cache *CachingStore, id string) (Bead, bool, bool) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	b, held := cache.beads[id]
	_, dirty := cache.dirty[id]
	return cloneBead(b), held, dirty
}

func assertEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("notifications = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("notifications = %q, want %q", got, want)
		}
	}
}

// TestCachingStoreRefreshRowInstalls refreshes a clean stale row, an uncached
// row and a dirty row: each read must install the backing row and notify as a
// re-scan would, and a row that did not change must notify nothing and leave
// the cache's freshness alone.
func TestCachingStoreRefreshRowInstalls(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// arrange leaves the backing row with k=v and the cache stale, dirty
		// or without it, and returns the row's id.
		arrange func(t *testing.T, cache *CachingStore, backing *casBackingStore) string
		want    string
	}{
		{"clean_stale", func(t *testing.T, cache *CachingStore, backing *casBackingStore) string {
			row := mustCreateCached(t, cache)
			mustSetBacking(t, backing, row.ID)
			if got, err := cache.Get(row.ID); err != nil || got.Metadata["k"] == "v" {
				t.Fatalf("Get = %v, %v; the clean row is not stale", got.Metadata, err)
			}
			return row.ID
		}, "bead.updated"},
		{"uncached", func(t *testing.T, _ *CachingStore, backing *casBackingStore) string {
			row, err := backing.Create(Bead{Title: "out of process", Metadata: map[string]string{"k": "v"}})
			if err != nil {
				t.Fatalf("backing Create: %v", err)
			}
			return row.ID
		}, "bead.created"},
		{"dirty", func(t *testing.T, cache *CachingStore, backing *casBackingStore) string {
			row := mustCreateCached(t, cache)
			mustSetBacking(t, backing, row.ID)
			cache.mu.Lock()
			cache.markDirtyLocked(row.ID)
			cache.mu.Unlock()
			return row.ID
		}, "bead.updated"},
		{"unchanged", func(t *testing.T, cache *CachingStore, _ *casBackingStore) string {
			row, err := cache.Create(Bead{Title: "seed", Metadata: map[string]string{"k": "v"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			return row.ID
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, rec := newRefreshCacheForTest(t, backing)
			id := tc.arrange(t, cache, backing)
			rec.take()
			reads := backing.getCalls
			freshAt := cache.Stats().LastFreshAt

			got, err := cache.RefreshRow(id)
			if err != nil {
				t.Fatalf("RefreshRow: %v", err)
			}
			if moved := !cache.Stats().LastFreshAt.Equal(freshAt); moved != (tc.want != "") {
				t.Fatalf("LastFreshAt moved = %v, want %v", moved, tc.want != "")
			}
			if backing.getCalls != reads+1 {
				t.Fatalf("RefreshRow made %d backing reads, want 1", backing.getCalls-reads)
			}
			if got.Metadata["k"] != "v" {
				t.Fatalf("RefreshRow returned k=%q, want v", got.Metadata["k"])
			}
			row, held, dirty := cachedRowState(cache, id)
			if !held || row.Metadata["k"] != "v" || dirty {
				t.Fatalf("cached row = %v held=%v dirty=%v, want the backing row installed clean", row.Metadata, held, dirty)
			}
			if tc.want == "" {
				assertEvents(t, rec.take())
			} else {
				assertEvents(t, rec.take(), tc.want+" "+id)
			}
			freshAt = cache.Stats().LastFreshAt
			if _, err := cache.RefreshRow(id); err != nil {
				t.Fatalf("second RefreshRow: %v", err)
			}
			assertEvents(t, rec.take())
			if !cache.Stats().LastFreshAt.Equal(freshAt) {
				t.Fatal("an unchanged refresh moved LastFreshAt")
			}
		})
	}
}

// TestCachingStoreRefreshRowFencedLosesToNewerLocalState races a local write,
// a local delete, an applied event and a local write against a not-found read
// into the refresh's backing read. The older read must not install over the newer state, and the
// caller must be told to retry.
func TestCachingStoreRefreshRowFencedLosesToNewerLocalState(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		race   func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string)
		verify func(t *testing.T, cache *CachingStore, id string)
	}{
		{"write", func(t *testing.T, cache *CachingStore, _ *casBackingStore, id string) {
			if err := cache.SetMetadata(id, "k", "newer"); err != nil {
				t.Errorf("SetMetadata: %v", err)
			}
		}, func(t *testing.T, cache *CachingStore, id string) {
			if row, held, _ := cachedRowState(cache, id); !held || row.Metadata["k"] != "newer" {
				t.Fatalf("cached row = %v held=%v, want the newer write", row.Metadata, held)
			}
		}},
		{"delete", func(t *testing.T, cache *CachingStore, _ *casBackingStore, id string) {
			if err := cache.Delete(id); err != nil {
				t.Errorf("Delete: %v", err)
			}
		}, func(t *testing.T, cache *CachingStore, id string) {
			if _, held, _ := cachedRowState(cache, id); held {
				t.Fatal("the refresh reinstalled a row deleted after its read")
			}
		}},
		{"event", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string) {
			if err := backing.SetMetadata(id, "k", "newer"); err != nil {
				t.Errorf("backing SetMetadata: %v", err)
			}
			b, err := backing.Store.Get(id)
			if err != nil {
				t.Errorf("backing Get: %v", err)
			}
			cache.ApplyEvent("bead.updated", eventPayload(t, b))
			if row, _, _ := cachedRowState(cache, id); row.Metadata["k"] != "newer" {
				t.Error("the event did not apply; the race is vacuous")
			}
		}, func(t *testing.T, cache *CachingStore, id string) {
			if row, held, _ := cachedRowState(cache, id); !held || row.Metadata["k"] != "newer" {
				t.Fatalf("cached row = %v held=%v, want the newer event", row.Metadata, held)
			}
		}},
		{"write_vs_not_found", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string) {
			if err := cache.SetMetadata(id, "k", "newer"); err != nil {
				t.Errorf("SetMetadata: %v", err)
			}
			backing.notFoundNextGet = true
		}, func(t *testing.T, cache *CachingStore, id string) {
			if row, held, _ := cachedRowState(cache, id); !held || row.Metadata["k"] != "newer" {
				t.Fatalf("cached row = %v held=%v, want the newer write kept over the proof of absence", row.Metadata, held)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, _ := newRefreshCacheForTest(t, backing)
			row := mustCreateCached(t, cache)
			// Settle the create, so only the raced state fences the read.
			ageLocalWrite(cache, row.ID)
			cache.ReconcileNowForTest()
			mustSetBacking(t, backing, row.ID)
			ran := false
			backing.onGetOnce = func() {
				ran = true
				tc.race(t, cache, backing, row.ID)
			}

			_, err := cache.RefreshRow(row.ID)
			if !ran {
				t.Fatal("the race hook did not run; the race is vacuous")
			}
			if !errors.Is(err, ErrRowRefreshFenced) || errors.Is(err, ErrNotFound) {
				t.Fatalf("RefreshRow err = %v, want ErrRowRefreshFenced only", err)
			}
			tc.verify(t, cache, row.ID)
		})
	}
}

// TestCachingStoreRefreshRowClosed closes a row out of process, cached and
// uncached: the refresh must report it closed, install it, and notify a close
// only for a row the cache held open.
func TestCachingStoreRefreshRowClosed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		cached bool
		want   []string
	}{
		{"cached_open", true, []string{"bead.closed"}},
		{"uncached", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, rec := newRefreshCacheForTest(t, backing)
			var id string
			if tc.cached {
				id = mustCreateCached(t, cache).ID
			} else {
				row, err := backing.Create(Bead{Title: "out of process"})
				if err != nil {
					t.Fatalf("backing Create: %v", err)
				}
				id = row.ID
			}
			if err := backing.Close(id); err != nil {
				t.Fatalf("backing Close: %v", err)
			}
			rec.take()

			got, err := cache.RefreshRow(id)
			if err != nil {
				t.Fatalf("RefreshRow: %v", err)
			}
			if got.Status != "closed" {
				t.Fatalf("RefreshRow status = %q, want closed", got.Status)
			}
			if row, held, _ := cachedRowState(cache, id); !held || row.Status != "closed" {
				t.Fatalf("cached row status=%q held=%v, want the closed row installed", row.Status, held)
			}
			want := make([]string, 0, len(tc.want))
			for _, e := range tc.want {
				want = append(want, e+" "+id)
			}
			assertEvents(t, rec.take(), want...)
		})
	}
}

// TestCachingStoreRefreshRowNotFound deletes a row out of process, cached open,
// cached closed, dirty or uncached: the refresh must report ErrNotFound, never
// as fenced, and drop a cached copy or mark, with the close a re-scan would
// emit for a row held open. A read error is neither.
func TestCachingStoreRefreshRowNotFound(t *testing.T) {
	t.Parallel()

	t.Run("cached", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, rec := newRefreshCacheForTest(t, backing)
		row := mustCreateCached(t, cache)
		if err := backing.Delete(row.ID); err != nil {
			t.Fatalf("backing Delete: %v", err)
		}
		rec.take()
		_, err := cache.RefreshRow(row.ID)
		if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrRowRefreshFenced) {
			t.Fatalf("RefreshRow err = %v, want ErrNotFound only", err)
		}
		if _, held, _ := cachedRowState(cache, row.ID); held {
			t.Fatal("the refresh kept a row the backing no longer has")
		}
		assertEvents(t, rec.take(), "bead.closed "+row.ID)
		if rec.lastStatus != "closed" {
			t.Fatalf("bead.closed payload status = %q, want closed", rec.lastStatus)
		}
	})
	t.Run("cached_closed", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, rec := newRefreshCacheForTest(t, backing)
		row := mustCreateCached(t, cache)
		if err := backing.Close(row.ID); err != nil {
			t.Fatalf("backing Close: %v", err)
		}
		if _, err := cache.RefreshRow(row.ID); err != nil {
			t.Fatalf("RefreshRow of the closed row: %v", err)
		}
		if err := backing.Delete(row.ID); err != nil {
			t.Fatalf("backing Delete: %v", err)
		}
		rec.take()
		if _, err := cache.RefreshRow(row.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("RefreshRow err = %v, want ErrNotFound", err)
		}
		if _, held, _ := cachedRowState(cache, row.ID); held {
			t.Fatal("the refresh kept a row the backing no longer has")
		}
		assertEvents(t, rec.take())
	})
	t.Run("dirty_uncached", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, rec := newRefreshCacheForTest(t, backing)
		cache.mu.Lock()
		cache.markDirtyLocked("gc-missing")
		cache.mu.Unlock()
		if _, err := cache.RefreshRow("gc-missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("RefreshRow err = %v, want ErrNotFound", err)
		}
		if _, _, dirty := cachedRowState(cache, "gc-missing"); dirty {
			t.Fatal("the refresh kept the dirty mark of a row the backing does not have")
		}
		assertEvents(t, rec.take())
	})
	t.Run("uncached", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, rec := newRefreshCacheForTest(t, backing)
		freshAt := cache.Stats().LastFreshAt
		_, err := cache.RefreshRow("gc-missing")
		if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrRowRefreshFenced) {
			t.Fatalf("RefreshRow err = %v, want ErrNotFound only", err)
		}
		assertEvents(t, rec.take())
		if !cache.Stats().LastFreshAt.Equal(freshAt) {
			t.Fatal("a refresh that changed nothing moved LastFreshAt")
		}
	})
	t.Run("evicts_without_tombstone", func(t *testing.T) {
		// Not found evicts but leaves no deletion fence: a lagging read that
		// missed a row that exists must not make the cache answer for it.
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, _ := newRefreshCacheForTest(t, backing)
		row := mustCreateCached(t, cache)
		backing.notFoundNextGet = true
		if _, err := cache.RefreshRow(row.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("RefreshRow err = %v, want ErrNotFound", err)
		}
		if got, err := cache.Get(row.ID); err != nil || got.ID != row.ID {
			t.Fatalf("Get after the eviction = %q, %v; want the backing row", got.ID, err)
		}
	})
	t.Run("read_error", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, _ := newRefreshCacheForTest(t, backing)
		row := mustCreateCached(t, cache)
		backing.failNextGet = true
		_, err := cache.RefreshRow(row.ID)
		if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrRowRefreshFenced) {
			t.Fatalf("RefreshRow err = %v, want a plain read error", err)
		}
		if _, held, _ := cachedRowState(cache, row.ID); !held {
			t.Fatal("a failed read dropped the cached row")
		}
	})
}

// TestCachingStoreRefreshRowSurvivesOlderScan refreshes a row while a
// reconcile's scan, taken before the change, is in flight: a changed row, an
// uncached row created after the scan, and a row whose refresh found it
// unchanged after an applied event. The scan's merge must not roll any of them
// back: the first two are stamped, and the unchanged refresh keeps the
// event's stamp.
func TestCachingStoreRefreshRowSurvivesOlderScan(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// race runs inside the scan's List, after it collected its rows, and
		// returns the id to check; the row must end with k=v.
		race func(t *testing.T, cache *CachingStore, backing *casBackingStore, seed string) string
	}{
		{"changed", func(t *testing.T, _ *CachingStore, backing *casBackingStore, seed string) string {
			mustSetBacking(t, backing, seed)
			return seed
		}},
		{"uncached", func(t *testing.T, _ *CachingStore, backing *casBackingStore, _ string) string {
			row, err := backing.Create(Bead{Title: "out of process", Metadata: map[string]string{"k": "v"}})
			if err != nil {
				t.Errorf("backing Create: %v", err)
			}
			return row.ID
		}},
		{"unchanged_after_event", func(t *testing.T, cache *CachingStore, backing *casBackingStore, seed string) string {
			mustSetBacking(t, backing, seed)
			b, err := backing.Store.Get(seed)
			if err != nil {
				t.Errorf("backing Get: %v", err)
			}
			cache.ApplyEvent("bead.updated", eventPayload(t, b))
			if row, _, _ := cachedRowState(cache, seed); row.Metadata["k"] != "v" {
				t.Error("the event did not apply; the case is vacuous")
			}
			return seed
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, _ := newRefreshCacheForTest(t, backing)
			seed := mustCreateCached(t, cache)
			ageLocalWrite(cache, seed.ID)
			cache.ReconcileNowForTest()
			var id string
			backing.onListOnce = func() {
				id = tc.race(t, cache, backing, seed.ID)
				if _, err := cache.RefreshRow(id); err != nil {
					t.Errorf("RefreshRow: %v", err)
				}
			}
			cache.ReconcileNowForTest()
			if id == "" {
				t.Fatal("the scan hook did not run; the race is vacuous")
			}
			if got, held, _ := cachedRowState(cache, id); !held || got.Metadata["k"] != "v" {
				t.Fatalf("cached row = %v held=%v, want the refreshed row kept over the older scan", got.Metadata, held)
			}
		})
	}
}

// TestCachingStoreRefreshRowConcurrent runs refreshes against local writes,
// out-of-process writes, events and reconciles. A local write must never be
// rolled back, and a final refresh must converge the row on the backing.
func TestCachingStoreRefreshRowConcurrent(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	cache, _ := newRefreshCacheForTest(t, backing)
	row := mustCreateCached(t, cache)
	const rounds = 200

	var wg sync.WaitGroup
	run := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 1; i <= rounds; i++ {
				fn(i)
			}
		}()
	}
	run(func(i int) {
		if err := cache.SetMetadata(row.ID, "n", strconv.Itoa(i)); err != nil {
			t.Errorf("SetMetadata: %v", err)
		}
		got, err := cache.Get(row.ID)
		if err != nil {
			t.Errorf("Get: %v", err)
			return
		}
		if n, _ := strconv.Atoi(got.Metadata["n"]); n < i {
			t.Errorf("local write n=%d rolled back to n=%d", i, n)
		}
	})
	run(func(i int) {
		if err := backing.SetMetadata(row.ID, "ext", strconv.Itoa(i)); err != nil {
			t.Errorf("backing SetMetadata: %v", err)
		}
	})
	run(func(int) {
		_, err := cache.RefreshRow(row.ID)
		if err != nil && !errors.Is(err, ErrRowRefreshFenced) {
			t.Errorf("RefreshRow: %v", err)
		}
	})
	run(func(int) {
		if b, err := backing.Get(row.ID); err == nil {
			cache.ApplyEvent("bead.updated", eventPayload(t, b))
		}
	})
	run(func(i int) {
		if i%20 == 0 {
			cache.ReconcileNowForTest()
		}
	})
	wg.Wait()

	got, err := cache.RefreshRow(row.ID)
	if err != nil {
		t.Fatalf("final RefreshRow: %v", err)
	}
	truth, err := backing.Get(row.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if beadChanged(truth, got, true) {
		t.Fatalf("final refresh = %v, backing = %v", got.Metadata, truth.Metadata)
	}
	if cached, held, dirty := cachedRowState(cache, row.ID); !held || dirty || beadChanged(truth, cached, true) {
		t.Fatalf("cached row = %v held=%v dirty=%v, want the backing row", cached.Metadata, held, dirty)
	}
}

// TestCachingStoreRefreshRowFencedByFullScan lands a reconcile merge, a
// reconcile eviction and a full Prime replace between the refresh's backing
// read and its install. None moves mutationSeq or a per-row fence, so only
// the scan generation can tell the read is older than the cache: it must
// install nothing and report fenced.
func TestCachingStoreRefreshRowFencedByFullScan(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// scan changes the backing row behind the cache's back, then merges a
		// full scan of it.
		scan   func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string)
		verify func(t *testing.T, cache *CachingStore, id string)
	}{
		{"reconcile_merge", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string) {
			if err := backing.SetMetadata(id, "k", "newer"); err != nil {
				t.Errorf("backing SetMetadata: %v", err)
			}
			cache.ReconcileNowForTest()
		}, wantCachedK("newer")},
		{"reconcile_evict", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string) {
			if err := backing.Close(id); err != nil {
				t.Errorf("backing Close: %v", err)
			}
			cache.ReconcileNowForTest()
		}, func(t *testing.T, cache *CachingStore, id string) {
			if row, held, _ := cachedRowState(cache, id); held {
				t.Fatalf("the refresh reinstalled a row the reconcile evicted closed (status %q)", row.Status)
			}
		}},
		{"prime_replace", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string) {
			if err := backing.SetMetadata(id, "k", "newer"); err != nil {
				t.Errorf("backing SetMetadata: %v", err)
			}
			if err := cache.Prime(context.Background()); err != nil {
				t.Errorf("Prime: %v", err)
			}
		}, wantCachedK("newer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, _ := newRefreshCacheForTest(t, backing)
			row := mustCreateCached(t, cache)
			ageLocalWrite(cache, row.ID)
			cache.ReconcileNowForTest()
			mustSetBacking(t, backing, row.ID)
			ran := false
			backing.onGetOnce = func() {
				ran = true
				tc.scan(t, cache, backing, row.ID)
			}

			_, err := cache.RefreshRow(row.ID)
			if !ran {
				t.Fatal("the scan hook did not run; the race is vacuous")
			}
			if !errors.Is(err, ErrRowRefreshFenced) {
				t.Fatalf("RefreshRow err = %v, want ErrRowRefreshFenced", err)
			}
			tc.verify(t, cache, row.ID)
		})
	}
}

func wantCachedK(want string) func(t *testing.T, cache *CachingStore, id string) {
	return func(t *testing.T, cache *CachingStore, id string) {
		t.Helper()
		if row, held, _ := cachedRowState(cache, id); !held || row.Metadata["k"] != want {
			t.Fatalf("cached row = %v held=%v, want k=%s", row.Metadata, held, want)
		}
	}
}

// TestCachingStoreRefreshRowNotFoundOutranksOlderScan proves a row absent
// while a reconcile's scan, taken before the delete, is in flight: a cached
// row, and a dirty row the cache does not hold whose beadSeq fenced it (as an
// ambiguous batch write leaves one). The eviction drops beadSeq, so it must
// stamp the row anew, or the scan's merge reinstalls the deleted row.
func TestCachingStoreRefreshRowNotFoundOutranksOlderScan(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// arrange returns the id of a backing row the scan will list.
		arrange func(t *testing.T, cache *CachingStore, backing *casBackingStore) string
		// fence runs inside the scan's List, before the delete.
		fence func(cache *CachingStore, id string)
	}{
		{"cached", func(t *testing.T, cache *CachingStore, _ *casBackingStore) string {
			row := mustCreateCached(t, cache)
			ageLocalWrite(cache, row.ID)
			cache.ReconcileNowForTest()
			return row.ID
		}, func(*CachingStore, string) {}},
		{"dirty_uncached", func(t *testing.T, _ *CachingStore, backing *casBackingStore) string {
			row, err := backing.Create(Bead{Title: "out of process"})
			if err != nil {
				t.Fatalf("backing Create: %v", err)
			}
			return row.ID
		}, func(cache *CachingStore, id string) {
			cache.mu.Lock()
			cache.noteMutationLocked(id)
			cache.markDirtyLocked(id)
			cache.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, _ := newRefreshCacheForTest(t, backing)
			id := tc.arrange(t, cache, backing)
			ran := false
			backing.onListOnce = func() {
				ran = true
				tc.fence(cache, id)
				if err := backing.Delete(id); err != nil {
					t.Errorf("backing Delete: %v", err)
				}
				if _, err := cache.RefreshRow(id); !errors.Is(err, ErrNotFound) {
					t.Errorf("RefreshRow err = %v, want ErrNotFound", err)
				}
			}
			cache.ReconcileNowForTest()
			if !ran {
				t.Fatal("the scan hook did not run; the race is vacuous")
			}
			if _, held, dirty := cachedRowState(cache, id); held || dirty {
				t.Fatalf("after the older scan: held=%v dirty=%v, want the deleted row gone", held, dirty)
			}
		})
	}
}

// foreignRigBacking stands in for a bd store whose show is routed by prefix to
// other rigs while its list reads only its own rig's rows.
type foreignRigBacking struct {
	*casBackingStore
	own string
}

func (s *foreignRigBacking) List(q ListQuery) ([]Bead, error) {
	items, err := s.casBackingStore.List(q)
	out := items[:0]
	for _, b := range items {
		if strings.HasPrefix(b.ID, s.own+"-") {
			out = append(out, b)
		}
	}
	return out, err
}

// TestCachingStoreRefreshRowRefusesForeignID refreshes an id another rig owns
// on a cache whose backing would answer it. The cache must refuse it unread,
// as unavailable rather than not found: installing it would hold a row no
// scan of this rig re-reads, and not found would read as closed.
func TestCachingStoreRefreshRowRefusesForeignID(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	mem.HonorExplicitIDs = true
	backing := &foreignRigBacking{casBackingStore: &casBackingStore{Store: mem}, own: "mc"}
	rec := &refreshRecorder{}
	cache := NewCachingStoreForTestWithPrefix(backing, "mc", rec.record)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if _, err := mem.Create(Bead{ID: "ga-1", Title: "another rig's row"}); err != nil {
		t.Fatalf("backing Create: %v", err)
	}
	reads := backing.getCalls

	_, err := cache.RefreshRow("ga-1")
	if !errors.Is(err, ErrCacheUnavailable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("RefreshRow err = %v, want ErrCacheUnavailable only", err)
	}
	if backing.getCalls != reads {
		t.Fatal("the refresh read a foreign id from the backing")
	}
	if _, held, _ := cachedRowState(cache, "ga-1"); held {
		t.Fatal("the refresh installed a foreign row")
	}
	assertEvents(t, rec.take())
}

// TestCachingStoreRefreshRowUnavailable refreshes on a cache that was never
// primed, and on one that stops being live during the backing read: each must
// report unavailable and install nothing, and the first must not read.
func TestCachingStoreRefreshRowUnavailable(t *testing.T) {
	t.Parallel()

	t.Run("unprimed", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		row, err := backing.Create(Bead{Title: "seed"})
		if err != nil {
			t.Fatalf("backing Create: %v", err)
		}
		cache := NewCachingStoreForTest(backing, nil)
		if _, err := cache.RefreshRow(row.ID); !errors.Is(err, ErrCacheUnavailable) {
			t.Fatalf("RefreshRow err = %v, want ErrCacheUnavailable", err)
		}
		if backing.getCalls != 0 {
			t.Fatalf("an unavailable cache made %d backing reads, want 0", backing.getCalls)
		}
	})
	t.Run("degraded_during_read", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, _ := newRefreshCacheForTest(t, backing)
		row, err := backing.Create(Bead{Title: "out of process"})
		if err != nil {
			t.Fatalf("backing Create: %v", err)
		}
		backing.onGetOnce = func() {
			cache.mu.Lock()
			cache.state = cacheDegraded
			cache.mu.Unlock()
		}
		if _, err := cache.RefreshRow(row.ID); !errors.Is(err, ErrCacheUnavailable) {
			t.Fatalf("RefreshRow err = %v, want ErrCacheUnavailable", err)
		}
		if _, held, _ := cachedRowState(cache, row.ID); held {
			t.Fatal("a cache that stopped being live installed the row")
		}
	})
}

// TestCachingStoreRefreshRowDropsDependentVerdicts refreshes a blocker whose
// dependent the cache holds with a ready verdict. A blocker new to the cache,
// one whose status changed, and one that is gone each turn the verdict, so it
// must drop, as an applied event drops it; a change that leaves the status
// alone must keep it.
func TestCachingStoreRefreshRowDropsDependentVerdicts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// blockerOpen is the blocker's status before the change, and cached
		// whether the cache holds it then.
		blockerOpen, cached bool
		change              func(t *testing.T, backing *casBackingStore, id string)
		wantErr             error
		wantDropped         bool
	}{
		{"uncached_reopened", false, false, func(t *testing.T, backing *casBackingStore, id string) {
			if err := backing.Reopen(id); err != nil {
				t.Fatalf("backing Reopen: %v", err)
			}
		}, nil, true},
		{"held_closed", true, true, func(t *testing.T, backing *casBackingStore, id string) {
			if err := backing.Close(id); err != nil {
				t.Fatalf("backing Close: %v", err)
			}
		}, nil, true},
		{"held_gone", true, true, func(t *testing.T, backing *casBackingStore, id string) {
			if err := backing.Delete(id); err != nil {
				t.Fatalf("backing Delete: %v", err)
			}
		}, ErrNotFound, true},
		{"held_status_kept", true, true, func(t *testing.T, backing *casBackingStore, id string) {
			mustSetBacking(t, backing, id)
		}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			blocker, err := backing.Create(Bead{Title: "blocker"})
			if err != nil {
				t.Fatalf("backing Create: %v", err)
			}
			if !tc.blockerOpen {
				if err := backing.Close(blocker.ID); err != nil {
					t.Fatalf("backing Close: %v", err)
				}
			}
			verdict := tc.blockerOpen
			dependent, err := backing.Create(Bead{Title: "dependent", Needs: []string{blocker.ID}, IsBlocked: &verdict})
			if err != nil {
				t.Fatalf("backing Create: %v", err)
			}
			cache, _ := newRefreshCacheForTest(t, backing)
			if _, held, _ := cachedRowState(cache, blocker.ID); held != tc.cached {
				t.Fatalf("blocker held=%v after Prime, want %v", held, tc.cached)
			}
			if d, _, _ := cachedRowState(cache, dependent.ID); d.IsBlocked == nil || *d.IsBlocked != verdict {
				t.Fatalf("dependent verdict = %v after Prime, want %v; the case is vacuous", d.IsBlocked, verdict)
			}
			tc.change(t, backing, blocker.ID)

			_, err = cache.RefreshRow(blocker.ID)
			if (tc.wantErr == nil && err != nil) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("RefreshRow err = %v, want %v", err, tc.wantErr)
			}
			d, _, _ := cachedRowState(cache, dependent.ID)
			if dropped := d.IsBlocked == nil; dropped != tc.wantDropped {
				t.Fatalf("dependent verdict dropped = %v, want %v", dropped, tc.wantDropped)
			}
		})
	}
}

// TestCachingStoreRefreshRowReadsEdges refreshes a row on a backing whose
// point read omits edges. The refresh must read the edges, install a change
// to them alone as a change, and install nothing when that read fails.
func TestCachingStoreRefreshRowReadsEdges(t *testing.T) {
	t.Parallel()

	newBacking := func() *depListFailStore {
		return &depListFailStore{writeRaceStore: &writeRaceStore{
			casBackingStore: &casBackingStore{Store: NewMemStore(), stripDepsFromGet: true},
		}}
	}
	arrange := func(t *testing.T, backing *depListFailStore) (*CachingStore, *refreshRecorder, string, string) {
		t.Helper()
		blocker, err := backing.Create(Bead{Title: "blocker"})
		if err != nil {
			t.Fatalf("backing Create: %v", err)
		}
		cache, rec := newRefreshCacheForTest(t, backing)
		row := mustCreateCached(t, cache)
		ageLocalWrite(cache, row.ID)
		cache.ReconcileNowForTest()
		if err := backing.Store.DepAdd(row.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("backing DepAdd: %v", err)
		}
		rec.take()
		return cache, rec, row.ID, blocker.ID
	}
	cachedEdge := func(cache *CachingStore, id, to string) bool {
		cache.mu.RLock()
		defer cache.mu.RUnlock()
		for _, d := range cache.deps[id] {
			if d.DependsOnID == to {
				return true
			}
		}
		return false
	}

	t.Run("edge_only_change", func(t *testing.T) {
		t.Parallel()
		backing := newBacking()
		cache, rec, id, blocker := arrange(t, backing)
		if _, err := cache.RefreshRow(id); err != nil {
			t.Fatalf("RefreshRow: %v", err)
		}
		if !cachedEdge(cache, id, blocker) {
			t.Fatal("the refresh did not install the edge its DepList read")
		}
		assertEvents(t, rec.take(), "bead.updated "+id)
	})
	t.Run("deplist_error", func(t *testing.T) {
		t.Parallel()
		backing := newBacking()
		cache, rec, id, blocker := arrange(t, backing)
		mustSetBacking(t, backing.casBackingStore, id)
		backing.failNextDepList = true
		_, err := cache.RefreshRow(id)
		if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrRowRefreshFenced) {
			t.Fatalf("RefreshRow err = %v, want a plain read error", err)
		}
		if row, _, _ := cachedRowState(cache, id); row.Metadata["k"] == "v" || cachedEdge(cache, id, blocker) {
			t.Fatal("a refresh whose edge read failed installed the row")
		}
		assertEvents(t, rec.take())
	})
}

// TestCachingStoreRefreshRowLabelsOnlyNotifiesNothing changes only a row's
// labels behind the cache's back. A re-scan absorbs that silently (reconcile
// compares rows without labels), so the refresh installs the labels but
// notifies nothing.
func TestCachingStoreRefreshRowLabelsOnlyNotifiesNothing(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache, rec := newRefreshCacheForTest(t, backing)
	row := mustCreateCached(t, cache)
	if err := backing.Update(row.ID, UpdateOpts{Labels: []string{"lagged"}}); err != nil {
		t.Fatalf("backing Update: %v", err)
	}
	rec.take()
	if _, err := cache.RefreshRow(row.ID); err != nil {
		t.Fatalf("RefreshRow: %v", err)
	}
	if got, _, _ := cachedRowState(cache, row.ID); !slices.Contains(got.Labels, "lagged") {
		t.Fatalf("cached labels = %q, want the backing's installed", got.Labels)
	}
	assertEvents(t, rec.take())
}

func mustCreateCached(t *testing.T, cache *CachingStore) Bead {
	t.Helper()
	row, err := cache.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return row
}

// mustSetBacking writes k=v to id behind the cache's back, as an out-of-process
// writer whose event was lost does.
func mustSetBacking(t *testing.T, backing *casBackingStore, id string) {
	t.Helper()
	if err := backing.SetMetadata(id, "k", "v"); err != nil {
		t.Errorf("backing SetMetadata: %v", err)
	}
}
