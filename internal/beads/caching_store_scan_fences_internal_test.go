package beads

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
)

// The tests in this file pin the scan-generation fences (mc-zndi7.37). A
// reconcile merge, a full Prime and a PrimeActive install or evict rows
// without moving mutationSeq or any per-row fence, so a refetch whose backing
// read predates one, or a Prime whose listing predates a reconcile, must learn
// of it from scanGen and fullScanGen rather than from the per-row fences.

// scanRaceCase lands a scan merge in a refetch's read window. It changes the
// backing row behind the cache's back first, as an out-of-process writer
// whose event was lost does, then merges a reconcile, a full Prime or a
// PrimeActive, so the scan holds the newer state and the refetch's read is
// older than it.
type scanRaceCase struct {
	name string
	scan func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string)
	// truth is what a backing read of the row reports once the scan ran.
	wantStatus string
	wantK      string
}

var scanRaceCases = []scanRaceCase{
	// Symptom 1: rollback. The scan absorbs k=newer.
	{"reconcile_merge", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string) {
		if err := backing.SetMetadata(id, "k", "newer"); err != nil {
			t.Errorf("backing SetMetadata: %v", err)
		}
		cache.ReconcileNowForTest()
	}, "open", "newer"},
	// Symptom 2: resurrection. The row closes and the scan evicts it.
	{"reconcile_evict", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string) {
		if err := backing.Close(id); err != nil {
			t.Errorf("backing Close: %v", err)
		}
		cache.ReconcileNowForTest()
	}, "closed", "v"},
	{"prime_replace", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string) {
		if err := backing.SetMetadata(id, "k", "newer"); err != nil {
			t.Errorf("backing SetMetadata: %v", err)
		}
		if err := cache.Prime(context.Background()); err != nil {
			t.Errorf("Prime: %v", err)
		}
	}, "open", "newer"},
	{"prime_active", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string) {
		if err := backing.SetMetadata(id, "k", "newer"); err != nil {
			t.Errorf("backing SetMetadata: %v", err)
		}
		if err := cache.PrimeActive(); err != nil {
			t.Errorf("PrimeActive: %v", err)
		}
	}, "open", "newer"},
}

// refetchPath drives one refetch path of id with backing.onGetOnce or
// onListOnce armed to run race inside its backing read window.
type refetchPath struct {
	name string
	run  func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string, race func())
}

var scanRefetchPaths = []refetchPath{
	{"get_dirty_refresh", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string, race func()) {
		markDirtyForTest(cache, id)
		backing.onGetOnce = race
		if _, err := cache.Get(id); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}},
	{"dirty_overlay", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string, race func()) {
		markDirtyForTest(cache, id)
		backing.onGetOnce = race
		if _, err := cache.List(ListQuery{Status: "open"}); err != nil {
			t.Fatalf("List: %v", err)
		}
	}},
	{"live_list_refresh", func(t *testing.T, cache *CachingStore, backing *casBackingStore, _ string, race func()) {
		backing.onListOnce = race
		if _, err := cache.List(ListQuery{Live: true, Status: "open"}); err != nil {
			t.Fatalf("List: %v", err)
		}
	}},
	{"conditional_install", func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string, race func()) {
		row, _, _ := cachedRowState(cache, id)
		title := "mine"
		backing.onGetOnce = race
		if err := cache.UpdateIfMatch(id, row.Revision, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("UpdateIfMatch: %v", err)
		}
	}},
}

// seedScanRaceRow creates a clean cached row with k=v in the backing, its
// local write aged out and a reconcile run, so no fence of its own protects
// it: only the scan generation can.
func seedScanRaceRow(t *testing.T, cache *CachingStore, backing *casBackingStore) Bead {
	t.Helper()
	row := mustCreateCached(t, cache)
	ageLocalWrite(cache, row.ID)
	cache.ReconcileNowForTest()
	mustSetBacking(t, backing, row.ID)
	cache.ReconcileNowForTest()
	if got, held, dirty := cachedRowState(cache, row.ID); !held || dirty || got.Metadata["k"] != "v" {
		t.Fatalf("seed row = %v held=%v dirty=%v, want clean k=v", got.Metadata, held, dirty)
	}
	return row
}

