package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestWorkSelectorCountForControllerUsesSharedSelector(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title: "ready",
		Type:  "task",
		Metadata: map[string]string{
			"gc.routed_to": "rig/worker",
		},
	}); err != nil {
		t.Fatalf("create ready: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "assigned",
		Type:     "task",
		Assignee: "other",
		Metadata: map[string]string{
			"gc.routed_to": "rig/worker",
		},
	}); err != nil {
		t.Fatalf("create assigned: %v", err)
	}
	selector := config.WorkSelector{
		Type:       "task",
		Unassigned: true,
		Metadata: map[string]string{
			"gc.routed_to": "rig/worker",
		},
	}
	count, err := workSelectorCountForController(store, selector)
	if err != nil {
		t.Fatalf("workSelectorCountForController: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestTypedDemandSelectorPrefersScaleCheckQuery(t *testing.T) {
	scaleSelector := config.WorkSelector{Type: "task", Label: "scale"}
	workSelector := config.WorkSelector{Type: "task", Label: "work"}
	selector, ok := typedDemandSelectorForAgent(&config.Agent{
		Name:            "worker",
		ScaleCheckQuery: scaleSelector,
		WorkSelector:    workSelector,
	})
	if !ok {
		t.Fatal("typedDemandSelectorForAgent ok = false, want true")
	}
	if !selector.Equivalent(scaleSelector) {
		t.Fatalf("selector = %+v, want scale selector %+v", selector, scaleSelector)
	}
}
