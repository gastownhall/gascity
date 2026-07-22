package beads

import "testing"

type canonicalReadBacking struct {
	Store
	canonical Bead
}

func (s canonicalReadBacking) GetCanonical(string) (Bead, error) {
	return cloneBead(s.canonical), nil
}

func TestGetCanonicalFallsBackToOrdinaryStoreGet(t *testing.T) {
	store := NewMemStore()
	want, err := store.Create(Bead{Title: "ordinary row"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := GetCanonical(store, want.ID)
	if err != nil {
		t.Fatalf("GetCanonical: %v", err)
	}
	if got.ID != want.ID || got.Title != want.Title {
		t.Fatalf("GetCanonical = %+v, want ordinary Get result %+v", got, want)
	}
}

func TestGetCanonicalForwardsThroughCachingStore(t *testing.T) {
	ordinary := NewMemStore()
	stale, err := ordinary.Create(Bead{Title: "stale duplicate issue row"})
	if err != nil {
		t.Fatalf("Create stale row: %v", err)
	}
	fresh := cloneBead(stale)
	fresh.Title = "canonical wisp row"

	cache := NewCachingStoreForTest(canonicalReadBacking{Store: ordinary, canonical: fresh}, nil)
	got, err := GetCanonical(cache, stale.ID)
	if err != nil {
		t.Fatalf("GetCanonical through cache: %v", err)
	}
	if got.Title != fresh.Title {
		t.Fatalf("GetCanonical through cache title = %q, want %q", got.Title, fresh.Title)
	}
}
