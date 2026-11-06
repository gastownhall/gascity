package beads

// CachePreferredReadyReader is implemented by stores that can answer a ready
// query from a warm, valid read cache and read live only when the cache is
// cold or invalidated. See CachingStore.ReadyPreferCache.
type CachePreferredReadyReader interface {
	ReadyPreferCache(query ReadyQuery) ([]Bead, error)
}

// ReadyPreferCache answers query from store's ready cache when store has one
// that can vouch for the answer, and reads live through the store's handles
// otherwise. A store with no cache is read exactly as Live.Ready reads it.
// Both lookups see through class handles (As, LiveFor): a handle over a cache
// reads that cache's ready set, and its live read goes to the cache's own
// live reader rather than through the logical wrapper over the handle, whose
// List would install rows into the cache and announce them.
//
// Warm and cold reads return the same rows in the same order only over a
// backing whose Ready sorts into the canonical (priority, created_at, id) order
// before it applies the Limit. The cache always sorts that way and cuts after
// the sort. A cold read takes the backing's own order and cut instead: bd's
// order for BdStore, and insertion order with the Limit applied mid-scan for
// MemStore. Under a Limit the two can therefore keep different rows.
func ReadyPreferCache(store Store, query ReadyQuery) ([]Bead, error) {
	if reader, ok := As[CachePreferredReadyReader](store); ok {
		return reader.ReadyPreferCache(query)
	}
	return LiveFor(store).Ready(query)
}
