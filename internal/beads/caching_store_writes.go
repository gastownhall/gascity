package beads

import (
	"errors"
	"fmt"
	"maps"
	"time"
)

// Create passes through to the backing store and updates the cache.
func (c *CachingStore) Create(b Bead) (Bead, error) {
	return c.createWith(func() (Bead, error) {
		return c.backing.Create(b)
	})
}

// CreateWithStorage passes through a policy-selected storage class to backing
// stores that support table-specific creates, then updates the cache.
func (c *CachingStore) CreateWithStorage(b Bead, storage StorageClass) (Bead, error) {
	storageBacking, ok := c.backing.(StorageCreateStore)
	if !ok {
		ephemeral, noHistory, err := effectiveStorageFlags(b, storage)
		if err != nil {
			return Bead{}, fmt.Errorf("cache create: %w", err)
		}
		b.Ephemeral = ephemeral
		b.NoHistory = noHistory
		return c.createWith(func() (Bead, error) {
			return c.backing.Create(b)
		})
	}
	return c.createWith(func() (Bead, error) {
		return storageBacking.CreateWithStorage(b, storage)
	})
}

func (c *CachingStore) createWith(create func() (Bead, error)) (Bead, error) {
	startSeq := c.currentMutationSeq()
	created, err := create()
	if err != nil {
		return created, err
	}

	if fresh, err := c.backing.Get(created.ID); err == nil {
		created = fresh
	} else if !errors.Is(err, ErrNotFound) {
		c.recordProblem("refresh bead after create", fmt.Errorf("%s: %w", created.ID, err))
	}

	c.mu.Lock()
	if c.racedWriteLocked(created.ID, startSeq) {
		// A local write to the new row landed after the refresh read.
		c.updateStatsLocked()
		c.mu.Unlock()
		c.notifyChange(ChangeLocal, "bead.created", created)
		return created, nil
	}
	c.noteLocalMutationLocked(created.ID)
	c.absorbFreshLocked(created.ID, created, c.clockNow(), absorbOpts{
		depsMode:   depsFromFieldsIfCarried,
		seqMode:    seqKeep,
		clearDirty: true,
	})
	c.markFreshLocked(c.clockNow())
	c.updateStatsLocked()
	c.mu.Unlock()

	c.notifyChange(ChangeLocal, "bead.created", created)
	return created, nil
}

// Update passes through to the backing store and refreshes the cache.
func (c *CachingStore) Update(id string, opts UpdateOpts) error {
	// Idempotence: if every non-nil field in opts already matches the
	// cached bead AND the cache is primed, the backing call is a no-op.
	// Skipping it avoids the bd subprocess invocation, the on_update
	// hook, and the post-update Get refresh — same payoff as the
	// SetMetadata short-circuit at metadataAlreadyMatchesCached.
	// See gastownhall/gascity#1978 Phase 1.
	if c.updateMatchesCached(id, opts) {
		return nil
	}
	startSeq := c.currentMutationSeq()
	if updateCloses(opts) {
		c.beginCloseIntent(id)
	}
	if err := c.backing.Update(id, opts); err != nil {
		if updateCloses(opts) {
			c.endCloseIntent(id)
		}
		return err
	}
	c.absorbUpdate(id, opts, startSeq)
	return nil
}

// UpdateIfAssignment forwards the guarded update to the backing store's
// capability and maintains the cache. It never short-circuits on the cached
// row the way Update does: the guard exists because that row may be stale, and
// only the backing can evaluate it. A landed update refreshes the row exactly
// as Update does. A rejected guard proves the cached row stale, so it is
// evicted and the next read fetches it again.
func (c *CachingStore) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts UpdateOpts) (bool, error) {
	updater, ok := AssignmentGuardedUpdaterFor(c.conditionalBacking())
	if !ok {
		return false, ErrConditionalWriteUnsupported
	}
	startSeq := c.currentMutationSeq()
	if updateCloses(opts) {
		c.beginCloseIntent(id)
	}
	updated, err := updater.UpdateIfAssignment(id, expectedStatus, expectedAssignee, opts)
	if (err != nil || !updated) && updateCloses(opts) {
		c.endCloseIntent(id)
	}
	switch {
	case err != nil:
		return false, err
	case !updated:
		c.evictForConditionalWrite(id)
		return false, nil
	}
	c.absorbUpdate(id, opts, startSeq)
	return true, nil
}

// absorbUpdate refreshes the cached row for id after opts landed on the
// backing store, starting from the backing's authoritative row.
func (c *CachingStore) absorbUpdate(id string, opts UpdateOpts, startSeq uint64) {
	fresh, err := c.backing.Get(id)
	c.mu.Lock()
	if updateCloses(opts) {
		// Every branch below claims (or declines) the close under this
		// lock, so the intent can end here.
		c.endCloseIntentLocked(id)
	}
	raced := c.racedWriteLocked(id, startSeq)
	if errors.Is(err, ErrNotFound) {
		closed, notifyClosed := c.patchedCachedRowLocked(id, func(b *Bead) { setBeadStatus(b, "closed") })
		// Only a held row's close is announced, once: a held row already
		// closed was announced by whoever installed it, unless a read queued
		// that close and has not drained it yet.
		notifyClosed = notifyClosed && c.claimCloseLocked(id, false, raced)
		if !raced {
			seq := c.noteLocalMutationLocked(id)
			c.tombstoneLocked(id, seq)
			c.clearDependentReadyProjectionsLocked(id)
			c.markFreshLocked(c.clockNow())
		}
		c.updateStatsLocked()
		c.mu.Unlock()
		// The write succeeded but the row is gone: this process closed
		// nothing, it read the row's absence. That is an inference, so a
		// consumer re-reads before treating it as a completion (mc-zndi7.60).
		if notifyClosed {
			c.notifyChange(ChangeRefresh, "bead.closed", closed)
		}
		return
	}
	if err != nil {
		patched, found := c.patchedCachedRowLocked(id, func(b *Bead) { *b = applyUpdateOptsToBead(*b, opts) })
		eventType := "bead.updated"
		if found {
			eventType = c.updateEventTypeLocked(id, patched, opts, raced)
		}
		if !raced {
			c.noteLocalMutationLocked(id)
			if found {
				c.absorbFreshLocked(id, patched, c.clockNow(), absorbOpts{
					depsMode:       depsKeepCached,
					seqMode:        seqKeep,
					clearDirty:     false,
					closeAnnounced: eventType == "bead.closed",
				})
				if opts.Status != nil {
					c.clearDependentReadyProjectionsLocked(id)
				}
			}
			c.markDirtyLocked(id)
		}
		c.updateStatsLocked()
		c.mu.Unlock()
		c.recordProblem("refresh bead after update", fmt.Errorf("%s: %w", id, err))
		if found {
			c.notifyChange(ChangeLocal, eventType, patched)
		}
		return
	}
	if raced {
		// Notify with the backing's row, not the write laid over it.
		eventType := c.updateEventTypeLocked(id, fresh, opts, true)
		c.updateStatsLocked()
		c.mu.Unlock()
		c.notifyChange(ChangeLocal, eventType, fresh)
		return
	}
	fresh = applyUpdateOptsToBead(fresh, opts)
	eventType := c.updateEventTypeLocked(id, fresh, opts, false)
	c.noteLocalMutationLocked(id)
	c.absorbFreshLocked(id, fresh, c.clockNow(), absorbOpts{
		depsMode:       depsFromFieldsIfCarried,
		seqMode:        seqKeep,
		clearDirty:     true,
		closeAnnounced: eventType == "bead.closed",
	})
	if opts.Status != nil {
		c.clearDependentReadyProjectionsLocked(id)
	}
	c.markFreshLocked(c.clockNow())
	c.updateStatsLocked()
	c.mu.Unlock()

	c.notifyChange(ChangeLocal, eventType, fresh)
}

