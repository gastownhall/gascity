package beads

// The bead cache's own read capabilities, named so a caller holding a class
// handle reaches them through As instead of asserting an anonymous interface
// on the store, which answers "no" on a handle (typed-stores T6a). All three
// are class-neutral reads, so As looks past a class guard for them
// (passesClassGate).

// CacheStatsReporter is a store that reports its bead cache's statistics: a
// CachingStore, and a policy layer over one that forwards Stats.
type CacheStatsReporter interface {
	Stats() CacheStats
}

// CacheLivenessReporter is a store that reports whether its bead cache is
// serving reads (CachingStore.IsLive).
type CacheLivenessReporter interface {
	IsLive() bool
}

// CachedLister is a store that can answer a ListQuery from its in-memory
// cache alone, reporting whether the cache was clean enough to serve it
// (CachingStore.CachedList).
type CachedLister interface {
	CachedList(ListQuery) ([]Bead, bool)
}

var (
	_ CacheStatsReporter    = (*CachingStore)(nil)
	_ CacheLivenessReporter = (*CachingStore)(nil)
	_ CachedLister          = (*CachingStore)(nil)
)
