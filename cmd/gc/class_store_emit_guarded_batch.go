package main

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// ApplyGuarded forwards a guarded batch to the backing store's batcher and,
// once the whole batch has landed, records the events its items produced in
// one journal open: bead.created for each create, and bead.updated or
// bead.closed for each update as afterUpdateEmission decides. A refused batch
// wrote nothing and emits nothing. A create that cannot be re-read after the
// batch is skipped with a diagnostic (guardedCreatedEmission).
//
// Only an update that writes a status is read before the batch: one that
// leaves the status alone cannot open or close the row, so it is an update
// whatever the row's state. Each row is still re-read once after the batch for
// its payload; an operation-journal row's re-read names the conversation
// whose controller doorbell emit rings for it instead of a bead.* event
// (DES-O1 D18, class_store_emit_oprows.go). The pre-batch reads run before
// the backing
// validates the batch, so a batch it refuses as invalid still pays them.
//
// None of the wrapper's own reads outlast ctx: a caller bounds the batch by it
// (the in-band hook pass, whose output is kept only if it exits in time), and
// the emission is best-effort. A ctx that has ended before the batch, during
// a pre-batch read too, applies nothing and returns its cause, whatever the
// backing does with an ended ctx. Once ctx ends after the batch landed, the
// re-reads stop and nothing is journaled, with one diagnostic: the batch
// still succeeded, and the op-journal rows guarded batches write are read
// live by the lane host. Nothing rings the controller for them then, so
// they are announced only as a write no controller answered is
// (class_store_emit_oprows.go).
func (s *emittingClassStore) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	batcher, ok := beads.GuardedBatcherFor(s.Store)
	if !ok {
		return beads.GuardedResult{}, beads.ErrGuardedBatchUnsupported
	}
	wasClosed := make(map[string]bool, len(b.Items))
	for _, it := range b.Items {
		if err := context.Cause(ctx); err != nil {
			return beads.GuardedResult{}, err
		}
		if it.Update == nil {
			continue
		}
		if it.Update.Opts.Status == nil {
			wasClosed[it.Update.ID] = true
			continue
		}
		wasClosed[it.Update.ID] = s.closedBefore(it.Update.ID)
	}
	// Asked again past the last read: not every backing refuses an ended ctx
	// before it starts work.
	if err := context.Cause(ctx); err != nil {
		return beads.GuardedResult{}, err
	}
	res, err := batcher.ApplyGuarded(ctx, b)
	if err != nil {
		return beads.GuardedResult{}, err
	}
	emissions, err := s.guardedBatchEmissions(ctx, b, wasClosed)
	if err != nil {
		warnClassStoreEmit(fmt.Errorf("bead.* skipped for a guarded batch of %d items that landed: %w", len(b.Items), err))
		return res, nil
	}
	s.emit(emissions...)
	return res, nil
}

// guardedBatchEmissions re-reads each row a landed batch wrote for its event,
// wasClosed saying which updated rows were closed before it. It stops with
// ctx's cause once ctx ends, before a read or before the journal append.
func (s *emittingClassStore) guardedBatchEmissions(ctx context.Context, b beads.GuardedBatch, wasClosed map[string]bool) ([]classStoreEmission, error) {
	emissions := make([]classStoreEmission, 0, len(b.Items))
	for _, it := range b.Items {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		var (
			emission classStoreEmission
			ok       bool
		)
		if it.Create != nil {
			emission, ok = s.guardedCreatedEmission(it.Create.ID)
		} else {
			emission, ok = s.afterUpdateEmission(it.Update.ID, it.Update.Opts, wasClosed[it.Update.ID])
		}
		if ok {
			emissions = append(emissions, emission)
		}
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return emissions, nil
}

// guardedCreatedEmission is the bead.created row for a create a guarded batch
// landed. Unlike Create, the batch hands back no stored row to fall back on,
// only revisions, and the request is not what the store persisted: its status
// may be empty where the store wrote "open", and its timestamps and revision
// are the caller's. Journaling the request would clobber the fold (see
// snapshot), so a read miss skips the row, as emitGraphApplyCreated does.
func (s *emittingClassStore) guardedCreatedEmission(id string) (classStoreEmission, bool) {
	bead, ok := s.snapshot(id)
	if !ok {
		warnClassStoreEmit(fmt.Errorf("bead.created skipped: batch-created %s could not be re-read", id))
		return classStoreEmission{}, false
	}
	return classStoreEmission{eventType: events.BeadCreated, bead: bead}, true
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