// ReleaseIfCurrent clears an in-progress assignment through the backing store
// and refreshes the cache only when the conditional release succeeds.
func (c *CachingStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := c.backing.(ConditionalAssignmentReleaser)
	if !ok {
		return false, ErrConditionalReleaseUnsupported
	}
	startSeq := c.currentMutationSeq()
	released, err := releaser.ReleaseIfCurrent(id, expectedAssignee)
	if err != nil || !released {
		return released, err
	}

	fresh, refreshed := c.refreshBeadAfterWrite(id, "refresh bead after release-if-current")
	c.mu.Lock()
	raced := c.racedWriteLocked(id, startSeq)
	row, found := fresh, refreshed
	if !refreshed {
		row, found = c.patchedCachedRowLocked(id, func(b *Bead) {
			setBeadStatus(b, "open")
			b.Assignee = ""
			b.UpdatedAt = time.Now()
		})
	}
	if !raced {
		c.noteLocalMutationLocked(id)
		switch {
		case refreshed:
			c.absorbFreshLocked(id, row, c.clockNow(), absorbOpts{
				depsMode:   depsFromFieldsIfCarried,
				seqMode:    seqKeep,
				clearDirty: true,
			})
		case found:
			c.absorbFreshLocked(id, row, c.clockNow(), absorbOpts{
				depsMode:   depsKeepCached,
				seqMode:    seqKeep,
				clearDirty: false,
			})
			c.markDirtyLocked(id)
		default:
			c.markDirtyLocked(id)
		}
		c.clearDependentReadyProjectionsLocked(id)
		c.markFreshLocked(c.clockNow())
	}
	c.updateStatsLocked()
	c.mu.Unlock()
	if found {
		c.notifyChange(ChangeLocal, "bead.updated", row)
	}
	return true, nil
}

// TransferIfCurrent moves an in-progress assignment from one exact assignee
// spelling to another through the backing store, refreshing the cache only
// when the conditional transfer succeeds. It follows ReleaseIfCurrent's own
// shape immediately above -- the same capability family, with the opposite
// terminal assignee (toAssignee instead of none) -- and the same reasoning
// for the cache-only fallback branch: a transfer never changes a bead's
// status away from in_progress, so there is no ready-projection fan-out to
// clear the way ReleaseIfCurrent's open transition has.
func (c *CachingStore) TransferIfCurrent(id, fromAssignee, toAssignee string) (bool, error) {
	mover, ok := c.backing.(ConditionalAssigneeTransferer)
	if !ok {
		return false, ErrConditionalTransferUnsupported
	}
	startSeq := c.currentMutationSeq()
	moved, err := mover.TransferIfCurrent(id, fromAssignee, toAssignee)
	if err != nil || !moved {
		return moved, err
	}

	fresh, refreshed := c.refreshBeadAfterWrite(id, "refresh bead after transfer-if-current")
	c.mu.Lock()
	raced := c.racedWriteLocked(id, startSeq)
	row, found := fresh, refreshed
	if !refreshed {
		row, found = c.patchedCachedRowLocked(id, func(b *Bead) {
			b.Assignee = toAssignee
			b.UpdatedAt = time.Now()
		})
	}
	if !raced {
		c.noteLocalMutationLocked(id)
		switch {
		case refreshed:
			c.absorbFreshLocked(id, row, c.clockNow(), absorbOpts{
				depsMode:   depsFromFieldsIfCarried,
				seqMode:    seqKeep,
				clearDirty: true,
			})
		case found:
			c.absorbFreshLocked(id, row, c.clockNow(), absorbOpts{
				depsMode:   depsKeepCached,
				seqMode:    seqKeep,
				clearDirty: false,
			})
			c.markDirtyLocked(id)
		default:
			c.markDirtyLocked(id)
		}
		c.markFreshLocked(c.clockNow())
	}
	c.updateStatsLocked()
	c.mu.Unlock()
	if found {
		c.notifyChange(ChangeLocal, "bead.updated", row)
	}
	return true, nil
}

