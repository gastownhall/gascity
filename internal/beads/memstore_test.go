package beads_test

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func TestMemStore(t *testing.T) {
	factory := func() beads.Store { return beads.NewMemStore() }
	beadstest.RunStoreTests(t, factory)
	beadstest.RunSequentialIDTests(t, factory)
	beadstest.RunCreationOrderTests(t, factory)
	beadstest.RunDepTests(t, factory)
	beadstest.RunMetadataTests(t, factory)
}

func TestMemStoreSetMetadata(t *testing.T) {
	s := beads.NewMemStore()
	b, err := s.Create(beads.Bead{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMetadata(b.ID, "merge_strategy", "mr"); err != nil {
		t.Errorf("SetMetadata on existing bead: %v", err)
	}
}

func TestMemStoreSetMetadataNotFound(t *testing.T) {
	s := beads.NewMemStore()
	err := s.SetMetadata("nonexistent-999", "key", "value")
	if err == nil {
		t.Fatal("SetMetadata on nonexistent bead should return error")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestMemStoreListByLabel(t *testing.T) {
	s := beads.NewMemStore()

	// Create beads: two with matching label, one without.
	if _, err := s.Create(beads.Bead{Title: "first", Labels: []string{"order-run:lint"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(beads.Bead{Title: "unrelated"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(beads.Bead{Title: "third", Labels: []string{"order-run:lint", "extra"}}); err != nil {
		t.Fatal(err)
	}

	// Unlimited — should return 2 in newest-first order.
	got, err := s.ListByLabel("order-run:lint", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByLabel returned %d beads, want 2", len(got))
	}
	if got[0].Title != "third" {
		t.Errorf("got[0].Title = %q, want %q (newest first)", got[0].Title, "third")
	}
	if got[1].Title != "first" {
		t.Errorf("got[1].Title = %q, want %q", got[1].Title, "first")
	}

	// With limit 1 — should return only the newest.
	got, err = s.ListByLabel("order-run:lint", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ListByLabel(limit=1) returned %d beads, want 1", len(got))
	}
	if got[0].Title != "third" {
		t.Errorf("got[0].Title = %q, want %q", got[0].Title, "third")
	}
}

func TestMemStoreListOpenExcludesClosedByDefault(t *testing.T) {
	s := beads.NewMemStore()

	open, err := s.Create(beads.Bead{Title: "open"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.Create(beads.Bead{Title: "closed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(closed.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != open.ID {
		t.Fatalf("ListOpen() = %+v, want only %s", got, open.ID)
	}

	got, err = s.ListOpen("closed")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != closed.ID {
		t.Fatalf("ListOpen(\"closed\") = %+v, want only %s", got, closed.ID)
	}
}

func TestMemStoreChildrenExcludeClosedByDefault(t *testing.T) {
	s := beads.NewMemStore()

	parent, err := s.Create(beads.Bead{Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	openChild, err := s.Create(beads.Bead{Title: "open", ParentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	closedChild, err := s.Create(beads.Bead{Title: "closed", ParentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(closedChild.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.Children(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != openChild.ID {
		t.Fatalf("Children() = %+v, want only %s", got, openChild.ID)
	}

	got, err = s.Children(parent.ID, beads.IncludeClosed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Children(IncludeClosed) = %d items, want 2", len(got))
	}
}

func TestMemStoreListByLabelRequiresIncludeClosed(t *testing.T) {
	s := beads.NewMemStore()

	open, err := s.Create(beads.Bead{Title: "open", Labels: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.Create(beads.Bead{Title: "closed", Labels: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(closed.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListByLabel("x", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != open.ID {
		t.Fatalf("ListByLabel() = %+v, want only %s", got, open.ID)
	}

	got, err = s.ListByLabel("x", 0, beads.IncludeClosed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByLabel(IncludeClosed) = %d items, want 2", len(got))
	}
}

func TestMemStoreListByMetadataRequiresIncludeClosed(t *testing.T) {
	s := beads.NewMemStore()

	open, err := s.Create(beads.Bead{Title: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMetadata(open.ID, "gc.root_bead_id", "root-1"); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Create(beads.Bead{Title: "closed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMetadata(closed.ID, "gc.root_bead_id", "root-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(closed.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListByMetadata(map[string]string{"gc.root_bead_id": "root-1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != open.ID {
		t.Fatalf("ListByMetadata() = %+v, want only %s", got, open.ID)
	}

	got, err = s.ListByMetadata(map[string]string{"gc.root_bead_id": "root-1"}, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByMetadata(IncludeClosed) = %d items, want 2", len(got))
	}
}

func TestMemStoreRemoveLabels(t *testing.T) {
	s := beads.NewMemStore()
	b, err := s.Create(beads.Bead{Title: "test", Labels: []string{"a", "b", "c"}})
	if err != nil {
		t.Fatal(err)
	}

	// Remove label "b".
	if err := s.Update(b.ID, beads.UpdateOpts{RemoveLabels: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "a" || got.Labels[1] != "c" {
		t.Errorf("Labels = %v, want [a c]", got.Labels)
	}
}

func TestMemStoreRemoveLabelsNonexistent(t *testing.T) {
	s := beads.NewMemStore()
	b, err := s.Create(beads.Bead{Title: "test", Labels: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}

	// Removing a label that doesn't exist is a no-op.
	if err := s.Update(b.ID, beads.UpdateOpts{RemoveLabels: []string{"z"}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Labels) != 2 {
		t.Errorf("Labels = %v, want [a b]", got.Labels)
	}
}

func TestMemStoreAddAndRemoveLabels(t *testing.T) {
	s := beads.NewMemStore()
	b, err := s.Create(beads.Bead{Title: "test", Labels: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}

	// Add "c" and remove "a" in the same call. Add happens first, then remove.
	if err := s.Update(b.ID, beads.UpdateOpts{
		Labels:       []string{"c"},
		RemoveLabels: []string{"a"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "b" || got.Labels[1] != "c" {
		t.Errorf("Labels = %v, want [b c]", got.Labels)
	}
}

// --- DepAdd / DepRemove / DepList ---

func TestMemStoreDepAddAndList(t *testing.T) {
	s := beads.NewMemStore()

	if err := s.DepAdd("a", "b", "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := s.DepAdd("a", "c", "tracks"); err != nil {
		t.Fatal(err)
	}

	// Down: what does "a" depend on?
	deps, err := s.DepList("a", "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 2 {
		t.Fatalf("DepList(a, down) = %d deps, want 2", len(deps))
	}
	if deps[0].DependsOnID != "b" || deps[0].Type != "blocks" {
		t.Errorf("dep[0] = %+v, want {a, b, blocks}", deps[0])
	}
	if deps[1].DependsOnID != "c" || deps[1].Type != "tracks" {
		t.Errorf("dep[1] = %+v, want {a, c, tracks}", deps[1])
	}

	// Up: what depends on "b"?
	deps, err = s.DepList("b", "up")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("DepList(b, up) = %d deps, want 1", len(deps))
	}
	if deps[0].IssueID != "a" {
		t.Errorf("dep.IssueID = %q, want %q", deps[0].IssueID, "a")
	}
}

func TestMemStoreDepAddIdempotent(t *testing.T) {
	s := beads.NewMemStore()

	if err := s.DepAdd("a", "b", "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := s.DepAdd("a", "b", "blocks"); err != nil {
		t.Fatal(err)
	}

	deps, _ := s.DepList("a", "down")
	if len(deps) != 1 {
		t.Errorf("DepList after duplicate DepAdd = %d deps, want 1", len(deps))
	}
}

func TestMemStoreDepRemove(t *testing.T) {
	s := beads.NewMemStore()

	_ = s.DepAdd("a", "b", "blocks")
	_ = s.DepAdd("a", "c", "blocks")

	if err := s.DepRemove("a", "b"); err != nil {
		t.Fatal(err)
	}

	deps, _ := s.DepList("a", "down")
	if len(deps) != 1 {
		t.Fatalf("DepList after remove = %d deps, want 1", len(deps))
	}
	if deps[0].DependsOnID != "c" {
		t.Errorf("remaining dep = %+v, want depends_on c", deps[0])
	}
}

func TestMemStoreDepRemoveNonexistent(t *testing.T) {
	s := beads.NewMemStore()

	// Removing nonexistent dep is a no-op.
	if err := s.DepRemove("x", "y"); err != nil {
		t.Errorf("DepRemove nonexistent should not error: %v", err)
	}
}

func TestMemStoreDepListEmpty(t *testing.T) {
	s := beads.NewMemStore()

	deps, err := s.DepList("nonexistent", "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("DepList on empty store = %d deps, want 0", len(deps))
	}
}

func TestMemStoreReadyRespectsBlockingDeps(t *testing.T) {
	s := beads.NewMemStore()

	blocker, err := s.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := s.Create(beads.Bead{Title: "blocked", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := s.Create(beads.Bead{Title: "ready", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Ready() returned %d beads, want 2", len(got))
	}
	if got[0].ID != blocker.ID || got[1].ID != ready.ID {
		t.Fatalf("Ready() IDs = [%s %s], want [%s %s]", got[0].ID, got[1].ID, blocker.ID, ready.ID)
	}

	if err := s.Close(blocker.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Ready() after closing blocker returned %d beads, want 2", len(got))
	}
	if got[0].ID != blocked.ID || got[1].ID != ready.ID {
		t.Fatalf("Ready() after closing blocker IDs = [%s %s], want [%s %s]", got[0].ID, got[1].ID, blocked.ID, ready.ID)
	}
}

func TestMemStoreDepListDefaultDirection(t *testing.T) {
	s := beads.NewMemStore()
	_ = s.DepAdd("a", "b", "blocks")

	// Empty direction string should default to "down".
	deps, err := s.DepList("a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Errorf("DepList(a, '') = %d deps, want 1", len(deps))
	}
}
