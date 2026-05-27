package main

import (
	"errors"
	"sync"
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

func TestClaimNextWorkSingleWinner(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{
		Title: "claim me",
		Type:  "task",
		Metadata: map[string]string{
			"gc.routed_to": "rig/worker",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	selector := config.WorkSelector{
		Type:       "task",
		Unassigned: true,
		Metadata:   map[string]string{"gc.routed_to": "rig/worker"},
	}

	var wg sync.WaitGroup
	type result struct {
		bead beads.Bead
		err  error
	}
	results := make(chan result, 2)
	for _, assignee := range []string{"worker-1", "worker-2"} {
		wg.Add(1)
		go func(assignee string) {
			defer wg.Done()
			claimed, claimErr := claimNextWork(store, selector, assignee, map[string]string{"claimed_by": assignee})
			results <- result{bead: claimed, err: claimErr}
		}(assignee)
	}
	wg.Wait()
	close(results)

	winners := 0
	losers := 0
	winner := ""
	for res := range results {
		if res.err == nil {
			winners++
			winner = res.bead.Assignee
			if res.bead.Status != "in_progress" {
				t.Fatalf("claimed status = %q, want in_progress", res.bead.Status)
			}
			continue
		}
		if errors.Is(res.err, beads.ErrClaimLost) || errors.Is(res.err, errWorkNotFound) {
			losers++
			continue
		}
		t.Fatalf("claim error = %v, want ErrClaimLost or no work", res.err)
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("winners=%d losers=%d, want 1/1", winners, losers)
	}
	final, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get final: %v", err)
	}
	if final.Status != "in_progress" || final.Assignee != winner {
		t.Fatalf("final = status %q assignee %q, want winner %q in_progress", final.Status, final.Assignee, winner)
	}
	if final.Metadata["claimed_by"] != winner {
		t.Fatalf("claimed_by metadata = %q, want winner %q", final.Metadata["claimed_by"], winner)
	}
}

func TestNormalizeWorkClaimStatus(t *testing.T) {
	for _, raw := range []string{"", "in_progress", "  in_progress  "} {
		got, err := normalizeWorkClaimStatus(raw)
		if err != nil {
			t.Fatalf("normalizeWorkClaimStatus(%q): %v", raw, err)
		}
		if got != "in_progress" {
			t.Fatalf("normalizeWorkClaimStatus(%q) = %q, want in_progress", raw, got)
		}
	}

	if _, err := normalizeWorkClaimStatus("open"); err == nil {
		t.Fatal("normalizeWorkClaimStatus(open) error = nil, want unsupported status error")
	}
}
