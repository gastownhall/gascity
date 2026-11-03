package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// expandedBackfillRoot returns an open kind=workflow root with the given extra
// metadata, the shape every case below starts from.
func expandedBackfillRoot(id string, extra map[string]string) beads.Bead {
	md := map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}
	for k, v := range extra {
		md[k] = v
	}
	return beads.Bead{ID: id, Title: "root", Type: "task", Status: "open", Metadata: md}
}

// expandedBackfillStep returns a step bead that belongs to rootID, in the given status.
func expandedBackfillStep(id, rootID, status string) beads.Bead {
	return beads.Bead{ID: id, Title: "step", Type: "task", Status: status, Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: rootID,
	}}
}

// expandedBackfillCheckFor builds the check over the shared scan.
func expandedBackfillCheckFor(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *workflowExpandedBackfillCheck {
	return newWorkflowExpandedBackfillCheck(newRunTargetRoutedToBackfillCheck(cfg, cityPath, newStore))
}

// TestWorkflowExpandedBackfillCheck covers the PR #5901 follow-up (ga-1rp4zb):
// graph.v2 roots that were already expanded before gc.workflow_expanded
// shipped carry no stamp, so the absence-permissive gate still lets the
// gc.run_target claim fallback resurrect them (#5900). The check must flag
// exactly the unstamped roots that have real child steps, --fix must stamp
// them, and every other shape must be left alone.
func TestWorkflowExpandedBackfillCheck(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}

	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		// Needs the stamp: unstamped root with a live child step.
		expandedBackfillRoot("WR-OPEN", map[string]string{beadmeta.RunTargetMetadataKey: "mayor"}),
		expandedBackfillStep("S-OPEN", "WR-OPEN", "open"),
		// Needs the stamp: the #5900 shape itself - every real child already
		// closed, only the root left. Closed children still prove expansion.
		expandedBackfillRoot("WR-DONE", nil),
		expandedBackfillStep("S-DONE", "WR-DONE", "closed"),
		// Needs the stamp even with an explicit route: the topology latch
		// excludes an expanded root whatever its gc.routed_to.
		expandedBackfillRoot("WR-ROUTED", map[string]string{beadmeta.RoutedToMetadataKey: "mayor"}),
		expandedBackfillStep("S-ROUTED", "WR-ROUTED", "open"),
		// Root-only (#2763) shape: the root IS the unit of work. Stamping it
		// would suppress the very fallback it still needs.
		expandedBackfillRoot("WR-ROOTONLY", map[string]string{beadmeta.RunTargetMetadataKey: "mayor"}),
		// A root that names itself as its own root has no child either.
		expandedBackfillRoot("WR-SELF", map[string]string{beadmeta.RootBeadIDMetadataKey: "WR-SELF"}),
		// A step of some other root is not a child of WR-ROOTONLY.
		expandedBackfillStep("S-ELSEWHERE", "WR-SOMEWHERE-ELSE", "open"),
		// Already stamped: nothing to do.
		expandedBackfillRoot("WR-STAMPED", map[string]string{beadmeta.WorkflowExpandedMetadataKey: "true"}),
		expandedBackfillStep("S-STAMPED", "WR-STAMPED", "open"),
		// Not a workflow root, though it has beads pointing at it.
		{ID: "T-1", Title: "work", Type: "task", Status: "open"},
		expandedBackfillStep("S-T1", "T-1", "open"),
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		expandedBackfillRoot("RR-1", nil),
		expandedBackfillStep("RS-1", "RR-1", "open"),
	}, nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}
	factory := func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}

	check := expandedBackfillCheckFor(cfg, cityDir, factory)
	if got := check.Name(); got != "workflow-expanded-backfill" {
		t.Fatalf("Name() = %q, want workflow-expanded-backfill", got)
	}
	if !check.CanFix() {
		t.Fatal("CanFix() = false, want true")
	}
	if check.WarmupEligible() {
		t.Fatal("WarmupEligible() = true, want false (correctness backfill, not a warmup probe)")
	}

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{"WR-OPEN", "WR-DONE", "WR-ROUTED", "RR-1"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	for _, notWant := range []string{"WR-ROOTONLY", "WR-SELF", "WR-STAMPED", "T-1"} {
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

	stamp := func(store beads.Store, id string) string {
		t.Helper()
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return b.Metadata[beadmeta.WorkflowExpandedMetadataKey]
	}
	for _, id := range []string{"WR-OPEN", "WR-DONE", "WR-ROUTED"} {
		if got := stamp(cityStore, id); got != "true" {
			t.Errorf("%s gc.workflow_expanded = %q, want true", id, got)
		}
	}
	if got := stamp(rigStore, "RR-1"); got != "true" {
		t.Errorf("RR-1 gc.workflow_expanded = %q, want true", got)
	}
	for _, id := range []string{"WR-ROOTONLY", "WR-SELF", "T-1", "S-OPEN", "S-DONE"} {
		if got := stamp(cityStore, id); got != "" {
			t.Errorf("%s gc.workflow_expanded = %q, want empty (must be left untouched)", id, got)
		}
	}

	// Re-running --fix on a fixed store is a no-op, not an error.
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("second Fix: %v", err)
	}
	if got := stamp(cityStore, "WR-ROOTONLY"); got != "" {
		t.Errorf("WR-ROOTONLY gc.workflow_expanded = %q after second Fix, want empty", got)
	}
}

