package beads

import (
	"errors"
	"fmt"
)

// listRevisionProbeRows bounds the rows CheckListRevisionFidelity samples per
// status.
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

// ListRevisionsUndecidedError reports a sample that held no row able to decide
// whether store's list reads carry revisions: it was empty, or every row in it
// is a legacy row at revision 0 (or was removed while it was read).
type ListRevisionsUndecidedError struct {
	Sampled int
}

func (e *ListRevisionsUndecidedError) Error() string {
	if e.Sampled == 0 {
		return "no open or in-progress row to sample"
	}
	return fmt.Sprintf("the %d sampled rows are all legacy rows at revision 0", e.Sampled)
}

// CheckListRevisionFidelity reports whether store's list reads carry the
// revision its live reads do. A conditional write is only as good as the
// revision it is fenced on, and a consumer fences on rows it listed (directly,
// or through a cache primed from a listing): a store that lists every row at
// 0 hands each such write a token that matches nothing but a legacy row
// (mc-3c8h5).
//
// It returns nil, a *ListRevisionsBlindError, a *ListRevisionsUndecidedError,
// or the read error that kept it from deciding; neither of the last two is a
// verdict on the store.
//
// It samples the rows consumers fence, as they list them through store: the
// newest open and the newest in-progress rows. Those are the statuses a cache
// serves from memory even before its full prime, so at boot the sample costs no
// backing read. The sample's own revisions decide nothing: a cache's rows may
// carry revisions installed from point reads (its own creates and updates,
// verified events), not from the list path. The verdict comes from one Live
// listing of the sampled rows, on the cache's backing when store's declared
// resolve targets lead to a CachingStore (whose own Live listing still answers
// a row written around the check from its point-read copy), and through store
// otherwise: one backing read per check, which over native Dolt walks the
// non-closed ledger, as PrimeActive's open listing does.
//
// A row in that listing at a revision other than 0 admits the store: a read
// path that drops revisions drops them on every row, while one row at 0 beside
// rows that carry theirs is that row's own anomaly. The one known is an id held
// in both the issues and the wisps table, where the native List reads the
// wisps row (which its CAS also fences on) and Get the issues row. With every
// row at 0, a row is re-read live: a live 0 is a legacy row, and 0 is its real
// revision. A live revision that is not 0 is re-confirmed on a Live listing of
// that row alone, where a legacy row written between the reads lists off 0, and
// a listing that still says 0 is the refusal. A listed revision that is non-zero
// but behind the live one is a stale read, which the conditional write itself
// refuses, and not this check's concern.
//
// An empty sample, or one of legacy rows alone, proves nothing either way and
// is undecided, not a pass.
//
// The live point read goes through the store's declared resolve target, so a
// wrapper over a cache reaches the cache's backing rather than its own rows:
// this check is handed the class wrappers themselves, which have no Handles of
// their own.
func CheckListRevisionFidelity(store Store) error {
	if store == nil {
		return nil
	}
	var ids []string
	seen := make(map[string]bool)
	for _, status := range partialPrimeStatuses {
		rows, err := store.List(ListQuery{Status: status, Sort: SortCreatedDesc, Limit: listRevisionProbeRows, TierMode: TierBoth})
		if err != nil {
			return fmt.Errorf("listing a %s sample: %w", status, err)
		}
		for _, row := range rows {
			if !seen[row.ID] {
				seen[row.ID] = true
				ids = append(ids, row.ID)
			}
		}
	}
	if len(ids) == 0 {
		return &ListRevisionsUndecidedError{}
	}
	listed, err := listLive(store, ids)
	if err != nil {
		return fmt.Errorf("listing the sample live: %w", err)
	}
	for _, row := range listed {
		if row.Revision != 0 {
			return nil
		}
	}
	live := HandlesFor(followConditionalWritesResolveTarget(store)).Live
	for _, row := range listed {
		current, err := live.Get(row.ID)
		switch {
		case errors.Is(err, ErrNotFound):
			continue
		case err != nil:
			return fmt.Errorf("reading %s live: %w", row.ID, err)
		case current.Revision == 0:
			continue
		}
		again, err := listLive(store, []string{row.ID})
		if err != nil {
			return fmt.Errorf("listing %s live: %w", row.ID, err)
		}
		if len(again) != 1 {
			continue
		}
		if again[0].Revision != 0 {
			return nil
		}
		return &ListRevisionsBlindError{ID: row.ID, Live: current.Revision}
	}
	return &ListRevisionsUndecidedError{Sampled: len(ids)}
}

// listLive lists the non-closed rows among ids live: through store, or on the
// backing of the CachingStore its declared resolve targets lead to. That cache
// would answer a Live listing of a row written through it since the listing
// began, or within its recent-local window, with its own copy, whose revision
// came from a point read (refreshCachedBeads). The backing is listed as it is,
// not followed through its own resolve targets: a wrapper there may be the
// list path being checked.
func listLive(store Store, ids []string) ([]Bead, error) {
	query := ListQuery{IDs: ids, TierMode: TierBoth, Live: true}
	if cache, ok := followConditionalWritesResolveTarget(store).(*CachingStore); ok {
		return cache.Backing().List(query)
	}
	return store.List(query)
}

// ListRevisionCheckIsCheap reports whether CheckListRevisionFidelity over store
// is cheap: its verdict's Live listing reaches the store its declared resolve
// targets lead to, and past a CachingStore there to the cache's backing, and
// that store is not a NativeDoltStore, whose listing walks every non-closed row
// (seconds at 80k rows) whatever the bound.
func ListRevisionCheckIsCheap(store Store) bool {
	if store == nil {
		return true
	}
	target := followConditionalWritesResolveTarget(store)
	if cache, ok := target.(*CachingStore); ok {
		target = followConditionalWritesResolveTarget(cache.Backing())
	}
	_, native := target.(*NativeDoltStore)
	return !native
}