// Claim claims id for assignee through the backing store's two-argument
// compare-and-swap claim and writes the claimed row through to the cache. A
// conflict (ok=false, nil error) and any error pass straight through without
// touching the cache, as ReleaseIfCurrent's refusals do.
//
// The backing claim answers with the bare post-claim row: labels, dependency
// records and comments stripped (see NativeDoltStore.Claim). Installing it
// would replace the cached row wholesale and drop those fields, so Claim
// refreshes the full row from the backing store's Get and installs that. When
// the refresh fails, only what a claim changes is merged onto the cached row,
// which is marked dirty: status, assignee and the update time, plus the
// revision and metadata the claimed row carries, so a caller chaining a
// conditional write on the returned row does not present the pre-claim
// revision. With no cached row, the bare row is installed as a dirty
// placeholder, because this process has not confirmed its labels, dependencies
// or comments. Every branch keeps the cached dependency edges
// (depsKeepCached): a claim never changes them.
//
// Like every other writer, Claim installs nothing when a local write or Delete
// on id landed after the claim began (racedWriteLocked): that write's row, or
// its tombstone, stands. The claim committed either way, so it is still
// notified and returned, as the claim's own acquisition row rather than the
// refresh, which may already show that newer write. Outside a local race the
// returned row is the refreshed one, which can likewise include a remote write
// that landed after the claim.
func (c *CachingStore) Claim(id, assignee string) (Bead, bool, error) {
	claimer, ok := c.backing.(interface {
		Claim(id, assignee string) (Bead, bool, error)
	})
	if !ok {
		return Bead{}, false, ErrClaimUnsupported
	}
	startSeq := c.currentMutationSeq()
	claimed, acquired, err := claimer.Claim(id, assignee)
	if err != nil || !acquired {
		return claimed, acquired, err
	}

	fresh, refreshed := c.refreshBeadAfterWrite(id, "refresh bead after claim")
	c.mu.Lock()
	raced := c.racedWriteLocked(id, startSeq)
	row, found := fresh, refreshed
	if !refreshed {
		row, found = c.patchedCachedRowLocked(id, func(b *Bead) {
			setBeadStatus(b, claimed.Status)
			b.Assignee = claimed.Assignee
			b.UpdatedAt = claimed.UpdatedAt
			if claimed.Revision != 0 {
				b.Revision = claimed.Revision
			}
			if claimed.Metadata != nil {
				b.Metadata = maps.Clone(claimed.Metadata)
			}
		})
	}
	if !found || raced {
		row = claimed
	}
	if !raced {
		c.noteLocalMutationLocked(id)
		c.absorbFreshLocked(id, row, c.clockNow(), absorbOpts{
			depsMode:   depsKeepCached,
			seqMode:    seqKeep,
			clearDirty: refreshed,
		})
		if !refreshed {
			c.markDirtyLocked(id)
		}
		c.clearDependentReadyProjectionsLocked(id)
		c.markFreshLocked(c.clockNow())
	}
	c.updateStatsLocked()
	c.mu.Unlock()
	c.notifyChange(ChangeLocal, "bead.updated", row)
	return row, true, nil
}

// Close marks a bead as closed in the backing store and cache.
func (c *CachingStore) Close(id string) error {
	// Idempotence: if the cached bead status is already "closed" AND the
	// cache is primed, the backing call is a no-op. Skipping it avoids
	// the bd subprocess invocation, the on_update hook, and the
	// post-close Get refresh. See gastownhall/gascity#1978 Phase 1.
	if c.closeAlreadyMatchesCached(id) {
		return nil
	}
	startSeq := c.currentMutationSeq()
	c.beginCloseIntent(id)
	if err := c.backing.Close(id); err != nil {
		c.endCloseIntent(id)
		return err
	}

	// Adopt the successful refresh read: it carries the post-close revision,
	// which Get→conditional-write consumers fence against — patching only the
	// cached entry's status would keep serving the pre-close revision forever.
	// Status is still forced to closed: the close is proven committed, but
	// backings with read visibility lag can serve the pre-close row on this
	// refresh. A lagged revision is self-healing (a fenced write against it
	// precondition-fails and evicts); a lagged status is not — Get would
	// report a bead this process just closed as still active.
	var closed Bead
	refreshed := false
	if row, err := c.backing.Get(id); err == nil {
		closed, refreshed = row, true
	} else if !errors.Is(err, ErrNotFound) {
		c.recordProblem("refresh bead after close", fmt.Errorf("%s: %w", id, err))
	}

	c.mu.Lock()
	// The claim below runs under this same lock.
	c.endCloseIntentLocked(id)
	// A fenced close installs nothing but still notifies the closed row, as
	// an unfenced one does: the close committed either way.
	raced := c.racedWriteLocked(id, startSeq)
	found := refreshed
	if !refreshed {
		closed, found = c.patchedCachedRowLocked(id, func(*Bead) {})
	}
	setBeadStatus(&closed, "closed")
	// A concurrent read that installed and announced this close first owns
	// the announcement; announcing again here would duplicate it.
	announce := found && c.claimCloseLocked(id, true, raced)
	if !raced {
		c.noteLocalMutationLocked(id)
		if found {
			// A patch reflects only this write, so any mark stands.
			c.absorbFreshLocked(id, closed, c.clockNow(), absorbOpts{
				depsMode:       depsKeepCached,
				seqMode:        seqKeep,
				clearDirty:     refreshed,
				closeAnnounced: true,
			})
		}
		if c.clearDependentReadyProjectionsLocked(id) || found {
			c.markFreshLocked(c.clockNow())
		}
	}
	c.updateStatsLocked()
	c.mu.Unlock()

	if announce {
		c.notifyChange(ChangeLocal, "bead.closed", closed)
	}
	return nil
}

// Reopen marks a bead as open in the backing store and cache.
func (c *CachingStore) Reopen(id string) error {
	startSeq := c.currentMutationSeq()
	if err := c.backing.Reopen(id); err != nil {
		return err
	}

	// Adopt the successful refresh read with the status written through —
	// same reasoning as Close.
	var reopened, edgesUnread Bead
	refreshed := false
	if row, err := c.backing.Get(id); err == nil {
		reopened, refreshed = row, true
		// A close may have dropped the cached edges (CloseAll does). A row from
		// a backing whose rows omit their edges gets them from the backing, as
		// the overlay does; a complete row already answers. Without them the
		// refresh failed: installing the row would clear the mark on the
		// cached, possibly pre-write, edges.
		if !c.rowAnswersEdges(reopened) {
			if deps, depErr := c.backing.DepList(id, "down"); depErr == nil {
				reopened.Dependencies = deps
			} else {
				c.recordProblem("refresh deps after reopen", fmt.Errorf("%s: %w", id, depErr))
				edgesUnread, reopened, refreshed = reopened, Bead{}, false
			}
		}
	} else if !errors.Is(err, ErrNotFound) {
		c.recordProblem("refresh bead after reopen", fmt.Errorf("%s: %w", id, err))
	}

	c.mu.Lock()
	// Same as Close: a fenced reopen installs nothing and notifies the
	// reopened row.
	raced := c.racedWriteLocked(id, startSeq)
	found := refreshed
	if !refreshed {
		// The backing's reopen dropped metadata.close_reason; the patch
		// mirrors it.
		reopened, found = c.patchedCachedRowLocked(id, dropCloseReasonMetadata)
	}
	setBeadStatus(&reopened, "open")
	if !raced {
		c.noteLocalMutationLocked(id)
		switch {
		case refreshed:
			c.absorbFreshLocked(id, reopened, c.clockNow(), absorbOpts{
				depsMode:   depsFromFieldsIfCarried,
				seqMode:    seqKeep,
				clearDirty: true,
			})
		case found:
			// The patch reflects only this write, so any mark stands.
			c.absorbFreshLocked(id, reopened, c.clockNow(), absorbOpts{
				depsMode:   depsKeepCached,
				seqMode:    seqKeep,
				clearDirty: false,
			})
		default:
			// No cached row to patch: the reopened row is live in the
			// backing but missing here, so keep readers and the census off it.
			c.markDirtyLocked(id)
		}
		if c.clearDependentReadyProjectionsLocked(id) || found {
			c.markFreshLocked(c.clockNow())
		}
	}
	c.updateStatsLocked()
	c.mu.Unlock()

	if !found && edgesUnread.ID != "" {
		// No cached row took the reopen, but the backing read found the row:
		// it still notifies, as a refresh would have.
		reopened, found = edgesUnread, true
	}
	if found {
		c.notifyChange(ChangeLocal, "bead.updated", reopened)
	}
	return nil
}

