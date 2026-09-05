package beads

// AvailabilityGate reports whether the backing store transport is
// currently believed reachable. The production implementation is the
// per-scope *resilience.Breaker wired by the controller; the methods
// must never mutate breaker state (probe admission stays on the
// operation path).
type AvailabilityGate interface {
	// Available reports the gate is closed (store believed reachable).
	Available() bool
	// ProbeDue reports an unavailable gate would currently admit a
	// recovery probe. Periodic loops use it to keep probing while
	// otherwise skipping cycles cheaply.
	ProbeDue() bool
}

// SetAvailabilityGate wires the transport availability gate. The verdict
// is surfaced through Degraded(); a nil gate (the default) disables
// gating. Swapping the gate takes effect for the next read, so a
// controller reload can rebind a scope's store to a fresh breaker.
func (c *CachingStore) SetAvailabilityGate(g AvailabilityGate) {
	if g == nil {
		c.availabilityGate.Store(nil)
		return
	}
	c.availabilityGate.Store(&g)
}

// availabilityGateRef returns the configured gate (nil when unset).
func (c *CachingStore) availabilityGateRef() AvailabilityGate {
	if p := c.availabilityGate.Load(); p != nil {
		return *p
	}
	return nil
}

// Degraded reports whether the cache is currently serving degraded data:
// the availability gate says the store is unreachable, or repeated
// reconcile failures pushed the cache into the degraded state. The state
// and the gate reference are snapshotted under one lock acquisition, but
// the gate's Available() deliberately runs outside c.mu (foreign breaker
// code must never execute under this lock), so the two facts are NOT
// evaluated atomically: a transition that lands between the snapshot and
// the gate call can make one reading disagree with an adjacent one. This
// is an observability indicator, not a synchronization primitive — do not
// build invariants on consecutive Degraded() readings agreeing.
func (c *CachingStore) Degraded() bool {
	gate := c.availabilityGateRef()
	c.mu.RLock()
	degraded := c.state == cacheDegraded
	c.mu.RUnlock()
	if degraded {
		return true
	}
	return gate != nil && !gate.Available()
}
