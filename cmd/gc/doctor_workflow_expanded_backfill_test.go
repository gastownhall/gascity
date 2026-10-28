package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// legacyExpandedRootFixture is a city and rig store holding graph.v2 workflow
// roots minted before gc.workflow_expanded existed (gc 1.4.2): each expanded
// root is open, routed and unstamped, and an open workflow-finalize step names
// it through gc.root_bead_id.
func legacyExpandedRootFixture(t *testing.T) (check *workflowExpandedBackfillCheck, cityStore, rigStore beads.Store) {
	t.Helper()
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}

	cityStore = beads.NewMemStoreFrom(0, []beads.Bead{
		// Legacy expanded root: no stamp, its finalizer names it.
		{ID: "WR-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "repo/worker",
		}},
		{ID: "WR-1.finalize", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow-finalize", "gc.root_bead_id": "WR-1",
		}},
		// Legacy expanded root already claimed by a stuck worker.
		{ID: "WR-2", Title: "root", Type: "task", Status: "in_progress", Assignee: "worker-1", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "repo/worker",
		}},
		{ID: "WR-2.finalize", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow-finalize", "gc.root_bead_id": "WR-2",
		}},
		// Already stamped: nothing to do.
		{ID: "WR-3", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.workflow_expanded": "true", "gc.routed_to": "repo/worker",
		}},
		{ID: "WR-3.finalize", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow-finalize", "gc.root_bead_id": "WR-3",
		}},
	}, nil)
	rigStore = beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RR-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "repo/worker",
		}},
		{ID: "RR-1.finalize", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow-finalize", "gc.root_bead_id": "RR-1",
		}},
	}, nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}
	check = newWorkflowExpandedBackfillCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})
	return check, cityStore, rigStore
}

// TestWorkflowExpandedBackfillCheckWarnsOnLegacyExpandedRoots covers issue
// #7436 on an upgraded store: an expanded root minted without
// gc.workflow_expanded passes every latch gate as a possibly root-only
// workflow, so the worker keeps claiming it. The check names every such root,
// in the city and in each rig, and marks the ones a worker already holds.
func TestWorkflowExpandedBackfillCheckWarnsOnLegacyExpandedRoots(t *testing.T) {
	check, _, _ := legacyExpandedRootFixture(t)
	if check.Name() != "workflow-expanded-backfill" {
		t.Fatalf("Name() = %q, want workflow-expanded-backfill", check.Name())
	}

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{"city bead WR-1", "city bead WR-2", "assigned to worker-1", "rig repo bead RR-1"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	if strings.Contains(details, "WR-3") {
		t.Fatalf("details name the already-stamped root WR-3:\n%s", details)
	}
	if !strings.Contains(res.FixHint, "gc doctor --fix") {
		t.Fatalf("FixHint = %q, want a gc doctor --fix hint", res.FixHint)
	}
}

// TestWorkflowExpandedBackfillCheckFixStampsLegacyExpandedRoots proves the
// repair closes #7436's claim loop: once --fix stamps a legacy expanded root,
// the hook claim matcher refuses it on its route and the check reports OK.
func TestWorkflowExpandedBackfillCheckFixStampsLegacyExpandedRoots(t *testing.T) {
	check, cityStore, rigStore := legacyExpandedRootFixture(t)
	route := []string{"repo/worker"}
	before, err := cityStore.Get("WR-1")
	if err != nil {
		t.Fatalf("get WR-1: %v", err)
	}
	if !hookCandidateClaimable(before, route, time.Now()) {
		t.Fatalf("fixture WR-1 is not claimable before the fix; the case proves nothing")
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	for _, tc := range []struct {
		store beads.Store
		id    string
	}{{cityStore, "WR-1"}, {cityStore, "WR-2"}, {rigStore, "RR-1"}} {
		b, err := tc.store.Get(tc.id)
		if err != nil {
			t.Fatalf("get %s: %v", tc.id, err)
		}
		if got := b.Metadata["gc.workflow_expanded"]; got != "true" {
			t.Errorf("%s gc.workflow_expanded = %q, want true", tc.id, got)
		}
		b.Assignee = ""
		if hookCandidateClaimable(b, route, time.Now()) {
			t.Errorf("%s is still claimable on %v after the fix", tc.id, route)
		}
	}
	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK: %#v", res.Status, res)
	}
}
