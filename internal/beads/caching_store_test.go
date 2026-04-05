package beads_test

import (
	"context"
	"encoding/json"
	"errors"
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
	list, err := cs.ListOpen()
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

func TestCachingStoreGetFallsBackForClosedBeadsAfterPrime(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	closed, err := mem.Create(beads.Bead{Title: "Closed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mem.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if items, err := cs.ListOpen(); err != nil {
		t.Fatalf("ListOpen: %v", err)
	} else if len(items) != 0 {
		t.Fatalf("ListOpen len = %d, want 0 active items", len(items))
	}

	got, err := cs.Get(closed.ID)
	if err != nil {
		t.Fatalf("Get(closed): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}

func TestCachingStoreReadyFallsBackForClosedBlockingDepsAfterPrime(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	blocker, err := mem.Create(beads.Bead{Title: "Closed blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	if err := mem.Close(blocker.ID); err != nil {
		t.Fatalf("Close(blocker): %v", err)
	}
	ready, err := mem.Create(beads.Bead{Title: "Ready after closed blocker"})
	if err != nil {
		t.Fatalf("Create(ready): %v", err)
	}
	if err := mem.DepAdd(ready.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	got, err := cs.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 1 || got[0].ID != ready.ID {
		t.Fatalf("Ready() = %v, want only %s", got, ready.ID)
	}
}

func TestCachingStoreListPartialAllowScanReturnsCompleteActiveSnapshot(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	openBead, err := mem.Create(beads.Bead{Title: "Open"})
	if err != nil {
		t.Fatalf("Create(open): %v", err)
	}
	inProgress, err := mem.Create(beads.Bead{Title: "In progress", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create(in_progress): %v", err)
	}
	closed, err := mem.Create(beads.Bead{Title: "Closed"})
	if err != nil {
		t.Fatalf("Create(closed): %v", err)
	}
	if err := mem.Close(closed.ID); err != nil {
		t.Fatalf("Close(closed): %v", err)
	}

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if cs.IsLive() {
		t.Fatal("cache should remain partial after PrimeActive")
	}

	got, err := cs.List(beads.ListQuery{AllowScan: true, Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatalf("List(AllowScan): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(AllowScan) len = %d, want 2 active beads", len(got))
	}
	if got[0].ID != openBead.ID || got[1].ID != inProgress.ID {
		t.Fatalf("List(AllowScan) IDs = [%s %s], want [%s %s]", got[0].ID, got[1].ID, openBead.ID, inProgress.ID)
	}
}

func TestCachingStoreListPartialMetadataMatchesActiveBeads(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	openBead, err := mem.Create(beads.Bead{Title: "Open"})
	if err != nil {
		t.Fatalf("Create(open): %v", err)
	}
	if err := mem.SetMetadata(openBead.ID, "gc.kind", "workflow"); err != nil {
		t.Fatalf("SetMetadata(open): %v", err)
	}
	inProgress, err := mem.Create(beads.Bead{Title: "In progress", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create(in_progress): %v", err)
	}
	if err := mem.SetMetadata(inProgress.ID, "gc.kind", "workflow"); err != nil {
		t.Fatalf("SetMetadata(in_progress): %v", err)
	}
	closed, err := mem.Create(beads.Bead{Title: "Closed"})
	if err != nil {
		t.Fatalf("Create(closed): %v", err)
	}
	if err := mem.SetMetadata(closed.ID, "gc.kind", "workflow"); err != nil {
		t.Fatalf("SetMetadata(closed): %v", err)
	}
	if err := mem.Close(closed.ID); err != nil {
		t.Fatalf("Close(closed): %v", err)
	}

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	got, err := cs.List(beads.ListQuery{
		Metadata: map[string]string{"gc.kind": "workflow"},
		Sort:     beads.SortCreatedAsc,
	})
	if err != nil {
		t.Fatalf("List(metadata): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(metadata) len = %d, want 2 active workflow beads", len(got))
	}
	if got[0].ID != openBead.ID || got[1].ID != inProgress.ID {
		t.Fatalf("List(metadata) IDs = [%s %s], want [%s %s]", got[0].ID, got[1].ID, openBead.ID, inProgress.ID)
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

func TestCachingStoreListIncludeClosedFallsBackToCachedMatches(t *testing.T) {
	t.Parallel()
	backing := &failingIncludeClosedMetadataStore{MemStore: beads.NewMemStore()}
	match, _ := backing.Create(beads.Bead{Title: "A"})
	_ = backing.SetMetadata(match.ID, "gc.kind", "workflow")
	other, _ := backing.Create(beads.Bead{Title: "B"})
	_ = backing.SetMetadata(other.ID, "gc.kind", "task")

	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	results, err := cs.List(beads.ListQuery{
		Metadata:      map[string]string{"gc.kind": "workflow"},
		IncludeClosed: true,
	})
	if err != nil {
		t.Fatalf("List(include closed): %v", err)
	}
	if len(results) != 1 || results[0].ID != match.ID {
		t.Fatalf("results = %v, want only %s", results, match.ID)
	}
}

type failingIncludeClosedMetadataStore struct {
	*beads.MemStore
}

func (s *failingIncludeClosedMetadataStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.IncludeClosed && len(query.Metadata) > 0 {
		return nil, errors.New("history unavailable")
	}
	return s.MemStore.List(query)
}

func strPtr(s string) *string { return &s }
