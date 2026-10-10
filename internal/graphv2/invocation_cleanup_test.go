package graphv2

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCloseSyntheticInputConvoy(t *testing.T) {
	newSynthetic := func(t *testing.T, store beads.Store) beads.Bead {
		t.Helper()
		c, err := store.Create(beads.Bead{Title: "input convoy for x", Type: "convoy", Metadata: map[string]string{syntheticMetadataKey: "true"}})
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
		if err := CloseSyntheticInputConvoy(store, c.ID, "bd-target"); err != nil {
			t.Fatalf("CloseSyntheticInputConvoy: %v", err)
		}
		if got := status(t, store, c.ID); got != "closed" {
			t.Fatalf("synthetic convoy status = %q, want closed", got)
		}
	})

	t.Run("never closes a caller-provided convoy target", func(t *testing.T) {
		store := beads.NewMemStore()
		c := newSynthetic(t, store)
		if err := CloseSyntheticInputConvoy(store, c.ID, c.ID); err != nil {
			t.Fatalf("CloseSyntheticInputConvoy: %v", err)
		}
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
		if err := CloseSyntheticInputConvoy(store, c.ID, "bd-target"); err != nil {
			t.Fatalf("CloseSyntheticInputConvoy: %v", err)
		}
		if got := status(t, store, c.ID); got == "closed" {
			t.Fatal("non-synthetic convoy was closed")
		}
	})

	t.Run("tolerates missing beads and nil store", func(t *testing.T) {
		if err := CloseSyntheticInputConvoy(nil, "c-1", "t-1"); err != nil {
			t.Fatalf("nil store: %v", err)
		}
		if err := CloseSyntheticInputConvoy(beads.NewMemStore(), "c-absent", "t-1"); err != nil {
			t.Fatalf("missing convoy: %v", err)
		}
	})

	t.Run("returns a failed close naming the convoy", func(t *testing.T) {
		mem := beads.NewMemStore()
		c := newSynthetic(t, mem)
		refused := errors.New("store refused the close")
		err := CloseSyntheticInputConvoy(closeFailingStore{Store: mem, err: refused}, c.ID, "bd-target")
		if !errors.Is(err, refused) {
			t.Fatalf("err = %v, want the store's close error", err)
		}
		if !strings.Contains(err.Error(), c.ID) {
			t.Fatalf("err = %q, want it to name convoy %s", err, c.ID)
		}
	})

	t.Run("returns a failed read", func(t *testing.T) {
		unreachable := errors.New("store unreachable")
		err := CloseSyntheticInputConvoy(getFailingStore{Store: beads.NewMemStore(), err: unreachable}, "c-1", "bd-target")
		if !errors.Is(err, unreachable) {
			t.Fatalf("err = %v, want the store's read error", err)
		}
	})
}
