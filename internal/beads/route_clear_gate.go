package beads

import (
	"context"
	"log"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The route-clearing gate at the capability seam (ga-l69b9m).
//
// Every capability resolver (MetadataCASWriterFor, ConditionalWriterForTarget,
// AssignmentGuardedUpdaterFor, ResolveConditionalWriter) peels a
// routeChangeClearingStore through its ConditionalWritesResolveTarget, so a
// gc.routed_to write made through a capability would reach the engine without
// passing the decorator's own SetMetadata / SetMetadataBatch / Update. When a
// resolve walk crosses the decorator, the resolver hands back one of the
// wrappers below instead of the bare capability. A wrapper passes every write
// to any other key straight through; a gc.routed_to write reads the bead first,
// delegates, and runs the decorator's clearIfGenuine only when the write landed.
//
// The decorator itself stays a non-conditional wrapper that declares its
// target. It is not a structural writer and not a mode source, so the rollout
// mode, liveness, capability probe and degrade latch still come from the
// terminal store, exactly as before.

// followResolveTargetThroughGate is followConditionalWritesResolveTarget that
// also reports the outermost route-clearing decorator the walk crossed, or nil
// when it crossed none.
func followResolveTargetThroughGate(store Store) (Store, *routeChangeClearingStore) {
	var gate *routeChangeClearingStore
	for range conditionalWritesMaxResolveDepth {
		if g := routeClearingOf(store); g != nil && gate == nil {
			gate = g
		}
		targeter, ok := store.(ConditionalWritesResolveTargeter)
		if !ok {
			return store, gate
		}
		target := targeter.ConditionalWritesResolveTarget()
		if target == nil || target == store {
			return store, gate
		}
		store = target
	}
	return store, gate
}

// guardRoutedToWrite runs write, a gc.routed_to write of newTarget on bead id
// that reaches the engine through a capability, and clears the bead's
// executor-identity stamps when it landed and the route genuinely changed.
// write reports whether it landed. A landed write is never failed by the
// clear, which clearIfGenuine logs and swallows. When the bead cannot be read
// first the write still proceeds, without a clear, and that is logged.
func (w *routeChangeClearingStore) guardRoutedToWrite(id, newTarget string, write func() (landed bool, err error)) (bool, error) {
	before, beforeErr := w.Get(id)
	landed, err := write()
	if !landed || err != nil {
		return landed, err
	}
	if beforeErr != nil {
		log.Printf("beads route-clear: reading %s before its gc.routed_to write to %q failed, so its executor-identity stamps were not cleared: %v", id, newTarget, beforeErr)
		return landed, err
	}
	w.clearIfGenuine(id, before, before.Metadata[beadmeta.RoutedToMetadataKey], newTarget)
	return landed, err
}

// gatedMetadataCAS is a MetadataCASWriter that clears stamps after a landed
// gc.routed_to swap.
type gatedMetadataCAS struct {
	MetadataCASWriter
	gate *routeChangeClearingStore
}

// CompareAndSetMetadataKey swaps through the wrapped writer and, for
// gc.routed_to, runs the gate around the swap.
func (g gatedMetadataCAS) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if key != beadmeta.RoutedToMetadataKey {
		return g.MetadataCASWriter.CompareAndSetMetadataKey(id, key, expected, next)
	}
	return g.gate.guardRoutedToWrite(id, next, func() (bool, error) {
		return g.MetadataCASWriter.CompareAndSetMetadataKey(id, key, expected, next)
	})
}

// gateMetadataCAS wraps writer in the gate. A nil gate or nil writer returns
// writer unchanged.
func (w *routeChangeClearingStore) gateMetadataCAS(writer MetadataCASWriter) MetadataCASWriter {
	if w == nil || writer == nil {
		return writer
	}
	return gatedMetadataCAS{MetadataCASWriter: writer, gate: w}
}

// gatedConditionalWriter is a ConditionalWriter that clears stamps after a
// landed gc.routed_to write through its metadata CAS or its fenced update.
type gatedConditionalWriter struct {
	ConditionalWriter
	gate *routeChangeClearingStore
}

// CompareAndSetMetadataKey is the gated metadata CAS.
func (g gatedConditionalWriter) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	return gatedMetadataCAS{MetadataCASWriter: g.ConditionalWriter, gate: g.gate}.CompareAndSetMetadataKey(id, key, expected, next)
}

// UpdateIfMatch applies the fenced update through the wrapped writer and, when
// opts sets gc.routed_to, runs the gate around it.
func (g gatedConditionalWriter) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	newTarget, routes := opts.Metadata[beadmeta.RoutedToMetadataKey]
	if !routes {
		return g.ConditionalWriter.UpdateIfMatch(id, expectedRevision, opts)
	}
	_, err := g.gate.guardRoutedToWrite(id, newTarget, func() (bool, error) {
		err := g.ConditionalWriter.UpdateIfMatch(id, expectedRevision, opts)
		return err == nil, err
	})
	return err
}

// conditionalLabelsGuarded forwards the wrapped writer's label guard, which
// CachingStore.UpdateIfMatch asks of the writer it resolved.
func (g gatedConditionalWriter) conditionalLabelsGuarded() bool {
	return conditionalLabelsGuarded(g.ConditionalWriter)
}

// gateConditionalWriter wraps writer in the gate. A nil gate or nil writer
// returns writer unchanged.
func (w *routeChangeClearingStore) gateConditionalWriter(writer ConditionalWriter) ConditionalWriter {
	if w == nil || writer == nil {
		return writer
	}
	return gatedConditionalWriter{ConditionalWriter: writer, gate: w}
}

