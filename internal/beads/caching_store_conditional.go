package beads

import (
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// This file holds CachingStore's ConditionalWriter forwarding. The cache
// rule for fenced writes is: forward, and EVICT — never patch, never adopt.
//
// Failure side: the unconditional write paths optimistically patch the cached
// clone when the post-write refresh fails; a conditional-write port of that
// fallback is poison, because the patch cannot synthesize the new revision,
// so every consumer's precondition recovery would re-read the stale revision
// through the cache and re-fail — a livelock indistinguishable from real
// contention. Eviction instead routes the next Get to the backing store
// (dirty-set + entry removal; NEVER a deletedSeq stamp, which would
// short-circuit Get to ErrNotFound without consulting the backing).
//
// Success side: the entry is evicted TOO. The backend does not return the
// committed row or its revision, so a post-write refresh cannot be attributed
// to our write — it may observe a LATER state, and installing anything
// derived from local knowledge over an independently-refreshed revision would
// fabricate a snapshot that never existed at that revision (a later IfMatch
// against it would succeed on fabricated content, defeating optimistic
// concurrency). Until a backend returns the exact committed row, the only
// honest cache action after a fenced write is a miss. The refresh, when it
// succeeds, feeds the change notification verbatim and nothing else.
var (
	_ ConditionalWriter                = (*CachingStore)(nil)
	_ conditionalWritesModeCarrier     = (*CachingStore)(nil)
	_ conditionalWriteCapabilityProber = (*CachingStore)(nil)
)

// The cache is a wrapper, not a second store, so it carries no
// conditional-writes stamp of its own (§6.3): the stamp, its read, and the
// degrade latch all delegate to the backing store. A backing that cannot
// carry a stamp (a wrapped or cross-package store) leaves the pair at
// ModeUnset, so the seam takes the legacy path — enforcement is never raised
// through a cache whose backing cannot express the mode.

// conditionalBacking resolves the store the cache's conditional-write
// machinery should operate on: the raw backing, or — when the backing is a
// target-declaring wrapper (the cmd/gc policy store in the production
// CachingStore→policy→store sandwich) — the wrapper's declared resolution
// target. Without this, a wrapped backing would hide the factory stamp and
// the cache would silently resolve unset→legacy even under require.
func (c *CachingStore) conditionalBacking() Store {
	return followConditionalWritesResolveTarget(c.backing)
}

// stampConditionalWritesMode forwards the factory stamp to the backing store
// and reports whether it landed there; false (carrier-less backing) tells the
// factory the mode was dropped so the miss is logged, never silently believed.
func (c *CachingStore) stampConditionalWritesMode(mode gate.Mode, defaulted bool) bool {
	if carrier, ok := c.conditionalBacking().(conditionalWritesModeCarrier); ok {
		return carrier.stampConditionalWritesMode(mode, defaulted)
	}
	return false
}

// conditionalWritesMode reads the backing store's stamp.
func (c *CachingStore) conditionalWritesMode() (gate.Mode, bool) {
	if carrier, ok := c.conditionalBacking().(conditionalWritesModeCarrier); ok {
		return carrier.conditionalWritesMode()
	}
	return gate.ModeUnset, false
}

// noteConditionalDegradeOnce shares the backing store's degrade latch: cache
// and backing are one store instance for emission purposes.
func (c *CachingStore) noteConditionalDegradeOnce() bool {
	if carrier, ok := c.conditionalBacking().(conditionalWritesModeCarrier); ok {
		return carrier.noteConditionalDegradeOnce()
	}
	return false
}

// setConditionalWritesDegradeCallback forwards the emission callback to the
// backing store (one latch, one callback, one store instance).
func (c *CachingStore) setConditionalWritesDegradeCallback(cb func(ConditionalWritesDegrade)) {
	if carrier, ok := c.conditionalBacking().(conditionalWritesModeCarrier); ok {
		carrier.setConditionalWritesDegradeCallback(cb)
	}
}

// fireConditionalWritesDegradeOnce forwards to the backing store's shared
// emission latch.
func (c *CachingStore) fireConditionalWritesDegradeOnce(d ConditionalWritesDegrade) {
	if carrier, ok := c.conditionalBacking().(conditionalWritesModeCarrier); ok {
		carrier.fireConditionalWritesDegradeOnce(d)
	}
}

// probeConditionalWriteCapability answers with the backing store's capability:
// the cache's own ConditionalWriter verbs forward to the backing, so its
// capability IS the backing's. A backing with CAS verbs but no prober is
// vacuously capable, mirroring the seam's default.
func (c *CachingStore) probeConditionalWriteCapability() (bool, string) {
	if prober, ok := c.conditionalBacking().(conditionalWriteCapabilityProber); ok {
		return prober.probeConditionalWriteCapability()
	}
	if _, ok := ConditionalWriterFor(c.conditionalBacking()); ok {
		return true, ""
	}
	return false, "backing store does not implement conditional writes"
}

// UpdateIfMatch forwards the fenced update to the backing store's conditional
// writer and maintains the cache: refresh on success, evict when the refresh
// fails or the precondition does. A backing without the capability yields
// ErrConditionalWriteUnsupported — never an unconditional write.
func (c *CachingStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	writer, ok := ConditionalWriterFor(c.conditionalBacking())
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	if err := writer.UpdateIfMatch(id, expectedRevision, opts); err != nil {
		c.applyConditionalWriteFailure(id, err)
		return err
	}
	// EVICT unconditionally: the backend does not return the committed row,
	// so a refresh cannot be attributed — it may observe a LATER state, and
	// installing local fields over an independently-refreshed revision would
	// fabricate a snapshot that never existed (and IfMatch against that
	// revision would then succeed on fabricated content, defeating OCC). The
	// next read consults the backing. The refresh, when it succeeds, feeds
	// the change notification only — verbatim, never overlaid.
	fresh, refreshed := c.refreshBeadAfterWrite(id, "refresh bead after conditional update")
	c.evictForConditionalWrite(id)
	if refreshed {
		c.notifyChange("bead.updated", fresh)
	}
	return nil
}

// CloseIfMatch forwards the fenced close and maintains the cache. A post-close
// refresh that reports ErrNotFound is tolerated silently — backings that hide
// closed beads from Get do this on every successful close — and resolves to an
// evict, so the next read reports exactly what the backing itself would.
// Unlike the unconditional Close, a fenced re-close of an already-closed bead
// is not suppressed and re-fires bead.closed: fenced paths carry no
// idempotence short-circuits, and only the backing evaluates the fence.
func (c *CachingStore) CloseIfMatch(id string, expectedRevision int64) error {
	writer, ok := ConditionalWriterFor(c.conditionalBacking())
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	if err := writer.CloseIfMatch(id, expectedRevision); err != nil {
		c.applyConditionalWriteFailure(id, err)
		return err
	}
	fresh, err := c.backing.Get(id)
	c.evictForConditionalWrite(id)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			c.recordProblem("refresh bead after conditional close", fmt.Errorf("%s: %w", id, err))
		}
		return nil
	}
	// The close is proven committed; forcing the status onto the event
	// payload states that fact without installing anything in the cache.
	fresh.Status = "closed"
	c.notifyChange("bead.closed", fresh)
	return nil
}

