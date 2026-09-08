package beads

// NewNativeDoltStoreForConformance returns a NativeDoltStore backed by the
// in-memory native storage fixture for the external conformance suite.
func NewNativeDoltStoreForConformance() Store {
	return newNativeDoltStoreForTest(newNativeDoltMemStorage())
}

// NewNativeDoltStoreForPinnedIDFenceConformance returns a NativeDoltStore
// minting under mintPrefix and fenced to exactly the given namespaces, for
// beadstest.RunPinnedIDFenceConformance.
//
// namespaces is forwarded VERBATIM to the same option production wiring uses,
// with no branch on emptiness — the suite needs an empty set to reach
// production's unfenced case, which is the control that tells a fence from a
// blanket refusal.
//
// The backing storage honors explicit ids so a pinned id survives the round
// trip; without that the fixture would clobber every id the suite pins and the
// rows would pass against a store that never had to decide anything.
func NewNativeDoltStoreForPinnedIDFenceConformance(mintPrefix string, namespaces ...string) Store {
	storage := &nativeDoltMemStorage{store: &MemStore{IDPrefix: mintPrefix, HonorExplicitIDs: true}}
	store := newNativeDoltStoreForTest(storage, WithNativeDoltStoreReservedIDPrefixes(namespaces...))
	store.idPrefix = normalizeIDPrefix(mintPrefix)
	return store
}

// NotifyChangeForTest drives the real producer (CachingStore.notifyChange) with
// a caller-supplied bead, bypassing the store-write path that rewrites ids and
// status. It lets cross-package guardrail tests (e.g. the run-view round-trip)
// emit an exact run-shaped bead through the production event-marshal + run/session
// id-resolution seam. The onChange callback receives the same 6-tuple the record
// site (cmd/gc/api_state.go) wraps into an events.Event.
func (c *CachingStore) NotifyChangeForTest(eventType string, b Bead) {
	c.notifyChange(eventType, b)
}
