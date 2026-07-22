package beads

import (
	"errors"
	"fmt"
	"time"
)

// WithLifecycleMetadataTransaction serializes one bead's lifecycle metadata
// mutations with the cache's ordinary write paths and the backing store's
// lifecycle critical section.
func (c *CachingStore) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	if c == nil {
		return errors.New("lifecycle metadata transaction: nil CachingStore")
	}
	if fn == nil {
		return errors.New("lifecycle metadata transaction: nil callback")
	}

	unlock := c.lockExclusiveCloseState(id)
	var drains []func()
	defer func() {
		unlock()
		for _, drain := range drains {
			drain()
		}
	}()

	var tx *cachingLifecycleMetadataTransaction
	err := WithLifecycleMetadataTransaction(c.backing, id, func(backingTx LifecycleMetadataTransaction) error {
		tx = &cachingLifecycleMetadataTransaction{
			id:      id,
			store:   c,
			backing: backingTx,
		}
		callbackErr := fn(tx)
		if tx.snapshotErr != nil && !errors.Is(callbackErr, tx.snapshotErr) {
			callbackErr = errors.Join(callbackErr, tx.snapshotErr)
		}
		return callbackErr
	})
	// The backing lifecycle critical section has now resolved. Successful
	// mutations are non-rollbackable by contract, including those followed by a
	// callback/snapshot error, so install their cache state and publication
	// intent exactly once here rather than from a callback defer.
	if tx != nil {
		drains = append(drains, c.commitLifecycleMetadataSnapshots(tx)...)
	}
	return err
}

type cachingLifecycleMetadataTransaction struct {
	id                     string
	store                  *CachingStore
	backing                LifecycleMetadataTransaction
	snapshots              []Bead
	unsnapshottedMutations int
	pendingSnapshot        bool
	uncertainMutations     int
	snapshotErr            error
	closeAttempted         bool
	closedSnapshot         *Bead
	closeReceipt           *cacheObserverDelivery
	backingCloseDelivery   CloseObserverDelivery
}

func (tx *cachingLifecycleMetadataTransaction) Get() (Bead, error) {
	fresh, err := tx.backing.Get()
	tx.retainClosedSnapshot(fresh, err)
	return fresh, err
}

func (tx *cachingLifecycleMetadataTransaction) GetByID(id string) (Bead, error) {
	reader, ok := tx.backing.(LifecycleReadTransaction)
	if !ok {
		return Bead{}, ErrLifecycleReadUnsupported
	}
	fresh, err := reader.GetByID(id)
	if id == tx.id {
		tx.retainClosedSnapshot(fresh, err)
	}
	return fresh, err
}

func (tx *cachingLifecycleMetadataTransaction) List(query ListQuery) ([]Bead, error) {
	reader, ok := tx.backing.(LifecycleReadTransaction)
	if !ok {
		return nil, ErrLifecycleReadUnsupported
	}
	return reader.List(query)
}

func (tx *cachingLifecycleMetadataTransaction) retainClosedSnapshot(fresh Bead, err error) {
	if err == nil && tx.closeAttempted && fresh.ID == tx.id && fresh.Status == "closed" {
		closed := cloneBead(fresh)
		tx.closedSnapshot = &closed
	}
}

func (tx *cachingLifecycleMetadataTransaction) SetMetadata(key, value string) error {
	if err := tx.backing.SetMetadata(key, value); err != nil {
		tx.uncertainMutations++
		return err
	}
	return tx.retainSnapshot("metadata")
}

func (tx *cachingLifecycleMetadataTransaction) SetMetadataBatch(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	if err := tx.backing.SetMetadataBatch(values); err != nil {
		// External stores may apply only part of a metadata batch before
		// returning an error. Without an authoritative post-write snapshot the
		// cached row must be treated as stale rather than patched optimistically.
		tx.uncertainMutations++
		return err
	}
	return tx.retainSnapshot("metadata batch")
}

func (tx *cachingLifecycleMetadataTransaction) CloseWithReasonWithoutObserver(reason string) (LifecycleCloseResult, error) {
	closer, ok := tx.backing.(LifecycleCloseTransaction)
	if !ok {
		return LifecycleCloseResult{}, ErrLifecycleCloseUnsupported
	}
	tx.closeAttempted = true
	tx.closeReceipt = &cacheObserverDelivery{}
	result, err := closer.CloseWithReasonWithoutObserver(reason)
	tx.backingCloseDelivery = result.ObserverDelivery
	if result.AuthoritativeClosed(tx.id) {
		closed := cloneBead(result.After)
		tx.closedSnapshot = &closed
		tx.pendingSnapshot = false
	}
	result.ObserverDelivery = tx.closeReceipt
	return result, err
}