// assertNotServedStale fails when the cache serves id clean in a state older
// than the backing's: a clean held row must match the backing, and an absent
// row is right only for a closed one. A dirty mark is always acceptable: it
// keeps the row out of every clean census until a backing read settles it.
func assertNotServedStale(t *testing.T, cache *CachingStore, id, wantStatus, wantK string) {
	t.Helper()
	got, held, dirty := cachedRowState(cache, id)
	if dirty {
		return
	}
	if !held {
		if wantStatus != "closed" {
			t.Fatalf("row evicted clean while the backing holds it %s", wantStatus)
		}
		return
	}
	if got.Status != wantStatus || got.Metadata["k"] != wantK {
		t.Fatalf("cached row clean at status=%q k=%q, want status=%q k=%q: an older read was installed over a newer scan",
			got.Status, got.Metadata["k"], wantStatus, wantK)
	}
}

// assertConvergesClean drains id with Gets that no scan overlaps and requires
// the row to settle clean at the backing's state within one read.
func assertConvergesClean(t *testing.T, cache *CachingStore, id, wantStatus, wantK string) {
	t.Helper()
	got, err := cache.Get(id)
	if err != nil {
		t.Fatalf("converge Get: %v", err)
	}
	if got.Status != wantStatus || got.Metadata["k"] != wantK {
		t.Fatalf("converge Get = status %q k=%q, want %q k=%q", got.Status, got.Metadata["k"], wantStatus, wantK)
	}
	if _, _, dirty := cachedRowState(cache, id); dirty {
		t.Fatal("the row is still dirty after a Get no scan overlapped")
	}
}

// TestCachingStoreRefetchFencedByScanMerge lands a reconcile merge (symptom 1)
// and a reconcile eviction (symptom 2) in the backing read window of every
// refetch path: Get's dirty refresh, the dirty overlay, the Live list refresh
// and the conditional write's install. The refetch's read is older than the
// scan, so it must not install over it, and the row must then converge.
func TestCachingStoreRefetchFencedByScanMerge(t *testing.T) {
	t.Parallel()

	for _, path := range scanRefetchPaths {
		for _, sc := range scanRaceCases {
			t.Run(path.name+"/"+sc.name, func(t *testing.T) {
				t.Parallel()
				backing := &casBackingStore{Store: NewMemStore()}
				cache, _ := newRefreshCacheForTest(t, backing)
				row := seedScanRaceRow(t, cache, backing)
				wantStatus, wantK := sc.wantStatus, sc.wantK
				ran := false
				path.run(t, cache, backing, row.ID, func() {
					ran = true
					sc.scan(t, cache, backing, row.ID)
				})
				if !ran {
					t.Fatal("the scan hook did not run; the race is vacuous")
				}
				truth, err := backing.Store.Get(row.ID)
				if err != nil {
					t.Fatalf("backing Get: %v", err)
				}
				if truth.Status != wantStatus || truth.Metadata["k"] != wantK {
					t.Fatalf("backing row = status %q k=%q, want %q k=%q", truth.Status, truth.Metadata["k"], wantStatus, wantK)
				}
				assertNotServedStale(t, cache, row.ID, wantStatus, wantK)
				assertConvergesClean(t, cache, row.ID, wantStatus, wantK)
			})
		}
	}
}

