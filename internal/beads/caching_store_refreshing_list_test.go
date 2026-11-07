package beads

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// refreshBacking is the backing of the tests' caches: it counts point reads
// (Get, DepList) and lists, and its point reads can be made to wait for
// release to close, then to take delay on the clock of the synctest bubble
// they run in, as a bd show or a bd dep list does, and to fail with err. Set
// those before the reads they shape.
type refreshBacking struct {
	Store
	release           chan struct{}
	delay             time.Duration
	err               error
	gets, deps, lists atomic.Int32
}

func (s *refreshBacking) pointRead() error {
	if s.release != nil {
		<-s.release
	}
	if s.delay > 0 {
		<-time.After(s.delay)
	}
	return s.err
}

func (s *refreshBacking) Get(id string) (Bead, error) {
	s.gets.Add(1)
	if err := s.pointRead(); err != nil {
		return Bead{}, err
	}
	return s.Store.Get(id)
}

func (s *refreshBacking) DepList(id, direction string) ([]Dep, error) {
	s.deps.Add(1)
	if err := s.pointRead(); err != nil {
		return nil, err
	}
	return s.Store.DepList(id, direction)
}

func (s *refreshBacking) List(query ListQuery) ([]Bead, error) {
	s.lists.Add(1)
	return s.Store.List(query)
}

// reset zeroes the read counts.
func (s *refreshBacking) reset() {
	s.gets.Store(0)
	s.deps.Store(0)
	s.lists.Store(0)
}

// refreshingListAssignee is the identity the tests' work is claimed for.
const refreshingListAssignee = "worker"

// refreshingListQuery is the API's per-row active-work read of
// refreshingListAssignee's work.
func refreshingListQuery() ListQuery {
	return ListQuery{Assignee: refreshingListAssignee, Status: "in_progress", Limit: 1, Sort: SortCreatedDesc}
}

// newRefreshingListCache primes a cache over a refreshBacking holding one
// open bead, which the caller then changes behind the cache.
func newRefreshingListCache(t *testing.T) (*CachingStore, *refreshBacking, *MemStore, Bead) {
	t.Helper()
	mem := NewMemStore()
	backing := &refreshBacking{Store: mem}
	bead, err := backing.Create(Bead{Title: "work", Status: "open"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	return cache, backing, mem, bead
}

// claimBehindTheCache claims id for refreshingListAssignee in the ledger the
// cache does not see and marks the cached row dirty, as a refresh that failed
// after the claim leaves it.
func claimBehindTheCache(t *testing.T, cache *CachingStore, mem *MemStore, id string) {
	t.Helper()
	status, assignee := "in_progress", refreshingListAssignee
	if err := mem.Update(id, UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("claim behind the cache: %v", err)
	}
	markDirtyForTest(cache, id)
}

func cacheDirtyCount(c *CachingStore) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.dirty)
}

// endedContext is a context that has already ended, as a reader's whose
// budget is spent.
func endedContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

// TestCachedListRefreshingRefreshesDirtyRowsFromTheBacking: a dirty row does
// not decline the read. The cache's refresh reads the row from the backing,
// one point read and no scan, installs it and clears its mark, and the reader
// waits for it and is answered; the next read is the clean cache's.
func TestCachedListRefreshingRefreshesDirtyRowsFromTheBacking(t *testing.T) {
	t.Parallel()
	cache, backing, mem, bead := newRefreshingListCache(t)
	claimBehindTheCache(t, cache, mem, bead.ID)
	backing.reset()

	rows, ok := cache.CachedListRefreshing(t.Context(), refreshingListQuery())

	if !ok || len(rows) != 1 || rows[0].ID != bead.ID {
		t.Fatalf("CachedListRefreshing = %v, %v; want the claimed %s", sortedIDs(rows), ok, bead.ID)
	}
	if g, l := backing.gets.Load(), backing.lists.Load(); g != 1 || l != 0 {
		t.Fatalf("backing reads: gets=%d lists=%d, want 1/0: one point read per dirty row, no scan", g, l)
	}
	if n := cacheDirtyCount(cache); n != 0 {
		t.Fatalf("dirty rows after the refresh = %d, want 0", n)
	}
	if rows, ok := cache.CachedList(refreshingListQuery()); !ok || len(rows) != 1 {
		t.Fatalf("CachedList after the refresh = %v, %v; want the clean cache's answer", sortedIDs(rows), ok)
	}
}

