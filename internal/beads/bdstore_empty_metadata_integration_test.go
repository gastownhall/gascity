//go:build integration

package beads_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// ListQuery.Matches reads an absent metadata key as "", so MemStore answers
// List({k: ""}) with the rows that lack k as well as the rows that store k = "".
// BdStore must answer the same, at every storage tier: the real bd on PATH
// returns only explicit-empty rows for a pushed --metadata-field k=, so the pair
// has to stay client-side.
func TestBdStoreListEmptyMetadataValueMatchesMemStoreAgainstRealBd(t *testing.T) {
	bd, _ := newConditionalIntegrationBdStore(t)
	mem := beads.NewMemStore()
	stores := []struct {
		name  string
		store beads.Store
	}{{"MemStore", mem}, {"BdStore", bd}}

	titles := map[string]map[string]string{"MemStore": {}, "BdStore": {}}
	for _, storage := range []string{"history", "no-history", "ephemeral"} {
		for _, shape := range []struct {
			name string
			meta map[string]string
		}{
			{"absent", map[string]string{"other": "x"}},
			{"empty", map[string]string{"other": "x", "k": ""}},
			{"set", map[string]string{"other": "x", "k": "v"}},
		} {
			for _, s := range stores {
				b := beads.Bead{
					Title:     shape.name + "-" + storage,
					Type:      "task",
					Metadata:  maps.Clone(shape.meta),
					NoHistory: storage == "no-history",
					Ephemeral: storage == "ephemeral",
				}
				created, err := s.store.Create(b)
				if err != nil {
					t.Fatalf("%s: Create(%s): %v", s.name, b.Title, err)
				}
				titles[s.name][created.ID] = b.Title
			}
		}
	}

	for _, tier := range []beads.TierMode{beads.TierIssues, beads.TierBoth} {
		got := map[string][]string{}
		for _, s := range stores {
			items, err := s.store.List(beads.ListQuery{Metadata: map[string]string{"k": ""}, TierMode: tier})
			if err != nil {
				t.Fatalf("%s: List(k=\"\", tier %d): %v", s.name, tier, err)
			}
			for _, b := range items {
				got[s.name] = append(got[s.name], titles[s.name][b.ID])
			}
			slices.Sort(got[s.name])
		}
		if !slices.Equal(got["MemStore"], got["BdStore"]) {
			t.Errorf("List k=\"\" tier=%d: MemStore rows %v, BdStore rows %v", tier, got["MemStore"], got["BdStore"])
		}
	}
}
