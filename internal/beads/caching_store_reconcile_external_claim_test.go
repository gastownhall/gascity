package beads

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// TestReconcileEmitsBeadUpdatedForAnExternallyClaimedBead pins the emission that a claim
// landing outside this process depends on.
//
// A claim arriving over HTTP (bd serve's claimIssue row) fires no bd on_update hook — the
// upstream documents and enforces that — and it is not one of gc's own writes, so
// CachingStore.notifyChange does not run at the write. The event that reaches the city's
// event log for such a claim is the one runReconciliation synthesizes from the diff: the
// claim moves status and assignee, both of which beadChanged compares, so the next pass
// absorbs the row and emits bead.updated with Actor "cache-reconcile"
// (cmd/gc/api_state.go wires onChange straight to events.Recorder.Record).
//
// That matters because bead.updated is the trigger of the one core order keyed on it,
// internal/bootstrap/packs/core/orders/nudge-on-route.toml. If this test fails, an
// externally claimed bead stops waking a warm-idle worker.
func TestReconcileEmitsBeadUpdatedForAnExternallyClaimedBead(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	seed, err := mem.Create(Bead{Title: "ready work", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var mu sync.Mutex
	type emitted struct {
		eventType string
		beadID    string
		payload   json.RawMessage
	}
	var events []emitted
	cs := NewCachingStoreForTest(mem, func(eventType, beadID string, payload json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, emitted{eventType, beadID, payload})
	})
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	mu.Lock()
	events = nil
	mu.Unlock()

	// The write goes to the BACKING store, never through the cache: that is what an HTTP
	// claim looks like from here, and it is the whole point — a claim made through cs
	// would emit at the write and prove nothing.
	claimed, inProgress := "agent@example.test", "in_progress"
	if err := mem.Update(seed.ID, UpdateOpts{Assignee: &claimed, Status: &inProgress}); err != nil {
		t.Fatalf("external claim: %v", err)
	}

	mu.Lock()
	atWrite := len(events)
	mu.Unlock()
	if atWrite != 0 {
		t.Fatalf("a write that bypassed the cache emitted %d events; the premise of this test is that it emits none", atWrite)
	}

	cs.runReconciliation()

	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if e.eventType != "bead.updated" || e.beadID != seed.ID {
			continue
		}
		var b Bead
		if err := json.Unmarshal(e.payload, &b); err != nil {
			t.Fatalf("decode payload: %v: %s", err, e.payload)
		}
		// The payload has to carry the claim, not just the id: a consumer that re-reads
		// is a consumer that races, and nudge-on-route routes on what the event says.
		if b.Assignee != claimed || b.Status != inProgress {
			t.Fatalf("bead.updated payload does not carry the claim: assignee=%q status=%q", b.Assignee, b.Status)
		}
		return
	}
	t.Fatalf("reconcile emitted no bead.updated for %s; got %v", seed.ID, events)
}
