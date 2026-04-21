package molecule

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCloseSubtreeClosesOpenDescendantThroughClosedParent(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "child", ParentID: root.ID})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	grandchild, err := store.Create(beads.Bead{Title: "grandchild", ParentID: child.ID})
	if err != nil {
		t.Fatalf("create grandchild: %v", err)
	}
	if err := store.Close(child.ID); err != nil {
		t.Fatalf("close child: %v", err)
	}

	closed, err := CloseSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("CloseSubtree: %v", err)
	}
	if closed != 2 {
		t.Fatalf("CloseSubtree closed %d beads, want 2", closed)
	}

	for _, id := range []string{root.ID, child.ID, grandchild.ID} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, b.Status)
		}
	}
}

func TestCloseSubtreeClosesLogicalRootMembersAndTheirChildren(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	detached, err := store.Create(beads.Bead{
		Title: "detached control",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("create detached: %v", err)
	}
	logicalChild, err := store.Create(beads.Bead{
		Title:    "logical child",
		ParentID: detached.ID,
	})
	if err != nil {
		t.Fatalf("create logical child: %v", err)
	}

	closed, err := CloseSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("CloseSubtree: %v", err)
	}
	if closed != 3 {
		t.Fatalf("CloseSubtree closed %d beads, want 3", closed)
	}

	for _, id := range []string{root.ID, detached.ID, logicalChild.ID} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, b.Status)
		}
	}
}

func TestCloseSubtreeHandlesParentCycles(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "child", ParentID: root.ID})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := store.Update(root.ID, beads.UpdateOpts{ParentID: &child.ID}); err != nil {
		t.Fatalf("Update(root.ParentID): %v", err)
	}

	closed, err := CloseSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("CloseSubtree: %v", err)
	}
	if closed != 2 {
		t.Fatalf("CloseSubtree closed %d beads, want 2", closed)
	}
	for _, id := range []string{root.ID, child.ID} {
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if bead.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, bead.Status)
		}
	}
}
