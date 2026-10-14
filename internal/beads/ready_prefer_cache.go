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
func ReadyPreferCache(store Store, query ReadyQuery) ([]Bead, error) {
	if reader, ok := store.(CachePreferredReadyReader); ok {
		return reader.ReadyPreferCache(query)
	}
	return HandlesFor(store).Live.Ready(query)
}
