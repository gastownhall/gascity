package beads

import (
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
		if g, ok := store.(*routeChangeClearingStore); ok && g != nil && gate == nil {
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