// TestCachedListRefreshingWithAnEndedContext: a reader whose context has
// ended gets the clean cache's answer, hit or authoritative miss, and nothing
// else: a dirty row declines the read, which starts no refresh, so the
// backing is not read and the mark is kept.
func TestCachedListRefreshingWithAnEndedContext(t *testing.T) {
	t.Parallel()
	cache, backing, mem, bead := newRefreshingListCache(t)
	ended := endedContext(t)
	if rows, ok := cache.CachedListRefreshing(ended, refreshingListQuery()); !ok || len(rows) != 0 {
		t.Fatalf("clean miss = %v, %v; want an authoritative empty answer", sortedIDs(rows), ok)
	}
	claimBehindTheCache(t, cache, mem, bead.ID)
	backing.reset()

	if rows, ok := cache.CachedListRefreshing(ended, refreshingListQuery()); ok {
		t.Fatalf("dirty read with an ended context = %v, answered; want declined", sortedIDs(rows))
	}
	if cache.dirtyRefreshRun.Load() != nil {
		t.Fatal("a read whose context had ended started a refresh")
	}
	if g, l := backing.gets.Load(), backing.lists.Load(); g != 0 || l != 0 {
		t.Fatalf("backing reads: gets=%d lists=%d, want none", g, l)
	}
	if n := cacheDirtyCount(cache); n != 1 {
		t.Fatalf("dirty rows = %d, want the mark kept", n)
	}

	if _, ok := cache.CachedListRefreshing(t.Context(), refreshingListQuery()); !ok {
		t.Fatal("a live read did not refresh the dirty row")
	}
	if rows, ok := cache.CachedListRefreshing(ended, refreshingListQuery()); !ok || len(rows) != 1 || rows[0].ID != bead.ID {
		t.Fatalf("clean hit with an ended context = %v, %v; want the claimed %s", sortedIDs(rows), ok, bead.ID)
	}
}

// TestCachedListRefreshingRefreshOutlivesItsReader is the regression for a
// refresh bound to its reader's deadline: one dirty row whose point reads,
// Get and then DepList at 750ms each as bd's are, outlast the reader's 1s.
// The reader is declined at its deadline, but the refresh runs on, installs
// the row and clears its mark, so a later reader is answered by the clean
// cache with no backing read of its own.
func TestCachedListRefreshingRefreshOutlivesItsReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, backing, mem, bead := newRefreshingListCache(t)
		claimBehindTheCache(t, cache, mem, bead.ID)
		backing.reset()
		backing.delay = 750 * time.Millisecond
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		start := time.Now()

		if rows, ok := cache.CachedListRefreshing(ctx, refreshingListQuery()); ok {
			t.Fatalf("reader answered %v before the refresh could end", sortedIDs(rows))
		}
		if waited := time.Since(start); waited > time.Second {
			t.Fatalf("reader waited %s, want at most its 1s deadline", waited)
		}
		if n := cacheDirtyCount(cache); n != 1 {
			t.Fatalf("dirty rows at the reader's deadline = %d, want the refresh still running", n)
		}

		<-time.After(time.Second)
		synctest.Wait()

		if n := cacheDirtyCount(cache); n != 0 {
			t.Fatalf("dirty rows after the refresh = %d, want the mark cleared", n)
		}
		rows, ok := cache.CachedListRefreshing(endedContext(t), refreshingListQuery())
		if !ok || len(rows) != 1 || rows[0].ID != bead.ID {
			t.Fatalf("later read = %v, %v; want the claimed %s from the clean cache", sortedIDs(rows), ok, bead.ID)
		}
		if g, d := backing.gets.Load(), backing.deps.Load(); g != 1 || d != 1 {
			t.Fatalf("backing point reads: gets=%d deps=%d, want the one refresh's 1/1", g, d)
		}
	})
}