func (tx *cachingLifecycleMetadataTransaction) retainSnapshot(operation string) error {
	fresh, err := tx.backing.Get()
	if err != nil {
		tx.unsnapshottedMutations++
		tx.pendingSnapshot = true
		snapshotErr := fmt.Errorf("refreshing bead %q after lifecycle %s mutation: %w", tx.id, operation, err)
		if tx.snapshotErr == nil {
			tx.snapshotErr = snapshotErr
		} else {
			tx.snapshotErr = errors.Join(tx.snapshotErr, snapshotErr)
		}
		return snapshotErr
	}
	tx.snapshots = append(tx.snapshots, cloneBead(fresh))
	tx.pendingSnapshot = false
	return nil
}

func (c *CachingStore) commitLifecycleMetadataSnapshots(tx *cachingLifecycleMetadataTransaction) []func() {
	if len(tx.snapshots) == 0 && tx.unsnapshottedMutations == 0 && tx.uncertainMutations == 0 && !tx.closeAttempted {
		return nil
	}

	now := time.Now()
	notifications := make([]cacheNotification, 0, len(tx.snapshots))
	c.mu.Lock()
	for _, snapshot := range tx.snapshots {
		c.noteLocalMutationLocked(tx.id)
		c.absorbFreshLocked(tx.id, snapshot, now, absorbOpts{
			depsMode:   depsFromFields,
			seqMode:    seqKeep,
			clearDirty: true,
		})
		notifications = append(notifications, cacheNotification{
			eventType: "bead.updated",
			bead:      cloneBead(snapshot),
		})
	}
	for range tx.unsnapshottedMutations {
		c.noteLocalMutationLocked(tx.id)
	}
	for range tx.uncertainMutations {
		c.noteLocalMutationLocked(tx.id)
	}
	if tx.closeAttempted {
		c.noteLocalMutationLocked(tx.id)
		if tx.closedSnapshot != nil {
			c.absorbFreshLocked(tx.id, *tx.closedSnapshot, now, absorbOpts{
				depsMode:   depsFromFields,
				seqMode:    seqKeep,
				clearDirty: true,
			})
			c.clearDependentReadyProjectionsLocked(tx.id)
		} else {
			// A close acknowledgement without an authoritative post-close row is
			// ambiguous. Decline cached reads until recovery converges with live
			// storage instead of synthesizing a closed snapshot.
			c.markDirtyLocked(tx.id)
		}
	}
	if tx.unsnapshottedMutations > 0 || tx.uncertainMutations > 0 {
		c.markDirtyLocked(tx.id)
	}
	c.markFreshLocked(now)
	c.updateStatsLocked()
	c.mu.Unlock()

	// Reserve the successful snapshot notifications in the per-ID observer queue
	// BEFORE the pending gate for the later unsnapshotted mutation. The gate is a
	// non-delivering placeholder and drainOrderedChanges stops at it, so a gate
	// reserved first would strand every earlier successful snapshot behind it.
	// Successful snapshots are strictly earlier in mutation order than the
	// unsnapshotted mutation the gate stands in for, so they must be queued first.
	drains := c.enqueueOrderedChanges(notifications)
	if tx.pendingSnapshot {
		c.retainPendingPublication(tx.id, "bead.updated")
	}
	if tx.closeReceipt == nil {
		return drains
	}
	if tx.closedSnapshot == nil {
		return append(drains, func() { tx.closeReceipt.markDelivered() })
	}
	drain, delivery := c.enqueueOrderedBarrier(tx.id)
	if drain != nil {
		drains = append(drains, drain)
	}
	completeLifecycleCloseDelivery(tx.closeReceipt, tx.backingCloseDelivery, delivery)
	return drains
}

func completeLifecycleCloseDelivery(target *cacheObserverDelivery, deliveries ...CloseObserverDelivery) {
	var advance func(int)
	advance = func(index int) {
		for index < len(deliveries) && deliveries[index] == nil {
			index++
		}
		if index == len(deliveries) {
			target.markDelivered()
			return
		}
		deliveries[index].AfterDelivery(func() { advance(index + 1) })
	}
	advance(0)
}

var (
	_ LifecycleMetadataTransactionStore = (*CachingStore)(nil)
	_ LifecycleCloseTransaction         = (*cachingLifecycleMetadataTransaction)(nil)
	_ LifecycleReadTransaction          = (*cachingLifecycleMetadataTransaction)(nil)
)
