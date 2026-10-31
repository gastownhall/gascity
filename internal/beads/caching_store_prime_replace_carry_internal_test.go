package beads

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// TestCachingStorePrimeReplaceCarriesScanMissingOpenRows pins what a full
// Prime's replace does with a cached open row the active-only scan does not
// return: the row is carried, not dropped, and the next reconcile's per-id
// re-verify (recoverMissingFromList) decides its fate — a close or delete
// behind the cache is announced exactly once, and an alive row one listing
// merely omitted stays cached open with no spurious events.

// carryNoteRecorder records every cache notification as "type id" pairs.
type carryNoteRecorder struct {
	mu    sync.Mutex
	notes []string
}

func (r *carryNoteRecorder) onChange(eventType, beadID string, _ json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, eventType+" "+beadID)
}

func (r *carryNoteRecorder) count(eventType, beadID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	note := eventType + " " + beadID
	n := 0
	for _, x := range r.notes {
		if x == note {
			n++
		}
	}
	return n
}

func TestCachingStorePrimeReplaceCarriesScanMissingOpenRows(t *testing.T) {
	// A row deleted behind the cache is absent from the scan like a closed
	// one, and the reconcile after the Prime announces the same synthetic
	// bead.closed it would without the Prime.
	t.Run("deleted_behind_cache", func(t *testing.T) {
		mem := NewMemStore()
		seed, err := mem.Create(Bead{Title: "deleted behind the cache"})
		if err != nil {
			t.Fatalf("Create seed: %v", err)
		}
		bystander, err := mem.Create(Bead{Title: "stays"})
		if err != nil {
			t.Fatalf("Create bystander: %v", err)
		}
		rec := &carryNoteRecorder{}
		cs := NewCachingStoreForTest(mem, rec.onChange)
		if err := cs.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		if err := mem.Delete(seed.ID); err != nil {
			t.Fatalf("backing Delete: %v", err)
		}
		if err := cs.Prime(context.Background()); err != nil {
			t.Fatalf("second Prime: %v", err)
		}
		cs.ReconcileNowForTest()
		if got := rec.count("bead.closed", seed.ID); got != 1 {
			t.Errorf("bead.closed for %s = %d, want exactly 1 (synthetic, from the reconcile's re-verify)", seed.ID, got)
		}
		if got := rec.count("bead.closed", bystander.ID); got != 0 {
			t.Errorf("bead.closed for bystander %s = %d, want 0", bystander.ID, got)
		}
	})
}
