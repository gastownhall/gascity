package beads

import (
	"context"
	"encoding/json"
	"testing"
)

// floodLoopHarness wires a cache to the controller's real event topology: every
// reconcile emission is recorded on the bus and then fed back into ApplyEvent on
// the emitting store (cmd/gc/api_state.go startBeadEventWatcher ->
// applyBeadEventToStores). Delivery is deferred to after the pass because the
// bus is asynchronous; applying inline would deadlock on the reconcile lock and
// would not match production ordering anyway.
type floodLoopHarness struct {
	cache   *CachingStore
	pending []cacheFloodEvent
	emitted []string
}

type cacheFloodEvent struct {
	eventType string
	payload   json.RawMessage
}

func newFloodLoopHarness(t *testing.T, backing Store) *floodLoopHarness {
	t.Helper()
	h := &floodLoopHarness{}
	h.cache = NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		h.emitted = append(h.emitted, eventType+":"+beadID)
		h.pending = append(h.pending, cacheFloodEvent{eventType: eventType, payload: payload})
	})
	if err := h.cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return h
}

// pass runs one reconcile cycle and then delivers everything it emitted back to
// the cache, the way the controller's watcher does.
func (h *floodLoopHarness) pass() {
	h.cache.runReconciliation()
	pending := h.pending
	h.pending = nil
	for _, evt := range pending {
		// The controller routes cache-reconcile-actored events through the
		// snapshot entry point; every emission here is one of those.
		h.cache.ApplyEventSnapshot(evt.eventType, evt.payload)
	}
}

// TestReconcileEmissionFedBackDoesNotSustainFloodLoop pins the fix for the
// cache-reconcile event flood (a rig emitting bead.updated for every open bead,
// every pass, forever — 2k+ events/minute against an unchanging backing store).
//
// The loop is closed by the cache re-absorbing its own reconcile emission:
//
//  1. Reconcile absorbs a row with an explicit dependency snapshot and an
//     is_blocked verdict, then emits bead.updated carrying that full snapshot.
//  2. Bead.Dependencies and Bead.Needs are `omitempty`, so a bead with no
//     dependencies marshals with neither key. The payload is now
//     indistinguishable from a bd on_update hook patch, which legitimately omits
//     dependencies after a removal.
//  3. ApplyEvent therefore treats dependency coverage as unknown: it drops the
//     cached dep set, clears the is_blocked verdict, clears depsComplete
//     store-wide, and reports a mutation.
//  4. The mutation stamps a sequence number, which fences the row out of the
//     next pass' absorb, so the cleared verdict is never repaired.
//  5. The pass after that absorbs again, sees cached is_blocked nil against a
//     fresh &false, calls that a change, and emits — returning to step 1.
//
// A dependency-free bead that nothing is writing to must go quiet.
func TestReconcileEmissionFedBackDoesNotSustainFloodLoop(t *testing.T) {
	mem := NewMemStore()
	unblocked := false
	subject, err := mem.Create(Bead{Title: "backlog issue", IsBlocked: &unblocked})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	h := newFloodLoopHarness(t, mem)
	h.pass()
	h.emitted = nil

	// Seed exactly one real divergence, the way any external writer would. Every
	// emission after the pass that reports it is the cache talking to itself.
	edited := "backlog issue (edited)"
	if err := mem.Update(subject.ID, UpdateOpts{Title: &edited}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	h.pass()
	seeded := len(h.emitted)
	if seeded == 0 {
		t.Fatalf("reconcile did not report the seeded title change; harness is not exercising the emit path")
	}
	h.emitted = nil

	for i := 0; i < 5; i++ {
		h.pass()
	}
	if len(h.emitted) != 0 {
		t.Fatalf("reconcile kept emitting for an unchanging bead after the seeded change settled: %v", h.emitted)
	}
}

// TestReconcileEmissionFedBackPreservesReadyVerdict isolates the state damage
// behind the loop above. Re-absorbing the cache's own snapshot must not nil out
// the is_blocked verdict reconcile just installed, and must not clear the
// store-wide depsComplete latch, which silently downgrades the reconcile
// dependency comparison for every other bead in the rig.
func TestReconcileEmissionFedBackPreservesReadyVerdict(t *testing.T) {
	mem := NewMemStore()
	unblocked := false
	subject, err := mem.Create(Bead{Title: "backlog issue", IsBlocked: &unblocked})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	h := newFloodLoopHarness(t, mem)
	h.pass()

	h.cache.mu.RLock()
	cached, ok := h.cache.beads[subject.ID]
	depsComplete := h.cache.depsComplete
	h.cache.mu.RUnlock()

	if !ok {
		t.Fatalf("bead %s dropped from cache", subject.ID)
	}
	if cached.IsBlocked == nil {
		t.Fatalf("re-absorbing the cache's own reconcile snapshot nilled %s's is_blocked verdict", subject.ID)
	}
	if !depsComplete {
		t.Fatalf("re-absorbing the cache's own reconcile snapshot cleared the store-wide depsComplete latch")
	}
}
