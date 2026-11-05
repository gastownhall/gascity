package sourceworkflow

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestListWorkflowBeadsReadsLiveThroughATarget pins the live read through a
// molecule target (typed-stores T5a): over a primed cache, a root the backing
// store has since closed is read as closed. HandlesFor on the target would
// fall back to the logical reader over the target, which the cache answers
// from its own stale copy.
func TestListWorkflowBeadsReadsLiveThroughATarget(t *testing.T) {
	backing := beads.NewMemStore()
	root, err := backing.Create(beads.Bead{Title: "root", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Close(root.ID); err != nil {
		t.Fatalf("closing the root behind the cache: %v", err)
	}
	if cached, err := cache.Get(root.ID); err != nil || cached.Status == "closed" {
		t.Fatalf("fixture: the cache answers %q (%v), want its stale open copy", cached.Status, err)
	}

	got, err := ListWorkflowBeads(beadstest.GraphMolecule(cache), root.ID)
	if err != nil {
		t.Fatalf("ListWorkflowBeads: %v", err)
	}
	if len(got) != 1 || got[0].ID != root.ID {
		t.Fatalf("ListWorkflowBeads = %+v, want the root alone", got)
	}
	if got[0].Status != "closed" {
		t.Errorf("ListWorkflowBeads read the root as %q through the target, want the live closed status", got[0].Status)
	}
}
