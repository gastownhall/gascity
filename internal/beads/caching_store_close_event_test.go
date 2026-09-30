package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// These tests pin gastownhall/gascity#6860: every path that moves a cached bead
// from not-closed to closed must put exactly one bead.closed on the event
// stream, whether the cache wrote the close itself, a read happened to observe a
// close made by another process (bd close, gc bd close), or the reconcile pass
// found it. Before the fix a status=closed Update announced bead.updated only,
// and a close that a live list or dirty read absorbed first was later evicted by
// the reconcile pass without any event at all.

type closeEventRecorder struct {
	mu     sync.Mutex
	events []recordedCacheEvent
}

type recordedCacheEvent struct {
	eventType string
	beadID    string
	status    string
}

func (r *closeEventRecorder) onChange(t *testing.T) func(eventType, beadID string, payload json.RawMessage) {
	return func(eventType, beadID string, payload json.RawMessage) {
		var b Bead
		if err := json.Unmarshal(payload, &b); err != nil {
			t.Errorf("decode %s payload for %s: %v", eventType, beadID, err)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, recordedCacheEvent{eventType: eventType, beadID: beadID, status: b.Status})
	}
}

func (r *closeEventRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

// count returns how many eventType events carried beadID; status filters on
// the payload status when non-empty.
func (r *closeEventRecorder) count(eventType, beadID, status string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.eventType == eventType && e.beadID == beadID && (status == "" || e.status == status) {
			n++
		}
	}
	return n
}

func (r *closeEventRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprintf("%+v", r.events)
}