// TestCachingStoreScanRacedRefetchThatAgreesLeavesNoMark is the anti-churn
// side of the fence: a scan that merges in a refetch's window but read the
// same row leaves nothing to settle, so the refetch must leave no dirty mark,
// and the census stays admitted.
func TestCachingStoreScanRacedRefetchThatAgreesLeavesNoMark(t *testing.T) {
	t.Parallel()

	for _, path := range scanRefetchPaths {
		if path.name == "conditional_install" {
			// The write changes the row the scan reads, so both reads agree
			// only by construction of the write; covered below.
			continue
		}
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, _ := newRefreshCacheForTest(t, backing)
			row := seedScanRaceRow(t, cache, backing)
			ran := false
			path.run(t, cache, backing, row.ID, func() {
				ran = true
				cache.ReconcileNowForTest()
			})
			if !ran {
				t.Fatal("the scan hook did not run; the race is vacuous")
			}
			if got, held, dirty := cachedRowState(cache, row.ID); !held || dirty || got.Metadata["k"] != "v" {
				t.Fatalf("row = %v held=%v dirty=%v, want clean k=v", got.Metadata, held, dirty)
			}
			if _, _, ok := cache.ObservedList(ListQuery{Status: "open"}); !ok {
				t.Fatal("the census is refused after a scan-raced refetch whose read agreed with the scan")
			}
		})
	}
	t.Run("get_point_read_omits_edges", func(t *testing.T) {
		// The scan's row carries a dependency the point read omits: the
		// edge fields are not the row's content, so the reads agree.
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, _ := newRefreshCacheForTest(t, backing)
		blocker := mustCreateCached(t, cache)
		row, err := cache.Create(Bead{Title: "dependent", Needs: []string{blocker.ID}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ageLocalWrite(cache, blocker.ID)
		ageLocalWrite(cache, row.ID)
		cache.ReconcileNowForTest()
		backing.stripDepsFromGet = true
		markDirtyForTest(cache, row.ID)
		backing.onGetOnce = cache.ReconcileNowForTest
		if _, err := cache.Get(row.ID); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if backing.onGetOnce != nil {
			t.Fatal("the scan hook did not run; the race is vacuous")
		}
		if _, held, dirty := cachedRowState(cache, row.ID); !held || dirty {
			t.Fatalf("held=%v dirty=%v, want held clean: an edge-less point read was taken to disagree", held, dirty)
		}
	})
	t.Run("get_closed_row_evicted", func(t *testing.T) {
		// The row closed before the read, and the scan evicted it: both
		// reads say closed, which the cache holds as absent.
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, _ := newRefreshCacheForTest(t, backing)
		row := seedScanRaceRow(t, cache, backing)
		if err := backing.Close(row.ID); err != nil {
			t.Fatalf("backing Close: %v", err)
		}
		markDirtyForTest(cache, row.ID)
		backing.onGetOnce = cache.ReconcileNowForTest
		got, err := cache.Get(row.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if backing.onGetOnce != nil {
			t.Fatal("the scan hook did not run; the race is vacuous")
		}
		if got.Status != "closed" {
			t.Fatalf("Get status = %q, want closed", got.Status)
		}
		if _, _, dirty := cachedRowState(cache, row.ID); dirty {
			t.Fatal("the row is dirty after reads that agreed it closed")
		}
		if _, _, ok := cache.ObservedList(ListQuery{Status: "open"}); !ok {
			t.Fatal("the census is refused after reads that agreed the row closed")
		}
	})
	t.Run("conditional_install", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, _ := newRefreshCacheForTest(t, backing)
		row := seedScanRaceRow(t, cache, backing)
		ran := false
		scanRefetchPaths[3].run(t, cache, backing, row.ID, func() {
			ran = true
			cache.ReconcileNowForTest()
		})
		if !ran {
			t.Fatal("the scan hook did not run; the race is vacuous")
		}
		if got, held, dirty := cachedRowState(cache, row.ID); !held || dirty || got.Title != "mine" {
			t.Fatalf("row title=%q held=%v dirty=%v, want the written row clean", got.Title, held, dirty)
		}
	})
}

// TestCachingStoreScanRacedRefetchConverges is the livelock check. A dirty
// row whose every Get overlaps a scan that read a different row stays dirty
// and is never served stale; the first Get no scan overlaps settles it. The
// overlay retries once within a read and settles there.
func TestCachingStoreScanRacedRefetchConverges(t *testing.T) {
	t.Parallel()

	t.Run("get", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, _ := newRefreshCacheForTest(t, backing)
		row := seedScanRaceRow(t, cache, backing)
		markDirtyForTest(cache, row.ID)
		const raced = 5
		for i := range raced {
			want := "w" + strconv.Itoa(i)
			backing.onGetOnce = func() {
				if err := backing.SetMetadata(row.ID, "k", want); err != nil {
					t.Errorf("backing SetMetadata: %v", err)
				}
				cache.ReconcileNowForTest()
			}
			if _, err := cache.Get(row.ID); err != nil {
				t.Fatalf("Get %d: %v", i, err)
			}
			if backing.onGetOnce != nil {
				t.Fatalf("Get %d: the scan hook did not run", i)
			}
			assertNotServedStale(t, cache, row.ID, "open", want)
			if _, _, dirty := cachedRowState(cache, row.ID); !dirty {
				t.Fatalf("Get %d: the mark cleared while the scan and the read disagreed", i)
			}
		}
		assertConvergesClean(t, cache, row.ID, "open", "w"+strconv.Itoa(raced-1))
	})

	t.Run("overlay", func(t *testing.T) {
		t.Parallel()
		backing := &casBackingStore{Store: NewMemStore()}
		cache, _ := newRefreshCacheForTest(t, backing)
		row := seedScanRaceRow(t, cache, backing)
		markDirtyForTest(cache, row.ID)
		backing.onGetOnce = func() {
			if err := backing.SetMetadata(row.ID, "k", "newer"); err != nil {
				t.Errorf("backing SetMetadata: %v", err)
			}
			cache.ReconcileNowForTest()
		}
		pre := backing.getCalls
		got, err := cache.List(ListQuery{Status: "open"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if n := backing.getCalls - pre; n != 2 {
			t.Fatalf("overlay made %d point reads, want 2: one raced, one that settles", n)
		}
		if len(got) != 1 || got[0].Metadata["k"] != "newer" {
			t.Fatalf("List = %v, want the one row at k=newer", got)
		}
		if r, _, dirty := cachedRowState(cache, row.ID); dirty || r.Metadata["k"] != "newer" {
			t.Fatalf("row k=%q dirty=%v, want clean k=newer", r.Metadata["k"], dirty)
		}
	})
}

// TestCachingStoreScanRaceMarkSurvivesOlderScan pins the stamp a disagreeing
// scan-raced refetch leaves with its mark. A reconcile lists k=v; inside its
// window a dirty Get reads k=w1 while a PrimeActive installs k=w2, so the Get
// marks the row. The reconcile then merges its older k=v: without the stamp
// it would absorb that row clean over the mark.
func TestCachingStoreScanRaceMarkSurvivesOlderScan(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache, _ := newRefreshCacheForTest(t, backing)
	row := seedScanRaceRow(t, cache, backing)
	ran := false
	backing.onListOnce = func() {
		if err := backing.SetMetadata(row.ID, "k", "w1"); err != nil {
			t.Errorf("backing SetMetadata: %v", err)
		}
		markDirtyForTest(cache, row.ID)
		backing.onGetOnce = func() {
			ran = true
			if err := backing.SetMetadata(row.ID, "k", "w2"); err != nil {
				t.Errorf("backing SetMetadata: %v", err)
			}
			if err := cache.PrimeActive(); err != nil {
				t.Errorf("PrimeActive: %v", err)
			}
		}
		if _, err := cache.Get(row.ID); err != nil {
			t.Errorf("Get: %v", err)
		}
		if _, _, dirty := cachedRowState(cache, row.ID); !dirty {
			t.Error("the scan-raced Get left no mark")
		}
	}
	cache.ReconcileNowForTest()
	if !ran {
		t.Fatal("the PrimeActive hook did not run; the race is vacuous")
	}
	assertNotServedStale(t, cache, row.ID, "open", "w2")
	assertConvergesClean(t, cache, row.ID, "open", "w2")
}

// TestCachingStorePrimeSkipsAfterNewerReconcile is symptom 3: a full Prime or
// a PrimeActive lists k=v, then an out-of-process write and a reconcile absorb
// k=newer before the Prime takes the lock. No mutation moved, so the Prime
// would take its full-replace branch and install k=v for every row. It must
// skip its merge, as a reconcile skips after a newer full Prime replace.
func TestCachingStorePrimeSkipsAfterNewerReconcile(t *testing.T) {
	t.Parallel()

	for _, prime := range []string{"prime", "prime_active"} {
		for _, sc := range scanRaceCases[:2] {
			t.Run(prime+"/"+sc.name, func(t *testing.T) {
				t.Parallel()
				backing := &casBackingStore{Store: NewMemStore()}
				cache, _ := newRefreshCacheForTest(t, backing)
				row := seedScanRaceRow(t, cache, backing)
				ran := false
				backing.onListOnce = func() {
					ran = true
					sc.scan(t, cache, backing, row.ID)
				}
				runPrimeForTest(t, cache, prime)
				if !ran {
					t.Fatal("the reconcile hook did not run; the race is vacuous")
				}
				got, held, _ := cachedRowState(cache, row.ID)
				switch {
				case sc.wantStatus == "closed" && held:
					t.Fatalf("an older %s reinstalled a row a newer reconcile evicted closed", prime)
				case sc.wantStatus != "closed" && (!held || got.Metadata["k"] != sc.wantK):
					t.Fatalf("cached row = %v held=%v, want k=%s: an older %s reverted a newer reconcile", got.Metadata, held, sc.wantK, prime)
				}
				if !cache.cacheFullyPrimed() {
					t.Fatal("the cache is not fully primed after the skipped Prime")
				}
			})
		}
	}
}

// TestCachingStoreReconcileSkipsAfterNewerPrime is symptom 3 reversed: a
// reconcile lists k=v, then an out-of-process write and a full Prime replace
// install k=newer before the reconcile takes the lock. No mutation moved, so
// the fence floor does not refuse the reconcile; it must skip its merge
// because a full Prime merged since it started.
func TestCachingStoreReconcileSkipsAfterNewerPrime(t *testing.T) {
	t.Parallel()

	backing := &casBackingStore{Store: NewMemStore()}
	cache, _ := newRefreshCacheForTest(t, backing)
	row := seedScanRaceRow(t, cache, backing)
	ran := false
	backing.onListOnce = func() {
		ran = true
		if err := backing.SetMetadata(row.ID, "k", "newer"); err != nil {
			t.Errorf("backing SetMetadata: %v", err)
		}
		if err := cache.Prime(context.Background()); err != nil {
			t.Errorf("Prime: %v", err)
		}
	}
	cache.ReconcileNowForTest()
	if !ran {
		t.Fatal("the Prime hook did not run; the race is vacuous")
	}
	if got, held, _ := cachedRowState(cache, row.ID); !held || got.Metadata["k"] != "newer" {
		t.Fatalf("cached row = %v held=%v, want k=newer: an older reconcile reverted a newer Prime", got.Metadata, held)
	}
}

// TestCachingStorePrimeKeepsNewerPrimeActive lands a PrimeActive that
// installs k=newer in a full Prime's listing window. No mutation moved, so
// only the scan generation keeps the Prime from its full-replace branch and
// from installing its older k=v over the PrimeActive's row.
func TestCachingStorePrimeKeepsNewerPrimeActive(t *testing.T) {
	t.Parallel()

	for _, prime := range []string{"prime", "prime_active"} {
		t.Run(prime, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, _ := newRefreshCacheForTest(t, backing)
			row := seedScanRaceRow(t, cache, backing)
			ran := false
			backing.onListOnce = func() {
				ran = true
				if err := backing.SetMetadata(row.ID, "k", "newer"); err != nil {
					t.Errorf("backing SetMetadata: %v", err)
				}
				if err := cache.PrimeActive(); err != nil {
					t.Errorf("PrimeActive: %v", err)
				}
			}
			runPrimeForTest(t, cache, prime)
			if !ran {
				t.Fatal("the PrimeActive hook did not run; the race is vacuous")
			}
			if got, held, _ := cachedRowState(cache, row.ID); !held || got.Metadata["k"] != "newer" {
				t.Fatalf("cached row = %v held=%v, want k=newer: an older %s overwrote a newer PrimeActive", got.Metadata, held, prime)
			}
			if !cache.cacheFullyPrimed() {
				t.Fatal("the cache is not fully primed after the Prime")
			}
		})
	}
}

func runPrimeForTest(t *testing.T, cache *CachingStore, prime string) {
	t.Helper()
	var err error
	switch prime {
	case "prime":
		err = cache.Prime(context.Background())
	case "prime_active":
		err = cache.PrimeActive()
	}
	if err != nil {
		t.Fatalf("%s: %v", prime, err)
	}
}

// TestCachingStorePrimeDoesNotReinstallEvictedRow is symptom 4: a Prime or
// PrimeActive lists a row, which is then deleted out of process and evicted
// by RefreshRow's not-found or by a Live list refresh before the Prime takes
// the lock. The eviction is fenced by beadSeq alone, so the Prime must skip
// the row by refetchFencedLocked, not by writeFencedLocked.
func TestCachingStorePrimeDoesNotReinstallEvictedRow(t *testing.T) {
	t.Parallel()

	evictions := map[string]func(t *testing.T, cache *CachingStore, id string){
		"refresh_row_not_found": func(t *testing.T, cache *CachingStore, id string) {
			if _, err := cache.RefreshRow(id); !errors.Is(err, ErrNotFound) {
				t.Errorf("RefreshRow err = %v, want ErrNotFound", err)
			}
		},
		"live_list_not_found": func(t *testing.T, cache *CachingStore, _ string) {
			if _, err := cache.List(ListQuery{Live: true, Status: "open"}); err != nil {
				t.Errorf("List: %v", err)
			}
		},
	}
	for _, prime := range []string{"prime", "prime_active"} {
		for name, evict := range evictions {
			t.Run(prime+"/"+name, func(t *testing.T) {
				t.Parallel()
				backing := &casBackingStore{Store: NewMemStore()}
				cache, _ := newRefreshCacheForTest(t, backing)
				row := seedScanRaceRow(t, cache, backing)
				ran := false
				backing.onListOnce = func() {
					ran = true
					if err := backing.Delete(row.ID); err != nil {
						t.Errorf("backing Delete: %v", err)
					}
					evict(t, cache, row.ID)
					if _, held, _ := cachedRowState(cache, row.ID); held {
						t.Errorf("%s did not evict the deleted row", name)
					}
				}
				runPrimeForTest(t, cache, prime)
				if !ran {
					t.Fatal("the eviction hook did not run; the race is vacuous")
				}
				if _, held, _ := cachedRowState(cache, row.ID); held {
					t.Fatalf("%s reinstalled a row evicted after its listing (backing: deleted)", prime)
				}
			})
		}
	}
}

// TestCachingStoreUncachedCloseEventSurvivesOlderInstall is symptom 5: a close
// event for an uncached row reads the closed backing row, then a RefreshRow or
// a Live list refresh installs an older, open read before the event takes the
// lock. The event must not drop the close and leave the open row clean.
func TestCachingStoreUncachedCloseEventSurvivesOlderInstall(t *testing.T) {
	t.Parallel()

	installs := map[string]func(t *testing.T, cache *CachingStore, backing *casBackingStore, id string, race func()){
		"refresh_row": func(_ *testing.T, cache *CachingStore, backing *casBackingStore, id string, race func()) {
			backing.onGetOnce = race
			_, _ = cache.RefreshRow(id)
		},
		"live_list": func(t *testing.T, cache *CachingStore, backing *casBackingStore, _ string, race func()) {
			backing.onListOnce = race
			if _, err := cache.List(ListQuery{Live: true, Status: "open"}); err != nil {
				t.Errorf("List: %v", err)
			}
		},
	}
	for name, install := range installs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			backing := &casBackingStore{Store: NewMemStore()}
			cache, _ := newRefreshCacheForTest(t, backing)
			row, err := backing.Create(Bead{Title: "out of process"})
			if err != nil {
				t.Fatalf("backing Create: %v", err)
			}
			reached := make(chan struct{})
			release := make(chan struct{})
			done := make(chan struct{})
			cache.applyEventBeforeCommitForTest = func() {
				close(reached)
				<-release
			}
			ran := false
			install(t, cache, backing, row.ID, func() {
				// The install has read the row open. It closes out of
				// process, and the close event reads the closed row and
				// parks before its lock phase.
				ran = true
				if err := backing.Close(row.ID); err != nil {
					t.Errorf("backing Close: %v", err)
				}
				closed, err := backing.Store.Get(row.ID)
				if err != nil {
					t.Errorf("backing Get: %v", err)
				}
				go func() {
					defer close(done)
					cache.ApplyEvent("bead.closed", eventPayload(t, closed))
				}()
				<-reached
			})
			if !ran {
				t.Fatal("the close hook did not run; the race is vacuous")
			}
			if _, held, _ := cachedRowState(cache, row.ID); !held {
				t.Fatal("the older open read was not installed; the race is vacuous")
			}
			close(release)
			<-done
			assertNotServedStale(t, cache, row.ID, "closed", "")
			got, err := cache.Get(row.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "closed" {
				t.Fatalf("Get status = %q, want closed", got.Status)
			}
			if _, _, dirty := cachedRowState(cache, row.ID); dirty {
				t.Fatal("the row is still dirty after a Get no scan overlapped")
			}
		})
	}
}

