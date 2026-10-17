package beadstest

import (
	"strings"
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
// newStore must return a fresh, empty store that honors pinned ids. The pinned
// id is derived from an id the store mints itself, so it lies inside whatever
// namespace the store serves.
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
