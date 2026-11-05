package beads

// ReadyGraphOnlyHandle returns a graph-only-ready handle when the backing store
// implements GraphOnlyReadyStore. It returns (nil, false) when the backing has
// no distinct graph-only backend, so capability absence is not promoted.
//
// ReadyGraphOnly bypasses the cachedReadyOnly path and delegates directly to
// the backing, ensuring controller demand sees fresh wisp-tier data without
// cache staleness.
func (c *CachingStore) ReadyGraphOnlyHandle() (GraphOnlyReadyStore, bool) {
	g, ok := GraphOnlyReadyFor(c.backing)
	if !ok {
		return nil, false
	}
	return cachingGraphOnlyReadyStore{g: g}, true
}

type cachingGraphOnlyReadyStore struct {
	g GraphOnlyReadyStore
}

func (s cachingGraphOnlyReadyStore) ReadyGraphOnly(query ...ReadyQuery) ([]Bead, error) {
	return s.g.ReadyGraphOnly(query...)
}