// TestCachingStoreScanFencesStress runs reconciles, dirty Gets, overlays, Live
// lists, RefreshRows, conditional writes and out-of-process writes together,
// for the race detector, then requires the cache to drain every dirty mark by
// reads alone once the writers stop, and to match the backing after one
// reconcile.
func TestCachingStoreScanFencesStress(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	cache := NewCachingStoreForTest(mem, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	const rows = 8
	ids := make([]string, rows)
	for i := range ids {
		b, err := cache.Create(Bead{Title: "stress"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids[i] = b.ID
		ageLocalWrite(cache, b.ID)
	}

	const iters = 200
	var wg sync.WaitGroup
	worker := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range iters {
				fn(i)
			}
		}()
	}
	worker(func(int) { cache.ReconcileNowForTest() })
	worker(func(i int) {
		id := ids[i%rows]
		if i%17 == 0 {
			_ = mem.Close(id)
			_ = mem.Reopen(id)
			return
		}
		_ = mem.SetMetadata(id, "k", fmt.Sprint(i))
	})
	worker(func(i int) {
		id := ids[(i+1)%rows]
		markDirtyForTest(cache, id)
		_, _ = cache.Get(id)
	})
	worker(func(int) { _, _ = cache.List(ListQuery{Status: "open"}) })
	worker(func(int) { _, _ = cache.List(ListQuery{Live: true, Status: "open"}) })
	worker(func(i int) { _, _ = cache.RefreshRow(ids[(i+2)%rows]) })
	worker(func(i int) {
		id := ids[(i+3)%rows]
		if b, err := cache.Get(id); err == nil {
			title := fmt.Sprint("t", i)
			_ = cache.UpdateIfMatch(id, b.Revision, UpdateOpts{Title: &title})
		}
	})
	wg.Wait()

	// Writers stopped: reads alone must drain every mark.
	for round := 0; round < 3; round++ {
		for _, id := range ids {
			if _, err := cache.Get(id); err != nil {
				t.Fatalf("drain Get %s: %v", id, err)
			}
		}
	}
	cache.mu.RLock()
	dirty := len(cache.dirty)
	cache.mu.RUnlock()
	if dirty != 0 {
		t.Fatalf("%d dirty marks survived reads no scan overlapped", dirty)
	}

	cache.ReconcileNowForTest()
	for _, id := range ids {
		truth, err := mem.Get(id)
		if err != nil {
			t.Fatalf("backing Get %s: %v", id, err)
		}
		got, held, dirty := cachedRowState(cache, id)
		if !held || dirty || got.Status != truth.Status || got.Metadata["k"] != truth.Metadata["k"] || got.Title != truth.Title {
			t.Fatalf("%s: cached status=%q k=%q title=%q held=%v dirty=%v, backing status=%q k=%q title=%q",
				id, got.Status, got.Metadata["k"], got.Title, held, dirty, truth.Status, truth.Metadata["k"], truth.Title)
		}
	}
	if _, _, ok := cache.ObservedList(ListQuery{Status: "open"}); !ok {
		t.Fatal("the census is refused once the cache settled")
	}
}
