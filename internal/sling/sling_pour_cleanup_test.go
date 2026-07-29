package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCloseSyntheticInputConvoy(t *testing.T) {
	newSynthetic := func(t *testing.T, store beads.Store) beads.Bead {
		t.Helper()
		c, err := store.Create(beads.Bead{Title: "input convoy for x", Type: "convoy", Metadata: map[string]string{"gc.synthetic": "true"}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	status := func(t *testing.T, store beads.Store, id string) string {
		t.Helper()
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		return b.Status
	}

	t.Run("closes the pour's synthetic convoy", func(t *testing.T) {
		store := beads.NewMemStore()
		c := newSynthetic(t, store)
		closeSyntheticInputConvoy(store, c.ID, "bd-target")
		if got := status(t, store, c.ID); got != "closed" {
			t.Fatalf("synthetic convoy status = %q, want closed", got)
		}
	})

	t.Run("never closes a caller-provided convoy target", func(t *testing.T) {
		store := beads.NewMemStore()
		c := newSynthetic(t, store)
		closeSyntheticInputConvoy(store, c.ID, c.ID)
		if got := status(t, store, c.ID); got == "closed" {
			t.Fatal("caller-provided convoy target was closed")
		}
	})

	t.Run("leaves non-synthetic convoys untouched", func(t *testing.T) {
		store := beads.NewMemStore()
		c, err := store.Create(beads.Bead{Title: "user convoy", Type: "convoy"})
		if err != nil {
			t.Fatal(err)
		}
		closeSyntheticInputConvoy(store, c.ID, "bd-target")
		if got := status(t, store, c.ID); got == "closed" {
			t.Fatal("non-synthetic convoy was closed")
		}
	})

	t.Run("tolerates missing beads and nil store", func(_ *testing.T) {
		closeSyntheticInputConvoy(nil, "c-1", "t-1")
		closeSyntheticInputConvoy(beads.NewMemStore(), "c-absent", "t-1")
	})
}
