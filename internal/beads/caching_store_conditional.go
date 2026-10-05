package beads

import (
	"errors"
	"fmt"
	"slices"

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
// honest cache action after a fenced write is a miss.
//
// The miss is then filled the way the next Get would fill it, so steady fenced
// writes do not leave the cache dirty and withhold a covering census (see
// CacheRevision). The atomic closer returns the exact committed row, which is
// installed as returned when the evicted row was still the one the write was
// fenced on. The other verbs refetch the row and install it verbatim, never
// overlaid, only when it demonstrably reflects the committed write: the
// written fields match, and either the revision moved off expectedRevision or
// the evicted row was clean at expectedRevision (an idempotent write that the
// backend did not re-stamp). A lagged pre-write read otherwise stays a miss.
// Either install is fenced at the eviction's sequence and keeps that fence
// (seqKeep, like every write path), so a newer local write, event or delete
// wins. Afterwards Get, the dirty-row overlay, reconcile's merge, a Live list,
// Prime's concurrent-mutation path and PrimeActive fence on the write's
// writeSeq as well as its beadSeq, so none of them can install a row read
// before the write. An event with no cached row to merge onto is fenced on
// writeSeq and deletedSeq. A field-changing bead.updated is always verified
// against the backing; a conflicting dependency-only update or bead.created is
// verified while beadSeq is present or the local write is younger than
// recentWriteVerifyWindow (see CacheRevision for the remaining known limits).
// The refetched row feeds the change notification verbatim.
var (
	_ ConditionalWriter                = (*CachingStore)(nil)
	_ conditionalWritesModeCarrier     = (*CachingStore)(nil)
	_ conditionalWriteCapabilityProber = (*CachingStore)(nil)
)

// cachingAtomicConditionalCloser preserves cache eviction and notification
// while exposing the capability only for a backing that actually supports it.
type cachingAtomicConditionalCloser struct{ cache *CachingStore }

func (h *cachingAtomicConditionalCloser) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (Bead, error) {
	closer, ok := AtomicConditionalCloserFor(h.cache.conditionalBacking())
	if !ok {
		return Bead{}, ErrConditionalWriteUnsupported
	}
	before := h.cache.currentMutationSeq()
	h.cache.beginCloseIntent(id)
	closed, err := closer.CloseWithMetadataIfMatch(id, expectedRevision, metadata)
	if err != nil {
		h.cache.endCloseIntent(id)
		h.cache.applyConditionalWriteFailure(id, err)
		return Bead{}, err
	}
	// The returned row predates the eviction, so it is attributable only if
	// nothing touched the entry since the write was fenced: no mutation raced
	// the backing write (ev.prior), and the evicted row, if any, is still the
	// one at expectedRevision (a reconcile can absorb an external change
	// without a seq bump).
	ev, own := h.cache.evictForConditionalClose(id)
	if closed.ID == id && closed.Status == "closed" && ev.prior <= before &&
		(!ev.cached || ev.revision == expectedRevision) {
		h.cache.installAfterConditionalWrite(id, ev, closed)
	}
	if own {
		h.cache.notifyChange(ChangeLocal, "bead.closed", closed)
	}
	return closed, nil
}

// AtomicConditionalCloserHandle exposes the cache forwarding surface only when
// its resolved backing can perform the atomic terminal write. Returning the
// cache (rather than the backing) preserves eviction and notification semantics
// for callers that discover the optional capability through this handle.
func (c *CachingStore) AtomicConditionalCloserHandle() (AtomicConditionalCloser, bool) {
	if _, ok := AtomicConditionalCloserFor(c.conditionalBacking()); !ok {
		return nil, false
	}
	return &cachingAtomicConditionalCloser{cache: c}, true
}

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

// conditionalWritesStoreOpen reports whether the backing store is still open:
// cache and backing are one store instance for liveness, as for capability.
func (c *CachingStore) conditionalWritesStoreOpen() error {
	if liveness, ok := c.conditionalBacking().(conditionalWritesLiveness); ok {
		return liveness.conditionalWritesStoreOpen()
	}
	return nil
}

// UpdateIfMatch forwards the fenced update to the backing store's conditional
// writer and maintains the cache: on success it evicts the entry and installs
// the refetched row when that row reflects the write; on failure it acts per
// applyConditionalWriteFailure. A backing without the capability yields
// ErrConditionalWriteUnsupported — never an unconditional write. Labels pass
// through only to a writer that guards them; otherwise they are refused here,
// before the backing or the cache is touched.
func (c *CachingStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	writer, ok := ConditionalWriterFor(c.conditionalBacking())
	if err := validateConditionalUpdateOpts(opts, ok && conditionalLabelsGuarded(writer)); err != nil {
		return fmt.Errorf("conditional update %s: %w", id, err)
	}
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	if updateCloses(opts) {
		c.beginCloseIntent(id)
	}
	if err := writer.UpdateIfMatch(id, expectedRevision, opts); err != nil {
		if updateCloses(opts) {
			c.endCloseIntent(id)
		}
		c.applyConditionalWriteFailure(id, err)
		return err
	}
	// EVICT unconditionally, then refetch verbatim: installing local fields
	// over an independently-refreshed revision would fabricate a snapshot
	// that never existed (see the file comment). A status=closed update is a
	// close, announced as bead.closed when it owns the close, as Update does.
	eventType := "bead.updated"
	var ev conditionalEviction
	if updateCloses(opts) {
		var own bool
		ev, own = c.evictForConditionalClose(id)
		if own {
			eventType = "bead.closed"
		}
	} else {
		ev = c.evictForConditionalWrite(id)
	}
	fresh, err := c.refetchAfterConditionalWrite(id, ev, func(b Bead) bool {
		return ev.postWriteRevision(b, expectedRevision) && updateReflected(b, opts)
	})
	if err != nil {
		c.recordProblem("refresh bead after conditional update", fmt.Errorf("%s: %w", id, err))
		return nil
	}
	c.notifyChange(ChangeLocal, eventType, fresh)
	return nil
}

// CloseIfMatch forwards the fenced close and maintains the cache. A post-close
// refresh that reports ErrNotFound is tolerated silently — backings that hide
// closed beads from Get do this on every successful close — and resolves to an
// evict, so the next read reports exactly what the backing itself would.
// Unlike the unconditional Close, a fenced re-close of an already-closed bead
// is not short-circuited: fenced paths carry no idempotence short-circuits, and
// only the backing evaluates the fence. Its bead.closed is announced only when
// the write owns the close (claimCloseLocked), so a close the cache already
// announced is not announced twice.
func (c *CachingStore) CloseIfMatch(id string, expectedRevision int64) error {
	writer, ok := ConditionalWriterFor(c.conditionalBacking())
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	c.beginCloseIntent(id)
	if err := writer.CloseIfMatch(id, expectedRevision); err != nil {
		c.endCloseIntent(id)
		c.applyConditionalWriteFailure(id, err)
		return err
	}
	ev, own := c.evictForConditionalClose(id)
	fresh, err := c.refetchAfterConditionalWrite(id, ev, func(b Bead) bool {
		return ev.postWriteRevision(b, expectedRevision) && b.Status == "closed"
	})
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			c.recordProblem("refresh bead after conditional close", fmt.Errorf("%s: %w", id, err))
		}
		return nil
	}
	// The close is proven committed; forcing the status onto the event
	// payload states that fact without installing anything in the cache.
	setBeadStatus(&fresh, "closed")
	if own {
		c.notifyChange(ChangeLocal, "bead.closed", fresh)
	}
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
	c.markFreshLocked(c.clockNow())
	c.updateStatsLocked()
	c.mu.Unlock()
	if haveDeleted {
		c.notifyChange(ChangeLocal, "bead.deleted", deleted)
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
	ev := c.evictForConditionalWrite(id)
	fresh, err := c.refetchAfterConditionalWrite(id, ev, func(b Bead) bool {
		return b.Metadata[key] == next
	})
	if err != nil {
		c.recordProblem("refresh bead after conditional metadata swap", fmt.Errorf("%s: %w", id, err))
		return true, nil
	}
	c.notifyChange(ChangeLocal, "bead.updated", fresh)
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

// conditionalEviction records what evictForConditionalWrite removed. seq is
// the eviction's mutation sequence: the write's WriteRev and the fence for
// installing a post-write row, and scanGen the scan generation then, which
// fences it against scan merges. prior is the newest fence id carried before the
// eviction, against which a row obtained before it is checked. deps are the
// row's dependencies, which no conditional verb changes; the install carries
// them when the post-write row has no dependency fields of its own. cached,
// dirty and revision describe the evicted row.
type conditionalEviction struct {
	seq      uint64
	scanGen  uint64
	prior    uint64
	deps     []Dep
	hadDeps  bool
	cached   bool
	dirty    bool
	revision int64
}

// postWriteRevision reports whether b's revision is consistent with b being
// the row a successful fenced write against expectedRevision left behind:
// either the revision moved, or the evicted row was clean at expectedRevision,
// which makes an unmoved revision an idempotent write the backend did not
// re-stamp rather than a lagged pre-write read.
func (ev conditionalEviction) postWriteRevision(b Bead, expectedRevision int64) bool {
	return revisionMoved(b, expectedRevision) ||
		(ev.cached && !ev.dirty && ev.revision == expectedRevision)
}

// evictForConditionalWrite removes the cached entry so the next Get re-reads
// the backing store and re-primes (the dirty flag routes it there).
// noteLocalMutationLocked keeps a concurrent scan's merge-back from
// re-installing its stale row as CLEAN; prime's concurrent-mutation branch
// re-adds a missing id only when no local write followed its snapshot, and
// even then leaves the dirty flag intact — the flag, not the entry's absence,
// is what keeps readers off stale state, so do not "simplify" the dirty-set
// away. deletedSeq is never
// stamped here: the bead still exists, and deletedSeq short-circuits Get to
// ErrNotFound without ever consulting the backing.
func (c *CachingStore) evictForConditionalWrite(id string) conditionalEviction {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evictForConditionalWriteLocked(id)
}

// evictForConditionalClose is evictForConditionalWrite for a fenced write that
// left id closed. Under the same lock it first claims the bead.closed
// announcement (claimCloseLocked) from the row it is about to evict, and
// reports whether the write owns it: a close a concurrent read already
// installed and announced is not announced again, and one a read queued is
// announced by the write instead of the queue. A read after the eviction finds
// no held row, so it queues nothing.
func (c *CachingStore) evictForConditionalClose(id string) (conditionalEviction, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	own := c.claimCloseIntentLocked(id, true, false)
	return c.evictForConditionalWriteLocked(id), own
}

// evictForConditionalWriteLocked is evictForConditionalWrite's body. Caller
// must hold c.mu in write mode.
func (c *CachingStore) evictForConditionalWriteLocked(id string) conditionalEviction {
	deps, hadDeps := c.deps[id]
	row, cached := c.beads[id]
	_, dirty := c.dirty[id]
	ev := conditionalEviction{
		scanGen:  c.scanGen,
		prior:    max(c.beadSeq[id], c.deletedSeq[id], c.writeSeq[id]),
		deps:     cloneDeps(deps),
		hadDeps:  hadDeps,
		cached:   cached,
		dirty:    dirty,
		revision: row.Revision,
	}
	ev.seq = c.noteLocalMutationLocked(id)
	delete(c.beads, id)
	delete(c.deps, id)
	c.dirty[id] = struct{}{}
	c.clearDependentReadyProjectionsLocked(id)
	c.markFreshLocked(c.clockNow())
	c.updateStatsLocked()
	return ev
}

// revisionMoved reports whether b can be the row a successful fenced write
// against expectedRevision committed: a store with revisions mints a fresh one
// on every whole-row write, so a row still at expectedRevision is a lagged
// pre-write read. Zero on either side means no usable token.
func revisionMoved(b Bead, expectedRevision int64) bool {
	return expectedRevision == 0 || b.Revision == 0 || b.Revision != expectedRevision
}

// updateReflected reports whether b carries every field opts writes.
// validateConditionalUpdateOpts has already rejected the parent. An empty
// metadata value matches an absent key, since stores may clear a key either
// way. Every store applies RemoveLabels after Labels, so a label named in both
// must be absent.
func updateReflected(b Bead, opts UpdateOpts) bool {
	switch {
	case opts.Title != nil && b.Title != *opts.Title,
		opts.Status != nil && b.Status != *opts.Status,
		opts.Type != nil && b.Type != *opts.Type,
		opts.Priority != nil && (b.Priority == nil || *b.Priority != *opts.Priority),
		opts.Description != nil && b.Description != *opts.Description,
		opts.Assignee != nil && b.Assignee != *opts.Assignee:
		return false
	}
	for key, value := range opts.Metadata {
		if b.Metadata[key] != value {
			return false
		}
	}
	for _, label := range opts.RemoveLabels {
		if slices.Contains(b.Labels, label) {
			return false
		}
	}
	for _, label := range opts.Labels {
		if !slices.Contains(b.Labels, label) && !slices.Contains(opts.RemoveLabels, label) {
			return false
		}
	}
	return true
}

// refetchAfterConditionalWrite reads id back from the backing after a
// successful fenced write evicted it, and installs the row when reflects
// confirms it carries the committed write. Otherwise the entry stays dirty for
// the next Get. The row is returned verbatim either way.
func (c *CachingStore) refetchAfterConditionalWrite(id string, ev conditionalEviction, reflects func(Bead) bool) (Bead, error) {
	fresh, err := c.backing.Get(id)
	if err != nil {
		return Bead{}, err
	}
	if reflects(fresh) {
		c.installAfterConditionalWrite(id, ev, fresh)
	}
	return fresh, nil
}

// installAfterConditionalWrite installs row, which reflects the fenced write
// that ev evicted, as id's clean cached row. Dependencies come from row's own
// fields when it carries any, else from the evicted row, as the overlay does.
// The evicted row's is_blocked verdict is not carried: a blocker's status
// change while the row was absent could not invalidate it, so readiness
// answers from the dependency predicate, as after any refetch of an evicted
// row. It
// declines when the cache is not serving, a mutation newer than the eviction
// touched id, or a scan merged since the eviction (scanRacedLocked, which
// leaves id dirty unless the scan's row agrees with row), and it keeps the
// eviction's beadSeq fence so no older scan, event or refetch can overwrite
// the row afterwards.
func (c *CachingStore) installAfterConditionalWrite(id string, ev conditionalEviction, row Bead) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if (c.state != cacheLive && c.state != cachePartial) || c.refetchFencedLocked(id, ev.seq) ||
		c.scanRacedLocked(id, ev.scanGen, row, true) {
		return
	}
	// A row that omits its edges leaves the evicted, possibly pre-write, edge
	// set standing, so it does not answer a raced write's mark.
	opts := absorbOpts{depsMode: depsFromFields, seqMode: seqKeep, clearDirty: !ev.dirty || c.rowAnswersEdges(row)}
	if ev.hadDeps && !beadCarriesDependencyFields(row) {
		opts.depsMode = depsExplicit
		opts.deps = ev.deps
	}
	c.absorbFreshLocked(id, row, c.clockNow(), opts)
	c.markFreshLocked(c.clockNow())
	c.updateStatsLocked()
}

// currentMutationSeq reads the mutation sequence under the read lock.
func (c *CachingStore) currentMutationSeq() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mutationSeq
}
