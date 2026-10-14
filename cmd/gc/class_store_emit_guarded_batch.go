package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
)

// ApplyGuarded forwards a guarded batch to the backing store's batcher and,
// once the whole batch has landed, records the events its items produced in
// one journal open: bead.created for each create, and bead.updated or
// bead.closed for each update as afterUpdateEmission decides. A refused batch
// wrote nothing and emits nothing.
//
// Only an update that writes a status is read before the batch: one that
// leaves the status alone cannot open or close the row, so it is an update
// whatever the row's state. Each row is still re-read once after the batch for
// its payload. Journal rows written through batches are expected to stop
// emitting bead.* events altogether (DES-O1 D18, V2-16); until then this keeps
// the cost to one read per item.
func (s *emittingClassStore) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	batcher, ok := beads.GuardedBatcherFor(s.Store)
	if !ok {
		return beads.GuardedResult{}, beads.ErrGuardedBatchUnsupported
	}
	wasClosed := make(map[string]bool, len(b.Items))
	for _, it := range b.Items {
		if it.Update == nil {
			continue
		}
		if it.Update.Opts.Status == nil {
			wasClosed[it.Update.ID] = true
			continue
		}
		wasClosed[it.Update.ID] = s.closedBefore(it.Update.ID)
	}
	res, err := batcher.ApplyGuarded(ctx, b)
	if err != nil {
		return beads.GuardedResult{}, err
	}
	emissions := make([]classStoreEmission, 0, len(b.Items))
	for _, it := range b.Items {
		var (
			emission classStoreEmission
			ok       bool
		)
		if it.Create != nil {
			emission, ok = s.createdEmission(*it.Create)
		} else {
			emission, ok = s.afterUpdateEmission(it.Update.ID, it.Update.Opts, wasClosed[it.Update.ID])
		}
		if ok {
			emissions = append(emissions, emission)
		}
	}
	s.emit(emissions...)
	return res, nil
}

// GuardedBatcherHandle keeps beads.GuardedBatcherFor honest over this
// wrapper, as AtomicConditionalCloserHandle does: the reflective capability
// guard forces ApplyGuarded to exist for every engine, so a bare assertion
// would advertise guarded batches over a backing that has none. This answers
// yes only when the backing can apply one, and returns the wrapper so the
// discovered batcher still emits.
func (s *emittingClassStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	if _, ok := beads.GuardedBatcherFor(s.Store); !ok {
		return nil, false
	}
	return s, true
}
