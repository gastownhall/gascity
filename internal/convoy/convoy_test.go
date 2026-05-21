package convoy

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func testConvoyDeps(store beads.Store) ConvoyDeps {
	return ConvoyDeps{
		Cfg: &config.City{},
		GetStore: func(_ string) (beads.Store, error) {
			return store, nil
		},
		FindStore: func(_ string) (beads.Store, error) {
			return store, nil
		},
		Recorder: events.NewFake(),
	}
}

func TestConvoyCreateOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	result, err := ConvoyCreate(deps, store, ConvoyCreateInput{
		Title: "my convoy",
		Fields: ConvoyFields{
			Owner:  "mayor",
			Target: "main",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Convoy.Title != "my convoy" {
		t.Errorf("title = %q, want %q", result.Convoy.Title, "my convoy")
	}
	if result.Convoy.Type != "convoy" {
		t.Errorf("type = %q, want convoy", result.Convoy.Type)
	}
	// Verify metadata was applied.
	got, err := store.Get(result.Convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["convoy.owner"] != "mayor" {
		t.Errorf("owner = %q, want mayor", got.Metadata["convoy.owner"])
	}

	// Verify event was emitted.
	fake := deps.Recorder.(*events.Fake)
	if len(fake.Events) == 0 {
		t.Error("expected ConvoyCreated event to be emitted")
	}
}

func TestConvoyCreateWithItemsOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	// Create child beads first.
	epic, _ := store.Create(beads.Bead{Title: "epic", Type: "epic"})
	b1, _ := store.Create(beads.Bead{Title: "task 1", ParentID: epic.ID})
	b2, _ := store.Create(beads.Bead{Title: "task 2"})

	result, err := ConvoyCreate(deps, store, ConvoyCreateInput{
		Title: "linked convoy",
		Items: []string{b1.ID, b2.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LinkedCount != 2 {
		t.Errorf("linked = %d, want 2", result.LinkedCount)
	}

	// Verify children are tracked without evicting their existing parent.
	child1, _ := store.Get(b1.ID)
	if child1.ParentID != epic.ID {
		t.Errorf("child1 parent = %q, want preserved epic parent %q", child1.ParentID, epic.ID)
	}
	requireTracksDep(t, store, result.Convoy.ID, b1.ID)
	requireTracksDep(t, store, result.Convoy.ID, b2.ID)
}

func TestConvoyProgressOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	b1, _ := store.Create(beads.Bead{Title: "task 1", ParentID: convoy.ID})
	if _, err := store.Create(beads.Bead{Title: "task 2", ParentID: convoy.ID}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b1.ID); err != nil {
		t.Fatal(err)
	}

	progress, err := ConvoyProgress(deps, store, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Total != 2 {
		t.Errorf("total = %d, want 2", progress.Total)
	}
	if progress.Closed != 1 {
		t.Errorf("closed = %d, want 1", progress.Closed)
	}
	if progress.Complete {
		t.Error("expected not complete")
	}
}

func TestConvoyProgressCompleteOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	b1, _ := store.Create(beads.Bead{Title: "task 1", ParentID: convoy.ID})
	if err := store.Close(b1.ID); err != nil {
		t.Fatal(err)
	}

	progress, err := ConvoyProgress(deps, store, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !progress.Complete {
		t.Error("expected complete")
	}
}

func TestConvoyProgressTracksDepsOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	b1, _ := store.Create(beads.Bead{Title: "task 1"})
	b2, _ := store.Create(beads.Bead{Title: "task 2"})
	if err := store.DepAdd(convoy.ID, b1.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(convoy.ID, b2.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b1.ID); err != nil {
		t.Fatal(err)
	}

	progress, err := ConvoyProgress(deps, store, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Total != 2 {
		t.Errorf("total = %d, want 2", progress.Total)
	}
	if progress.Closed != 1 {
		t.Errorf("closed = %d, want 1", progress.Closed)
	}
	if progress.Complete {
		t.Error("expected not complete")
	}
}

func TestConvoyProgressTreatsTombstoneAsCompleteOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	b1, _ := store.Create(beads.Bead{Title: "task 1"})
	if err := store.DepAdd(convoy.ID, b1.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	tombstone := "tombstone"
	if err := store.Update(b1.ID, beads.UpdateOpts{Status: &tombstone}); err != nil {
		t.Fatal(err)
	}

	progress, err := ConvoyProgress(deps, store, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Total != 1 {
		t.Errorf("total = %d, want 1", progress.Total)
	}
	if progress.Closed != 1 {
		t.Errorf("closed = %d, want 1", progress.Closed)
	}
	if !progress.Complete {
		t.Error("expected tombstone tracked item to complete convoy")
	}
}

func TestConvoyMembersKeepsDanglingTracksUnknownOps(t *testing.T) {
	store := beads.NewMemStore()

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	if err := store.DepAdd(convoy.ID, "gc-missing", "tracks"); err != nil {
		t.Fatal(err)
	}

	members, err := Members(store, convoy.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1", len(members))
	}
	if members[0].ID != "gc-missing" {
		t.Errorf("member ID = %q, want gc-missing", members[0].ID)
	}
	if members[0].Status != "unknown" {
		t.Errorf("member status = %q, want unknown", members[0].Status)
	}
}

func TestConvoyAddItemsOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	epic, _ := store.Create(beads.Bead{Title: "epic", Type: "epic"})
	b1, _ := store.Create(beads.Bead{Title: "task 1", ParentID: epic.ID})

	err := ConvoyAddItems(deps, store, convoy.ID, []string{b1.ID})
	if err != nil {
		t.Fatal(err)
	}

	child, _ := store.Get(b1.ID)
	if child.ParentID != epic.ID {
		t.Errorf("parent = %q, want preserved epic parent %q", child.ParentID, epic.ID)
	}
	requireTracksDep(t, store, convoy.ID, b1.ID)
}

func TestConvoyCloseOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})

	err := ConvoyClose(deps, store, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := store.Get(convoy.ID)
	if got.Status != "closed" {
		t.Errorf("status = %q, want closed", got.Status)
	}

	// Verify event was emitted.
	fake := deps.Recorder.(*events.Fake)
	found := false
	for _, e := range fake.Events {
		if e.Type == events.ConvoyClosed {
			found = true
		}
	}
	if !found {
		t.Error("expected ConvoyClosed event to be emitted")
	}
}

func TestConvoyCloseNotFoundOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	err := ConvoyClose(deps, store, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent convoy")
	}
}

func requireTracksDep(t *testing.T, store beads.Store, convoyID, itemID string) {
	t.Helper()
	deps, err := store.DepList(convoyID, "down")
	if err != nil {
		t.Fatalf("DepList(%s): %v", convoyID, err)
	}
	for _, dep := range deps {
		if dep.IssueID == convoyID && dep.DependsOnID == itemID && dep.Type == "tracks" {
			return
		}
	}
	t.Fatalf("missing tracks dep %s -> %s; deps=%v", convoyID, itemID, deps)
}
