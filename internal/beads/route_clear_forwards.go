package beads

import (
	"context"

	"github.com/steveyegge/beads/journalops"
)

// Optional capabilities the route-clearing decorator forwards untouched. None
// of them writes gc.routed_to on an existing bead, so none passes through the
// gate (route_clear_gate.go); each is forwarded because the embedded Store field
// promotes only the methods of the Store interface, and an optional capability
// the decorator does not re-declare stops matching every type assertion made
// against it. A backing that lacks the capability answers the capability's own
// unsupported sentinel, which callers already treat as the cue to take the
// per-item path. Each capability has its own sentinel and its own consumer:
// ErrBatchCopyUnsupported for the foreign-id create and the dependency add,
// ErrDepRemoveBatchUnsupported, ErrConditionalTransferUnsupported,
// ErrDepListBatchUnsupported and ErrDepMetadataUnsupported for the others;
// ChildrenOfAny has no sentinel, it answers per parent.

var (
	_ ForeignIDBatchCreator           = (*routeChangeClearingStore)(nil)
	_ DepBatchWriter                  = (*routeChangeClearingStore)(nil)
	_ DepBatchRemover                 = (*routeChangeClearingStore)(nil)
	_ ConditionalAssigneeTransferer   = (*routeChangeClearingStore)(nil)
	_ DependencyBatchLister           = (*routeChangeClearingStore)(nil)
	_ DepMetadataBatchReader          = (*routeChangeClearingStore)(nil)
	_ OrderedReadyReader              = (*routeChangeClearingStore)(nil)
	_ AbsentBlockedReader             = (*routeChangeClearingStore)(nil)
	_ journalops.Journal              = (*routeChangeClearingStore)(nil)
	_ CachedReadExact                 = (*routeChangeClearingStore)(nil)
	_ ChildrenBatchLister             = (*routeChangeClearingStore)(nil)
	_ listDependencyCompletenessStore = (*completeEdgesRouteChangeClearingStore)(nil)
	_ routeClearingGate               = (*completeEdgesRouteChangeClearingStore)(nil)
)

// CreateWithForeignIDBatch forwards the backing's batched foreign-id create.
func (w *routeChangeClearingStore) CreateWithForeignIDBatch(rows []Bead) error {
	creator, ok := w.Store.(ForeignIDBatchCreator)
	if !ok {
		return ErrBatchCopyUnsupported
	}
	return creator.CreateWithForeignIDBatch(rows)
}

// DepAddBatch forwards the backing's batched edge write.
func (w *routeChangeClearingStore) DepAddBatch(edges []DepWithPayload) error {
	writer, ok := w.Store.(DepBatchWriter)
	if !ok {
		return ErrBatchCopyUnsupported
	}
	return writer.DepAddBatch(edges)
}

// DepRemoveBatch forwards the backing's batched edge removal.
func (w *routeChangeClearingStore) DepRemoveBatch(pairs []DepPair) error {
	remover, ok := w.Store.(DepBatchRemover)
	if !ok {
		return ErrDepRemoveBatchUnsupported
	}
	return remover.DepRemoveBatch(pairs)
}

// TransferIfCurrent forwards the backing's conditional assignee transfer. It
// writes the assignee only and takes no metadata.
func (w *routeChangeClearingStore) TransferIfCurrent(id, fromAssignee, toAssignee string) (bool, error) {
	mover, ok := w.Store.(ConditionalAssigneeTransferer)
	if !ok {
		return false, ErrConditionalTransferUnsupported
	}
	return mover.TransferIfCurrent(id, fromAssignee, toAssignee)
}

// DepListBatch forwards the backing's batched edge read.
func (w *routeChangeClearingStore) DepListBatch(ids []string) (map[string][]Dep, error) {
	lister, ok := w.Store.(DependencyBatchLister)
	if !ok {
		return nil, ErrDepListBatchUnsupported
	}
	return lister.DepListBatch(ids)
}

