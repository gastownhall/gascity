package beads

import (
	"errors"
	"fmt"
)

// listRevisionProbeRows bounds the rows CheckListRevisionFidelity samples.
const listRevisionProbeRows = 16

// ListRevisionsBlindError reports a store whose list read gave a row
// revision 0 while a live read of the same row gives a real one.
type ListRevisionsBlindError struct {
	ID   string
	Live int64
}

func (e *ListRevisionsBlindError) Error() string {
	return fmt.Sprintf("list reads carry no revision (%s listed at 0, live at %d)", e.ID, e.Live)
}

// CheckListRevisionFidelity reports whether store's list reads carry the
// revision its live reads do. A conditional write is only as good as the
// revision it is fenced on, and a consumer fences on rows it listed (directly,
// or through a cache primed from a listing): a store that lists every row at
// 0 hands each such write a token that matches nothing but a legacy row, and
// tells "absent" apart from that legacy row nowhere (mc-3c8h5).
//
// It lists a bounded sample as consumers do (through store, so a cache answers
// from its primed rows): the newest rows of both tiers, since a row written by
// current code is never at 0 and so is the sharpest evidence, and since that
// shape lets a native backing apply the bound itself rather than scan. It
// re-reads each row listed at 0 live. A row whose
// live revision is also 0 is a legacy row, and 0 is its real revision; a row
// listed at 0 but live at anything else is the refusal. A listed revision that
// is non-zero but behind the live one is a stale read, which the conditional
// write itself refuses, and not this check's concern. An empty sample proves
// nothing either way and passes.
//
// The live read goes through the store's declared resolve target, so a
// wrapper over a cache reaches the cache's backing rather than its own rows.
func CheckListRevisionFidelity(store Store) error {
	if store == nil {
		return nil
	}
	rows, err := store.List(ListQuery{
		AllowScan:                true,
		IncludeClosed:            true,
		TierMode:                 TierBoth,
		Sort:                     SortCreatedDesc,
		AllowBackingCreatedLimit: true,
		Limit:                    listRevisionProbeRows,
	})
	if err != nil {
		return fmt.Errorf("revision fidelity not checked: %w", err)
	}
	live := HandlesFor(followConditionalWritesResolveTarget(store)).Live
	for _, row := range rows {
		if row.Revision != 0 {
			continue
		}
		current, err := live.Get(row.ID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return fmt.Errorf("revision fidelity not checked: %w", err)
		case current.Revision != 0:
			return &ListRevisionsBlindError{ID: row.ID, Live: current.Revision}
		}
	}
	return nil
}
