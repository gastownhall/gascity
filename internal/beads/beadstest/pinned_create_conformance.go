package beadstest

import (
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// RunPinnedCreateConformance pins the beads.PinnedIDCreator contract on a
// store that declares it: a pinned create needs an id, keeps it verbatim, and
// refuses an id that already names a row (open or closed) without touching
// that row, including when two writers race for the same id.
//
// newStore must return a fresh, empty store. Each returned store is resolved
// through beads.PinnedIDCreatorFor, so a wrapper that hides the capability
// fails here instead of passing vacuously. The pinned ids are derived from an
// id the store mints itself, so they lie inside whatever namespace it serves.
//
// newPeer, when non-nil, returns a second handle over the store s (a separate
// FileStore over the same file, say); the race then runs one writer on each
// handle, which is the cross-process shape transcript appends depend on.
func RunPinnedCreateConformance(t *testing.T, newStore func(t *testing.T) beads.Store, newPeer func(t *testing.T, s beads.Store) beads.Store) {
	t.Helper()

	pinned := func(t *testing.T, s beads.Store) beads.PinnedIDCreator {
		t.Helper()
		creator, ok := beads.PinnedIDCreatorFor(s)
		if !ok {
			t.Fatalf("PinnedIDCreatorFor(%T) = false; this suite needs a store that declares pinned create", s)
		}
		return creator
	}
	pinnedID := func(t *testing.T, s beads.Store, suffix string) string {
		t.Helper()
		minted, err := s.Create(beads.Bead{Title: "namespace probe"})
		if err != nil {
			t.Fatalf("Create(minted): %v", err)
		}
		cut := strings.LastIndex(minted.ID, "-")
		if cut <= 0 {
			t.Fatalf("minted id %q has no prefix to pin under", minted.ID)
		}
		return minted.ID[:cut] + "-" + suffix
	}

	t.Run("RefusesAMissingOrPaddedIDWithoutWriting", func(t *testing.T) {
		s := newStore(t)
		creator := pinned(t, s)
		before, err := s.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
		if err != nil {
			t.Fatalf("List before: %v", err)
		}
		for _, id := range []string{"", "   ", " " + pinnedID(t, s, "padded") + " "} {
			if got, err := creator.CreatePinned(beads.Bead{ID: id, Title: "no id"}); err == nil {
				t.Fatalf("CreatePinned(id=%q) succeeded with %q; a pinned create must not mint", id, got.ID)
			}
		}
		after, err := s.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
		if err != nil {
			t.Fatalf("List after: %v", err)
		}
		// pinnedID's namespace probe is the one row the loop itself created.
		if len(after) != len(before)+1 {
			t.Fatalf("refused pinned creates wrote rows: %d before, %d after (want %d)", len(before), len(after), len(before)+1)
		}
	})

	t.Run("KeepsThePinnedIDVerbatim", func(t *testing.T) {
		s := newStore(t)
		id := pinnedID(t, s, "pinnedkeep")
		created, err := pinned(t, s).CreatePinned(beads.Bead{ID: id, Title: "kept", Metadata: map[string]string{"conf.k": "a"}})
		if err != nil {
			t.Fatalf("CreatePinned(%q): %v", id, err)
		}
		if created.ID != id {
			t.Fatalf("CreatePinned(%q) returned id %q", id, created.ID)
		}
		got, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}
		if got.Title != "kept" || got.Metadata["conf.k"] != "a" {
			t.Fatalf("Get(%q) = title %q conf.k %q, want kept/a", id, got.Title, got.Metadata["conf.k"])
		}
	})

	t.Run("RefusesAnOccupiedIDAndLeavesTheOccupantUnchanged", func(t *testing.T) {
		for _, closed := range []bool{false, true} {
			s := newStore(t)
			creator := pinned(t, s)
			id := pinnedID(t, s, "pinnedocc")
			if _, err := creator.CreatePinned(beads.Bead{ID: id, Title: "first", Metadata: map[string]string{"conf.k": "a"}}); err != nil {
				t.Fatalf("CreatePinned(%q): %v", id, err)
			}
			if closed {
				if err := s.Close(id); err != nil {
					t.Fatalf("Close(%q): %v", id, err)
				}
			}
			before, err := s.Get(id)
			if err != nil {
				t.Fatalf("Get(%q): %v", id, err)
			}
			if again, err := creator.CreatePinned(beads.Bead{ID: id, Title: "second", Metadata: map[string]string{"conf.k": "b"}}); err == nil {
				t.Fatalf("CreatePinned(%q) over an occupied id (closed=%v) succeeded with %q", id, closed, again.ID)
			}
			after, err := s.Get(id)
			if err != nil {
				t.Fatalf("Get(%q) after the refusal: %v", id, err)
			}
			if after.Title != before.Title || after.Status != before.Status || after.Metadata["conf.k"] != before.Metadata["conf.k"] {
				t.Fatalf("refused CreatePinned changed the occupant (closed=%v): before (%q %s %q), after (%q %s %q)",
					closed, before.Title, before.Status, before.Metadata["conf.k"], after.Title, after.Status, after.Metadata["conf.k"])
			}
		}
	})

	t.Run("ConcurrentWritersInsertExactlyOnce", func(t *testing.T) {
		s := newStore(t)
		id := pinnedID(t, s, "pinnedrace")
		const writers = 8
		handles := make([]beads.PinnedIDCreator, writers)
		for i := range handles {
			handle := s
			if newPeer != nil && i%2 == 1 {
				handle = newPeer(t, s)
			}
			handles[i] = pinned(t, handle)
		}
		var (
			wg      sync.WaitGroup
			start   = make(chan struct{})
			mu      sync.Mutex
			winners []string
		)
		for i, creator := range handles {
			wg.Add(1)
			go func(i int, creator beads.PinnedIDCreator) {
				defer wg.Done()
				<-start
				title := "writer-" + string(rune('a'+i))
				if _, err := creator.CreatePinned(beads.Bead{ID: id, Title: title}); err == nil {
					mu.Lock()
					winners = append(winners, title)
					mu.Unlock()
				}
			}(i, creator)
		}
		close(start)
		wg.Wait()
		if len(winners) != 1 {
			t.Fatalf("%d writers won the pinned create of %q (%v), want exactly 1", len(winners), id, winners)
		}
		got, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}
		if got.Title != winners[0] {
			t.Fatalf("occupant of %q has title %q, want the winner %q: a loser overwrote it", id, got.Title, winners[0])
		}
	})
}
