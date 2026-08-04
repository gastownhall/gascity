package beads

// GraphOnlyReadyStore is an optional capability for querying only a store's
// graph-work backend. Implementations without a distinct graph backend must
// delegate to the full Ready query.
type GraphOnlyReadyStore interface {
	ReadyGraphOnly(query ...ReadyQuery) ([]Bead, error)
}

// GraphOnlyReadyProvider exposes a graph-only-ready handle when a wrapper's
// runtime backing supports the capability.
type GraphOnlyReadyProvider interface {
	ReadyGraphOnlyHandle() (GraphOnlyReadyStore, bool)
}

// GraphOnlyReadyFor returns a store's graph-only-ready capability, including
// capabilities exposed through wrappers.
func GraphOnlyReadyFor(store Store) (GraphOnlyReadyStore, bool) {
	if store == nil {
		return nil, false
	}
	if provider, ok := store.(GraphOnlyReadyProvider); ok {
		return provider.ReadyGraphOnlyHandle()
	}
	if g, ok := store.(GraphOnlyReadyStore); ok {
		return g, true
	}
	return nil, false
}
