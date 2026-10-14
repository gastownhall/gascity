package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
)

// ApplyGuarded forwards a guarded batch to the backing store's batcher and,
// once the whole batch has landed, records the events its items produced:
// bead.created for each create, and bead.updated or bead.closed for each update
// as emitAfterUpdate decides. A refused batch wrote nothing and emits nothing.
func (s *emittingClassStore) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	batcher, ok := beads.GuardedBatcherFor(s.Store)
	if !ok {
		return beads.GuardedResult{}, beads.ErrGuardedBatchUnsupported
	}
	wasClosed := make(map[string]bool, len(b.Items))
	for _, it := range b.Items {
		if it.Update != nil {
			wasClosed[it.Update.ID] = s.closedBefore(it.Update.ID)
		}
	}
	res, err := batcher.ApplyGuarded(ctx, b)
	if err != nil {
		return beads.GuardedResult{}, err
	}
	for _, it := range b.Items {
		if it.Create != nil {
			s.emitCreated(*it.Create, nil)
			continue
		}
		s.emitAfterUpdate(it.Update.ID, it.Update.Opts, wasClosed[it.Update.ID])
	}
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
