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

// TestWorkflowExpandedBackfillCheckLeavesRootOnlyRootsClaimable pins the
// evidence rule: a root no workflow-finalize step names is root-only — the
// root itself is the work — even when attached fragment steps (which carry no
// finalizer) point at it, so --fix leaves it unstamped and claimable.
func TestWorkflowExpandedBackfillCheckLeavesRootOnlyRootsClaimable(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RO-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "repo/worker",
		}},
		{ID: "RO-2", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "repo/worker",
		}},
		// An attached fragment's steps name the root but bring no finalizer.
		{ID: "RO-2.frag", Title: "fragment step", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "task", "gc.root_bead_id": "RO-2", "gc.routed_to": "repo/worker",
		}},
		// A finalizer for another root is not evidence about these.
		{ID: "OTHER.finalize", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow-finalize", "gc.root_bead_id": "OTHER",
		}},
	}, nil)
	check := newWorkflowExpandedBackfillCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("Run status = %v, want OK for root-only roots: %#v", res.Status, res)
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for _, id := range []string{"RO-1", "RO-2"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got, ok := b.Metadata["gc.workflow_expanded"]; ok {
			t.Errorf("%s gc.workflow_expanded = %q, want unset on a root-only root", id, got)
		}
		if !hookCandidateClaimable(b, []string{"repo/worker"}, time.Now()) {
			t.Errorf("%s is no longer claimable on its route after the fix", id)
		}
	}
}

// TestWorkflowExpandedBackfillCheckFixLeavesRootOnlyNeighborsUnstamped runs
// --fix with a real target beside root-only roots: the expanded root is
// stamped, while the root-only root and the fragment parent sharing its store
// stay unstamped, claimable, and absent from the post-fix report.
func TestWorkflowExpandedBackfillCheckFixLeavesRootOnlyNeighborsUnstamped(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "WR-1", Title: "expanded root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "repo/worker",
		}},
		{ID: "WR-1.finalize", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow-finalize", "gc.root_bead_id": "WR-1",
		}},
		{ID: "WR-3", Title: "root-only root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "repo/worker",
		}},
		{ID: "WR-4", Title: "fragment parent", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "repo/worker",
		}},
		{ID: "WR-4.frag", Title: "fragment member", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.root_bead_id": "WR-4", "gc.scope_role": "member",
		}},
	}, nil)
	check := newWorkflowExpandedBackfillCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	expanded, err := store.Get("WR-1")
	if err != nil {
		t.Fatalf("get WR-1: %v", err)
	}
	if got := expanded.Metadata["gc.workflow_expanded"]; got != "true" {
		t.Fatalf("WR-1 gc.workflow_expanded = %q, want true on the expanded neighbor", got)
	}
	for _, id := range []string{"WR-3", "WR-4"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got, ok := b.Metadata["gc.workflow_expanded"]; ok {
			t.Errorf("%s gc.workflow_expanded = %q, want unset on a root-only root", id, got)
		}
		if !hookCandidateClaimable(b, []string{"repo/worker"}, time.Now()) {
			t.Errorf("%s is no longer claimable on its route after the fix", id)
		}
	}

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK: %#v", res.Status, res)
	}
	if details := strings.Join(res.Details, "\n"); strings.Contains(details, "WR-3") || strings.Contains(details, "WR-4") {
		t.Fatalf("post-fix Run details must not name the root-only roots: %#v", res)
	}
}

// TestWorkflowExpandedBackfillCheckOmitsARefusedGraphBinding covers a city
// whose graph binding this process may not read, so the graph class resolves
// to refusedClassStore: a worker site whose descriptor does not attach the
// binding, and a city whose storage the gate refused. The refusal says nothing
// about any root and --fix cannot clear it, so the check omits that scope
// rather than warn on every run and fail every --fix: it still names and
// stamps the work store's legacy root, then reports OK.
func TestWorkflowExpandedBackfillCheckOmitsARefusedGraphBinding(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, cityDir string)
	}{
		{"worker site without the graph binding", func(t *testing.T, cityDir string) {
			routes := splitRoutes(beads.NewMemStore())
			routes.binding = "infra"
			seedCLIStorageRoutes(t, cityDir, routes)
			resetControlPlaneForTest(t)
			writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
		}},
		{"refused city storage", func(t *testing.T, cityDir string) {
			seedCLIStorageRoutes(t, cityDir, splitRoutes(refusedClassStore{err: standingStorageRefusal{err: errStorageRefusedForTest{}}}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			tc.setup(t, cityDir)
			store := beads.NewMemStoreFrom(0, []beads.Bead{
				{ID: "WR-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
					"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "repo/worker",
				}},
				{ID: "WR-1.finalize", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
					"gc.kind": "workflow-finalize", "gc.root_bead_id": "WR-1",
				}},
			}, nil)
			if _, refused := scopeGraphStore(cityDir, cityDir, nil, store).(refusedClassStore); !refused {
				t.Fatal("fixture: the city graph class does not resolve to a refused store; the case proves nothing")
			}
			check := newWorkflowExpandedBackfillCheck(nil, cityDir, func(path string) (beads.Store, error) {
				if path != cityDir {
					return nil, fmt.Errorf("unexpected store path %q", path)
				}
				return store, nil
			})

			res := check.Run(&doctor.CheckContext{})
			details := strings.Join(res.Details, "\n")
			if res.Status != doctor.StatusWarning || !strings.Contains(details, "city bead WR-1") {
				t.Fatalf("Run = %v, want a warning naming the work store's root WR-1: %#v", res.Status, res)
			}
			if strings.Contains(details, "skipped") {
				t.Fatalf("Run counted the refused graph binding as a skipped scope:\n%s", details)
			}
			if err := check.Fix(&doctor.CheckContext{}); err != nil {
				t.Fatalf("Fix: %v; a refused graph binding must not fail --fix", err)
			}
			root, err := store.Get("WR-1")
			if err != nil {
				t.Fatalf("get WR-1: %v", err)
			}
			if got := root.Metadata["gc.workflow_expanded"]; got != "true" {
				t.Fatalf("WR-1 gc.workflow_expanded = %q, want true", got)
			}
			if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
				t.Fatalf("post-fix Run status = %v, want OK: %#v", res.Status, res)
			}
		})
	}
}