// TestCachedListRefreshingSharesOneRefresh: readers that arrive while a
// refresh runs wait for it instead of starting their own, so a burst of reads
// of a dirty cache reads each dirty row from the backing once, and every
// reader is answered.
func TestCachedListRefreshingSharesOneRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, backing, mem, bead := newRefreshingListCache(t)
		claimBehindTheCache(t, cache, mem, bead.ID)
		backing.reset()
		backing.delay = 100 * time.Millisecond
		const readers = 5
		var answered atomic.Int32
		var wg sync.WaitGroup

		for range readers {
			wg.Go(func() {
				rows, ok := cache.CachedListRefreshing(t.Context(), refreshingListQuery())
				if ok && len(rows) == 1 && rows[0].ID == bead.ID {
					answered.Add(1)
				}
			})
		}
		wg.Wait()

		if n := answered.Load(); n != readers {
			t.Fatalf("%d of %d readers answered with the claimed %s", n, readers, bead.ID)
		}
		if g, d := backing.gets.Load(), backing.deps.Load(); g != 1 || d != 1 {
			t.Fatalf("backing point reads: gets=%d deps=%d, want one refresh's 1/1", g, d)
		}
	})
}

// TestCachedListRefreshingDeclines: the read declines, never falling back to
// a scan, when the cache cannot answer: a read of closed rows, a cache that is
// not servable, and more dirty rows than the overlay refreshes (none of which
// reads the backing or starts a refresh), and a refresh whose backing read
// fails (the mark is kept).
func TestCachedListRefreshingDeclines(t *testing.T) {
	t.Parallel()
	t.Run("closed rows", func(t *testing.T) {
		cache, backing, _, _ := newRefreshingListCache(t)
		backing.reset()
		if _, ok := cache.CachedListRefreshing(t.Context(), ListQuery{Status: "closed"}); ok {
			t.Fatal("a closed-row read answered from the cache")
		}
		if g, l := backing.gets.Load(), backing.lists.Load(); g != 0 || l != 0 {
			t.Fatalf("backing reads: gets=%d lists=%d, want none", g, l)
		}
	})
	t.Run("not servable", func(t *testing.T) {
		backing := &refreshBacking{Store: NewMemStore()}
		cache := NewCachingStoreForTest(backing, nil)
		if _, ok := cache.CachedListRefreshing(t.Context(), refreshingListQuery()); ok {
			t.Fatal("an unprimed cache answered")
		}
		if g, l := backing.gets.Load(), backing.lists.Load(); g != 0 || l != 0 {
			t.Fatalf("backing reads: gets=%d lists=%d, want none", g, l)
		}
		if cache.dirtyRefreshRun.Load() != nil {
			t.Fatal("an unprimed cache started a refresh")
		}
	})
	t.Run("more dirty rows than the overlay refreshes", func(t *testing.T) {
		cache, backing, mem, bead := newRefreshingListCache(t)
		claimBehindTheCache(t, cache, mem, bead.ID)
		for i := range dirtyOverlayMaxGets {
			extra, err := mem.Create(Bead{Title: "extra", Status: "open"})
			if err != nil {
				t.Fatalf("create extra %d: %v", i, err)
			}
			markDirtyForTest(cache, extra.ID)
		}
		backing.reset()
		if _, ok := cache.CachedListRefreshing(t.Context(), refreshingListQuery()); ok {
			t.Fatalf("a cache with %d dirty rows answered", cacheDirtyCount(cache))
		}
		if g, l := backing.gets.Load(), backing.lists.Load(); g != 0 || l != 0 {
			t.Fatalf("backing reads: gets=%d lists=%d, want none past the overlay's bound", g, l)
		}
		if cache.dirtyRefreshRun.Load() != nil {
			t.Fatal("a cache past the overlay's bound started a refresh")
		}
	})
	t.Run("a backing read fails", func(t *testing.T) {
		cache, backing, mem, bead := newRefreshingListCache(t)
		claimBehindTheCache(t, cache, mem, bead.ID)
		backing.reset()
		backing.err = errors.New("bd show: connection refused")
		if _, ok := cache.CachedListRefreshing(t.Context(), refreshingListQuery()); ok {
			t.Fatal("a refresh whose backing read failed answered")
		}
		if n := cacheDirtyCount(cache); n != 1 {
			t.Fatalf("dirty rows = %d, want the mark kept", n)
		}
		if g, l := backing.gets.Load(), backing.lists.Load(); g != 1 || l != 0 {
			t.Fatalf("backing reads: gets=%d lists=%d, want the one failed Get and no scan", g, l)
		}
	})
}

