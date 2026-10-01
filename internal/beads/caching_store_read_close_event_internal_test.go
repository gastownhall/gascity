package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestParentListDoesNotConsumeExternalCloseEvent(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	parent, err := backing.Create(Bead{Title: "workflow"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	step, err := backing.Create(Bead{Title: "review", ParentID: parent.ID, Metadata: map[string]string{"gc.root_bead_id": parent.ID}})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}

	var events []string
	var observedRunID string
	var closePayload Bead
	cache := NewCachingStore(backing, func(eventType, beadID, runID, _, _ string, _ *[]string, payload json.RawMessage) {
		events = append(events, eventType+":"+beadID)
		observedRunID = runID
		if err := json.Unmarshal(payload, &closePayload); err != nil {
			t.Errorf("decode close event: %v", err)
		}
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	if err := backing.Close(step.ID); err != nil {
		t.Fatalf("close backing step: %v", err)
	}

	// Workflow reads can reach the backing store before the periodic cache scan.
	// They must not consume the transition without recording it for run views.
	children, err := cache.List(ListQuery{ParentID: parent.ID})
	if err != nil {
		t.Fatalf("list workflow children: %v", err)
	}
	if len(children) != 0 {
		t.Fatalf("active children = %v, want none", children)
	}
	cache.runReconciliation()

	want := "bead.closed:" + step.ID
	if len(events) != 1 || events[0] != want {
		t.Fatalf("events = %v, want exactly [%s]", events, want)
	}
	if observedRunID != parent.ID || closePayload.Status != "closed" || closePayload.ID != step.ID {
		t.Fatalf("close event run=%q bead=%+v, want run %q closed step %q", observedRunID, closePayload, parent.ID, step.ID)
	}
}

func TestClosedHistoryListDoesNotConsumeExternalCloseEvent(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	parent, err := backing.Create(Bead{Title: "workflow"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	step, err := backing.Create(Bead{Title: "review", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	if err := backing.Close(step.ID); err != nil {
		t.Fatalf("close backing step: %v", err)
	}

	children, err := cache.List(ListQuery{ParentID: parent.ID, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list workflow history: %v", err)
	}
	if len(children) != 1 || children[0].ID != step.ID || children[0].Status != "closed" {
		t.Fatalf("children = %v, want closed step %q", children, step.ID)
	}
	cache.runReconciliation()

	want := "bead.closed:" + step.ID
	if len(events) != 1 || events[0] != want {
		t.Fatalf("events = %v, want exactly [%s]", events, want)
	}
}

func TestDirtyListDoesNotConsumeExternalCloseEvent(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	step, err := backing.Create(Bead{Title: "review"})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	if err := backing.Close(step.ID); err != nil {
		t.Fatalf("close backing step: %v", err)
	}
	cache.mu.Lock()
	cache.markDirtyLocked(step.ID)
	cache.mu.Unlock()

	active, err := cache.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("list active beads: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("active beads = %v, want none", active)
	}
	cache.runReconciliation()

	want := "bead.closed:" + step.ID
	if len(events) != 1 || events[0] != want {
		t.Fatalf("events = %v, want exactly [%s]", events, want)
	}
}

func TestDirtyGetDoesNotConsumeExternalCloseEvent(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	step, err := backing.Create(Bead{Title: "review"})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	if err := backing.Close(step.ID); err != nil {
		t.Fatalf("close backing step: %v", err)
	}
	cache.mu.Lock()
	cache.markDirtyLocked(step.ID)
	cache.mu.Unlock()

	got, err := cache.Get(step.ID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("step status = %q, want closed", got.Status)
	}
	cache.runReconciliation()

	want := "bead.closed:" + step.ID
	if len(events) != 1 || events[0] != want {
		t.Fatalf("events = %v, want exactly [%s]", events, want)
	}
}

func TestParentListMissingClosedChildStillEmitsClose(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	parent, err := mem.Create(Bead{Title: "workflow"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	step, err := mem.Create(Bead{Title: "review", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	if err := mem.Close(step.ID); err != nil {
		t.Fatalf("close backing step: %v", err)
	}
	backing.getErr = map[string]error{step.ID: fmt.Errorf("hidden closed row: %w", ErrNotFound)}

	if _, err := cache.List(ListQuery{ParentID: parent.ID}); err != nil {
		t.Fatalf("list workflow children: %v", err)
	}
	cache.runReconciliation()

	want := "bead.closed:" + step.ID
	if len(events) != 1 || events[0] != want {
		t.Fatalf("events = %v, want exactly [%s]", events, want)
	}
}