// newPrimedCloseEventCache seeds one open bead in the BACKING store before the
// cache primes, so the row carries no local-write recency stamp and a later
// reconcile pass is free to act on it at once.
func newPrimedCloseEventCache(t *testing.T) (*MemStore, *CachingStore, *closeEventRecorder, Bead) {
	t.Helper()
	mem := NewMemStore()
	seed, err := mem.Create(Bead{Title: "plain task", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := &closeEventRecorder{}
	cs := NewCachingStoreForTest(mem, rec.onChange(t))
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	rec.reset()
	return mem, cs, rec, seed
}

func assertClosedExactlyOnce(t *testing.T, rec *closeEventRecorder, id, when string) {
	t.Helper()
	if got := rec.count("bead.closed", id, "closed"); got != 1 {
		t.Fatalf("%s: bead.closed events for %s = %d, want exactly 1; events=%s", when, id, got, rec)
	}
	if got := rec.count("bead.updated", id, "closed"); got != 0 {
		t.Fatalf("%s: bead.updated(status=closed) events for %s = %d, want 0 (the close must be announced as bead.closed); events=%s", when, id, got, rec)
	}
}

// Path D of #6860: POST /bead/{id}/update {"status":"closed"} reaches the store
// as Update with Status=closed.
func TestCachingStoreUpdateToClosedEmitsBeadClosedOnce(t *testing.T) {
	t.Parallel()
	_, cs, rec, seed := newPrimedCloseEventCache(t)

	closed := "closed"
	if err := cs.Update(seed.ID, UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("Update status=closed: %v", err)
	}
	assertClosedExactlyOnce(t, rec, seed.ID, "after Update(status=closed)")

	cs.runReconciliation()
	assertClosedExactlyOnce(t, rec, seed.ID, "after the next reconcile pass")
}

// An Update that does not touch status on an open bead is still bead.updated.
func TestCachingStoreUpdateWithoutCloseStillEmitsBeadUpdated(t *testing.T) {
	t.Parallel()
	_, cs, rec, seed := newPrimedCloseEventCache(t)

	title := "renamed"
	if err := cs.Update(seed.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update title: %v", err)
	}
	if got := rec.count("bead.updated", seed.ID, "open"); got != 1 {
		t.Fatalf("bead.updated events = %d, want 1; events=%s", got, rec)
	}
	if got := rec.count("bead.closed", seed.ID, ""); got != 0 {
		t.Fatalf("bead.closed events = %d, want 0; events=%s", got, rec)
	}
}

// Paths B and C of #6860: bd close / gc bd close write the backing store behind
// the cache. When nothing else reads the bead, the reconcile pass is the one
// that notices and announces it.
func TestExternalCloseSeenByReconcileEmitsBeadClosedOnce(t *testing.T) {
	t.Parallel()
	mem, cs, rec, seed := newPrimedCloseEventCache(t)

	if err := mem.Close(seed.ID); err != nil {
		t.Fatalf("external close: %v", err)
	}
	cs.runReconciliation()
	assertClosedExactlyOnce(t, rec, seed.ID, "after the reconcile pass")

	cs.runReconciliation()
	assertClosedExactlyOnce(t, rec, seed.ID, "after a second reconcile pass")
}

// Paths B and C of #6860 as they actually ran: a controller sweep's live list
// (route recovery, pool orphan sweep) re-reads the cached open bead, finds it
// closed, and installs the closed row. That absorb used to be silent, and the
// reconcile pass then evicted an already-closed row without an event.
func TestExternalCloseSeenByLiveListEmitsBeadClosedOnce(t *testing.T) {
	t.Parallel()
	mem, cs, rec, seed := newPrimedCloseEventCache(t)

	if err := mem.Close(seed.ID); err != nil {
		t.Fatalf("external close: %v", err)
	}
	if _, err := cs.List(ListQuery{Status: "open", AllowScan: true, Live: true}); err != nil {
		t.Fatalf("live List: %v", err)
	}
	assertClosedExactlyOnce(t, rec, seed.ID, "after the live list observed the close")

	cs.runReconciliation()
	assertClosedExactlyOnce(t, rec, seed.ID, "after the reconcile pass evicted the closed row")
}

// A dirty-row Get refreshes from the backing store and can be the first reader
// to see an external close.
func TestExternalCloseSeenByDirtyGetEmitsBeadClosedOnce(t *testing.T) {
	t.Parallel()
	mem, cs, rec, seed := newPrimedCloseEventCache(t)

	if err := mem.Close(seed.ID); err != nil {
		t.Fatalf("external close: %v", err)
	}
	cs.mu.Lock()
	cs.markDirtyLocked(seed.ID)
	cs.mu.Unlock()

	got, err := cs.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("Get status = %q, want closed", got.Status)
	}
	assertClosedExactlyOnce(t, rec, seed.ID, "after the dirty Get observed the close")

	cs.runReconciliation()
	assertClosedExactlyOnce(t, rec, seed.ID, "after the reconcile pass")
}

// Path A of #6860 stays single: Close announces, and nothing later repeats it.
func TestCachingStoreCloseEmitsBeadClosedOnceAcrossReconcile(t *testing.T) {
	t.Parallel()
	_, cs, rec, seed := newPrimedCloseEventCache(t)

	if err := cs.Close(seed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertClosedExactlyOnce(t, rec, seed.ID, "after Close")

	if _, err := cs.List(ListQuery{Status: "open", AllowScan: true, Live: true}); err != nil {
		t.Fatalf("live List: %v", err)
	}
	cs.runReconciliation()
	assertClosedExactlyOnce(t, rec, seed.ID, "after a live list and a reconcile pass")
}

// A bead.closed event that arrives on the bus (another process already
// announced it) is applied to the cache without being announced a second time.
func TestAppliedCloseEventIsNotReannounced(t *testing.T) {
	t.Parallel()
	mem, cs, rec, seed := newPrimedCloseEventCache(t)

	if err := mem.Close(seed.ID); err != nil {
		t.Fatalf("external close: %v", err)
	}
	closedRow, err := mem.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get closed row: %v", err)
	}
	payload, err := EncodeBeadEventPayload(closedRow)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	cs.ApplyEvent("bead.closed", payload)
	cs.runReconciliation()
	if got := rec.count("bead.closed", seed.ID, ""); got != 0 {
		t.Fatalf("bead.closed events = %d, want 0: the close was already on the bus; events=%s", got, rec)
	}
}

// A cache-reconcile snapshot is another cache's own emission. That cache owns
// the announcement of any close it carries, so applying it must not announce.
func TestAppliedSnapshotOfClosedRowIsNotAnnounced(t *testing.T) {
	t.Parallel()
	mem, cs, rec, seed := newPrimedCloseEventCache(t)

	if err := mem.Close(seed.ID); err != nil {
		t.Fatalf("external close: %v", err)
	}
	closedRow, err := mem.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get closed row: %v", err)
	}
	payload, err := EncodeBeadEventPayload(closedRow)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	cs.ApplyEventSnapshot("bead.updated", payload)
	cs.runReconciliation()
	if got := rec.count("bead.closed", seed.ID, ""); got != 0 {
		t.Fatalf("bead.closed events = %d, want 0: the snapshot's emitter owns the announcement; events=%s", got, rec)
	}
}

// A close that is seen and then reopened before anything announced it leaves
// no stale bead.closed behind.
func TestReopenBeforeAnnouncementDropsThePendingClose(t *testing.T) {
	t.Parallel()
	mem, cs, rec, seed := newPrimedCloseEventCache(t)

	if err := mem.Close(seed.ID); err != nil {
		t.Fatalf("external close: %v", err)
	}
	if _, err := cs.List(ListQuery{Status: "open", AllowScan: true, Live: true}); err != nil {
		t.Fatalf("live List: %v", err)
	}
	if err := cs.Reopen(seed.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	rec.reset()
	cs.runReconciliation()
	if got := rec.count("bead.closed", seed.ID, ""); got != 0 {
		t.Fatalf("bead.closed events after reopen = %d, want 0; events=%s", got, rec)
	}
}