// CloseAll closes multiple beads and sets metadata on each.
func (c *CachingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	startSeq := c.currentMutationSeq()
	n, err := c.backing.CloseAll(ids, metadata)
	if err != nil && n == 0 {
		return n, err
	}

	type refreshedBead struct {
		id   string
		bead Bead
	}
	refreshed := make([]refreshedBead, 0, len(ids))
	var refreshErr error
	refreshFailed := make(map[string]struct{})
	for _, id := range ids {
		fresh, getErr := c.backing.Get(id)
		if getErr != nil {
			refreshFailed[id] = struct{}{}
			refreshErr = errors.Join(refreshErr, fmt.Errorf("refresh bead after close-all %s: %w", id, getErr))
			continue
		}
		refreshed = append(refreshed, refreshedBead{id: id, bead: fresh})
	}

	notifications := make([]cacheNotification, 0, len(refreshed))
	c.mu.Lock()
	raced := c.racedWritesLocked(ids, startSeq)
	c.noteLocalMutationLocked(ids...)
	if refreshErr != nil {
		c.recordProblemLocked("close-all refresh", refreshErr)
	}
	for id := range refreshFailed {
		c.markDirtyLocked(id)
	}
	for _, item := range refreshed {
		// CloseAll announces only closes of rows it had cached, and only the
		// ones no concurrent read announced first.
		_, skip := raced[item.id]
		announce := item.bead.Status == "closed" && c.claimCloseLocked(item.id, false, skip)
		if !skip {
			opts := absorbOpts{depsMode: depsKeepCached, seqMode: seqKeep, clearDirty: true}
			if item.bead.Status == "closed" {
				opts.depsMode = depsDrop
				// Announced below whenever this close is claimed.
				opts.closeAnnounced = true
			}
			c.absorbFreshLocked(item.id, item.bead, c.clockNow(), opts)
			if item.bead.Status == "closed" {
				c.clearDependentReadyProjectionsLocked(item.id)
			}
		}
		if announce {
			notifications = append(notifications, cacheNotification{
				eventType: "bead.closed",
				bead:      cloneBead(item.bead),
			})
		}
	}
	c.markFreshLocked(c.clockNow())
	c.updateStatsLocked()
	c.mu.Unlock()
	c.notifyChanges(ChangeLocal, notifications)
	return n, errors.Join(err, refreshErr)
}

// SetMetadata sets a single metadata key-value on a bead.
func (c *CachingStore) SetMetadata(id, key, value string) error {
	// Idempotence: if the cached bead already has metadata[key] == value,
	// the backing call is a no-op semantically. Skipping it avoids the
	// bd subprocess invocation and — crucially — avoids firing bd's
	// on_update hook, which calls "gc event emit bead.updated" and
	// appends a line to the city's events.jsonl. Reconciler tick logic
	// repeatedly writes the same heartbeat / deferral fields every ~2s,
	// producing thousands of no-op events per hour. The cache is the
	// supervisor's authoritative read source, so a value-match here is
	// a value-match in the store.
	kvs := map[string]string{key: value}
	if c.metadataAlreadyMatchesCached(id, kvs) {
		return nil
	}
	startSeq := c.currentMutationSeq()
	if err := c.backing.SetMetadata(id, key, value); err != nil {
		return err
	}
	c.finishMetadataWrite(id, kvs, startSeq, "refresh bead after metadata")
	return nil
}

// SetMetadataBatch sets multiple metadata key-values on a bead.
func (c *CachingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	// Idempotence: see SetMetadata. If every kv pair already matches the
	// cached bead's metadata, skip the backing write — no bd subprocess,
	// no on_update hook fire, no events.jsonl entry. Reconciler ticks
	// re-stamp deferral timestamps and other "I observed this" markers
	// on every cycle; without this guard each cycle generates a
	// bead.updated event even when nothing changed.
	if c.metadataAlreadyMatchesCached(id, kvs) {
		return nil
	}
	startSeq := c.currentMutationSeq()
	if err := c.backing.SetMetadataBatch(id, kvs); err != nil {
		// The backing may have rejected, partially committed, or fully committed
		// the batch before returning an error. Fence the cached pre-write row
		// until an ordinary read installs backing truth.
		c.mu.Lock()
		c.noteMutationLocked(id)
		c.markDirtyLocked(id)
		c.mu.Unlock()
		return err
	}
	c.finishMetadataWrite(id, kvs, startSeq, "refresh bead after metadata batch")
	return nil
}

// finishMetadataWrite refreshes id after a committed SetMetadata or
// SetMetadataBatch and installs and notifies the result. When the refresh
// fails it patches kvs into a clone of the cached row instead, leaving any
// dirty mark as it was, or marks a row the cache does not hold dirty. A write
// racedWriteLocked fences installs nothing but still notifies.
func (c *CachingStore) finishMetadataWrite(id string, kvs map[string]string, startSeq uint64, op string) {
	fresh, refreshed := c.refreshBeadAfterWrite(id, op)
	c.mu.Lock()
	raced := c.racedWriteLocked(id, startSeq)
	row, found := fresh, refreshed
	if !refreshed {
		row, found = c.patchedCachedRowLocked(id, func(b *Bead) {
			if b.Metadata == nil {
				b.Metadata = make(map[string]string, len(kvs))
			}
			maps.Copy(b.Metadata, kvs)
		})
	}
	if !raced {
		c.noteLocalMutationLocked(id)
		switch {
		case refreshed:
			c.absorbFreshLocked(id, row, c.clockNow(), absorbOpts{
				depsMode:   depsFromFieldsIfCarried,
				seqMode:    seqKeep,
				clearDirty: true,
			})
		case found:
			// The patch reflects only this write, so any mark stands.
			c.absorbFreshLocked(id, row, c.clockNow(), absorbOpts{
				depsMode:   depsKeepCached,
				seqMode:    seqKeep,
				clearDirty: false,
			})
		default:
			c.markDirtyLocked(id)
		}
		c.markFreshLocked(c.clockNow())
	}
	c.updateStatsLocked()
	c.mu.Unlock()
	if found {
		c.notifyChange(ChangeLocal, "bead.updated", row)
	}
}

