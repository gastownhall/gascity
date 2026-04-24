package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestReassignOpenWorkAssignedToSession_UsesLiveOpenOwnership(t *testing.T) {
	t.Parallel()

	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{
		Title:    "open work",
		Type:     "task",
		Status:   "open",
		Assignee: "retired-session",
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}

	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	reassigned := "replacement-picked-elsewhere"
	if err := backing.Update(work.ID, beads.UpdateOpts{Assignee: &reassigned}); err != nil {
		t.Fatalf("Update(%s, reassigned): %v", work.ID, err)
	}

	if err := reassignOpenWorkAssignedToSession(cache, "retired-session", "new-canonical"); err != nil {
		t.Fatalf("reassignOpenWorkAssignedToSession: %v", err)
	}

	got, err := backing.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", work.ID, err)
	}
	if got.Assignee != reassigned {
		t.Fatalf("Assignee = %q, want %q; stale open ownership should not be overwritten", got.Assignee, reassigned)
	}
}
