package beads_test

import (
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestMemStoreMintPrefixFuncIsOptIn pins that a store with no mint-prefix
// function keeps minting every bead under IDPrefix.
func TestMemStoreMintPrefixFuncIsOptIn(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	created, err := store.Create(beads.Bead{Title: "session", Type: "session"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID != "gc-1" {
		t.Fatalf("created id = %q, want %q", created.ID, "gc-1")
	}
}

// TestMemStoreMintPrefixFuncChoosesThePrefixPerBead pins what the hook buys:
// one physical store mints each bead under the prefix the function picks for
// it, an empty answer falls back to IDPrefix, and the shared sequence keeps
// every id unique across prefixes.
func TestMemStoreMintPrefixFuncChoosesThePrefixPerBead(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	store.SetMintPrefixFunc(func(b beads.Bead) string {
		if b.Type == "session" {
			return "gcg"
		}
		return ""
	})

	work, err := store.Create(beads.Bead{Title: "task"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	session, err := store.Create(beads.Bead{Title: "session", Type: "session"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if work.ID != "gc-1" || session.ID != "gcg-2" {
		t.Fatalf("ids = %q, %q; want gc-1, gcg-2", work.ID, session.ID)
	}
	if got, err := store.Get("gcg-2"); err != nil || got.Type != "session" {
		t.Fatalf("Get(gcg-2) = %+v, %v; want the session", got, err)
	}
}

// TestMemStoreMintPrefixFuncLeavesPinnedIDsAlone pins that the hook decides
// only what the store MINTS: a pinned id the store honors keeps its own
// prefix.
func TestMemStoreMintPrefixFuncLeavesPinnedIDsAlone(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	store.SetMintPrefixFunc(func(beads.Bead) string { return "gcg" })

	created, err := store.Create(beads.Bead{ID: "gc-wisp-7", Title: "pinned"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID != "gc-wisp-7" {
		t.Fatalf("created id = %q, want the pinned id", created.ID)
	}
}

// TestMemStoreSetMintPrefixFuncIsSafeAlongsideCreates pins that installing the
// hook on a store other goroutines are already writing is not a data race (the
// split-strict test gate installs it on a store a fixture may share).
func TestMemStoreSetMintPrefixFuncIsSafeAlongsideCreates(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if _, err := store.Create(beads.Bead{Title: "x"}); err != nil {
				t.Errorf("create: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		store.SetMintPrefixFunc(func(beads.Bead) string { return "gcg" })
	}()
	wg.Wait()
}
