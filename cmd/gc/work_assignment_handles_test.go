package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// fenceOnlyBacking is a store with a revision fence (UpdateIfMatch) and no
// guarded assignment update, so a release through a cache over it takes
// releaseAssignmentFenced's re-read-and-fence path.
type fenceOnlyBacking struct {
	beads.Store
	mem *beads.MemStore
}

func (s fenceOnlyBacking) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	return s.mem.UpdateIfMatch(id, expectedRevision, opts)
}

// TestReleaseAssignmentFencedThroughAHandleReadsPastTheCache is the T7a
// HandlesFor audit's pin for releaseAssignmentFenced, the one cmd/gc
// HandlesFor call a test hands a typed handle
// (TestReleaseOrphanedPoolAssignment_FencedFallbackFollowsAResolveTarget):
// through a handle over a CachingStore, the fence's re-read is the backing's
// live row, so a claim made past the cache reads as the bead having moved on
// (changed, no error). Before the handles forwarded HandlesFor, the re-read
// served the cache's stale row, passed the snapshot compare, and the fence on
// its stale revision then failed as lost.
func TestReleaseAssignmentFencedThroughAHandleReadsPastTheCache(t *testing.T) {
	mem := beads.NewMemStore()
	claimed, err := mem.Create(beads.Bead{Title: "pool work", Assignee: "dead-session"})
	if err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStoreForTest(fenceOnlyBacking{Store: mem, mem: mem}, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := cache.Get(claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	live := "live-session"
	if err := mem.Update(claimed.ID, beads.UpdateOpts{Assignee: &live}); err != nil {
		t.Fatal(err)
	}
	if stale, err := cache.Get(claimed.ID); err != nil || stale.Assignee != "dead-session" {
		t.Fatalf("fixture: the cache reads %q, %v; want the stale dead-session claim", stale.Assignee, err)
	}

	open, none := "open", ""
	outcome, err := releaseAssignmentFenced(beadstest.Work(cache), snapshot, beads.UpdateOpts{Status: &open, Assignee: &none})
	if err != nil || outcome != fencedReleaseChanged {
		t.Fatalf("release through a handle over a stale cache = %v, %v; want changed, no error", outcome, err)
	}
	if got, err := mem.Get(claimed.ID); err != nil || got.Assignee != live || got.Status != snapshot.Status {
		t.Fatalf("the live claim = %q %q, %v; want it kept", got.Assignee, got.Status, err)
	}
}