// TestCachedListRefreshingOmitsRowsGoneFromTheBacking: a dirty row the
// backing no longer has is left out of the answer, as List's overlay leaves it
// out, and keeps its mark for the reconciler. A later read leaves it out too,
// with no backing read, while it is the only dirty row and has not changed;
// once another row goes dirty, a scan merges or the row changes, the cache
// must refresh again before it answers.
func TestCachedListRefreshingOmitsRowsGoneFromTheBacking(t *testing.T) {
	t.Parallel()
	cache, backing, mem, bead := newRefreshingListCache(t)
	if err := cache.Update(bead.ID, UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr(refreshingListAssignee)}); err != nil {
		t.Fatalf("claim through the cache: %v", err)
	}
	if err := mem.Delete(bead.ID); err != nil {
		t.Fatalf("delete behind the cache: %v", err)
	}
	markDirtyForTest(cache, bead.ID)
	backing.reset()
	ended := endedContext(t)

	rows, ok := cache.CachedListRefreshing(t.Context(), refreshingListQuery())

	if !ok || len(rows) != 0 {
		t.Fatalf("CachedListRefreshing = %v, %v; want an answer without the deleted %s", sortedIDs(rows), ok, bead.ID)
	}
	if n := cacheDirtyCount(cache); n != 1 {
		t.Fatalf("dirty rows = %d, want the deleted row's mark kept", n)
	}
	if rows, ok := cache.CachedListRefreshing(ended, refreshingListQuery()); !ok || len(rows) != 0 {
		t.Fatalf("later read = %v, %v; want the refresh's answer, the deleted row left out", sortedIDs(rows), ok)
	}
	if g := backing.gets.Load(); g != 1 {
		t.Fatalf("backing Gets = %d, want the refresh's one: the later read reads nothing", g)
	}

	other, err := mem.Create(Bead{Title: "other", Status: "open"})
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	markDirtyForTest(cache, other.ID)
	if rows, ok := cache.CachedListRefreshing(ended, refreshingListQuery()); ok {
		t.Fatalf("read with another row dirty = %v, answered; want declined until a refresh", sortedIDs(rows))
	}
	if rows, ok := cache.CachedListRefreshing(t.Context(), refreshingListQuery()); !ok || len(rows) != 0 {
		t.Fatalf("read after a new refresh = %v, %v; want an answer without the deleted %s", sortedIDs(rows), ok, bead.ID)
	}

	for _, change := range []struct {
		name  string
		apply func()
	}{
		{name: "a scan merged", apply: func() { cache.scanGen++ }},
		{name: "the deleted row changed", apply: func() { cache.noteMutationLocked(bead.ID) }},
	} {
		cache.mu.Lock()
		change.apply()
		cache.mu.Unlock()
		if rows, ok := cache.CachedListRefreshing(ended, refreshingListQuery()); ok {
			t.Fatalf("read after %s = %v, answered; want declined until a refresh", change.name, sortedIDs(rows))
		}
		if rows, ok := cache.CachedListRefreshing(t.Context(), refreshingListQuery()); !ok || len(rows) != 0 {
			t.Fatalf("read after %s and a new refresh = %v, %v; want an answer without the deleted %s", change.name, sortedIDs(rows), ok, bead.ID)
		}
	}
}
