package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// primeCarryRecorder counts the notifications a cache emits by type and id.
type primeCarryRecorder struct {
	mu     sync.Mutex
	counts map[string]int
	events []string
}

func newPrimeCarryRecorder() *primeCarryRecorder {
	return &primeCarryRecorder{counts: map[string]int{}}
}

func (r *primeCarryRecorder) onChange(eventType, beadID string, _ json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[eventType+" "+beadID]++
	r.events = append(r.events, eventType+" "+beadID)
}

func (r *primeCarryRecorder) count(eventType, id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[eventType+" "+id]
}

func (r *primeCarryRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprint(r.events)
}

// TestCachingStorePrimeReplaceCarriesScanMissingOpenRows pins that a full
// Prime's replace carries every open row its active-only listing omits, so
// absence from the scan is re-verified per id by the next reconcile instead
// of dropping the row: a close behind the cache is announced exactly once
// whatever the row's local-write history, and an alive row the listing merely
// missed stays cached with no notification.
func TestCachingStorePrimeReplaceCarriesScanMissingOpenRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("aged_local_write", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name     string
			liveList bool
		}{
			{"reconcile", false},
			{"live_list_then_reconcile", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				mem := NewMemStore()
				seed, err := mem.Create(Bead{Title: "written, then closed behind the cache"})
				if err != nil {
					t.Fatalf("Create seed: %v", err)
				}
				bystander, err := mem.Create(Bead{Title: "stays open"})
				if err != nil {
					t.Fatalf("Create bystander: %v", err)
				}
				rec := newPrimeCarryRecorder()
				cs := NewCachingStoreForTest(mem, rec.onChange)
				if err := cs.Prime(ctx); err != nil {
					t.Fatalf("Prime: %v", err)
				}
				if err := cs.SetMetadata(seed.ID, "k", "v"); err != nil {
					t.Fatalf("SetMetadata: %v", err)
				}
				// Past the five-second recency keep, within the write's
				// verify window.
				ageLocalWriteBy(cs, seed.ID, 10*time.Second)
				if err := mem.Close(seed.ID); err != nil {
					t.Fatalf("backing Close: %v", err)
				}
				if err := cs.Prime(ctx); err != nil {
					t.Fatalf("Prime: %v", err)
				}
				if tc.liveList {
					if _, err := cs.List(ListQuery{AllowScan: true, IncludeClosed: true, Live: true}); err != nil {
						t.Fatalf("live List: %v", err)
					}
				}
				cs.ReconcileNowForTest()
				cs.ReconcileNowForTest()

				if got := rec.count("bead.closed", seed.ID); got != 1 {
					t.Fatalf("bead.closed for %s = %d, want 1; notifications=%s", seed.ID, got, rec)
				}
				if got := rec.count("bead.closed", bystander.ID); got != 0 {
					t.Fatalf("bead.closed for bystander %s = %d, want 0; notifications=%s", bystander.ID, got, rec)
				}
				if got, err := cs.Get(seed.ID); err != nil || got.Status != "closed" {
					t.Fatalf("Get(%s) = (%q, %v), want closed", seed.ID, got.Status, err)
				}
			})
		}
	})

	t.Run("deleted_behind_cache", func(t *testing.T) {
		t.Parallel()
		mem := NewMemStore()
		seed, err := mem.Create(Bead{Title: "deleted behind the cache"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		rec := newPrimeCarryRecorder()
		cs := NewCachingStoreForTest(mem, rec.onChange)
		if err := cs.PrimeActive(); err != nil {
			t.Fatalf("PrimeActive: %v", err)
		}
		if err := mem.Delete(seed.ID); err != nil {
			t.Fatalf("backing Delete: %v", err)
		}
		if err := cs.Prime(ctx); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		cs.ReconcileNowForTest()
		if got := rec.count("bead.closed", seed.ID); got != 1 {
			t.Fatalf("bead.closed for deleted %s = %d, want 1; notifications=%s", seed.ID, got, rec)
		}
	})
}
