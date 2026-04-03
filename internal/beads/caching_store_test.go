package beads_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCachingStoreReadThrough(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	b1, _ := mem.Create(beads.Bead{Title: "Task 1"})
	b2, _ := mem.Create(beads.Bead{Title: "Task 2"})
	if err := mem.DepAdd(b2.ID, b1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if !cs.IsLive() {
		t.Fatal("should be live after prime")
	}

	// List
	list, err := cs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}

	// Get
	got, err := cs.Get(b1.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Task 1" {
		t.Fatalf("title = %q, want Task 1", got.Title)
	}

	// DepList
	deps, err := cs.DepList(b2.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != b1.ID {
		t.Fatalf("deps = %v, want 1 dep on %s", deps, b1.ID)
	}

	// Ready (b1 has no deps, b2 is blocked)
	ready, err := cs.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != b1.ID {
		t.Fatalf("Ready = %v, want only %s", ready, b1.ID)
	}
}

func TestCachingStoreWriteThrough(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Create through caching store
	b, err := cs.Create(beads.Bead{Title: "New"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Should be in cache
	got, err := cs.Get(b.ID)
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if got.Title != "New" {
		t.Fatalf("title = %q, want New", got.Title)
	}

	// Update
	if err := cs.Update(b.ID, beads.UpdateOpts{Title: strPtr("Updated")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = cs.Get(b.ID)
	if got.Title != "Updated" {
		t.Fatalf("title after update = %q, want Updated", got.Title)
	}

	// Close
	if err := cs.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, _ = cs.Get(b.ID)
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}

func TestCachingStoreApplyEvent(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	b1, _ := mem.Create(beads.Bead{Title: "Existing"})

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Apply a create event for a bead that doesn't exist in cache yet.
	newBead := beads.Bead{ID: "ext-1", Title: "External", Status: "open"}
	payload, _ := json.Marshal(newBead)
	cs.ApplyEvent("bead.created", payload)

	got, err := cs.Get("ext-1")
	if err != nil {
		t.Fatalf("Get after ApplyEvent create: %v", err)
	}
	if got.Title != "External" {
		t.Fatalf("title = %q, want External", got.Title)
	}

	// Apply an update event.
	updated := beads.Bead{ID: b1.ID, Title: "Modified by agent", Status: "open", Metadata: map[string]string{"gc.step_ref": "mol.review"}}
	payload, _ = json.Marshal(updated)
	cs.ApplyEvent("bead.updated", payload)

	got, _ = cs.Get(b1.ID)
	if got.Title != "Modified by agent" {
		t.Fatalf("title after update event = %q, want Modified by agent", got.Title)
	}
	if got.Metadata["gc.step_ref"] != "mol.review" {
		t.Fatalf("metadata after update = %v, want gc.step_ref=mol.review", got.Metadata)
	}

	// Apply a close event.
	closed := beads.Bead{ID: b1.ID, Status: "closed", Metadata: map[string]string{"gc.outcome": "pass"}}
	payload, _ = json.Marshal(closed)
	cs.ApplyEvent("bead.closed", payload)

	got, _ = cs.Get(b1.ID)
	if got.Status != "closed" {
		t.Fatalf("status after close event = %q, want closed", got.Status)
	}
	if got.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("outcome = %q, want pass", got.Metadata["gc.outcome"])
	}
	// Original metadata should be preserved (merged, not replaced).
	if got.Metadata["gc.step_ref"] != "mol.review" {
		t.Fatalf("step_ref lost after close event: %v", got.Metadata)
	}
}

func TestCachingStoreApplyEventIgnoredWhenDegraded(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	cs := beads.NewCachingStoreForTest(mem, nil)
	// Don't prime — stays uninitialized.

	payload, _ := json.Marshal(beads.Bead{ID: "gc-1", Title: "Test"})
	cs.ApplyEvent("bead.created", payload)

	// Should not be findable (not live).
	_, err := cs.Get("gc-1")
	if err == nil {
		t.Fatal("Get should fail when not live")
	}
}

func TestCachingStoreDegradedFallsThrough(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	b, _ := mem.Create(beads.Bead{Title: "Backing"})

	cs := beads.NewCachingStoreForTest(mem, nil)
	// Don't prime — reads fall through to backing.

	got, err := cs.Get(b.ID)
	if err != nil {
		t.Fatalf("Get fallthrough: %v", err)
	}
	if got.Title != "Backing" {
		t.Fatalf("title = %q, want Backing", got.Title)
	}
}

func TestCachingStoreOnChangeCallback(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()

	var events []string
	cs := beads.NewCachingStoreForTest(mem, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	b, _ := cs.Create(beads.Bead{Title: "Test"})
	_ = cs.Update(b.ID, beads.UpdateOpts{Title: strPtr("Changed")})
	_ = cs.Close(b.ID)

	if len(events) != 3 {
		t.Fatalf("events = %v, want 3", events)
	}
	if events[0] != "bead.created:"+b.ID {
		t.Errorf("events[0] = %q", events[0])
	}
	if events[1] != "bead.updated:"+b.ID {
		t.Errorf("events[1] = %q", events[1])
	}
	if events[2] != "bead.closed:"+b.ID {
		t.Errorf("events[2] = %q", events[2])
	}
}

func TestCachingStoreReconcilerStopsOnCancel(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cs.StartReconciler(ctx)
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)
	// Should not hang.
}

func TestCachingStoreListByMetadata(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	b1, _ := mem.Create(beads.Bead{Title: "A"})
	_ = mem.SetMetadata(b1.ID, "gc.kind", "workflow")
	b2, _ := mem.Create(beads.Bead{Title: "B"})
	_ = mem.SetMetadata(b2.ID, "gc.kind", "task")

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	results, err := cs.ListByMetadata(map[string]string{"gc.kind": "workflow"}, 0)
	if err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	if len(results) != 1 || results[0].ID != b1.ID {
		t.Fatalf("results = %v, want only %s", results, b1.ID)
	}
}

func strPtr(s string) *string { return &s }
