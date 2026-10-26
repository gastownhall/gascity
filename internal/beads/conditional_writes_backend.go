package beads

// ConditionalWritesBackend describes the store a store's conditional writes
// land in once every wrapper is unwrapped: the typed class wrappers and the
// cmd/gc policy store (their declared resolve targets), a mode-sourcing
// wrapper (its source), a CachingStore (its backing) and a ProxiedStore (its
// bd write leaf).
type ConditionalWritesBackend struct {
	// Kind names the backend store type in the diagnostic vocabulary
	// (SQLiteStore, NativeDoltStore, BdStore, ...; %T for other types).
	Kind string
	// SQLite reports a backend that is a SQLite store.
	SQLite bool
	// DoltServed reports a backend whose writes a Dolt server takes: a
	// native Dolt store, or a store served through bd.
	DoltServed bool
}

// ResolveConditionalWritesBackend unwraps store to the store its conditional
// writes land in and describes it. It does no I/O and runs no capability
// probe: it answers which engine would take the writes, not whether they can
// succeed (ProbeConditionalWrites answers that). A nil store, or a wrapper
// chain deeper than the resolve-target bound, describes as neither SQLite nor
// Dolt-served.
func ResolveConditionalWritesBackend(store Store) ConditionalWritesBackend {
	leaf := conditionalWritesLeaf(store)
	backend := ConditionalWritesBackend{Kind: conditionalStoreKind(leaf)}
	switch leaf.(type) {
	case *SQLiteStore:
		backend.SQLite = true
	case *NativeDoltStore, *BdStore, *BeadsLibStore:
		backend.DoltServed = true
	}
	return backend
}

// conditionalWritesLeaf walks store's declared resolve targets, mode source
// and caching backing to the innermost store, bounded like
// followConditionalWritesResolveTarget so a cyclic wrapper terminates.
func conditionalWritesLeaf(store Store) Store {
	for range conditionalWritesMaxResolveDepth {
		if store == nil {
			return nil
		}
		store = followConditionalWritesResolveTarget(store)
		if src, sourced := conditionalWritesModeSource(store); sourced {
			store = src
			continue
		}
		cache, ok := store.(*CachingStore)
		if !ok {
			return store
		}
		store = cache.backing
	}
	return nil
}