// DeleteIfMatch forwards the fenced delete and, on success, mirrors the
// unconditional Delete's full scrub — the one place the deletedSeq stamp is
// correct, because the bead is actually gone.
func (c *CachingStore) DeleteIfMatch(id string, expectedRevision int64) error {
	writer, ok := ConditionalWriterFor(c.conditionalBacking())
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	deleted, haveDeleted := c.snapshotBeadBeforeDelete(id)
	if err := writer.DeleteIfMatch(id, expectedRevision); err != nil {
		c.applyConditionalWriteFailure(id, err)
		return err
	}

	c.mu.Lock()
	seq := c.noteLocalMutationLocked(id)
	c.tombstoneLocked(id, seq)
	c.clearDependentReadyProjectionsLocked(id)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	if haveDeleted {
		c.notifyChange("bead.deleted", deleted)
	}
	return nil
}

// CompareAndSetMetadataKey forwards the metadata CAS. There is deliberately no
// cached-value pre-check: only the backing evaluates the fence, and a cached
// value-match proves nothing about the revision. A clean value-loss
// (false, nil) evicts too — the cached value fed this process its losing
// `expected`, and without the evict a cross-process loser re-reads the same
// stale value through the cache and re-loses until an unrelated reconcile.
func (c *CachingStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	// Resolve the NARROW capability, not ConditionalWriter: a backing that can
	// do value-CAS but cannot soundly fence on a revision (NativeDoltStore)
	// declares MetadataCASWriter only, and every ConditionalWriter satisfies
	// MetadataCASWriter anyway, so this widens the backings that forward
	// without changing behavior for fully capable ones. The trio above keeps
	// resolving through ConditionalWriterFor — a narrow backing must not
	// unlock a revision fence it cannot honor.
	writer, ok := MetadataCASWriterFor(c.conditionalBacking())
	if !ok {
		return false, ErrConditionalWriteUnsupported
	}
	swapped, err := writer.CompareAndSetMetadataKey(id, key, expected, next)
	if err != nil {
		c.applyConditionalWriteFailure(id, err)
		return swapped, err
	}
	if !swapped {
		c.evictForConditionalWrite(id)
		return false, nil
	}
	fresh, refreshed := c.refreshBeadAfterWrite(id, "refresh bead after conditional metadata swap")
	c.evictForConditionalWrite(id)
	if refreshed {
		c.notifyChange("bead.updated", fresh)
	}
	return true, nil
}

