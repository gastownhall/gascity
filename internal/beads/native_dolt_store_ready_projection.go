package beads

import (
	"context"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

// NativeDoltStore hands readiness to the cache — listIncludesCompleteDependencies
// reports true, which latches depsComplete — so it must also supply the column
// that makes the cache's answer equal its own. Losing this method silently
// reverts readiness to the weaker dependency-derived predicate, so pin it.
var _ readyProjectionEnrichmentStore = (*NativeDoltStore)(nil)

// enrichReadyProjectionForCache fills bd's denormalized is_blocked column onto
// items so a cache over this store answers readiness with the same predicate
// the store's own Ready() uses.
//
// It is not an optimization; it is what makes the cache's answer EQUAL the
// backing's. NativeDoltStore.listIncludesCompleteDependencies reports true, so
// a CachingStore over it latches depsComplete and serves readiness itself. With
// the column absent every Bead.IsBlocked is nil — beadFromNativeIssue cannot
// set it, because beadslib's types.Issue carries no is_blocked field — and
// cachedBeadReady then derives readiness from the bead's OWN direct
// blocks/waits-for/conditional-blocks deps. That fallback is weaker than the
// column in two ways that matter to gascity, which creates parent-child edges
// pervasively:
//
//   - is_blocked propagates transitively DOWN parent-child edges
//     (issueops.markBlockedTemplateForIssues joins `d.type = 'parent-child' AND
//     p.is_blocked = 1`), so a child of a blocked parent carries no blocking
//     edge of its own and reads ready to the fallback.
//   - cachedBeadReady treats a dep as blocking only when the target's status is
//     resident in the same cache, so an edge onto another store's row — a
//     relocated `gcg-` graph bead — is invisible to it.
//
// GetReadyWork filters `is_blocked = 0` (sqlbuild.ReadyWhere), so either gap
// offers the control dispatcher a step whose gate has not opened, the
// regression #3218 closed.
//
// The read is one batched IsBlockedBatch over the active set — the same
// SELECT id, is_blocked FROM {issues,wisps} WHERE id IN (...) that BdStore
// spends a `bd sql` subprocess on — issued in-process on the store's existing
// connection and inside withReadRetry, so it recovers from a managed-Dolt
// rebind like every other native read.
//
// A storage that cannot answer the column at all reports
// ErrReadyProjectionUnsupported rather than a silently weaker cache: the
// CachingStore then latches its degrade, declines every readiness handle, and
// takes the backing's own Ready. Slower, and correct. The core beadslib.Storage
// interface does not declare IsBlockedBatch (only DoltStorage composes
// DependencyQueryStore), so this is a real branch, not a defensive one.
func (s *NativeDoltStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	if len(items) == 0 {
		return items, nil
	}
	ids := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		// Same exclusions as the bd path: message and nudge rows are
		// notifications rather than dependency-blocked work, and stamping them
		// makes the CachingStore reconciler re-emit bead.updated every cycle.
		// Leaving their IsBlocked nil keeps the reconcile diff convergent.
		if skipBDReadyProjectionEnrichment(item) {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		ids = append(ids, item.ID)
	}
	if len(ids) == 0 {
		return items, nil
	}

	var projection map[string]bool
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		querier, ok := beadslib.AsBlockedQuerier(storage)
		if !ok {
			return fmt.Errorf("native ready projection: %w: storage %T does not expose beads.BlockedQuerier",
				ErrReadyProjectionUnsupported, storage)
		}
		blocked, err := querier.IsBlockedBatch(ctx, ids)
		if err != nil {
			return fmt.Errorf("native ready projection: %w", err)
		}
		projection = blocked
		return nil
	})
	if err != nil {
		return items, err
	}

	enriched := make([]Bead, len(items))
	copy(enriched, items)
	for i := range enriched {
		if skipBDReadyProjectionEnrichment(enriched[i]) {
			continue
		}
		blocked, ok := projection[enriched[i].ID]
		if !ok {
			// ids present in neither the issues nor the wisps table are absent
			// from the map. A row this store listed and then could not find is
			// one that raced out of the ledger; leave its last value rather
			// than inventing a verdict, matching the bd path exactly.
			continue
		}
		enriched[i].IsBlocked = cloneBoolPtr(&blocked)
	}
	return enriched, nil
}
