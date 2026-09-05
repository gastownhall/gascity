package beads

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

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

// SetAvailabilityGate wires the transport availability gate. While the
// gate reports unavailable, List and Get serve last-good cached data
// tagged degraded (or ErrStoreUnavailable when the cache cannot answer)
// and the reconciler skips cycles except when a recovery probe is due.
// A nil gate (the default) disables gating.
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

// servingDegraded reports whether reads must avoid the backing store
// because the availability gate says the transport is unavailable.
func (c *CachingStore) servingDegraded() bool {
	g := c.availabilityGateRef()
	return g != nil && !g.Available()
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

// listLastGood answers a List query purely from the in-memory snapshot while
// the backing store is unavailable, refusing the shapes the snapshot cannot
// answer honestly:
//
//   - An unprimed cache returns ErrStoreUnavailable — unavailable must
//     never read as empty.
//   - Live queries return ErrStoreUnavailable. Live declares staleness
//     unacceptable (see ListQuery.Live): lifecycle gates that treat absence
//     as authoritative would release live pool assignments on a stale short
//     list.
//   - Closed-only shapes return ErrStoreUnavailable: the snapshot holds
//     active beads only, so an empty answer would read as "none exist".
//   - A partially-primed snapshot (primePartialErr set) is tagged with the
//     package's PartialResultError convention on every answer: the active
//     set itself is known-incomplete (a partial prime can hold wisps only),
//     and serving it as complete would present "no work" as fact.
//
// ParentID queries are served: active children live in the snapshot, and
// this process's own closes are absorbed into it, so molecule/convergence
// child listings keep advancing during an outage.
//
// Known limitation: rows marked dirty are served as-is. The overlay's
// per-read suppression (backing Get returning ErrNotFound) cannot run while
// the store is unavailable, so an externally-deleted bead whose delete event
// was missed can reappear until the store recovers. Staleness, including
// this form, is the price of answering at all.
//
// The snapshot is not frozen during an outage: local writes still absorb
// into c.beads (createWith/closeWith/update run with no state check), so
// last-good includes this process's own activity even while the reconcile
// scan is failing.
func (c *CachingStore) listLastGood(query ListQuery) ([]Bead, error) {
	if query.Live {
		return nil, fmt.Errorf("listing beads (live): %w", ErrStoreUnavailable)
	}
	if query.Status == "closed" {
		return nil, fmt.Errorf("listing beads (closed history): %w", ErrStoreUnavailable)
	}
	c.mu.RLock()
	if c.state == cacheUninitialized {
		c.mu.RUnlock()
		return nil, fmt.Errorf("listing beads: %w", ErrStoreUnavailable)
	}
	partialPrime := c.primePartialErr
	cached := make([]Bead, 0, len(c.beads))
	for _, b := range c.beads {
		if !query.Matches(b) {
			continue
		}
		cached = append(cached, cloneBead(b))
	}
	c.mu.RUnlock()
	c.degradedReads.Add(1)
	sortBeadsForQuery(cached, query.Sort)
	if query.Limit > 0 && len(cached) > query.Limit {
		cached = cached[:query.Limit]
	}
	if partialPrime != nil {
		return cached, &PartialResultError{
			Op:  "cache list last-good",
			Err: fmt.Errorf("snapshot from partial prime: %w", partialPrime),
		}
	}
	return cached, nil
}

// lastGoodCount answers a Count from the in-memory snapshot while the
// backing store is unavailable, under listLastGood's honesty boundary. A
// count cannot carry a partial tag, so shapes that would need one (closed
// history, partial prime) report ok=false and the caller surfaces the
// backing failure instead. Lock acquisition observes ctx with the same
// TryRLock guard cachedCountContext documents as mandatory for this verb,
// so a cache writer cannot strand a deadline-bounded caller.
func (c *CachingStore) lastGoodCount(ctx context.Context, query ListQuery, excludeTypes []string) (int, bool) {
	if query.Live || query.Status == "closed" || query.IncludesClosed() {
		return 0, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return 0, false
	}
	if !c.mu.TryRLock() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for !c.mu.TryRLock() {
			select {
			case <-ctx.Done():
				return 0, false
			case <-ticker.C:
			}
		}
	}
	defer c.mu.RUnlock()
	if c.state == cacheUninitialized || c.primePartialErr != nil {
		return 0, false
	}
	n := 0
	for _, b := range c.beads {
		if query.Matches(b) && !slices.Contains(excludeTypes, b.Type) {
			n++
		}
	}
	c.degradedReads.Add(1)
	return n, true
}

// getLastGood answers a Get purely from the in-memory cache while the
// backing store is unavailable. A bead absent from the cache returns
// ErrStoreUnavailable: the cache cannot distinguish "missing" from
// "unreachable" (closed beads are not fully cached).
func (c *CachingStore) getLastGood(id string) (Bead, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, deleted := c.deletedSeq[id]; deleted {
		return Bead{}, ErrNotFound
	}
	if b, ok := c.beads[id]; ok {
		c.degradedReads.Add(1)
		return cloneBead(b), nil
	}
	return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrStoreUnavailable)
}

// reconcileUnavailableSkip reports whether the current reconciliation
// cycle must be skipped because the gate is unavailable and no recovery
// probe is due. The first skip of an episode records one problem
// ("emit once"); recovery re-arms the log for the next episode.
func (c *CachingStore) reconcileUnavailableSkip() bool {
	g := c.availabilityGateRef()
	if g == nil || g.Available() {
		c.mu.Lock()
		c.unavailableSkipLogged = false
		c.mu.Unlock()
		return false
	}
	if g.ProbeDue() {
		return false
	}
	c.mu.Lock()
	logged := c.unavailableSkipLogged
	c.unavailableSkipLogged = true
	c.mu.Unlock()
	if !logged {
		c.recordProblem("reconcile skipped", errors.New("store unavailable (circuit breaker open); skipping reconcile cycles until a recovery probe is due"))
	}
	return true
}