// SetLocalString delegates directly to the backing store. Clone-local data
// is never cached or notified: it isn't part of Bead.Metadata, so there is
// no cached Bead field to keep in sync, and (unlike SetMetadata) writing it
// never invokes bd's subprocess/hook path, so there is no idempotence check
// to save a redundant call.
func (c *CachingStore) SetLocalString(id, key, value string) error {
	return c.backing.SetLocalString(id, key, value)
}

// GetLocalString delegates directly to the backing store. See SetLocalString.
func (c *CachingStore) GetLocalString(id, key string) (string, error) {
	return c.backing.GetLocalString(id, key)
}

// patchedCachedRowLocked returns a clone of id's cached row with patch
// applied: what a write whose refresh failed installs or notifies. The clone
// never aliases the cached row, so neither patch nor a notification marshaled
// after unlock touches maps another write may be changing. Caller must hold
// c.mu.
func (c *CachingStore) patchedCachedRowLocked(id string, patch func(*Bead)) (Bead, bool) {
	b, ok := c.beads[id]
	if !ok {
		return Bead{}, false
	}
	b = cloneBead(b)
	patch(&b)
	return b, true
}

func (c *CachingStore) refreshBeadAfterWrite(id, op string) (Bead, bool) {
	fresh, err := c.backing.Get(id)
	if err != nil {
		c.recordProblem(op, fmt.Errorf("%s: %w", id, err))
		return Bead{}, false
	}
	return fresh, true
}

// refreshBeadWithDepsAfterWrite reads id and its edges back after a write. It
// reports false when either read failed; when only the edges of a row that
// omits them failed, it still returns the row, which must not be installed.
func (c *CachingStore) refreshBeadWithDepsAfterWrite(id, op string) (Bead, []Dep, bool) {
	fresh, ok := c.refreshBeadAfterWrite(id, op)
	if !ok {
		return Bead{}, nil, false
	}
	deps, err := c.backing.DepList(id, "down")
	if err != nil {
		c.recordProblem(op+" deps", fmt.Errorf("%s: %w", id, err))
		if !c.rowAnswersEdges(fresh) {
			// Field-derived edges would install the row's empty set as
			// clean. The row still returns, for a caller that notifies it.
			return fresh, nil, false
		}
		fresh.Dependencies = depsFromBeadFields(fresh)
		return fresh, cloneDeps(fresh.Dependencies), true
	}
	fresh.Dependencies = cloneDeps(deps)
	fresh.Needs = nil
	return fresh, cloneDeps(deps), true
}

// Tx executes fn through the backing store transaction and refreshes touched
// cache entries after a successful commit.
func (c *CachingStore) Tx(commitMsg string, fn func(Tx) error) error {
	if fn == nil {
		return errors.New("beads tx: nil callback")
	}
	tx := newCachingStoreTx()
	startSeq := c.currentMutationSeq()
	if err := c.backing.Tx(commitMsg, func(backingTx Tx) error {
		tx.backing = backingTx
		return fn(tx)
	}); err != nil {
		return err
	}
	c.refreshTxTouchedBeads(tx.ids, tx.closed, startSeq)
	return nil
}

// AtomicTx reports whether Tx is atomic, which for the caching store is exactly
// whether its backing store provides an atomic transaction: CachingStore.Tx is a
// transparent pass-through to backing.Tx, so it inherits the backing's
// all-or-nothing (or partial-write) failure semantics.
func (c *CachingStore) AtomicTx() bool { return StoreSupportsAtomicTx(c.backing) }

type cachingStoreTx struct {
	backing Tx
	seen    map[string]struct{}
	closed  map[string]struct{}
	ids     []string
}

func newCachingStoreTx() *cachingStoreTx {
	return &cachingStoreTx{
		seen:   make(map[string]struct{}),
		closed: make(map[string]struct{}),
	}
}

func (tx *cachingStoreTx) Create(b Bead) (Bead, error) {
	created, err := tx.backing.Create(b)
	if err != nil {
		return Bead{}, err
	}
	tx.touch(created.ID)
	return created, nil
}

func (tx *cachingStoreTx) Update(id string, opts UpdateOpts) error {
	if err := tx.backing.Update(id, opts); err != nil {
		return err
	}
	tx.touch(id)
	return nil
}

func (tx *cachingStoreTx) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	if err := tx.backing.SetMetadataBatch(id, kvs); err != nil {
		return err
	}
	tx.touch(id)
	return nil
}

func (tx *cachingStoreTx) Close(id string) error {
	if err := tx.backing.Close(id); err != nil {
		return err
	}
	tx.touch(id)
	tx.closed[id] = struct{}{}
	return nil
}

func (tx *cachingStoreTx) touch(id string) {
	if id == "" {
		return
	}
	if _, ok := tx.seen[id]; ok {
		return
	}
	tx.seen[id] = struct{}{}
	tx.ids = append(tx.ids, id)
}

type txTouchedBead struct {
	id     string
	bead   Bead
	found  bool
	closed bool
	err    error
}

