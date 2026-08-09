package beads_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestMemStoreHonorExplicitIDsIsOptIn pins that the knob changes nothing until
// it is set: every existing caller keeps minting over whatever id it passed.
func TestMemStoreHonorExplicitIDsIsOptIn(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	created, err := store.Create(beads.Bead{ID: "gcg-wisp-abc", Title: "pinned"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID != "gc-1" {
		t.Fatalf("created id = %q, want the minted %q", created.ID, "gc-1")
	}
}

// TestMemStoreHonorExplicitIDs pins the behavior the knob buys: a pinned id
// round-trips, an unpinned create still mints under IDPrefix, and the two
// sequences do not collide. This is what lets a MemStore stand in for a real
// per-class database, whose ids are pinned by the caller for wisps and minted
// by the store otherwise.
func TestMemStoreHonorExplicitIDs(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	store.IDPrefix = "gcg"
	store.HonorExplicitIDs = true

	pinned, err := store.Create(beads.Bead{ID: "gcg-wisp-abc", Title: "wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("create pinned: %v", err)
	}
	if pinned.ID != "gcg-wisp-abc" {
		t.Fatalf("pinned id = %q, want it kept verbatim", pinned.ID)
	}
	got, err := store.Get("gcg-wisp-abc")
	if err != nil {
		t.Fatalf("get pinned: %v", err)
	}
	if !got.Ephemeral || got.Title != "wisp" {
		t.Fatalf("stored bead = %+v, want the ephemeral wisp", got)
	}

	minted, err := store.Create(beads.Bead{Title: "minted"})
	if err != nil {
		t.Fatalf("create minted: %v", err)
	}
	if minted.ID != "gcg-1" {
		t.Fatalf("minted id = %q, want %q; pinning must not consume the sequence", minted.ID, "gcg-1")
	}
}

// TestMemStoreHonorExplicitIDsRejectsDuplicates pins the hard duplicate-id
// contract SQLiteStore.Create has. A silent fallback to the sequence id would
// hide exactly the id collision the caller asked about.
func TestMemStoreHonorExplicitIDsRejectsDuplicates(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true

	if _, err := store.Create(beads.Bead{ID: "gc-pinned", Title: "first"}); err != nil {
		t.Fatalf("create first: %v", err)
	}
	_, err := store.Create(beads.Bead{ID: "gc-pinned", Title: "second"})
	if err == nil {
		t.Fatal("duplicate pinned id was accepted")
	}
	if !strings.Contains(err.Error(), "duplicate id") {
		t.Errorf("error %q does not report a duplicate id", err)
	}
	if _, err := store.Get("gc-pinned"); errors.Is(err, beads.ErrNotFound) {
		t.Error("the rejected create removed the original bead")
	}
}

// TestMemStoreHonoringIDsPassesStoreConformance pins that turning the knob on
// does not cost the store any of its contract.
func TestMemStoreHonoringIDsPassesStoreConformance(t *testing.T) {
	factory := func() beads.Store {
		store := beads.NewMemStore()
		store.HonorExplicitIDs = true
		return store
	}
	beadstest.RunStoreTests(t, factory)
	beadstest.RunSequentialIDTests(t, factory)
	beadstest.RunDepTests(t, factory)
	beadstest.RunMetadataTests(t, factory)
}
