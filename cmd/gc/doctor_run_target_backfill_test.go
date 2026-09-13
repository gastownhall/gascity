package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestRunTargetRoutedToBackfillCheck covers the ga-eld2x backfill: graph.v2
// workflow roots stamped before the deprecation carry gc.run_target but no
// gc.routed_to. The check must flag them, --fix must set gc.routed_to from
// gc.run_target, and roots that already have gc.routed_to (or are not workflow
// roots) must be left untouched.
func TestRunTargetRoutedToBackfillCheck(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}

	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		// Needs backfill: workflow root, run_target set, routed_to empty.
		{ID: "WR-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": "mayor",
		}},
		// Healthy: already routed — must be left alone.
		{ID: "WR-2", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": "mayor", "gc.routed_to": "mayor",
		}},
		// Not a workflow root — must be ignored even though run_target is set.
		{ID: "T-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": "mayor",
		}},
		// Control-dispatcher and topology beads may legitimately carry bare
		// gc.run_target, but they are not claimable through pool demand.
		{ID: "CTRL-1", Title: "retry", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "retry", "gc.run_target": "mayor",
		}},
		{ID: "CTRL-2", Title: "ralph", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "ralph", "gc.run_target": "mayor",
		}},
		{ID: "TOPO-1", Title: "scope", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "scope", "gc.run_target": "mayor",
		}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RR-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": "repo/polecat",
		}},
	}, nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}
	factory := func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}

	check := newRunTargetRoutedToBackfillCheck(cfg, cityDir, factory)

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{"WR-1", "RR-1"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	for _, notWant := range []string{"WR-2", "T-1", "CTRL-1", "CTRL-2", "TOPO-1"} {
		if strings.Contains(details, notWant) {
			t.Fatalf("details should not mention %q:\n%s", notWant, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	if res2 := check.Run(&doctor.CheckContext{}); res2.Status != doctor.StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK: %#v", res2.Status, res2)
	}

	wr1, err := cityStore.Get("WR-1")
	if err != nil {
		t.Fatalf("get WR-1: %v", err)
	}
	if got := wr1.Metadata["gc.routed_to"]; got != "mayor" {
		t.Errorf("WR-1 gc.routed_to = %q, want mayor (backfilled from gc.run_target)", got)
	}
	rr1, err := rigStore.Get("RR-1")
	if err != nil {
		t.Fatalf("get RR-1: %v", err)
	}
	if got := rr1.Metadata["gc.routed_to"]; got != "repo/polecat" {
		t.Errorf("RR-1 gc.routed_to = %q, want repo/polecat", got)
	}
	t1, err := cityStore.Get("T-1")
	if err != nil {
		t.Fatalf("get T-1: %v", err)
	}
	if got := t1.Metadata["gc.routed_to"]; got != "" {
		t.Errorf("T-1 gc.routed_to = %q, want empty (non-workflow bead must be untouched)", got)
	}
	for _, id := range []string{"CTRL-1", "CTRL-2", "TOPO-1"} {
		b, err := cityStore.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got := b.Metadata["gc.routed_to"]; got != "" {
			t.Errorf("%s gc.routed_to = %q, want empty (control/topology bead must be untouched)", id, got)
		}
	}
}

// TestRunTargetRoutedToBackfillCheckCleanStore confirms a store with no stale
// roots reports OK and CanFix advertises remediation.
func TestRunTargetRoutedToBackfillCheckCleanStore(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "WR-9", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": "mayor", "gc.routed_to": "mayor",
		}},
	}, nil)
	check := newRunTargetRoutedToBackfillCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})
	if !check.CanFix() {
		t.Fatal("CanFix() = false, want true")
	}
	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("Run status = %v, want OK: %#v", res.Status, res)
	}
}

func TestRunTargetRoutedToBackfillFixReportsOpenFailures(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "WR-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": "worker",
		}},
	}, nil)
	check := newRunTargetRoutedToBackfillCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path == rigDir {
			return nil, errors.New("permission denied")
		}
		return cityStore, nil
	})

	err := check.Fix(&doctor.CheckContext{})
	if err == nil {
		t.Fatal("Fix error = nil, want skipped scope error")
	}
	if got := err.Error(); !strings.Contains(got, "rig repo skipped") || !strings.Contains(got, "permission denied") {
		t.Fatalf("Fix error = %q, want rig open failure detail", got)
	}
	wr1, getErr := cityStore.Get("WR-1")
	if getErr != nil {
		t.Fatalf("get WR-1: %v", getErr)
	}
	if got := wr1.Metadata["gc.routed_to"]; got != "worker" {
		t.Fatalf("WR-1 gc.routed_to = %q, want available repair applied despite skipped rig", got)
	}
}

func TestRunTargetRoutedToBackfillFixReportsListFailures(t *testing.T) {
	cityDir := t.TempDir()
	check := newRunTargetRoutedToBackfillCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return runTargetBackfillListErrorStore{Store: beads.NewMemStore()}, nil
	})

	err := check.Fix(&doctor.CheckContext{})
	if err == nil {
		t.Fatal("Fix error = nil, want skipped scope error")
	}
	if got := err.Error(); !strings.Contains(got, "city skipped") || !strings.Contains(got, "listing failed") {
		t.Fatalf("Fix error = %q, want list failure detail", got)
	}
}

type runTargetBackfillListErrorStore struct {
	beads.Store
}

func (s runTargetBackfillListErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("listing failed")
}