func (c *CachingStore) refreshTxTouchedBeads(ids []string, closed map[string]struct{}, startSeq uint64) {
	if len(ids) == 0 {
		return
	}

	refreshed := make([]txTouchedBead, 0, len(ids))
	var refreshErr error
	for _, id := range ids {
		_, wasClosed := closed[id]
		fresh, err := c.backing.Get(id)
		item := txTouchedBead{id: id, closed: wasClosed, err: err}
		if err == nil {
			item.bead = fresh
			item.found = true
		} else if !wasClosed || !errors.Is(err, ErrNotFound) {
			refreshErr = errors.Join(refreshErr, fmt.Errorf("refresh bead after tx %s: %w", id, err))
		}
		refreshed = append(refreshed, item)
	}

	notifications := make([]cacheNotification, 0, len(refreshed))
	now := c.clockNow()
	c.mu.Lock()
	raced := c.racedWritesLocked(ids, startSeq)
	c.noteLocalMutationLocked(ids...)
	if refreshErr != nil {
		c.recordProblemLocked("tx refresh", refreshErr)
	}
	for _, item := range refreshed {
		_, skip := raced[item.id]
		if item.found {
			previous, hadPrevious := c.beads[item.id]
			fresh := cloneBead(item.bead)
			statusChanged := item.closed || fresh.Status == "closed"
			if hadPrevious && previous.Status != fresh.Status {
				statusChanged = true
			}
			// A closed row this refresh owns is announced as bead.closed; one
			// a concurrent read already announced is not.
			announceClose := fresh.Status == "closed" && c.claimCloseLocked(item.id, true, skip)
			if !skip {
				c.absorbFreshLocked(item.id, fresh, now, absorbOpts{
					depsMode:   depsFromFieldsIfCarried,
					seqMode:    seqKeep,
					clearDirty: true,
					// A closed row is announced below when claimed above.
					closeAnnounced: fresh.Status == "closed",
				})
				if statusChanged {
					c.clearDependentReadyProjectionsLocked(item.id)
				}
			}
			eventType := "bead.updated"
			if announceClose {
				eventType = "bead.closed"
			}
			if announceClose || !hadPrevious || beadChanged(previous, fresh, false) {
				notifications = append(notifications, cacheNotification{
					eventType: eventType,
					bead:      cloneBead(fresh),
				})
			}
			continue
		}
		if item.closed {
			if b, ok := c.patchedCachedRowLocked(item.id, func(b *Bead) { setBeadStatus(b, "closed") }); ok {
				announceClose := c.claimCloseLocked(item.id, true, skip)
				if !skip {
					// The patch reflects only this write, so any mark stands.
					c.absorbFreshLocked(item.id, b, now, absorbOpts{
						depsMode:       depsKeepCached,
						seqMode:        seqKeep,
						clearDirty:     false,
						closeAnnounced: true,
					})
					c.clearDependentReadyProjectionsLocked(item.id)
				}
				if announceClose {
					notifications = append(notifications, cacheNotification{
						eventType: "bead.closed",
						bead:      cloneBead(b),
					})
				}
			}
			continue
		}
		if item.err != nil && !skip {
			c.markDirtyLocked(item.id)
		}
	}
	c.markFreshLocked(now)
	c.updateStatsLocked()
	c.mu.Unlock()

	c.notifyChanges(ChangeLocal, notifications)
}

// updateMatchesCached returns true when every non-nil field in opts already
// reflects the cached bead's state AND the cache is primed. Returns false on
// cache miss, uninitialized cache, or any field mismatch — in which case the
// caller falls through to the backing write. Companion to
// metadataAlreadyMatchesCached but covers the full UpdateOpts surface
// (Title, Status, Type, Priority, Description, ParentID, Assignee, Metadata,
// Labels, RemoveLabels). See gastownhall/gascity#1978 Phase 1.
//
// The short-circuit path skips the deduplication that
// applyUpdateOptsToBead performs on the non-short-circuit pass. Cached
// bead labels come from bd/dolt's canonical state, which never produces
// duplicates, so a Labels-equal match here is a Labels-equal match in
// the store after applyUpdateOptsToBead would have run. If a future
// path injects duplicate labels into the cache, this short-circuit
// would skip the dedup-fixup — file an issue rather than relaxing the
// invariant here.
func (c *CachingStore) updateMatchesCached(id string, opts UpdateOpts) bool {
	if id == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return false
	}
	if _, dirty := c.dirty[id]; dirty {
		return false
	}
	b, ok := c.beads[id]
	if !ok {
		return false
	}
	if opts.Title != nil && b.Title != *opts.Title {
		return false
	}
	if opts.Status != nil {
		if b.Status != *opts.Status {
			return false
		}
		// bd's indefinite "deferred" status normalizes to open. An explicit
		// open update is nevertheless the operation that removes the deferral,
		// so it must reach the backing store.
		if *opts.Status == "open" && b.IndefinitelyDeferred {
			return false
		}
	}
	if opts.Type != nil && b.Type != *opts.Type {
		return false
	}
	if opts.Priority != nil {
		if b.Priority == nil || *b.Priority != *opts.Priority {
			return false
		}
	}
	if opts.Description != nil && b.Description != *opts.Description {
		return false
	}
	if opts.ParentID != nil && b.ParentID != *opts.ParentID {
		return false
	}
	if opts.Assignee != nil && b.Assignee != *opts.Assignee {
		return false
	}
	for k, v := range opts.Metadata {
		if b.Metadata == nil {
			if v != "" {
				return false
			}
			continue
		}
		if b.Metadata[k] != v {
			return false
		}
	}
	if len(opts.Labels) > 0 || len(opts.RemoveLabels) > 0 {
		// Set-equality check: opts.Labels ⊆ existing AND
		// (opts.RemoveLabels ∩ existing) = ∅ implies the final label set
		// after applyUpdateOptsToBead equals the current set. We skip
		// that function's dedup pass here — see the doc comment above
		// for why that's safe under bd/dolt's canonical labels.
		existing := make(map[string]struct{}, len(b.Labels))
		for _, l := range b.Labels {
			existing[l] = struct{}{}
		}
		for _, l := range opts.Labels {
			if _, present := existing[l]; !present {
				return false
			}
		}
		for _, l := range opts.RemoveLabels {
			if _, present := existing[l]; present {
				return false
			}
		}
	}
	return true
}

// closeAlreadyMatchesCached returns true when the cached bead status is
// already "closed" AND the cache is primed. Returns false on cache miss or
// uninitialized cache. See gastownhall/gascity#1978 Phase 1.
func (c *CachingStore) closeAlreadyMatchesCached(id string) bool {
	if id == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return false
	}
	if _, dirty := c.dirty[id]; dirty {
		return false
	}
	b, ok := c.beads[id]
	if !ok {
		return false
	}
	return b.Status == "closed"
}

// metadataAlreadyMatchesCached returns true when the cache holds a primed
// copy of the bead and every key/value in kvs is already present with the
// same value. A cache miss returns false (we cannot prove no-op), so the
// caller falls through to the backing write. Empty maps (no keys) match
// trivially, but callers should handle len==0 explicitly to avoid acquiring
// the lock for a guaranteed no-op.
func (c *CachingStore) metadataAlreadyMatchesCached(id string, kvs map[string]string) bool {
	if id == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return false
	}
	if _, dirty := c.dirty[id]; dirty {
		return false
	}
	b, ok := c.beads[id]
	if !ok {
		return false
	}
	if b.Metadata == nil {
		// Cache has the bead but no metadata map — any non-empty value
		// would be a write; an empty value (clearing a never-set key)
		// is already the desired state.
		for _, v := range kvs {
			if v != "" {
				return false
			}
		}
		return true
	}
	for k, v := range kvs {
		if b.Metadata[k] != v {
			return false
		}
	}
	return true
}

