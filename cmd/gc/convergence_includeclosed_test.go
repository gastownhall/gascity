package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
)

// TestConvergenceStoreAdapter_ChildrenIncludesClosedWisps is a regression test
// for gascity#8505: the adapter's Children() must include closed beads so
// that deriveIterationCount (and the other callers that walk Children()) can
// see closed wisps. Before the fix, the adapter's underlying List query
// omitted IncludeClosed, so Children() for a root with only a closed wisp
// always came back empty.
func TestConvergenceStoreAdapter_ChildrenIncludesClosedWisps(t *testing.T) {
	store := beads.NewMemStore()
	adapter := newConvergenceStoreAdapter(store, nil, false)

	root, err := store.Create(beads.Bead{Title: "root", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatalf("creating root: %v", err)
	}
	wisp, err := store.Create(beads.Bead{
		Title:    "wisp",
		Type:     "task",
		Status:   "in_progress",
		ParentID: root.ID,
		Metadata: map[string]string{"idempotency_key": convergence.IdempotencyKey(root.ID, 1)},
	})
	if err != nil {
		t.Fatalf("creating wisp: %v", err)
	}
	if err := store.Close(wisp.ID); err != nil {
		t.Fatalf("closing wisp: %v", err)
	}

	children, err := adapter.Children(root.ID)
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("Children returned %d beads, want 1 (closed wisp should be included)", len(children))
	}
	if children[0].ID != wisp.ID {
		t.Fatalf("Children[0].ID = %q, want %q", children[0].ID, wisp.ID)
	}
	if children[0].Status != "closed" {
		t.Fatalf("Children[0].Status = %q, want closed", children[0].Status)
	}
}

// TestHandleWispClosed_StoredIterationSurvivesClosedChildren reproduces the
// end-to-end regression from gascity#8505 using the real adapter and a real
// beads store (not the package-level fakeStore, which never filtered closed
// beads to begin with and so could not have caught this bug).
//
// deriveIterationCount counts closed children via Children(), and the
// handler overwrites the stored iteration count with whatever
// deriveIterationCount returns whenever the two disagree ("stored disagrees
// with derived, use derived"). If Children() fails to include closed beads,
// the derived count is always 0, and the correct stored count of 1 gets
// stomped back to 0 every time a wisp closes.
func TestHandleWispClosed_StoredIterationSurvivesClosedChildren(t *testing.T) {
	store := beads.NewMemStore()
	adapter := newConvergenceStoreAdapter(store, nil, false)

	root, err := store.Create(beads.Bead{Title: "root", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatalf("creating root: %v", err)
	}
	wispKey := convergence.IdempotencyKey(root.ID, 1)
	wisp, err := store.Create(beads.Bead{
		Title:    "wisp",
		Type:     "task",
		Status:   "in_progress",
		ParentID: root.ID,
		Metadata: map[string]string{"idempotency_key": wispKey},
	})
	if err != nil {
		t.Fatalf("creating wisp: %v", err)
	}

	rootMeta := map[string]string{
		convergence.FieldState:             convergence.StateActive,
		convergence.FieldIteration:         convergence.EncodeInt(1),
		convergence.FieldMaxIterations:     "5",
		convergence.FieldFormula:           "test-formula",
		convergence.FieldTarget:            "test-agent",
		convergence.FieldGateMode:          convergence.GateModeManual,
		convergence.FieldGateTimeout:       "60s",
		convergence.FieldGateTimeoutAction: convergence.TimeoutActionIterate,
	}
	for k, v := range rootMeta {
		if err := store.SetMetadata(root.ID, k, v); err != nil {
			t.Fatalf("SetMetadata(%s): %v", k, err)
		}
	}

	// Close lap 1's wisp.
	if err := store.Close(wisp.ID); err != nil {
		t.Fatalf("closing wisp: %v", err)
	}

	handler := &convergence.Handler{
		Store: adapter,
		Clock: time.Now,
	}

	if _, err := handler.HandleWispClosed(context.Background(), root.ID, wisp.ID); err != nil {
		t.Fatalf("HandleWispClosed: %v", err)
	}

	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	got, _ := convergence.DecodeInt(rootAfter.Metadata[convergence.FieldIteration])
	if got != 1 {
		t.Fatalf("stored %s = %d, want 1 (closed wisp should count toward derived iteration, not reset it to 0)", convergence.FieldIteration, got)
	}
}
