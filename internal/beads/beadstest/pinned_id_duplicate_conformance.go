package beadstest

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// RunPinnedIDDuplicateConformance pins that Create refuses a pinned id that
// already names a row and leaves that row exactly as it was, and that ids
// derived as children of a minted id are preserved and read exactly.
//
// Create-if-absent callers depend on it: the op journal's Propose writes an op
// at a hash-derived id and treats a refused create as "already proposed", and
// the extmsg transcript pins each entry as <state-id>-<sequence>. A store
// that upserts instead — bd before v1.3.0 runs `bd create --id` through
// INSERT ... ON DUPLICATE KEY UPDATE — silently resets status, assignee and
// metadata of the stored row and reports a fresh create; one that resolves
// ids fuzzily conflates <state-id>-2 with <state-id>-20.
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

	t.Run("ChildDerivedIDsArePreservedAndReadExactly", func(t *testing.T) {
		s := newStore(t)
		minted, err := s.Create(beads.Bead{Title: "namespace probe"})
		if err != nil {
			t.Fatalf("Create(minted): %v", err)
		}
		cut := strings.LastIndex(minted.ID, "-")
		if cut <= 0 {
			t.Fatalf("minted id %q has no prefix to pin under", minted.ID)
		}
		prefix := minted.ID[:cut]

		// The extmsg transcript shape: <state-id>-<sequence>. Two sibling
		// sequences that share a textual prefix (2 and 20) must stay distinct
		// rows, each read exactly by its own id.
		two := prefix + "-2"
		twenty := prefix + "-20"
		first, err := s.Create(beads.Bead{ID: two, Title: "entry two"})
		if err != nil {
			t.Fatalf("Create(%q): %v", two, err)
		}
		if first.ID != two {
			t.Fatalf("Create(%q) minted %q; this suite needs a store that honors pinned ids", two, first.ID)
		}
		second, err := s.Create(beads.Bead{ID: twenty, Title: "entry twenty"})
		if err != nil {
			t.Fatalf("Create(%q): %v", twenty, err)
		}
		if second.ID != twenty {
			t.Fatalf("Create(%q) minted %q", twenty, second.ID)
		}

		gotTwo, err := s.Get(two)
		if err != nil {
			t.Fatalf("Get(%q): %v; a reader of sequence 2 must not have to read sequence 20's row", two, err)
		}
		if gotTwo.Title != "entry two" {
			t.Fatalf("Get(%q).Title = %q, want %q", two, gotTwo.Title, "entry two")
		}
		gotTwenty, err := s.Get(twenty)
		if err != nil {
			t.Fatalf("Get(%q): %v", twenty, err)
		}
		if gotTwenty.Title != "entry twenty" {
			t.Fatalf("Get(%q).Title = %q, want %q", twenty, gotTwenty.Title, "entry twenty")
		}

		// A missing exact id reads as not-found — never as a prefix match of
		// the sibling rows — and never mutates them.
		if _, err := s.Get(prefix + "-200"); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("Get(%q) = %v; want ErrNotFound, not a fuzzy match of its siblings", prefix+"-200", err)
		}
		stillTwo, err := s.Get(two)
		if err != nil || stillTwo.Title != "entry two" {
			t.Fatalf("Get(%q) after the missing-id read = (%q, %v); the sibling row must be unchanged", two, stillTwo.Title, err)
		}
	})
}