// DepAdd adds a dependency and updates the cache.
func (c *CachingStore) DepAdd(issueID, dependsOnID, depType string) error {
	startSeq := c.currentMutationSeq()
	if err := c.backing.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}

	fresh, deps, refreshed := c.refreshBeadWithDepsAfterWrite(issueID, "refresh bead after dependency add")
	c.mu.Lock()
	if c.racedWriteLocked(issueID, startSeq) {
		c.updateStatsLocked()
		c.mu.Unlock()
		if refreshed {
			c.notifyChange(ChangeLocal, "bead.updated", fresh)
		}
		return nil
	}
	c.noteLocalMutationLocked(issueID)
	if refreshed {
		c.absorbFreshLocked(issueID, fresh, c.clockNow(), absorbOpts{
			depsMode:   depsExplicit,
			deps:       deps,
			seqMode:    seqKeep,
			clearDirty: true,
		})
		c.clearReadyProjectionLocked(issueID)
		c.markFreshLocked(c.clockNow())
		c.updateStatsLocked()
		c.mu.Unlock()
		c.notifyChange(ChangeLocal, "bead.updated", fresh)
		return nil
	}
	if !c.depsComplete {
		if _, known := c.deps[issueID]; !known {
			c.markFreshLocked(c.clockNow())
			c.updateStatsLocked()
			c.mu.Unlock()
			return nil
		}
	}
	cachedDeps := c.deps[issueID]
	for i, d := range cachedDeps {
		if d.DependsOnID == dependsOnID {
			cachedDeps[i].Type = depType
			c.deps[issueID] = cachedDeps
			c.clearReadyProjectionLocked(issueID)
			c.markFreshLocked(c.clockNow())
			c.updateStatsLocked()
			c.mu.Unlock()
			return nil
		}
	}
	c.deps[issueID] = append(cachedDeps, Dep{IssueID: issueID, DependsOnID: dependsOnID, Type: depType})
	c.clearReadyProjectionLocked(issueID)
	c.markFreshLocked(c.clockNow())
	c.updateStatsLocked()
	c.mu.Unlock()
	return nil
}

// DepRemove removes a dependency and updates the cache.
func (c *CachingStore) DepRemove(issueID, dependsOnID string) error {
	startSeq := c.currentMutationSeq()
	if err := c.backing.DepRemove(issueID, dependsOnID); err != nil {
		return err
	}

	fresh, deps, refreshed := c.refreshBeadWithDepsAfterWrite(issueID, "refresh bead after dependency remove")
	c.mu.Lock()
	if c.racedWriteLocked(issueID, startSeq) {
		c.updateStatsLocked()
		c.mu.Unlock()
		if refreshed {
			c.notifyChange(ChangeLocal, "bead.updated", fresh)
		}
		return nil
	}
	c.noteLocalMutationLocked(issueID)
	if refreshed {
		c.absorbFreshLocked(issueID, fresh, c.clockNow(), absorbOpts{
			depsMode:   depsExplicit,
			deps:       deps,
			seqMode:    seqKeep,
			clearDirty: true,
		})
		c.clearReadyProjectionLocked(issueID)
		c.markFreshLocked(c.clockNow())
		c.updateStatsLocked()
		c.mu.Unlock()
		c.notifyChange(ChangeLocal, "bead.updated", fresh)
		return nil
	}
	if !c.depsComplete {
		if _, known := c.deps[issueID]; !known {
			c.markFreshLocked(c.clockNow())
			c.updateStatsLocked()
			c.mu.Unlock()
			return nil
		}
	}
	cachedDeps := c.deps[issueID]
	for i, d := range cachedDeps {
		if d.DependsOnID == dependsOnID {
			c.deps[issueID] = append(cachedDeps[:i], cachedDeps[i+1:]...)
			c.clearReadyProjectionLocked(issueID)
			break
		}
	}
	c.markFreshLocked(c.clockNow())
	c.updateStatsLocked()
	c.mu.Unlock()
	return nil
}