// applyConditionalWriteFailure maps the backing writer's error class onto the
// cache action it dictates. A precondition failure proves the cached revision
// stale → evict. CAS exhaustion proves the backing revision kept moving under
// repeated re-reads → the cached row cannot be trusted either → evict. Gate
// refusal and unsupported prove the write did not commit and say nothing
// about this entry's freshness → no action. Anything else (transport
// failures, not-found, ambiguous may-have-committed errors) marks the entry
// dirty: the next Get re-reads the backing and re-primes, without dropping
// the entry from cached listings. The error itself is always returned to the
// caller untouched — the backing stores stamp ID/Expected/Current; this layer
// adds cache maintenance, not decoration.
func (c *CachingStore) applyConditionalWriteFailure(id string, err error) {
	switch {
	case IsPreconditionFailed(err), IsCASRetriesExhausted(err):
		c.evictForConditionalWrite(id)
	case IsGateRefusal(err), IsConditionalWriteUnsupported(err):
	default:
		// noteLocalMutationLocked bumps the mutation seq so a scan that
		// started before this failure cannot merge its pre-write row back
		// over the mark and delete it.
		c.mu.Lock()
		c.noteLocalMutationLocked(id)
		c.dirty[id] = struct{}{}
		c.mu.Unlock()
	}
}

// evictForConditionalWrite removes the cached entry so the next Get re-reads
// the backing store and re-primes (the dirty flag routes it there).
// noteLocalMutationLocked keeps a concurrent scan's merge-back from
// re-installing its stale row as CLEAN; prime's concurrent-mutation branch
// can still re-add a stale row for the missing id, but it leaves the dirty
// flag intact — the flag, not the entry's absence, is what keeps readers off
// stale state, so do not "simplify" the dirty-set away. deletedSeq is never
// stamped here: the bead still exists, and deletedSeq short-circuits Get to
// ErrNotFound without ever consulting the backing.
func (c *CachingStore) evictForConditionalWrite(id string) {
	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	delete(c.beads, id)
	delete(c.deps, id)
	c.dirty[id] = struct{}{}
	c.clearDependentReadyProjectionsLocked(id)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
}
