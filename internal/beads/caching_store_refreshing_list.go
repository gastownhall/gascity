package beads

import (
	"context"
	"maps"
)

// CachedListRefreshing answers query from the in-memory cache as CachedList
// does, except that dirty rows need not decline it. When a refresh can clear
// them (a cache servable for query, holding at most dirtyOverlayMaxGets dirty
// rows), the read starts the cache's refresh of its dirty rows, unless one is
// running, and waits for it until ctx ends. The refresh is List's dirty
// overlay, reading each row from the backing and installing it under the same
// fences, clearing its mark, but it runs apart from its readers, one at a
// time per cache (startDirtyRefresh). A reader that stops waiting leaves it
// running, so a later read finds the rows it refreshed, and readers that
// arrive while it runs wait for it instead of reading the backing again. Its
// backing reads end with the backing's own timeouts, not with any reader's
// context. A ctx that has already ended neither starts a refresh nor waits
// for one: the read answers only if the cache can at once.
//
// A dirty row the latest refresh found gone from the backing keeps its mark,
// as the overlay leaves it, and is left out of the answer while every dirty
// row is such a row and none has changed since (refreshCoversLocked).
//
// The boolean reports whether the cache answered. It is false when query
// includes closed rows, the cache is not servable for query, more rows are
// dirty than the overlay refreshes, or the refresh failed, left rows dirty,
// or did not end before ctx. The caller then reads elsewhere: this read never
// falls back to a scan.
func (c *CachingStore) CachedListRefreshing(ctx context.Context, query ListQuery) ([]Bead, bool) {
	query = c.listTier(query)
	if query.IncludesClosed() {
		return nil, false
	}
	rows, ok, refreshable := c.cachedListAroundDirty(query)
	if ok || !refreshable || ctx.Err() != nil {
		return rows, ok
	}
	run := c.startDirtyRefresh()
	select {
	case <-run.done:
	case <-ctx.Done():
		return nil, false
	}
	rows, ok, _ = c.cachedListAroundDirty(query)
	return rows, ok
}

// cachedListAroundDirty answers query from the cache when it is clean, or when
// every dirty row is one the latest refresh found gone from the backing,
// leaving those rows out. Otherwise refreshable reports whether a refresh can
// let the cache answer: it is servable for query and holds no more dirty rows
// than the overlay refreshes.
func (c *CachingStore) cachedListAroundDirty(query ListQuery) (rows []Bead, ok, refreshable bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.cacheServableForListQueryLocked(query) {
		return nil, false, false
	}
	if len(c.dirty) == 0 {
		return c.collectCachedListLocked(query, nil), true, false
	}
	if run := c.dirtyRefreshRun.Load(); c.refreshCoversLocked(run) {
		return c.collectCachedListLocked(query, run.absent), true, false
	}
	return nil, false, len(c.dirty) <= dirtyOverlayMaxGets
}

// dirtyRefresh is one run of the cache's refresh of its dirty rows
// (startDirtyRefresh). done closes when the run ends; ok, absent, seq and
// scan are set before it closes and never change after.
type dirtyRefresh struct {
	done chan struct{}
	// ok reports that the run left no row dirty but the ones it found gone
	// from the backing. absent holds those rows, which keep their marks
	// (fetchDirtyOverlay), and seq and scan are the cache's mutation sequence
	// and scan generation when the run found every other dirty row refreshed.
	ok     bool
	absent map[string]struct{}
	seq    uint64
	scan   uint64
}

// ended reports whether r has ended.
func (r *dirtyRefresh) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// startDirtyRefresh returns the refresh of the dirty rows in flight, starting
// one when none is: the dirty overlay, run through the backing on its own
// goroutine with no read to serve. One runs at a time per cache, so a burst
// of readers forks one set of backing reads, not one each.
func (c *CachingStore) startDirtyRefresh() *dirtyRefresh {
	for {
		last := c.dirtyRefreshRun.Load()
		if last != nil && !last.ended() {
			return last
		}
		run := &dirtyRefresh{done: make(chan struct{})}
		if c.dirtyRefreshRun.CompareAndSwap(last, run) {
			go c.runDirtyRefresh(run)
			return run
		}
	}
}

// runDirtyRefresh runs the dirty overlay for run (readCacheWithOverlay). It
// installs the rows it reads under the overlay's fences, clearing their marks,
// and records, in the lock hold that finds every row still dirty gone from
// the backing, which rows those are.
func (c *CachingStore) runDirtyRefresh(run *dirtyRefresh) {
	defer close(run.done)
	err := c.readCacheWithOverlay(c.cacheServableLocked, func(suppressed map[string]struct{}) {
		run.absent = maps.Clone(suppressed)
		run.seq, run.scan = c.mutationSeq, c.scanGen
	})
	run.ok = err == nil
}

// refreshCoversLocked reports whether run, the latest refresh, lets the cache
// answer around its dirty rows: run ended ok, every row dirty now is one it
// found gone from the backing, and none of them has changed since it did. A
// scan merged since then may have installed or evicted those rows unfenced
// (scanRacedLocked), so it ends the cover too. Caller must hold c.mu.
func (c *CachingStore) refreshCoversLocked(run *dirtyRefresh) bool {
	if run == nil || !run.ended() || !run.ok || run.scan != c.scanGen || len(c.dirty) != len(run.absent) {
		return false
	}
	for id := range c.dirty {
		if _, gone := run.absent[id]; !gone || c.refetchFencedLocked(id, run.seq) {
			return false
		}
	}
	return true
}
