package beads

// NewNativeDoltStoreForConformance returns a NativeDoltStore backed by the
// in-memory native storage fixture for the external conformance suite.
func NewNativeDoltStoreForConformance() Store {
	return newNativeDoltStoreForTest(newNativeDoltMemStorage())
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
