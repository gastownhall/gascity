package opjournal

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// NewLaneStore is NewStore for conversation lanes, which write only through
// guarded batches. Before any I/O it requires beads.GuardedBatcherFor(store)
// and refuses otherwise with beads.ErrGuardedBatchUnsupported, so a BdStore
// (no batch applier behind the CLI) is refused without running NewStore's
// conditional-write probe.
func NewLaneStore(store beads.SessionStore, clk clock.Clock, opts ...Option) (*Store, error) {
	if isNilStore(store.Store) {
		return nil, errors.New("opening lane store: store is required")
	}
	batcher, ok := beads.GuardedBatcherFor(store)
	if !ok || batcher == nil {
		return nil, fmt.Errorf("opening lane store: %w", beads.ErrGuardedBatchUnsupported)
	}
	s, err := NewStore(store, clk, opts...)
	if err != nil {
		return nil, err
	}
	s.batcher = batcher
	return s, nil
}

// Runtime reads conversation conv's runtime row: the decoded row, its
// revision, and whether it exists. Absence returns (zero, 0, false, nil). An
// undecodable row returns an error wrapping ErrCorruptOp or
// ErrUnsupportedSchema.
func (s *Store) Runtime(conv string) (RuntimeRow, int64, bool, error) {
	b, found, err := s.runtimeBead(conv)
	if err != nil || !found {
		return RuntimeRow{}, 0, false, err
	}
	r, err := DecodeRuntimeRow(b)
	if err != nil {
		return RuntimeRow{}, 0, false, err
	}
	return r, b.Revision, true, nil
}

// runtimeBead reads conv's runtime row as stored.
func (s *Store) runtimeBead(conv string) (beads.Bead, bool, error) {
	id, err := RuntimeRowID(s.prefix, conv)
	if err != nil {
		return beads.Bead{}, false, err
	}
	b, err := s.store.Get(id)
	switch {
	case err == nil:
		return b, true, nil
	case errors.Is(err, beads.ErrNotFound):
		return beads.Bead{}, false, nil
	default:
		return beads.Bead{}, false, fmt.Errorf("reading runtime row %s: %w", id, err)
	}
}

// batchErr classifies a guarded batch's error.
type batchErr int

const (
	// batchOK: every item landed.
	batchOK batchErr = iota
	// batchRefused: an item's guard, pinned create or target refused the
	// batch; nothing was written.
	batchRefused
	// batchInvalid: the batch or the store is wrong; nothing was written. It
	// is a bug.
	batchInvalid
	// batchUnknown: the batch may or may not have committed; re-read.
	batchUnknown
)

// apply runs one guarded batch: through the lane store's batcher, or, on a
// store opened with NewStore, through its backend's when it has one (a
// Cancel that closes a lane respond with its occurrence's end). The returned
// error names the cause; for a refusal it is the *beads.BatchItemError.
func (s *Store) apply(ctx context.Context, reason string, items []beads.GuardedItem) (beads.GuardedResult, batchErr, error) {
	batcher := s.batcher
	if batcher == nil {
		batcher, _ = beads.GuardedBatcherFor(s.store)
	}
	if batcher == nil {
		return beads.GuardedResult{}, batchInvalid, fmt.Errorf("%w: the op store's backend applies no guarded batches", beads.ErrGuardedBatchUnsupported)
	}
	res, err := batcher.ApplyGuarded(ctx, beads.GuardedBatch{Reason: reason, Items: items})
	if err == nil {
		return res, batchOK, nil
	}
	var item *beads.BatchItemError
	switch {
	case errors.Is(err, beads.ErrInvalidGuardedBatch), errors.Is(err, beads.ErrGuardedBatchUnsupported):
		return beads.GuardedResult{}, batchInvalid, err
	case errors.As(err, &item):
		return beads.GuardedResult{}, batchRefused, err
	default:
		return beads.GuardedResult{}, batchUnknown, err
	}
}
