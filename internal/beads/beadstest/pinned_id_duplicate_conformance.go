package beadstest

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// RunPinnedIDDuplicateConformance pins that Create refuses a pinned id that
// already names a row, and leaves that row exactly as it was.
//
// Create-if-absent callers depend on it: the op journal's Propose writes an op
// at a hash-derived id and treats a refused create as "already proposed". A
// store that upserts instead — bd before v1.3.0 runs `bd create --id` through
// INSERT ... ON DUPLICATE KEY UPDATE — silently resets status, assignee and
// metadata of the stored row and reports a fresh create.
//
// The suite also pins the pinned-CREATE capability (beads.PinnedIDCreator,
// resolved via beads.PinnedIDCreatorFor): CreatePinned persists exactly the
// caller-chosen id, refuses an empty id before creating anything, refuses a
// duplicate without touching the occupant, and under contention admits
// exactly one of two writers racing for the same id — the loser learns its
// slot was taken instead of storing a second row for it.
//
// newStore must return a fresh, empty store that honors pinned ids and
// implements the pinned-create capability. The pinned id is derived from an
// id the store mints itself, so it lies inside whatever namespace the store
// serves.
func RunPinnedIDDuplicateConformance(t *testing.T, newStore func(t *testing.T) beads.Store) {
	t.Helper()

	pinnedID := func(t *testing.T, s beads.Store) string {
		t.Helper()
		minted, err := s.Create(beads.Bead{Title: "namespace probe"})
		if err != nil {
			t.Fatalf("Create(minted): %v", err)
		}
		cut := strings.LastIndex(minted.ID, "-")
		if cut <= 0 {
			t.Fatalf("minted id %q has no prefix to pin under", minted.ID)
		}
		return minted.ID[:cut] + "-pinneddup"
	}

	// creatorFor resolves the pinned-create capability or fails loudly: the
	// capability legs below are only meaningful against a capable store.
	creatorFor := func(t *testing.T, s beads.Store) beads.PinnedIDCreator {
		t.Helper()
		creator, ok := beads.PinnedIDCreatorFor(s)
		if !ok {
			t.Fatalf("store %T does not resolve beads.PinnedIDCreator; the pinned-create capability is required by this suite", s)
		}
		return creator
	}

	t.Run("PinnedCreatePersistsTheExactIDAndRefusesAnEmptyOne", func(t *testing.T) {
		s := newStore(t)
		creator := creatorFor(t, s)
		id := pinnedID(t, s)

		// An empty id is refused before anything is created: the capability
		// must not fall back to minting, silently or otherwise.
		if _, err := creator.CreatePinned(beads.Bead{Title: "no id"}); err == nil {
			t.Fatal("CreatePinned without an id succeeded; the capability must refuse, never mint")
		}

		created, err := creator.CreatePinned(beads.Bead{ID: id, Title: "pinned", Metadata: map[string]string{"k": "v"}})
		if err != nil {
			t.Fatalf("CreatePinned(%q): %v", id, err)
		}
		if created.ID != id {
			t.Fatalf("CreatePinned(%q) returned %q; a pinned create persists exactly the caller-chosen id", id, created.ID)
		}
		stored, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}
		if stored.Title != "pinned" || stored.Metadata["k"] != "v" {
			t.Fatalf("stored row = (%q %q), want the pinned create's own fields", stored.Title, stored.Metadata["k"])
		}
	})

	t.Run("PinnedCreateRefusesAnExistingPinnedIDAndLeavesTheRowUnchanged", func(t *testing.T) {
		s := newStore(t)
		creator := creatorFor(t, s)
		id := pinnedID(t, s)
		if _, err := creator.CreatePinned(beads.Bead{ID: id, Title: "first", Metadata: map[string]string{"conf.k": "a"}}); err != nil {
			t.Fatalf("CreatePinned(%q): %v", id, err)
		}
		assignee := "worker-1"
		if err := s.Update(id, beads.UpdateOpts{Assignee: &assignee, Metadata: map[string]string{"conf.k": "b"}}); err != nil {
			t.Fatalf("Update(%q): %v", id, err)
		}
		if err := s.Close(id); err != nil {
			t.Fatalf("Close(%q): %v", id, err)
		}
		before, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}

		again, err := creator.CreatePinned(beads.Bead{ID: id, Title: "second", Metadata: map[string]string{"conf.k": "c"}})
		if err == nil {
			t.Fatalf("CreatePinned(%q) over an existing row succeeded (returned %q); the loser of a pinned race must learn its slot was taken", id, again.ID)
		}

		after, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%q) after the refused create: %v", id, err)
		}
		if after.Title != before.Title || after.Status != before.Status || after.Assignee != before.Assignee ||
			after.Metadata["conf.k"] != before.Metadata["conf.k"] {
			t.Fatalf("the refused pinned create changed the row: before (%q %s %q %q), after (%q %s %q %q)",
				before.Title, before.Status, before.Assignee, before.Metadata["conf.k"],
				after.Title, after.Status, after.Assignee, after.Metadata["conf.k"])
		}
	})

	// The exclusion property two racing writers depend on: a deterministic
	// id derives the same slot on both sides, and the store must admit
	// exactly one of them. A store that admits both has no collision guard
	// at all; one that upserts has the pre-1.3.0 bd overwrite.
	t.Run("PinnedCreateContentionAdmitsExactlyOneWinner", func(t *testing.T) {
		s := newStore(t)
		creator := creatorFor(t, s)
		id := pinnedID(t, s)

		const racers = 8
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			winners []int
			errs    []error
		)
		start := make(chan struct{})
		for i := range racers {
			wg.Add(1)
			go func(racer int) {
				defer wg.Done()
				<-start
				created, err := creator.CreatePinned(beads.Bead{ID: id, Title: "racer " + strconv.Itoa(racer)})
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				if created.ID != id {
					errs = append(errs, fmt.Errorf("racer %d: CreatePinned(%q) returned %q", racer, id, created.ID))
					return
				}
				winners = append(winners, racer)
			}(i)
		}
		close(start)
		wg.Wait()

		if len(winners) != 1 {
			t.Fatalf("winners = %d %v, want exactly 1 (no mutual exclusion); errors: %v", len(winners), winners, errs)
		}
		if len(errs) != racers-1 {
			t.Fatalf("loser errors = %d, want %d; errs: %v", len(errs), racers-1, errs)
		}
		got, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}
		if got.Title != "racer "+strconv.Itoa(winners[0]) {
			t.Fatalf("stored row = %q, want the sole winner's row", got.Title)
		}
	})

	t.Run("CreateRefusesAnExistingPinnedIDAndLeavesTheRowUnchanged", func(t *testing.T) {
		s := newStore(t)
		id := pinnedID(t, s)
		first, err := s.Create(beads.Bead{ID: id, Title: "first", Metadata: map[string]string{"conf.k": "a"}})
		if err != nil {
			t.Fatalf("Create(%q): %v", id, err)
		}
		if first.ID != id {
			t.Fatalf("Create(%q) minted %q; this suite needs a store that honors pinned ids", id, first.ID)
		}
		assignee := "worker-1"
		if err := s.Update(id, beads.UpdateOpts{Assignee: &assignee, Metadata: map[string]string{"conf.k": "b"}}); err != nil {
			t.Fatalf("Update(%q): %v", id, err)
		}
		if err := s.Close(id); err != nil {
			t.Fatalf("Close(%q): %v", id, err)
		}
		before, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}

		again, err := s.Create(beads.Bead{ID: id, Title: "second", Metadata: map[string]string{"conf.k": "c"}})
		if err == nil {
			t.Fatalf("Create(%q) over an existing row succeeded (returned %q); a create-if-absent caller now believes it created a row it overwrote", id, again.ID)
		}

		after, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%q) after the refused create: %v", id, err)
		}
		if after.Title != before.Title || after.Status != before.Status || after.Assignee != before.Assignee ||
			after.Metadata["conf.k"] != before.Metadata["conf.k"] {
			t.Fatalf("the refused create changed the row: before (%q %s %q %q), after (%q %s %q %q)",
				before.Title, before.Status, before.Assignee, before.Metadata["conf.k"],
				after.Title, after.Status, after.Assignee, after.Metadata["conf.k"])
		}
	})
}