// TestWorkflowExpandedBackfillCheckCleanStore confirms a store with nothing to
// stamp reports OK.
func TestWorkflowExpandedBackfillCheckCleanStore(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		expandedBackfillRoot("WR-ROOTONLY", nil),
		expandedBackfillRoot("WR-STAMPED", map[string]string{beadmeta.WorkflowExpandedMetadataKey: "true"}),
		expandedBackfillStep("S-STAMPED", "WR-STAMPED", "open"),
	}, nil)
	check := expandedBackfillCheckFor(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})
	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("Run status = %v, want OK: %#v", res.Status, res)
	}
}

// TestWorkflowExpandedBackfillFixReportsOpenFailures: a scope whose store will
// not open is reported, and the repair that was possible elsewhere still lands.
func TestWorkflowExpandedBackfillFixReportsOpenFailures(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		expandedBackfillRoot("WR-1", nil),
		expandedBackfillStep("S-1", "WR-1", "open"),
	}, nil)
	check := expandedBackfillCheckFor(cfg, cityDir, func(path string) (beads.Store, error) {
		if path == rigDir {
			return nil, errors.New("permission denied")
		}
		return cityStore, nil
	})

	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
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
	if got := wr1.Metadata[beadmeta.WorkflowExpandedMetadataKey]; got != "true" {
		t.Fatalf("WR-1 gc.workflow_expanded = %q, want available repair applied despite skipped rig", got)
	}
}

// TestWorkflowExpandedBackfillFixReportsListFailures: a listing failure is a
// skipped scope, never a silent "nothing to do".
func TestWorkflowExpandedBackfillFixReportsListFailures(t *testing.T) {
	cityDir := t.TempDir()
	check := expandedBackfillCheckFor(nil, cityDir, func(path string) (beads.Store, error) {
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

// TestWorkflowExpandedBackfillFindsRootsInTheGraphBinding: workflow roots are
// graph class, so on a split city they and their steps live in the graph
// binding, which none of the work-scope stores is.
func TestWorkflowExpandedBackfillFindsRootsInTheGraphBinding(t *testing.T) {
	cityPath := t.TempDir()
	binding := sessionSplitRoutes(t, cityPath)
	root, err := binding.Create(beads.Bead{
		Title:    "legacy expanded root",
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.Create(beads.Bead{
		Title:    "step",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	}); err != nil {
		t.Fatal(err)
	}
	work := splittest.NewWorkStore(t, "gc")
	check := expandedBackfillCheckFor(nil, cityPath, func(string) (beads.Store, error) { return work, nil })

	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusWarning || !strings.Contains(strings.Join(res.Details, "\n"), root.ID) {
		t.Fatalf("Run = %#v, want a warning naming the binding's root %s", res, root.ID)
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	got, err := binding.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[beadmeta.WorkflowExpandedMetadataKey] != "true" {
		t.Fatalf("root %s gc.workflow_expanded = %q after --fix, want true", root.ID, got.Metadata[beadmeta.WorkflowExpandedMetadataKey])
	}
}

// TestWorkflowExpandedBackfillClosesTheClaimFallback ties the stamp to its
// purpose: before --fix the unstamped expanded root is still claim-eligible
// through the gc.run_target fallback, after it is not.
func TestWorkflowExpandedBackfillClosesTheClaimFallback(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		expandedBackfillRoot("WR-1", map[string]string{beadmeta.RunTargetMetadataKey: "mayor"}),
		expandedBackfillStep("S-1", "WR-1", "closed"),
	}, nil)
	check := expandedBackfillCheckFor(nil, cityDir, func(string) (beads.Store, error) { return store, nil })

	before, _ := store.Get("WR-1")
	if !workflowRunTargetFallbackEligible(before) {
		t.Fatal("precondition: unstamped root should be fallback-eligible")
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	after, _ := store.Get("WR-1")
	if workflowRunTargetFallbackEligible(after) {
		t.Fatal("root is still fallback-eligible after --fix")
	}
}
