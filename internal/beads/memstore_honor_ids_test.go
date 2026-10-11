package beads_test

import (
	"errors"
	"fmt"
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

// TestMemStoreMintNeverReissuesAnExistingID pins the mint-side guard on the
// DEFAULT path, where HonorExplicitIDs plays no part: NewMemStoreFrom seeds a
// sequence independently of the rows it seeds (callers routinely pass
// len(corpus)), so the sequence can lag an id already present. SQLiteStore's
// mintUniqueIDTx re-checks every auto-minted id for exactly this reason. MemStore
// is slice-backed, so a re-issued id aliases the earlier row instead of
// conflicting: Get returns the first, and the second bead is unreachable under
// the id its own Create handed back.
func TestMemStoreMintNeverReissuesAnExistingID(t *testing.T) {
	t.Parallel()
	seeded := []beads.Bead{{ID: "gc-1", Title: "one"}, {ID: "gc-3", Title: "three"}}
	store := beads.NewMemStoreFrom(len(seeded), seeded, nil)

	minted, err := store.Create(beads.Bead{Title: "minted"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if minted.ID == "gc-3" {
		t.Fatalf("minted id = %q, which is already taken by the seeded bead", minted.ID)
	}
	got, err := store.Get(minted.ID)
	if err != nil {
		t.Fatalf("get %q: %v", minted.ID, err)
	}
	if got.Title != "minted" {
		t.Errorf("get %q returned title %q, want the bead that was just created", minted.ID, got.Title)
	}
}

// TestMemStoreHonorExplicitIDsMatchesSQLiteStore is the differential the
// HonorExplicitIDs doc comment claims: "matching SQLiteStore.Create". It runs the
// same sequences against both stores and requires the same answers, so the claim
// is pinned against the store it names rather than against MemStore itself.
//
// Pinning a numeric id must consume its suffix (or the next mint re-issues it),
// pinning a wisp-shaped id must not (it has no numeric suffix), and a duplicate
// explicit create must be rejected with the same message.
func TestMemStoreHonorExplicitIDsMatchesSQLiteStore(t *testing.T) {
	t.Parallel()

	newMem := func() beads.Store {
		store := beads.NewMemStore()
		store.HonorExplicitIDs = true
		return store
	}
	newSQLite := func(t *testing.T) beads.Store {
		t.Helper()
		store, err := beads.OpenSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatalf("open sqlite store: %v", err)
		}
		return store
	}

	for _, tc := range []struct {
		name string
		pin  string
	}{
		{"adjacent numeric pin", "gc-1"},
		{"distant numeric pin", "gc-500"},
		{"wisp pin has no numeric suffix", "gc-wisp-abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mintedAfterPin := func(store beads.Store) (string, error) {
				if _, err := store.Create(beads.Bead{ID: tc.pin, Title: "pinned"}); err != nil {
					return "", fmt.Errorf("create pinned %q: %w", tc.pin, err)
				}
				minted, err := store.Create(beads.Bead{Title: "minted"})
				if err != nil {
					return "", fmt.Errorf("create minted: %w", err)
				}
				return minted.ID, nil
			}
			wantID, err := mintedAfterPin(newSQLite(t))
			if err != nil {
				t.Fatalf("sqlite: %v", err)
			}
			gotID, err := mintedAfterPin(newMem())
			if err != nil {
				t.Fatalf("memstore: %v", err)
			}
			if gotID != wantID {
				t.Errorf("after pinning %q, MemStore minted %q but SQLiteStore minted %q", tc.pin, gotID, wantID)
			}
		})
	}

	t.Run("duplicate explicit create is rejected the same way", func(t *testing.T) {
		t.Parallel()
		duplicateErr := func(store beads.Store) string {
			if _, err := store.Create(beads.Bead{ID: "gc-1", Title: "first"}); err != nil {
				t.Fatalf("create first: %v", err)
			}
			_, err := store.Create(beads.Bead{ID: "gc-1", Title: "second"})
			if err == nil {
				t.Fatalf("%T accepted a duplicate explicit id", store)
			}
			return err.Error()
		}
		want := duplicateErr(newSQLite(t))
		if got := duplicateErr(newMem()); got != want {
			t.Errorf("MemStore duplicate error = %q, SQLiteStore = %q", got, want)
		}
	})
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