// Delete passes through to the backing store and removes from cache.
func (c *CachingStore) Delete(id string) error {
	deleted, haveDeleted := c.snapshotBeadBeforeDelete(id)
	if err := c.backing.Delete(id); err != nil {
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

// DeleteBatch forwards the batched delete capability to the backing store when
// it implements BatchDeleter, then evicts exactly those ids from the cache in
// one pass: it installs a deletion fence per id, drops their outgoing deps (via
// tombstoneLocked), and scrubs stale incoming edges from surviving beads so the
// cache mirrors the backend's ON DELETE CASCADE cleanup of edge rows. Because
// the backend orphans external dependents rather than recursively deleting them
// (see BatchDeleter), beads that merely depend on a deleted bead are preserved.
// This lets the wisp GC tear down a molecule closure with a single backing call
// instead of an O(subprocess-per-edge) loop. When the backing store lacks
// BatchDeleter, DeleteBatch falls back to a per-bead delete that first strips
// the dependency rows touching each id, so surviving dependents are orphaned
// rather than left with a dangling edge — the same contract a BatchDeleter
// backing gets from the schema's ON DELETE CASCADE. It satisfies BatchDeleter.
func (c *CachingStore) DeleteBatch(ids []string) error {
	cd, ok := c.backing.(BatchDeleter)
	if !ok {
		for _, id := range ids {
			if id == "" {
				continue
			}
			if err := c.deleteOrphaningDeps(id); err != nil {
				return err
			}
		}
		return nil
	}
	if len(ids) == 0 {
		return nil
	}

	// Snapshot bead.deleted payloads from the cache only. Unlike Delete, which
	// reads each snapshot from the backing store, forwarding N backing Get calls
	// here would reintroduce the per-bead subprocess storm this batch path exists
	// to remove; a bead absent from the cache is still evicted but emits no
	// event. In practice the wisp GC lists the closure just before deleting it,
	// so members are warm in the cache.
	deleted := make(map[string]struct{}, len(ids))
	c.mu.RLock()
	events := make([]Bead, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		deleted[id] = struct{}{}
		if b, ok := c.beads[id]; ok {
			events = append(events, cloneBead(b))
		}
	}
	c.mu.RUnlock()

	if err := cd.DeleteBatch(ids); err != nil {
		return c.reconcilePartialBatchDelete(err, events)
	}

	c.mu.Lock()
	c.evictBatchDeletedLocked(deleted)
	c.mu.Unlock()

	for _, b := range events {
		c.notifyChange(ChangeLocal, "bead.deleted", b)
	}
	return nil
}

// evictBatchDeletedLocked removes exactly the ids in deleted from the cache:
// per-id local-mutation fence + tombstone and dropped dependent ready
// projections, then a single scrub of inbound edges from survivors — mirroring
// the backend's ON DELETE CASCADE cleanup of the deleted beads' edge rows.
// Callers must hold c.mu.
func (c *CachingStore) evictBatchDeletedLocked(deleted map[string]struct{}) {
	for id := range deleted {
		seq := c.noteLocalMutationLocked(id)
		c.tombstoneLocked(id, seq)
		c.clearDependentReadyProjectionsLocked(id)
	}
	c.dropIncomingEdgesToDeletedLocked(deleted)
	c.markFreshLocked(c.clockNow())
	c.updateStatsLocked()
}

// reconcilePartialBatchDelete handles a backing DeleteBatch error. A
// chunk-committing backend (BdStore) can durably remove earlier chunks before a
// later chunk fails, reporting the removed ids via *BatchDeleteError. This
// evicts exactly those committed ids and fires their bead.deleted events so a
// mid-batch failure never leaves a deleted bead present-but-stale in the cache,
// while the uncommitted ids stay resident because the backing did not remove
// them. The original error is always returned so the caller still sees the
// failure; a backing that reports no committed ids leaves the cache untouched.
func (c *CachingStore) reconcilePartialBatchDelete(err error, events []Bead) error {
	var batchErr *BatchDeleteError
	if !errors.As(err, &batchErr) || len(batchErr.Committed) == 0 {
		return err
	}
	committed := make(map[string]struct{}, len(batchErr.Committed))
	for _, id := range batchErr.Committed {
		if id != "" {
			committed[id] = struct{}{}
		}
	}
	if len(committed) == 0 {
		return err
	}
	c.mu.Lock()
	c.evictBatchDeletedLocked(committed)
	c.mu.Unlock()
	for _, b := range events {
		if _, ok := committed[b.ID]; ok {
			c.notifyChange(ChangeLocal, "bead.deleted", b)
		}
	}
	return err
}

// deleteOrphaningDeps removes id after stripping every dependency row touching
// it in both directions, so surviving beads that depend on id are orphaned
// rather than left with a dangling edge. DeleteBatch uses it as the fallback
// when the backing store does not implement BatchDeleter: a bare backing Delete
// (MemStore, FileStore, and other non-BatchDeleter stores) drops only the bead
// row, so DeleteBatch must clean the edge rows itself to honor the same
// orphaning contract a BatchDeleter backing gets from the schema's ON DELETE
// CASCADE. It performs the same edge-strip-then-delete as the per-bead workflow
// delete (deleteWorkflowBead in cmd/gc), and the DepRemove/Delete methods it
// calls keep the cache coherent. Unlike deleteWorkflowBead it intentionally does
// not roll back already-stripped edges when a mid-strip DepRemove or the final
// Delete fails: this fallback runs only against non-BatchDeleter backings
// (MemStore/FileStore), whose in-memory edge operations do not fail partway, and
// the wisp GC re-collects and re-deletes any half-stripped member idempotently
// on a later tick.
func (c *CachingStore) deleteOrphaningDeps(id string) error {
	downDeps, err := c.DepList(id, "down")
	if err != nil {
		return fmt.Errorf("list down deps for %s: %w", id, err)
	}
	for _, dep := range downDeps {
		if err := c.DepRemove(id, dep.DependsOnID); err != nil {
			return fmt.Errorf("remove down dep %s -> %s: %w", id, dep.DependsOnID, err)
		}
	}
	upDeps, err := c.DepList(id, "up")
	if err != nil {
		return fmt.Errorf("list up deps for %s: %w", id, err)
	}
	for _, dep := range upDeps {
		if err := c.DepRemove(dep.IssueID, id); err != nil {
			return fmt.Errorf("remove up dep %s -> %s: %w", dep.IssueID, id, err)
		}
	}
	return c.Delete(id)
}

// dropIncomingEdgesToDeletedLocked scrubs cached dependency rows that point at a
// just-deleted bead, mirroring the backend's ON DELETE CASCADE cleanup of the
// deleted beads' edge rows. Surviving beads are kept; only their now-dangling
// edges into the deleted set are removed. Callers must hold c.mu.
func (c *CachingStore) dropIncomingEdgesToDeletedLocked(deleted map[string]struct{}) {
	for issueID, deps := range c.deps {
		if _, gone := deleted[issueID]; gone {
			continue
		}
		kept := deps[:0]
		changed := false
		for _, d := range deps {
			if _, gone := deleted[d.DependsOnID]; gone {
				changed = true
				continue
			}
			kept = append(kept, d)
		}
		if !changed {
			continue
		}
		if len(kept) == 0 {
			delete(c.deps, issueID)
		} else {
			c.deps[issueID] = kept
		}
		c.clearReadyProjectionLocked(issueID)
	}
}

func (c *CachingStore) snapshotBeadBeforeDelete(id string) (Bead, bool) {
	deleted, err := c.backing.Get(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Bead{}, false
		}
		c.recordProblem("snapshot bead before delete", fmt.Errorf("%s: %w", id, err))
		return Bead{}, false
	}
	return deleted, true
}

func applyUpdateOptsToBead(bead Bead, opts UpdateOpts) Bead {
	if opts.Title != nil {
		bead.Title = *opts.Title
	}
	if opts.Status != nil {
		setBeadStatus(&bead, *opts.Status)
	}
	if opts.Type != nil {
		bead.Type = *opts.Type
	}
	if opts.Priority != nil {
		bead.Priority = cloneIntPtr(opts.Priority)
	}
	if opts.Description != nil {
		bead.Description = *opts.Description
	}
	if opts.ParentID != nil {
		bead.ParentID = *opts.ParentID
	}
	if opts.Assignee != nil {
		bead.Assignee = *opts.Assignee
	}
	if len(opts.Metadata) > 0 {
		if bead.Metadata == nil {
			bead.Metadata = make(map[string]string, len(opts.Metadata))
		}
		for key, value := range opts.Metadata {
			bead.Metadata[key] = value
		}
	}
	if len(opts.Labels) > 0 || len(opts.RemoveLabels) > 0 {
		remove := make(map[string]struct{}, len(opts.RemoveLabels))
		for _, label := range opts.RemoveLabels {
			remove[label] = struct{}{}
		}

		labels := make([]string, 0, len(bead.Labels)+len(opts.Labels))
		seen := make(map[string]struct{}, len(bead.Labels)+len(opts.Labels))
		for _, label := range bead.Labels {
			if _, drop := remove[label]; drop {
				continue
			}
			if _, exists := seen[label]; exists {
				continue
			}
			labels = append(labels, label)
			seen[label] = struct{}{}
		}
		for _, label := range opts.Labels {
			if _, drop := remove[label]; drop {
				continue
			}
			if _, exists := seen[label]; exists {
				continue
			}
			labels = append(labels, label)
			seen[label] = struct{}{}
		}
		bead.Labels = labels
	}
	return bead
}