// gatedAssignmentUpdater is an AssignmentGuardedUpdater that clears stamps
// after a landed gc.routed_to update.
type gatedAssignmentUpdater struct {
	AssignmentGuardedUpdater
	gate *routeChangeClearingStore
}

// UpdateIfAssignment applies the guarded update through the wrapped updater
// and, when opts sets gc.routed_to, runs the gate around it.
func (g gatedAssignmentUpdater) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts UpdateOpts) (bool, error) {
	newTarget, routes := opts.Metadata[beadmeta.RoutedToMetadataKey]
	if !routes {
		return g.AssignmentGuardedUpdater.UpdateIfAssignment(id, expectedStatus, expectedAssignee, opts)
	}
	return g.gate.guardRoutedToWrite(id, newTarget, func() (bool, error) {
		return g.AssignmentGuardedUpdater.UpdateIfAssignment(id, expectedStatus, expectedAssignee, opts)
	})
}

// gateAssignmentUpdater wraps updater in the gate. A nil gate or nil updater
// returns updater unchanged.
func (w *routeChangeClearingStore) gateAssignmentUpdater(updater AssignmentGuardedUpdater) AssignmentGuardedUpdater {
	if w == nil || updater == nil {
		return updater
	}
	return gatedAssignmentUpdater{AssignmentGuardedUpdater: updater, gate: w}
}

// gatedGuardedBatcher is a GuardedBatcher that clears stamps after a landed
// guarded batch whose Update items write gc.routed_to. It holds the wrapped
// batcher in a named field so its method set is ApplyGuarded alone.
type gatedGuardedBatcher struct {
	inner GuardedBatcher
	gate  *routeChangeClearingStore
}

// routedBatchItem is one Update item that writes gc.routed_to, with the bead's
// state read before the batch.
type routedBatchItem struct {
	id        string
	newTarget string
	before    Bead
	beforeErr error
}

// routedBatchItems returns the Update items of b that write gc.routed_to. A
// Create never qualifies: a new row has no earlier route to supersede.
func routedBatchItems(b GuardedBatch) []routedBatchItem {
	var items []routedBatchItem
	for _, it := range b.Items {
		if it.Update == nil {
			continue
		}
		if target, ok := it.Update.Opts.Metadata[beadmeta.RoutedToMetadataKey]; ok {
			items = append(items, routedBatchItem{id: it.Update.ID, newTarget: target})
		}
	}
	return items
}

// ApplyGuarded applies b through the wrapped batcher. A batch with no
// gc.routed_to Update goes straight through, with no extra read. Otherwise each
// such bead is read first, the batch is delegated unchanged, and only a batch
// that landed (nil error) runs the clear for each pre-read bead. Any refusal
// returns the batcher's result and error as they are, with nothing cleared.
func (g gatedGuardedBatcher) ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	items := routedBatchItems(b)
	if len(items) == 0 {
		return g.inner.ApplyGuarded(ctx, b)
	}
	for i := range items {
		items[i].before, items[i].beforeErr = g.gate.Get(items[i].id)
	}
	// Asked again past the pre-reads: not every backing refuses an ended ctx
	// before it starts work, and the cause is what the caller waits on.
	if err := context.Cause(ctx); err != nil {
		return GuardedResult{}, err
	}
	res, err := g.inner.ApplyGuarded(ctx, b)
	if err != nil {
		return res, err
	}
	if g.gate.clearRoutedBatchItems(items) {
		g.gate.refreshBatchRevisions(&res)
	}
	return res, nil
}

// clearRoutedBatchItems runs the clear for each landed item whose pre-read
// succeeded and reports whether any clear ran. The clears do not consult the
// batch's context: the batch has landed, and a stamp left behind is the stale
// state this gate exists to remove.
func (w *routeChangeClearingStore) clearRoutedBatchItems(items []routedBatchItem) bool {
	cleared := false
	for _, it := range items {
		if it.beforeErr != nil {
			log.Printf("beads route-clear: reading %s before its guarded gc.routed_to write to %q failed, so its executor-identity stamps were not cleared: %v", it.id, it.newTarget, it.beforeErr)
			continue
		}
		if w.clearIfGenuine(it.id, it.before, it.before.Metadata[beadmeta.RoutedToMetadataKey], it.newTarget) {
			cleared = true
		}
	}
	return cleared
}

// refreshBatchRevisions overwrites each revision in res with the row's current
// one. A clear writes after the batch, so the revision the batch reported is
// stale, and a caller chaining its next guarded batch on it would be refused.
// A row the refresh cannot read keeps the batch's own revision, and the failure
// is logged: the batch itself landed.
func (w *routeChangeClearingStore) refreshBatchRevisions(res *GuardedResult) {
	ids := make([]string, 0, len(res.Revisions))
	for id := range res.Revisions {
		ids = append(ids, id)
	}
	exact, err := PrefetchExact(w.Store, ids)
	if err != nil {
		log.Printf("beads route-clear: refreshing batch revisions after clearing stamps: %v", err)
		exact = map[string]Bead{}
	}
	for _, id := range ids {
		row, ok := exact[id]
		if !ok {
			var getErr error
			if row, getErr = w.Get(id); getErr != nil {
				log.Printf("beads route-clear: refreshing the revision of %s after clearing stamps: %v", id, getErr)
				continue
			}
		}
		res.Revisions[id] = row.Revision
	}
}