// DepMetadataBatch forwards the backing's batched edge-payload read.
func (w *routeChangeClearingStore) DepMetadataBatch(ids []string) (map[DepPair]string, error) {
	reader, ok := w.Store.(DepMetadataBatchReader)
	if !ok {
		return nil, ErrDepMetadataUnsupported
	}
	return reader.DepMetadataBatch(ids)
}

// ReadyOrdered forwards a bounded ready read through ReadyOrdered, which cuts in
// order when the backing can and reads the whole ready set otherwise, reporting
// which through ordered.
func (w *routeChangeClearingStore) ReadyOrdered(query ReadyQuery, order ReadyOrder) ([]Bead, bool, error) {
	return ReadyOrdered(w.Store, query, order)
}

// StrandsAbsentTargets reports whether the backing lists work held by an absent
// blocker (ReadyBlockedByAbsentFor), so the decorator never claims a listing its
// backing cannot serve.
func (w *routeChangeClearingStore) StrandsAbsentTargets() bool {
	_, ok := ReadyBlockedByAbsentFor(w.Store)
	return ok
}

// ReadyBlockedByAbsent forwards the backing's absent-blocker listing, and lists
// nothing when the backing strands no target.
func (w *routeChangeClearingStore) ReadyBlockedByAbsent(query ReadyQuery) ([]Bead, error) {
	reader, ok := ReadyBlockedByAbsentFor(w.Store)
	if !ok {
		return nil, nil
	}
	return reader.ReadyBlockedByAbsent(query)
}

// ChildrenOfAny forwards the backing's batched children read, so a cache above
// this decorator keeps reading a frontier's children in one round trip. A
// backing without the batch is read one parent at a time through the
// decorator's own Children (ChildrenOfAnyPerParent), the contract
// ChildrenBatchLister gives a forwarding wrapper.
func (w *routeChangeClearingStore) ChildrenOfAny(parentIDs []string, opts ...QueryOpt) ([]Bead, error) {
	batch, ok := w.Store.(ChildrenBatchLister)
	if !ok {
		return ChildrenOfAnyPerParent(w, parentIDs, opts...)
	}
	return batch.ChildrenOfAny(parentIDs, opts...)
}

// EventsJournalActive reports the backing's journal activation.
func (w *routeChangeClearingStore) EventsJournalActive() bool {
	return EventsJournalActiveThrough(w.Store)
}

// ReadEventsJournalPage pages the backing's journal, refusing with
// ErrEventsJournalUnsupported when the backing has none.
func (w *routeChangeClearingStore) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	return ReadEventsJournalPageThrough(ctx, w.Store, since, limit)
}

// CachedReadExact forwards the backing's declaration that a CachingStore over it
// holds rows exactly as its live reads return them (cached_read_exact.go). The
// decorator forwards enrichReadyProjectionForCache, which is what makes the
// cached ready projection exact for the engine, so it forwards the declaration
// with it: dropping the enrichment forward means this must report false in the
// same change.
func (w *routeChangeClearingStore) CachedReadExact() bool {
	declarer, ok := w.Store.(CachedReadExact)
	return ok && declarer.CachedReadExact()
}

// completeEdgesRouteChangeClearingStore is the decorator over a backing that
// declares its List rows carry their complete edge set, as BdStore, SQLiteStore,
// NativeDoltStore and ProxiedStore do. It adds only that declaration, so the
// decorator over a silent backing (MemStore, FileStore, an exec store) does not
// make it: a cache trusts the declaration's answer and treats a missing one as
// "read each row's edges one DepList at a time", so claiming completeness the
// backing never declared would make every cached ready read decline.
type completeEdgesRouteChangeClearingStore struct {
	*routeChangeClearingStore
}

// listIncludesCompleteDependencies answers the backing's own declaration at call
// time, never a value captured when the decorator was built, because a backing's
// answer changes with its live state. dependencySnapshotForCache is deliberately
// not forwarded, for the reason given there: a wrapper that claimed it would
// preempt this answer.
func (w *completeEdgesRouteChangeClearingStore) listIncludesCompleteDependencies() bool {
	completeness, ok := w.Store.(listDependencyCompletenessStore)
	return ok && completeness.listIncludesCompleteDependencies()
}
