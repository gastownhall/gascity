package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestStatusReadyCountIncludesTheSplitGraphBinding is read-audit R12: on a
// split city /status and the dashboard Ready tile counted only the work stores'
// ready work, while GET /beads/ready also served the graph binding's ready
// steps. The count now reads the binding as one more ready leg, and a city whose
// graph class is not relocated counts exactly what it did.
func TestStatusReadyCountIncludesTheSplitGraphBinding(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := map[bool]string{false: "single-store", true: "split"}[split]
		t.Run(name, func(t *testing.T) {
			state := newFakeState(t)
			work := beads.NewMemStore()
			if _, err := work.Create(beads.Bead{Type: "task", Title: "ready work"}); err != nil {
				t.Fatal(err)
			}
			state.cityBeadStore = work
			state.stores = map[string]beads.Store{}
			want := 1
			if split {
				binding, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = binding.(interface{ CloseStore() error }).CloseStore() })
				if _, err := binding.Create(beads.Bead{Type: "task", Title: "ready graph step"}); err != nil {
					t.Fatal(err)
				}
				state.graphBeadStore = binding
				want = 2
			}
			wc, errs := (&Server{state: state}).statusWorkCounts(context.Background(), nil)
			if len(errs) != 0 {
				t.Fatalf("partial errors = %v", errs)
			}
			if wc.Ready != want {
				t.Errorf("status ready = %d, want %d", wc.Ready, want)
			}
			// The city store is in BeadStores under the city's name, so its
			// one open row is counted there; the graph leg adds no stored
			// rows. This pins today's count, not a judgement of it.
			if wc.Open != 1 {
				t.Errorf("status open = %d, want 1 (the city store's row through its BeadStores entry; the binding's stored rows are not added)", wc.Open)
			}
		})
	}
}
