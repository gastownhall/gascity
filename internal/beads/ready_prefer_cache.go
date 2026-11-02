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
//
// Warm and cold reads return the same rows in the same order only over a
// backing whose Ready sorts into the canonical (priority, created_at, id) order
// before it applies the Limit. The cache always sorts that way and cuts after
// the sort. A cold read takes the backing's own order and cut instead: bd's
// order for BdStore, and insertion order with the Limit applied mid-scan for
// MemStore. Under a Limit the two can therefore keep different rows.
func ReadyPreferCache(store Store, query ReadyQuery) ([]Bead, error) {
	if reader, ok := store.(CachePreferredReadyReader); ok {
		return reader.ReadyPreferCache(query)
	}
	return HandlesFor(store).Live.Ready(query)
}
