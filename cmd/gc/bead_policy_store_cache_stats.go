package main

import "github.com/gastownhall/gascity/internal/beads"

// cacheStatsReporter is a store that reports its bead cache's statistics.
type cacheStatsReporter interface{ Stats() beads.CacheStats }

var (
	_ cacheStatsReporter = (*beadPolicyStore)(nil)
	_ cacheStatsReporter = (*beadPolicyGraphStore)(nil)
)

// Stats reports the statistics of the bead cache under this layer, or zero
// stats when the wrapped store has no cache. The controller's city and rig
// caches sit under this layer (wrapWithCachingStoreFeed puts it back over
// them), and the API reads their age through it for X-GC-Cache-Age-S. The
// forward is explicit for the reason DeleteBatch is: the embedded Store
// interface does not promote optional capabilities, so the cache's Stats was
// hidden and every API read reported a cache age of 0.
//
// IsLive is deliberately not forwarded. The API refuses reads with a
// cache_not_live 503 when the store it reads reports not-live, and a city or
// rig cache is not live from every controller start and store rebuild until
// its full prime lands. Remote CLI targets never fall back on that 503 and the
// dashboard does not retry it, while a not-live cache already answers from its
// backing store. beadPolicyGraphStore inherits this through its embedded
// *beadPolicyStore.
func (s *beadPolicyStore) Stats() beads.CacheStats {
	cache, ok := beads.As[cacheStatsReporter](s.Store)
	if !ok {
		return beads.CacheStats{}
	}
	return cache.Stats()
}
