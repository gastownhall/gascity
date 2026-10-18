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
// 0 hands each such write a token that matches nothing but a legacy row
// (mc-3c8h5).
//
// It returns nil, a *ListRevisionsBlindError, or the read error that kept it
// from deciding; a read error is no verdict on the store.
//
// It lists a bounded sample as consumers do, through store: the newest open
// rows. Open is a status a cache serves from memory even before its full
// prime, so at boot the sample costs no backing read.
//
// Each row listed at 0 is re-read live. A live 0 is a legacy row, and 0 is its
// real revision. A live revision that is not 0 is re-confirmed on a Live
// listing of that row through store (past a cache, to its backing): a legacy
// row written between the reads, or a cache that has not yet seen that write,
// lists off 0 there. A listing that still says 0 is evidence, and it is the
// refusal only when a Live listing of the whole sample carries no revision: a
// read path that drops revisions drops them on every row, while one row that
// disagrees beside rows that carry theirs is that row's own anomaly. The one
// known is an id held in both the issues and the wisps table, where the
// native List reads the wisps row (which its CAS also fences on) and Get the
// issues row. The sample's own revisions are no such evidence: a cache's rows
// may carry revisions installed from point reads (its own creates and
// updates, verified events), not from the list path. A listed revision that is non-zero but behind the live one is a
// stale read, which the conditional write itself refuses, and not this
// check's concern. An empty sample proves nothing either way and passes.
//
// The live point read goes through the store's declared resolve target, so a
// wrapper over a cache reaches the cache's backing rather than its own rows.
func CheckListRevisionFidelity(store Store) error {
	if store == nil {
		return nil
	}
	rows, err := store.List(ListQuery{Status: "open", Sort: SortCreatedDesc, Limit: listRevisionProbeRows})
	if err != nil {
		return fmt.Errorf("listing a sample: %w", err)
	}
	live := HandlesFor(followConditionalWritesResolveTarget(store)).Live
	var blind *ListRevisionsBlindError
	for _, row := range rows {
		if row.Revision != 0 || blind != nil {
			continue
		}
		current, err := live.Get(row.ID)
		switch {
		case errors.Is(err, ErrNotFound):
			continue
		case err != nil:
			return fmt.Errorf("reading %s live: %w", row.ID, err)
		case current.Revision == 0:
			continue
		}
		again, err := store.List(ListQuery{IDs: []string{row.ID}, IncludeClosed: true, TierMode: TierBoth, Live: true})
		if err != nil {
			return fmt.Errorf("listing %s live: %w", row.ID, err)
		}
		if len(again) != 1 || again[0].Revision != 0 {
			continue
		}
		blind = &ListRevisionsBlindError{ID: row.ID, Live: current.Revision}
	}
	if blind == nil {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	listed, err := store.List(ListQuery{IDs: ids, IncludeClosed: true, TierMode: TierBoth, Live: true})
	if err != nil {
		return fmt.Errorf("listing the sample live: %w", err)
	}
	for _, row := range listed {
		if row.Revision != 0 {
			return nil
		}
	}
	return blind
}

// ListRevisionSampleIsCheap reports whether CheckListRevisionFidelity's
// sample over store is cheap: a CachingStore answers it (store, or the store
// its declared resolve targets lead to), or the store is not a NativeDoltStore,
// whose uncached listing runs a counts query over whole tables (seconds at
// 80k rows) whatever the bound.
func ListRevisionSampleIsCheap(store Store) bool {
	if store == nil {
		return true
	}
	if _, ok := store.(*CachingStore); ok {
		return true
	}
	switch followConditionalWritesResolveTarget(store).(type) {
	case *CachingStore:
		return true
	case *NativeDoltStore:
		return false
	default:
		return true
	}
}
