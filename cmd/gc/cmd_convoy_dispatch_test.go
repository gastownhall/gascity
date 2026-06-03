package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

func TestDrainItemRecipeVarsIncludesRuntimeMetadata(t *testing.T) {
	recipe := &formula.Recipe{
		Steps: []formula.RecipeStep{{
			ID:     "item",
			IsRoot: true,
			Metadata: map[string]string{
				"gc.input_convoy_id":              "CONVOY-1",
				graphv2.RuntimeVarsMetadataKey:    graphv2.RuntimeVarsMetadata(map[string]string{"region": "west", graphv2.ConvoyIDVar: "ignored"}),
				"gc.unrelated_runtime_vars_noise": "ignored",
			},
		}},
	}

	vars, err := drainItemRecipeVars(recipe)
	if err != nil {
		t.Fatalf("drainItemRecipeVars: %v", err)
	}
	if vars["convoy_id"] != "CONVOY-1" || vars["region"] != "west" {
		t.Fatalf("vars = %#v, want convoy_id and inherited region", vars)
	}
	if _, ok := vars["issue"]; ok {
		t.Fatalf("vars = %#v, want reserved issue excluded", vars)
	}
}

func TestOpenSourceWorkflowStoresSkipsBrokenRigs(t *testing.T) {
	// Regression: when a single rig's bead store is unopenable (broken
	// filesystem permissions, missing .gc directory, corrupt dolt, etc.),
	// the previous implementation failed the whole source-workflow call
	// site — so every graph-workflow launch and every workflow
	// delete-source/reopen-source aborted city-wide. That turned a
	// rig-local problem into a global outage. Now a broken non-selected
	// rig is skipped in favor of any store that opens.
	cityPath := "/city"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: "rigs/alpha"},
			{Name: "broken", Path: "rigs/broken"},
		},
	}

	openStore := func(dir string) (beads.Store, error) {
		if strings.Contains(dir, "rigs/broken") {
			return nil, fmt.Errorf("simulated broken rig store at %s", dir)
		}
		return beads.NewMemStore(), nil
	}

	stores, skips, err := openSourceWorkflowStoresWith(cfg, cityPath, "", openStore)
	if err != nil {
		t.Fatalf("openSourceWorkflowStoresWith returned err = %v; want tolerance of broken rig", err)
	}
	if len(stores) == 0 {
		t.Fatal("len(stores) = 0, want at least one store (city + alpha rig)")
	}
	for _, s := range stores {
		if strings.Contains(s.path, "rigs/broken") {
			t.Fatalf("broken rig should have been skipped, got path %q", s.path)
		}
	}
	// The broken rig must appear in skips so callers can surface a warning.
	// Without this, singleton coverage silently degrades.
	foundBrokenSkip := false
	for _, skip := range skips {
		if strings.Contains(skip.path, "rigs/broken") {
			foundBrokenSkip = true
			break
		}
	}
	if !foundBrokenSkip {
		t.Fatalf("skips = %#v, want an entry for the broken rig so callers can warn", skips)
	}
	msg := formatSourceWorkflowStoreSkips(skips)
	if !strings.Contains(msg, "rigs/broken") || !strings.Contains(msg, "invisible") {
		t.Fatalf("format message = %q, want reference to broken rig and invisibility", msg)
	}
}

func TestOpenSourceWorkflowStoresFailsOnlyWhenEverythingBroken(t *testing.T) {
	// If every candidate store is unopenable, the singleton check cannot
	// run safely — surface the first underlying error so the caller knows
	// why. This is the only case where intolerance is correct.
	cityPath := "/city"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
	}

	openStore := func(dir string) (beads.Store, error) {
		return nil, fmt.Errorf("every store at %s is broken", dir)
	}

	_, _, err := openSourceWorkflowStoresWith(cfg, cityPath, "", openStore)
	if err == nil {
		t.Fatal("openSourceWorkflowStoresWith returned nil error; want underlying store failure")
	}
	if !strings.Contains(err.Error(), "every store") {
		t.Fatalf("error = %v, want propagation of underlying failure", err)
	}
}

func TestWorkflowFinalizeRetriesWhenSourceWorkflowStoreScanSkipsLiveRoot(t *testing.T) {
	cityPath := "/city"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: "rigs/alpha"},
			{Name: "broken", Path: "rigs/broken"},
		},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	brokenStore := beads.NewMemStore()

	citySource, err := cityStore.Create(beads.Bead{Title: "Adopt PR", Type: "task"})
	if err != nil {
		t.Fatalf("Create(city source): %v", err)
	}
	rigLaunch, err := rigStore.Create(beads.Bead{
		Title: "Rig launch",
		Type:  "task",
		Metadata: map[string]string{
			"gc.source_bead_id":   citySource.ID,
			"gc.source_store_ref": "city:test-city",
		},
	})
	if err != nil {
		t.Fatalf("Create(rig launch): %v", err)
	}
	workflow, err := rigStore.Create(beads.Bead{
		Title: "mol-adopt-pr-v2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   rigLaunch.ID,
			"gc.source_store_ref": "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(workflow): %v", err)
	}
	cleanup, err := rigStore.Create(beads.Bead{
		Title: "cleanup",
		Type:  "task",
		Metadata: map[string]string{
			"gc.outcome": "pass",
		},
	})
	if err != nil {
		t.Fatalf("Create(cleanup): %v", err)
	}
	if err := rigStore.Close(cleanup.ID); err != nil {
		t.Fatalf("Close(cleanup): %v", err)
	}
	finalizer, err := rigStore.Create(beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(finalizer): %v", err)
	}
	if err := rigStore.DepAdd(finalizer.ID, cleanup.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(finalizer->cleanup): %v", err)
	}
	if err := rigStore.DepAdd(workflow.ID, finalizer.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(workflow->finalizer): %v", err)
	}
	hiddenRoot, err := brokenStore.Create(beads.Bead{
		Title: "hidden live workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                                "workflow",
			"gc.formula_contract":                    "graph.v2",
			"gc.source_bead_id":                      citySource.ID,
			sourceworkflow.SourceStoreRefMetadataKey: "city:test-city",
		},
	})
	if err != nil {
		t.Fatalf("Create(hidden root): %v", err)
	}

	openStore := func(dir string) (beads.Store, error) {
		switch filepath.Clean(dir) {
		case filepath.Clean(cityPath):
			return cityStore, nil
		case filepath.Clean(filepath.Join(cityPath, "rigs/alpha")):
			return rigStore, nil
		case filepath.Clean(filepath.Join(cityPath, "rigs/broken")):
			return nil, fmt.Errorf("simulated broken rig with live root %s", hiddenRoot.ID)
		default:
			return nil, fmt.Errorf("unexpected store path %s", dir)
		}
	}
	resolver := func(ref string) (beads.Store, error) {
		switch ref {
		case "city:test-city":
			return cityStore, nil
		case "rig:alpha":
			return rigStore, nil
		default:
			return nil, fmt.Errorf("unknown ref %s", ref)
		}
	}

	_, err = dispatch.ProcessControl(rigStore, finalizer, dispatch.ProcessOptions{
		ResolveStoreRef:      resolver,
		SourceWorkflowStores: makeSourceWorkflowStoresListerWithOpenStore(cityPath, cfg, openStore),
		SourceWorkflowLock:   func(_ string, _ string, fn func() error) error { return fn() },
	})
	if err == nil {
		t.Fatal("ProcessControl(workflow-finalize) err = nil, want retryable skipped-store error")
	}
	if !strings.Contains(err.Error(), "source-workflow singleton scan skipped") {
		t.Fatalf("ProcessControl error = %v, want skipped-store scan error", err)
	}

	workflowAfter, err := rigStore.Get(workflow.ID)
	if err != nil {
		t.Fatalf("Get(workflow): %v", err)
	}
	if workflowAfter.Status == "closed" {
		t.Fatal("workflow status = closed; want open so singleton scans still see the retrying root")
	}
	finalizerAfter, err := rigStore.Get(finalizer.ID)
	if err != nil {
		t.Fatalf("Get(finalizer): %v", err)
	}
	if finalizerAfter.Status == "closed" {
		t.Fatal("finalizer status = closed; want open so source-chain closure retries after skipped scan")
	}
	rigLaunchAfter, err := rigStore.Get(rigLaunch.ID)
	if err != nil {
		t.Fatalf("Get(rig launch): %v", err)
	}
	if rigLaunchAfter.Status == "closed" {
		t.Fatal("rig launch status = closed; want open until all source-workflow stores are scanned")
	}
	citySourceAfter, err := cityStore.Get(citySource.ID)
	if err != nil {
		t.Fatalf("Get(city source): %v", err)
	}
	if citySourceAfter.Status == "closed" {
		t.Fatal("city source status = closed; want open while a skipped store may contain a live root")
	}
	hiddenRootAfter, err := brokenStore.Get(hiddenRoot.ID)
	if err != nil {
		t.Fatalf("Get(hidden root): %v", err)
	}
	if hiddenRootAfter.Status == "closed" {
		t.Fatal("hidden root status = closed; want unchanged")
	}
}

func TestSourceWorkflowLockScopeForStoreRefUsesSharedHelper(t *testing.T) {
	cityPath := "/city"
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Path: "rigs/alpha"},
		},
	}

	got := sourceWorkflowLockScopeForStoreRef(cityPath, cfg, "", "rig:alpha")
	want := sourceworkflow.LockScopeForStoreRef(cityPath, "", "rig:alpha", func(rigName string) (string, bool) {
		if rigName != "alpha" {
			return "", false
		}
		return "rigs/alpha", true
	})
	if got != want {
		t.Fatalf("sourceWorkflowLockScopeForStoreRef = %q, want shared helper scope %q", got, want)
	}
}

type closeAllFailStore struct {
	beads.Store
	failOn map[string]struct{}
}

func (s closeAllFailStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	for _, id := range ids {
		if _, ok := s.failOn[id]; ok {
			return 0, fmt.Errorf("forced close failure for %s", id)
		}
	}
	return s.Store.CloseAll(ids, metadata)
}

func TestDecorateDynamicFragmentRecipeSupportsExplicitPerStepAgents(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "reviewer", MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)

	mayorSession := lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "mayor", cfg.Workspace.SessionTemplate)
	reviewerSession := lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "reviewer", cfg.Workspace.SessionTemplate)

	source := beads.Bead{
		ID:       "gc-source",
		Title:    "Source",
		Assignee: mayorSession,
		Metadata: map[string]string{
			"gc.routed_to": "mayor",
		},
	}
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{
				ID:    "expansion-review.review",
				Title: "Review",
				Metadata: map[string]string{
					"gc.run_target": "reviewer",
				},
			},
			{
				ID:    "expansion-review.review-scope-check",
				Title: "Finalize review",
				Metadata: map[string]string{
					"gc.kind":        "scope-check",
					"gc.control_for": "expansion-review.review",
				},
			},
			{
				ID:    "expansion-review.submit",
				Title: "Submit",
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "expansion-review.review-scope-check", DependsOnID: "expansion-review.review", Type: "blocks"},
			{StepID: "expansion-review.submit", DependsOnID: "expansion-review.review-scope-check", Type: "blocks"},
		},
	}

	if err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, "", cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}

	steps := map[string]formula.RecipeStep{}
	for _, step := range fragment.Steps {
		steps[step.ID] = step
	}

	review := steps["expansion-review.review"]
	if review.Assignee != reviewerSession {
		t.Fatalf("review assignee = %q, want %q", review.Assignee, reviewerSession)
	}
	if review.Metadata["gc.routed_to"] != "reviewer" {
		t.Fatalf("review gc.routed_to = %q, want reviewer", review.Metadata["gc.routed_to"])
	}

	control := steps["expansion-review.review-scope-check"]
	if control.Assignee != config.ControlDispatcherAgentName {
		t.Fatalf("review scope-check assignee = %q, want %q", control.Assignee, config.ControlDispatcherAgentName)
	}
	if got := control.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("review scope-check gc.routed_to = %q, want empty direct dispatcher assignee", got)
	}
	if control.Metadata[graphExecutionRouteMetaKey] != "reviewer" {
		t.Fatalf("review scope-check execution route = %q, want reviewer", control.Metadata[graphExecutionRouteMetaKey])
	}
	submit := steps["expansion-review.submit"]
	if submit.Assignee != mayorSession {
		t.Fatalf("submit assignee = %q, want %q", submit.Assignee, mayorSession)
	}
	if submit.Metadata["gc.routed_to"] != "mayor" {
		t.Fatalf("submit gc.routed_to = %q, want mayor", submit.Metadata["gc.routed_to"])
	}
}

func TestWorkflowFormulaSearchPathsUsesRoutedRigLayers(t *testing.T) {
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
			Rigs: map[string][]string{
				"frontend": {"/city/formulas", "/rig/frontend/formulas"},
			},
		},
	}

	paths := workflowFormulaSearchPaths(cfg, beads.Bead{
		Metadata: map[string]string{"gc.routed_to": "frontend/reviewer"},
	})
	if len(paths) != 2 || paths[1] != "/rig/frontend/formulas" {
		t.Fatalf("workflowFormulaSearchPaths(frontend) = %#v, want rig-specific layers", paths)
	}

	fallback := workflowFormulaSearchPaths(cfg, beads.Bead{
		Metadata: map[string]string{"gc.routed_to": "mayor"},
	})
	if len(fallback) != 1 || fallback[0] != "/city/formulas" {
		t.Fatalf("workflowFormulaSearchPaths(mayor) = %#v, want city layers", fallback)
	}

	control := workflowFormulaSearchPaths(cfg, beads.Bead{
		Metadata: map[string]string{
			"gc.routed_to":             config.ControlDispatcherAgentName,
			graphExecutionRouteMetaKey: "frontend/reviewer",
		},
	})
	if len(control) != 2 || control[1] != "/rig/frontend/formulas" {
		t.Fatalf("workflowFormulaSearchPaths(control frontend) = %#v, want rig-specific layers", control)
	}

	directControl := workflowFormulaSearchPaths(cfg, beads.Bead{
		Metadata: map[string]string{
			"gc.routed_to":                  config.ControlDispatcherAgentName,
			graphExecutionRouteMetaKey:      "session-123",
			graphExecutionRigContextMetaKey: "frontend",
		},
	})
	if len(directControl) != 2 || directControl[1] != "/rig/frontend/formulas" {
		t.Fatalf("workflowFormulaSearchPaths(direct control frontend) = %#v, want rig-specific layers", directControl)
	}
}

func TestDecorateDrainItemRecipeUsesDirectExecutionRoute(t *testing.T) {
	store := beads.NewMemStore()
	direct, err := store.Create(beads.Bead{
		Title:  "direct session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "sky",
			"session_name": "sky-session",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
	}
	config.InjectImplicitAgents(cfg)
	recipe := &formula.Recipe{
		Name: "item",
		Steps: []formula.RecipeStep{
			{
				ID:     "item",
				IsRoot: true,
				Type:   "task",
				Metadata: map[string]string{
					"gc.kind":             "workflow",
					"gc.formula_contract": "graph.v2",
				},
			},
			{
				ID:    "item.work",
				Title: "Work",
				Type:  "task",
			},
			{
				ID:    "item.check",
				Title: "Check",
				Type:  "task",
				Metadata: map[string]string{
					"gc.kind": "check",
				},
			},
		},
	}
	source := beads.Bead{
		ID: "drain-control",
		Metadata: map[string]string{
			graphExecutionRouteMetaKey:      direct.ID,
			graphExecutionRigContextMetaKey: "frontend",
		},
	}

	if err := decorateDrainItemRecipe(recipe, source, store, "city:test", "test", t.TempDir(), cfg); err != nil {
		t.Fatalf("decorateDrainItemRecipe: %v", err)
	}
	work := recipe.StepByID("item.work")
	if work == nil {
		t.Fatal("missing item.work")
	}
	if work.Assignee != direct.ID {
		t.Fatalf("item.work assignee = %q, want direct session %s", work.Assignee, direct.ID)
	}
	if got := work.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("item.work gc.routed_to = %q, want direct session assignment without route metadata", got)
	}
	check := recipe.StepByID("item.check")
	if check == nil {
		t.Fatal("missing item.check")
	}
	if got := check.Metadata[graphExecutionRouteMetaKey]; got != direct.ID {
		t.Fatalf("item.check execution route = %q, want direct session %s", got, direct.ID)
	}
	if got := check.Metadata[graphExecutionRigContextMetaKey]; got != "frontend" {
		t.Fatalf("item.check execution rig context = %q, want frontend", got)
	}
}

func TestDecorateDrainItemRecipeDoesNotFallbackToControllerAssignee(t *testing.T) {
	store := beads.NewMemStore()
	maxSessions := 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		Agents: []config.Agent{{
			Name:              "worker",
			MaxActiveSessions: &maxSessions,
		}},
	}
	config.InjectImplicitAgents(cfg)
	recipe := &formula.Recipe{
		Name: "item",
		Steps: []formula.RecipeStep{
			{
				ID:     "item",
				IsRoot: true,
				Type:   "task",
				Metadata: map[string]string{
					"gc.kind":             "workflow",
					"gc.formula_contract": "graph.v2",
				},
			},
			{
				ID:    "item.work",
				Title: "Work",
				Type:  "task",
			},
			{
				ID:    "item.explicit",
				Title: "Explicit route",
				Type:  "task",
				Metadata: map[string]string{
					"gc.run_target": "worker",
				},
			},
		},
	}
	source := beads.Bead{
		ID:       "drain-control",
		Assignee: config.ControlDispatcherAgentName,
		Metadata: map[string]string{
			"gc.kind": "drain",
		},
	}

	if err := decorateDrainItemRecipe(recipe, source, store, "city:test", "test", t.TempDir(), cfg); err != nil {
		t.Fatalf("decorateDrainItemRecipe: %v", err)
	}
	work := recipe.StepByID("item.work")
	if work == nil {
		t.Fatal("missing item.work")
	}
	if work.Assignee != "" {
		t.Fatalf("item.work assignee = %q, want empty without execution route", work.Assignee)
	}
	if got := work.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("item.work gc.routed_to = %q, want empty without execution route", got)
	}
	explicit := recipe.StepByID("item.explicit")
	if explicit == nil {
		t.Fatal("missing item.explicit")
	}
	if got := explicit.Metadata["gc.routed_to"]; got != "worker" {
		t.Fatalf("item.explicit gc.routed_to = %q, want explicit worker route", got)
	}
	if explicit.Assignee != "" {
		t.Fatalf("item.explicit assignee = %q, want pool route without controller fallback", explicit.Assignee)
	}
}

func TestFindWorkflowBeadsIncludesClosedDescendants(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title:  "Workflow",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":        "workflow",
			"gc.workflow_id": "wf-delete",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:  "Closed child",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	found := findWorkflowBeads(store, root.ID)
	ids := make([]string, 0, len(found))
	for _, bead := range found {
		ids = append(ids, bead.ID)
	}
	if !slices.Contains(ids, root.ID) {
		t.Fatalf("findWorkflowBeads(...) missing root %q: %#v", root.ID, ids)
	}
	if !slices.Contains(ids, child.ID) {
		t.Fatalf("findWorkflowBeads(...) missing closed child %q: %#v", child.ID, ids)
	}
}

func TestFindWorkflowBeadsResolvesLogicalWorkflowID(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title:  "Workflow",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":        "workflow",
			"gc.workflow_id": "wf-delete-logical",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:  "Closed child",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	found := findWorkflowBeads(store, "wf-delete-logical")
	ids := make([]string, 0, len(found))
	for _, bead := range found {
		ids = append(ids, bead.ID)
	}
	if !slices.Contains(ids, root.ID) {
		t.Fatalf("findWorkflowBeads(logical) missing root %q: %#v", root.ID, ids)
	}
	if !slices.Contains(ids, child.ID) {
		t.Fatalf("findWorkflowBeads(logical) missing child %q: %#v", child.ID, ids)
	}
}

func TestDeleteWorkflowMatchesUsesCascadeWithoutPreClose(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title: "Workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title: "Child",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	var gotDir, gotName string
	var gotArgs []string
	deleted, err := deleteWorkflowMatches([]workflowStoreMatch{{
		store: store,
		beads: []beads.Bead{root, child},
		label: "city",
		path:  "/city",
		runner: func(dir, name string, args ...string) ([]byte, error) {
			gotDir = dir
			gotName = name
			gotArgs = append([]string(nil), args...)
			return nil, nil
		},
	}})
	if err != nil {
		t.Fatalf("deleteWorkflowMatches: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if gotDir != "/city" || gotName != "bd" {
		t.Fatalf("runner target = (%q, %q), want (/city, bd)", gotDir, gotName)
	}
	wantArgs := []string{"delete", root.ID, child.ID, "--cascade", "--force"}
	if !slices.Equal(gotArgs, wantArgs) {
		t.Fatalf("delete args = %#v, want %#v", gotArgs, wantArgs)
	}
	for _, id := range []string{root.ID, child.ID} {
		after, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if after.Status != "open" || after.Metadata["gc.outcome"] == "skipped" {
			t.Fatalf("bead %s mutated before delete: status=%q metadata=%#v", id, after.Status, after.Metadata)
		}
	}
}

func TestDeleteWorkflowMatchesFailureDoesNotCloseBeads(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title: "Workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	deleted, err := deleteWorkflowMatches([]workflowStoreMatch{{
		store: store,
		beads: []beads.Bead{root},
		label: "city",
		path:  "/city",
		runner: func(string, string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("delete failed")
		},
	}})
	if err == nil {
		t.Fatal("deleteWorkflowMatches returned nil error, want delete failure")
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 after failed delete", deleted)
	}
	after, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if after.Status != "open" || after.Metadata["gc.outcome"] == "skipped" {
		t.Fatalf("root mutated after failed delete: status=%q metadata=%#v", after.Status, after.Metadata)
	}
}

func TestCmdWorkflowDeleteSourceClosesMatchedRootsAndClearsWorkflowID(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	source, err := store.Create(beads.Bead{Title: "Source", Type: "task", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create(source): %v", err)
	}
	root, err := store.Create(beads.Bead{
		Title:  "Workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   source.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:  "Child",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	if err := store.SetMetadata(source.ID, "workflow_id", root.ID); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(source.ID, sourceWorkflowStoreSelector{}, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWorkflowDeleteSource returned %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result=cleaned") {
		t.Fatalf("stdout = %q, want cleaned result", stdout.String())
	}
	reloaded, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(reload): %v", err)
	}
	updatedSource, err := reloaded.Get(source.ID)
	if err != nil {
		t.Fatalf("Get(source): %v", err)
	}
	if got := strings.TrimSpace(updatedSource.Metadata["workflow_id"]); got != "" {
		t.Fatalf("source workflow_id = %q, want empty", got)
	}
	updatedRoot, err := reloaded.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if updatedRoot.Status != "closed" {
		t.Fatalf("root status = %q, want closed", updatedRoot.Status)
	}
	updatedChild, err := reloaded.Get(child.ID)
	if err != nil {
		t.Fatalf("Get(child): %v", err)
	}
	if updatedChild.Status != "closed" {
		t.Fatalf("child status = %q, want closed", updatedChild.Status)
	}
}

func TestCmdWorkflowDeleteSourceFollowsRigLaunchSourceChain(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "alpha")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rigDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"

[daemon]
formula_v2 = true

[[rigs]]
name = "alpha"
prefix = "BL"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeCatalogFile(t, cityDir, ".gc/site.toml", "[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(city): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(rig .gc): %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(rig): %v", err)
	}

	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	citySource, err := cityStore.Create(beads.Bead{Title: "City source", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create(city source): %v", err)
	}
	if err := cityStore.SetMetadata(citySource.ID, "workflow_id", "wf-stale"); err != nil {
		t.Fatalf("SetMetadata(city workflow_id): %v", err)
	}
	rigLaunch, err := rigStore.Create(beads.Bead{
		Title:  "Rig launch",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.source_bead_id":                      citySource.ID,
			sourceworkflow.SourceStoreRefMetadataKey: "city:test-city",
		},
	})
	if err != nil {
		t.Fatalf("Create(rig launch): %v", err)
	}
	root, err := rigStore.Create(beads.Bead{
		Title:  "Workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.formula_contract":                    "graph.v2",
			"gc.source_bead_id":                      rigLaunch.ID,
			sourceworkflow.SourceStoreRefMetadataKey: "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := rigStore.Create(beads.Bead{
		Title:  "Child",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	var stdout, stderr bytes.Buffer
	selector := sourceWorkflowStoreSelector{storeRef: "city:test-city"}
	if code := cmdWorkflowDeleteSource(citySource.ID, selector, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWorkflowDeleteSource returned %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result=cleaned") {
		t.Fatalf("stdout = %q, want cleaned result", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	reloadedRig, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig reload): %v", err)
	}
	updatedRoot, err := reloadedRig.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if updatedRoot.Status != "closed" {
		t.Fatalf("root status = %q, want closed", updatedRoot.Status)
	}
	updatedChild, err := reloadedRig.Get(child.ID)
	if err != nil {
		t.Fatalf("Get(child): %v", err)
	}
	if updatedChild.Status != "closed" {
		t.Fatalf("child status = %q, want closed", updatedChild.Status)
	}
	reloadedCity, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city reload): %v", err)
	}
	updatedCitySource, err := reloadedCity.Get(citySource.ID)
	if err != nil {
		t.Fatalf("Get(city source): %v", err)
	}
	if got := strings.TrimSpace(updatedCitySource.Metadata["workflow_id"]); got != "" {
		t.Fatalf("city source workflow_id = %q, want empty", got)
	}
}

func TestCmdWorkflowDeleteSourceClosesGraphV2OnlyRoot(t *testing.T) {
	// Regression: after the ListLiveRoots contract fix, the singleton
	// scanner surfaces graph.v2-only roots (marked with
	// gc.formula_contract=graph.v2 and no gc.kind=workflow). But
	// findWorkflowBeads — the cleanup collector called from
	// collectSourceWorkflowMatches — still required gc.kind=workflow, so
	// delete-source would list the root and close nothing. This is the
	// exact root shape #720 exists to recover.
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	source, err := store.Create(beads.Bead{Title: "Source", Type: "task", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create(source): %v", err)
	}
	// graph.v2-only root: no gc.kind=workflow label.
	root, err := store.Create(beads.Bead{
		Title:  "Graph workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   source.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:  "Child",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	if err := store.SetMetadata(source.ID, "workflow_id", root.ID); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(source.ID, sourceWorkflowStoreSelector{}, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWorkflowDeleteSource = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result=cleaned") {
		t.Fatalf("stdout = %q, want cleaned result", stdout.String())
	}

	reloaded, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(reload): %v", err)
	}
	updatedRoot, err := reloaded.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if updatedRoot.Status != "closed" {
		t.Fatalf("root status = %q, want closed (graph.v2-only root must be collected by findWorkflowBeads)", updatedRoot.Status)
	}
	updatedChild, err := reloaded.Get(child.ID)
	if err != nil {
		t.Fatalf("Get(child): %v", err)
	}
	if updatedChild.Status != "closed" {
		t.Fatalf("child status = %q, want closed", updatedChild.Status)
	}
	updatedSource, err := reloaded.Get(source.ID)
	if err != nil {
		t.Fatalf("Get(source): %v", err)
	}
	if got := strings.TrimSpace(updatedSource.Metadata["workflow_id"]); got != "" {
		t.Fatalf("source workflow_id = %q, want cleared", got)
	}
}

func TestCmdWorkflowReopenSourceClearsRoutedToForResling(t *testing.T) {
	// Regression: reopen-source is the documented recovery path for a
	// closed/assigned source bead whose workflow died. It cleared
	// workflow_id + status + assignee but left gc.routed_to populated.
	// sling.CheckBeadState treats a bead with gc.routed_to == target as
	// already-routed and short-circuits on idempotency — so a re-sling
	// of the recovered bead to the same target appeared to succeed while
	// producing no live workflow. Operators following the cleanup hint
	// ended up silently stuck.
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	source, err := store.Create(beads.Bead{Title: "Source", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("Create(source): %v", err)
	}
	// Simulate the state left behind by a previous sling that died:
	// workflow_id pointed at a now-gone root, gc.routed_to still set.
	if err := store.SetMetadata(source.ID, "workflow_id", "wf-gone"); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}
	if err := store.SetMetadata(source.ID, "gc.routed_to", "mayor"); err != nil {
		t.Fatalf("SetMetadata(gc.routed_to): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowReopenSource(source.ID, sourceWorkflowStoreSelector{}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWorkflowReopenSource returned %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result=reopened") {
		t.Fatalf("stdout = %q, want reopened result", stdout.String())
	}

	reloaded, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(reload): %v", err)
	}
	updated, err := reloaded.Get(source.ID)
	if err != nil {
		t.Fatalf("Get(source): %v", err)
	}
	if got := strings.TrimSpace(updated.Metadata["workflow_id"]); got != "" {
		t.Fatalf("workflow_id = %q, want cleared", got)
	}
	if got := strings.TrimSpace(updated.Metadata["gc.routed_to"]); got != "" {
		t.Fatalf("gc.routed_to = %q, want cleared (else re-sling hits idempotency short-circuit)", got)
	}
	if updated.Status != "open" {
		t.Fatalf("status = %q, want open", updated.Status)
	}
	if updated.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", updated.Assignee)
	}
}

func TestCmdWorkflowReopenSourceConflictsWhenLiveRootExists(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	source, err := store.Create(beads.Bead{Title: "Source", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("Create(source): %v", err)
	}
	root, err := store.Create(beads.Bead{
		Title:  "Workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": source.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowReopenSource(source.ID, sourceWorkflowStoreSelector{}, &stdout, &stderr); code != 3 {
		t.Fatalf("cmdWorkflowReopenSource returned %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "blocking_workflow_ids="+root.ID) {
		t.Fatalf("stderr = %q, want blocking root id", stderr.String())
	}
}

func TestCmdWorkflowDeleteSourcePreviewDoesNotClearStaleMetadata(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	source, err := store.Create(beads.Bead{Title: "Source", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create(source): %v", err)
	}
	if err := store.SetMetadata(source.ID, "workflow_id", "wf-stale"); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(source.ID, sourceWorkflowStoreSelector{}, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWorkflowDeleteSource returned %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	reloaded, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(reload): %v", err)
	}
	updatedSource, err := reloaded.Get(source.ID)
	if err != nil {
		t.Fatalf("Get(source): %v", err)
	}
	if got := updatedSource.Metadata["workflow_id"]; got != "wf-stale" {
		t.Fatalf("source workflow_id = %q, want stale metadata preserved in preview", got)
	}
	if !strings.Contains(stdout.String(), "result=already_clean") {
		t.Fatalf("stdout = %q, want already_clean result", stdout.String())
	}
	if !strings.Contains(stdout.String(), "metadata_cleared=false") {
		t.Fatalf("stdout = %q, want metadata_cleared=false", stdout.String())
	}
}

func TestApplySourceWorkflowMatchCleanupSkipsDeleteAfterCloseError(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "workflow", Type: "task", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := base.Create(beads.Bead{
		Title:  "child",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	store := closeAllFailStore{
		Store:  base,
		failOn: map[string]struct{}{root.ID: {}},
	}

	var stderr bytes.Buffer
	closed, deleted, incomplete := applySourceWorkflowMatchCleanup(sourceWorkflowStoreMatch{
		label: "city",
		store: store,
		roots: []beads.Bead{root},
		beads: []beads.Bead{root, child},
	}, true, &stderr)

	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	if !incomplete {
		t.Fatal("incomplete = false, want true")
	}
	if !strings.Contains(stderr.String(), "close_error") {
		t.Fatalf("stderr = %q, want close_error", stderr.String())
	}
	if _, err := base.Get(root.ID); err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if _, err := base.Get(child.ID); err != nil {
		t.Fatalf("Get(child): %v", err)
	}
}

func TestRunWorkflowReopenSourceConflictPropagatesExitCode(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	source, err := store.Create(beads.Bead{Title: "Source", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("Create(source): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "Workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": source.ID,
		},
	}); err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"workflow", "reopen-source", source.ID}, &stdout, &stderr); code != 3 {
		t.Fatalf("run(...) returned %d, want 3; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestDecorateDynamicFragmentRecipePreservesPoolFallbackAndScopeMetadata(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		Agents: []config.Agent{
			{Name: "reviewer", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)},
		},
	}
	config.InjectImplicitAgents(cfg)

	source := beads.Bead{
		ID:    "gc-source",
		Title: "Source",
		Metadata: map[string]string{
			"gc.routed_to": "frontend/reviewer",
			"gc.scope_ref": "body",
			"gc.on_fail":   "abort_scope",
		},
	}
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{
				ID:    "expansion-review.review",
				Title: "Review",
			},
			{
				ID:    "expansion-review.review-scope-check",
				Title: "Finalize review",
				Metadata: map[string]string{
					"gc.kind":        "scope-check",
					"gc.control_for": "expansion-review.review",
				},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "expansion-review.review-scope-check", DependsOnID: "expansion-review.review", Type: "blocks"},
		},
	}

	if err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, "", cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}

	steps := map[string]formula.RecipeStep{}
	for _, step := range fragment.Steps {
		steps[step.ID] = step
	}

	review := steps["expansion-review.review"]
	if review.Assignee != "" {
		t.Fatalf("review assignee = %q, want empty for pool-routed work", review.Assignee)
	}
	if review.Metadata["gc.routed_to"] != "frontend/reviewer" {
		t.Fatalf("review gc.routed_to = %q, want frontend/reviewer", review.Metadata["gc.routed_to"])
	}
	for _, label := range review.Labels {
		if label == "pool:frontend/reviewer" {
			t.Fatalf("review labels = %#v, should not contain legacy pool label", review.Labels)
		}
	}
	if review.Metadata["gc.scope_ref"] != "body" {
		t.Fatalf("review gc.scope_ref = %q, want body", review.Metadata["gc.scope_ref"])
	}
	if review.Metadata["gc.on_fail"] != "abort_scope" {
		t.Fatalf("review gc.on_fail = %q, want abort_scope", review.Metadata["gc.on_fail"])
	}
	if review.Metadata["gc.scope_role"] != "member" {
		t.Fatalf("review gc.scope_role = %q, want member", review.Metadata["gc.scope_role"])
	}

	control := steps["expansion-review.review-scope-check"]
	if control.Metadata["gc.scope_ref"] != "body" {
		t.Fatalf("control gc.scope_ref = %q, want body", control.Metadata["gc.scope_ref"])
	}
	if control.Metadata["gc.scope_role"] != "control" {
		t.Fatalf("control gc.scope_role = %q, want control", control.Metadata["gc.scope_role"])
	}
	if control.Assignee != config.ControlDispatcherAgentName {
		t.Fatalf("control assignee = %q, want %q", control.Assignee, config.ControlDispatcherAgentName)
	}
	if got := control.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("control gc.routed_to = %q, want empty direct dispatcher assignee", got)
	}
	if control.Metadata[graphExecutionRouteMetaKey] != "frontend/reviewer" {
		t.Fatalf("control execution route = %q, want frontend/reviewer", control.Metadata[graphExecutionRouteMetaKey])
	}
}

func TestDecorateDynamicFragmentRecipeUsesDirectExecutionRoute(t *testing.T) {
	store := beads.NewMemStore()
	direct, err := store.Create(beads.Bead{
		Title:  "direct session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "frontend/sky",
			"session_name": "frontend-sky",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
	}
	config.InjectImplicitAgents(cfg)
	source := beads.Bead{
		ID:    "gc-source",
		Title: "Source",
		Metadata: map[string]string{
			graphExecutionRouteMetaKey:      direct.ID,
			graphExecutionRigContextMetaKey: "frontend",
		},
	}
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{
				ID:    "expansion-review.review",
				Title: "Review",
			},
			{
				ID:    "expansion-review.check",
				Title: "Check",
				Metadata: map[string]string{
					"gc.kind": "check",
				},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "expansion-review.check", DependsOnID: "expansion-review.review", Type: "blocks"},
		},
	}

	if err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, t.TempDir(), cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}
	steps := map[string]formula.RecipeStep{}
	for _, step := range fragment.Steps {
		steps[step.ID] = step
	}
	review, ok := steps["expansion-review.review"]
	if !ok {
		t.Fatal("missing expansion-review.review")
	}
	if review.Assignee != direct.ID {
		t.Fatalf("review assignee = %q, want direct session %s", review.Assignee, direct.ID)
	}
	if got := review.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("review gc.routed_to = %q, want direct session assignment without route metadata", got)
	}
	check, ok := steps["expansion-review.check"]
	if !ok {
		t.Fatal("missing expansion-review.check")
	}
	if check.Assignee != config.ControlDispatcherAgentName {
		t.Fatalf("check assignee = %q, want %q", check.Assignee, config.ControlDispatcherAgentName)
	}
	if got := check.Metadata[graphExecutionRouteMetaKey]; got != direct.ID {
		t.Fatalf("check execution route = %q, want direct session %s", got, direct.ID)
	}
	if got := check.Metadata[graphExecutionRigContextMetaKey]; got != "frontend" {
		t.Fatalf("check execution rig context = %q, want frontend", got)
	}
}

func TestDecorateDynamicFragmentRecipeUsesSourceRouteRigContextForBareTargets(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		Agents: []config.Agent{
			{Name: "reviewer", Dir: "frontend", MaxActiveSessions: intPtr(1)},
			{Name: "reviewer", Dir: "backend", MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)

	source := beads.Bead{
		ID:    "gc-source",
		Title: "Source",
		Metadata: map[string]string{
			"gc.routed_to": "frontend/reviewer",
		},
	}
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{
				ID:    "expansion-review.review",
				Title: "Review",
				Metadata: map[string]string{
					"gc.run_target": "reviewer",
				},
			},
		},
	}

	if err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, "", cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}

	review := fragment.Steps[0]
	wantSession := lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "frontend/reviewer", cfg.Workspace.SessionTemplate)
	if review.Assignee != wantSession {
		t.Fatalf("review assignee = %q, want %q", review.Assignee, wantSession)
	}
	if review.Metadata["gc.routed_to"] != "frontend/reviewer" {
		t.Fatalf("review gc.routed_to = %q, want frontend/reviewer", review.Metadata["gc.routed_to"])
	}
}

func TestDecorateDynamicFragmentRecipeMarksRetryEvalAsScopedControl(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		Agents: []config.Agent{
			{Name: "reviewer", Dir: "frontend", MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)

	source := beads.Bead{
		ID:       "gc-source",
		Title:    "Source",
		Assignee: "frontend--reviewer",
		Metadata: map[string]string{
			"gc.scope_ref": "body",
			"gc.on_fail":   "abort_scope",
			"gc.routed_to": "frontend/reviewer",
		},
	}
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{
				ID:    "expansion-review.review",
				Title: "Review",
				Metadata: map[string]string{
					"gc.kind": "retry-run",
				},
			},
			{
				ID:    "expansion-review.review-eval",
				Title: "Evaluate Review",
				Metadata: map[string]string{
					"gc.kind": "retry-eval",
				},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "expansion-review.review-eval", DependsOnID: "expansion-review.review", Type: "blocks"},
		},
	}

	if err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, "", cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}

	steps := map[string]formula.RecipeStep{}
	for _, step := range fragment.Steps {
		steps[step.ID] = step
	}

	eval := steps["expansion-review.review-eval"]
	if eval.Metadata["gc.scope_ref"] != "body" {
		t.Fatalf("retry-eval gc.scope_ref = %q, want body", eval.Metadata["gc.scope_ref"])
	}
	if eval.Metadata["gc.scope_role"] != "control" {
		t.Fatalf("retry-eval gc.scope_role = %q, want control", eval.Metadata["gc.scope_role"])
	}
}

func TestRunWorkflowServeProcessesReadyControlBeadsThenExits(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	cdAgent := config.Agent{Name: config.ControlDispatcherAgentName}
	wantQuery := workflowServeControlReadyQuery(cdAgent, "control-dispatcher")
	var gotQueries []string
	var gotDirs []string
	var gotEnv []map[string]string
	var controlled []string
	sequence := [][]hookBead{
		{{ID: "gc-ctrl-1", Metadata: map[string]string{"gc.kind": "scope-check"}}},
		{{ID: "gc-ctrl-2", Metadata: map[string]string{"gc.kind": "workflow-finalize"}}},
	}

	workflowServeList = func(workQuery, dir string, env map[string]string) ([]hookBead, error) {
		gotQueries = append(gotQueries, workQuery)
		gotDirs = append(gotDirs, dir)
		gotEnv = append(gotEnv, maps.Clone(env))
		if len(sequence) == 0 {
			return nil, nil
		}
		next := sequence[0]
		sequence = sequence[1:]
		return next, nil
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		controlled = append(controlled, beadID)
		return nil
	}

	if err := runWorkflowServe("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	if !slices.Equal(controlled, []string{"gc-ctrl-1", "gc-ctrl-2"}) {
		t.Fatalf("controlled beads = %#v, want two ready control beads in order", controlled)
	}
	if len(gotQueries) != 3 {
		t.Fatalf("workflowServeList calls = %d, want 3", len(gotQueries))
	}
	for i, got := range gotQueries {
		if got != wantQuery {
			t.Fatalf("workflowServeList query[%d] = %q, want %q", i, got, wantQuery)
		}
	}
	for i, got := range gotDirs {
		if canonicalTestPath(got) != canonicalTestPath(cityDir) {
			t.Fatalf("workflowServeList dir[%d] = %q, want %q", i, got, cityDir)
		}
	}
	for i, env := range gotEnv {
		if env["GC_STORE_ROOT"] != cityDir {
			t.Fatalf("workflowServeList env[%d] GC_STORE_ROOT = %q, want %q", i, env["GC_STORE_ROOT"], cityDir)
		}
		if env["GC_STORE_SCOPE"] != "city" {
			t.Fatalf("workflowServeList env[%d] GC_STORE_SCOPE = %q, want city", i, env["GC_STORE_SCOPE"])
		}
	}
}

func TestRunWorkflowServeDrainsReadyBatchBeforeRequery(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	var controlled []string
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		switch calls {
		case 1:
			return []hookBead{
				{ID: "gc-ctrl-1", Metadata: map[string]string{"gc.kind": "scope-check"}},
				{ID: "gc-ctrl-2", Metadata: map[string]string{"gc.kind": "workflow-finalize"}},
			}, nil
		default:
			return nil, nil
		}
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		controlled = append(controlled, beadID)
		return nil
	}

	if err := runWorkflowServe("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	if !slices.Equal(controlled, []string{"gc-ctrl-1", "gc-ctrl-2"}) {
		t.Fatalf("controlled beads = %#v, want ready batch drained in order", controlled)
	}
	if calls != 2 {
		t.Fatalf("workflowServeList calls = %d, want first ready batch plus idle check", calls)
	}
}

func TestRunWorkflowServeFollowRequiresManagedSessionEnv(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_TEMPLATE", "")

	err := runWorkflowServe("control-dispatcher", true, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("runWorkflowServe returned nil error, want missing managed session env")
	}
	msg := err.Error()
	if !strings.Contains(msg, "GC_SESSION_ID") || !strings.Contains(msg, "GC_SESSION_NAME") {
		t.Fatalf("runWorkflowServe error = %q, want missing GC_SESSION_ID and GC_SESSION_NAME", msg)
	}
}

func TestRequireWorkflowServeFollowSessionEnvAllowsManagedSession(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_SESSION_ID", "sess-123")
	t.Setenv("GC_SESSION_NAME", "test-city/control-dispatcher")

	if err := requireWorkflowServeFollowSessionEnv(); err != nil {
		t.Fatalf("requireWorkflowServeFollowSessionEnv: %v", err)
	}
}

func TestRunWorkflowServeReturnsControlErrorWithoutQuarantine(t *testing.T) {
	clearInheritedBeadsEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	cleanupManagedDoltTestCity(t, cityDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	calls := 0
	var controlled []string
	retryableErr := errors.New("source store temporarily unavailable")
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		if calls == 1 {
			return []hookBead{
				{ID: "gc-ctrl-bad", Metadata: map[string]string{"gc.kind": "fanout"}},
				{ID: "gc-ctrl-good", Metadata: map[string]string{"gc.kind": "scope-check"}},
			}, nil
		}
		return nil, nil
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		controlled = append(controlled, beadID)
		if beadID == "gc-ctrl-bad" {
			return retryableErr
		}
		return nil
	}

	err := runWorkflowServe("", false, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("runWorkflowServe err = nil, want retryable control error")
	}
	if !strings.Contains(err.Error(), "processing control bead gc-ctrl-bad") || !errors.Is(err, retryableErr) {
		t.Fatalf("runWorkflowServe err = %v, want wrapped retryable control error", err)
	}
	if !slices.Equal(controlled, []string{"gc-ctrl-bad"}) {
		t.Fatalf("controlled beads = %#v, want stop at retryable bad bead", controlled)
	}
}

func TestQuarantineControlFailureBeadClosesWithDiagnostics(t *testing.T) {
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title:  "control",
		Status: "open",
		Labels: []string{"gc:control"},
		Metadata: map[string]string{
			"gc.kind": "fanout",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}

	if err := quarantineControlFailureBead(store, control.ID, fmt.Errorf("%w: bad workflow", dispatch.ErrControlGraphMalformed)); err != nil {
		t.Fatalf("quarantineControlFailureBead: %v", err)
	}

	got, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	if got.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("outcome = %q, want fail", got.Metadata["gc.outcome"])
	}
	if got.Metadata["gc.failure_class"] != "hard" {
		t.Fatalf("failure_class = %q, want hard", got.Metadata["gc.failure_class"])
	}
	if got.Metadata["gc.failure_reason"] != "malformed_control_graph" {
		t.Fatalf("failure_reason = %q, want malformed_control_graph", got.Metadata["gc.failure_reason"])
	}
	if got.Metadata["gc.controller_error_class"] != "hard" {
		t.Fatalf("controller_error_class = %q, want hard", got.Metadata["gc.controller_error_class"])
	}
	if got.Metadata["gc.final_disposition"] != "control_quarantined" {
		t.Fatalf("final_disposition = %q, want control_quarantined", got.Metadata["gc.final_disposition"])
	}
	if !strings.Contains(got.Metadata["gc.controller_error"], "bad workflow") {
		t.Fatalf("controller_error = %q, want bad workflow", got.Metadata["gc.controller_error"])
	}
	if got.Metadata["gc.control_quarantined"] != "true" {
		t.Fatalf("control_quarantined = %q, want true", got.Metadata["gc.control_quarantined"])
	}
	if !strings.Contains(got.Metadata["gc.control_quarantine_reason"], "bad workflow") {
		t.Fatalf("control_quarantine_reason = %q, want bad workflow", got.Metadata["gc.control_quarantine_reason"])
	}
	if got.Metadata["gc.control_quarantined_at"] == "" {
		t.Fatal("control_quarantined_at is empty")
	}
	if !slices.Contains(got.Labels, "gc:control-quarantined") {
		t.Fatalf("labels = %#v, want gc:control-quarantined", got.Labels)
	}
}

func TestQuarantineControlFailureBeadTruncatesReasonAtUTF8Boundary(t *testing.T) {
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title:  "control",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind": "fanout",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	reason := strings.Repeat("a", maxControlQuarantineReasonMetadata-1) + "é tail"

	if err := quarantineControlFailureBead(store, control.ID, errors.New(reason)); err != nil {
		t.Fatalf("quarantineControlFailureBead: %v", err)
	}

	got, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	recorded := got.Metadata["gc.control_quarantine_reason"]
	if len(recorded) > maxControlQuarantineReasonMetadata {
		t.Fatalf("recorded reason length = %d, want <= %d", len(recorded), maxControlQuarantineReasonMetadata)
	}
	if !utf8.ValidString(recorded) {
		t.Fatalf("recorded reason is invalid UTF-8: %q", recorded)
	}
}

func TestRunControlDispatcherReturnsTransientControlErrorWithoutQuarantine(t *testing.T) {
	clearGCEnv(t)

	base := beads.NewMemStore()
	missingRootID := "gc-missing-root"
	control, err := base.Create(beads.Bead{
		Title: "orphan check",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "fanout",
			"gc.root_bead_id":   missingRootID,
			"gc.root_store_ref": "city:test",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	store := transientGetStore{
		Store:  base,
		failID: missingRootID,
		err:    errors.New("bad connection: root lookup timed out"),
	}

	var stderr bytes.Buffer
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	err = runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, control, control.ID, cfg, io.Discard, &stderr)
	if err == nil {
		t.Fatal("runControlDispatcherWithStoreAndConfig error = nil, want transient error")
	}
	if !dispatch.IsTransientControllerError(err) {
		t.Fatalf("runControlDispatcherWithStoreAndConfig error = %v, want transient classifier match", err)
	}

	after, err := base.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if after.Status != "open" {
		t.Fatalf("control status = %q, want open", after.Status)
	}
	if got := after.Metadata["gc.control_quarantined"]; got != "" {
		t.Fatalf("gc.control_quarantined = %q, want empty", got)
	}
	if got := after.Metadata["gc.final_disposition"]; got != "" {
		t.Fatalf("gc.final_disposition = %q, want empty", got)
	}
	if slices.Contains(after.Labels, "gc:control-quarantined") {
		t.Fatalf("labels = %#v, want no gc:control-quarantined", after.Labels)
	}
}

type transientGetStore struct {
	beads.Store
	failID string
	err    error
}

func (s transientGetStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

func TestRunControlDispatcherQuarantineReconcilesScopedControlFailure(t *testing.T) {
	clearGCEnv(t)

	store := beads.NewMemStore()
	workflow, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	body, err := store.Create(beads.Bead{
		Title: "scope body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "review-loop.iteration.1",
			"gc.scope_role":   "body",
		},
	})
	if err != nil {
		t.Fatalf("create scope body: %v", err)
	}
	control, err := store.Create(beads.Bead{
		Title: "unsupported scoped control",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "unknown-control-kind",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "review-loop.iteration.1",
			"gc.scope_role":   "member",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}

	var stderr bytes.Buffer
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if err := runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, control, control.ID, cfg, io.Discard, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStoreAndConfig: %v", err)
	}

	afterControl, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if afterControl.Status != "closed" {
		t.Fatalf("control status = %q, want closed", afterControl.Status)
	}
	afterBody, err := store.Get(body.ID)
	if err != nil {
		t.Fatalf("get scope body: %v", err)
	}
	if afterBody.Status != "closed" {
		t.Fatalf("scope body status = %q, want closed", afterBody.Status)
	}
	if got := afterBody.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("scope body gc.outcome = %q, want fail", got)
	}
}

func TestRunWorkflowServeRoutesTraceOpenWarningsToCommandStderr(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	tracePath := filepath.Join(t.TempDir(), "missing", "workflow-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", tracePath)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		return nil, nil
	}

	var stderr bytes.Buffer
	if err := runWorkflowServe("", false, io.Discard, &stderr); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	got := stderr.String()
	if count := strings.Count(got, "opening workflow trace"); count != 1 {
		t.Fatalf("warning count = %d, want 1; stderr=%q", count, got)
	}
	wantPrefix := fmt.Sprintf("gc convoy control --serve: warning: opening workflow trace %q:", tracePath)
	if !strings.Contains(got, wantPrefix) {
		t.Fatalf("stderr = %q, want warning prefix %q", got, wantPrefix)
	}
}

func TestRunWorkflowServeWarnsOnLegacyTracePath(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_WORKFLOW_TRACE", filepath.Join(cityDir, "control-dispatcher-trace.log"))

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		return nil, nil
	}

	var stderr bytes.Buffer
	if err := runWorkflowServe("", false, io.Discard, &stderr); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	got := stderr.String()
	if !strings.Contains(got, "legacy control-dispatcher trace path") {
		t.Fatalf("stderr = %q, want legacy-trace warning", got)
	}
	if !strings.Contains(got, "change or unset GC_WORKFLOW_TRACE") {
		t.Fatalf("stderr = %q, want explicit override guidance", got)
	}
	if !strings.Contains(got, filepath.Join(cityDir, ".gc", "runtime", "control-dispatcher-trace.log")) {
		t.Fatalf("stderr = %q, want canonical runtime trace path guidance", got)
	}
}

func TestRunWorkflowServeWarnsWhenLegacyTraceFileStillExists(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	legacyTracePath := filepath.Join(cityDir, "control-dispatcher-trace.log")
	if err := os.WriteFile(legacyTracePath, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write legacy trace: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		return nil, nil
	}

	var stderr bytes.Buffer
	if err := runWorkflowServe("", false, io.Discard, &stderr); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	got := stderr.String()
	if !strings.Contains(got, "legacy control-dispatcher trace file") {
		t.Fatalf("stderr = %q, want legacy-trace artifact warning", got)
	}
	if !strings.Contains(got, legacyTracePath) {
		t.Fatalf("stderr = %q, want legacy trace path %q", got, legacyTracePath)
	}
	if !strings.Contains(got, filepath.Join(cityDir, ".gc", "runtime", "control-dispatcher-trace.log")) {
		t.Fatalf("stderr = %q, want canonical runtime trace path guidance", got)
	}
	if !strings.Contains(got, "restart or recycle the control-dispatcher session") {
		t.Fatalf("stderr = %q, want restart guidance for still-growing legacy trace", got)
	}
}

func TestRunWorkflowServeWarnsWhenLegacyRigTraceFileStillExists(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n\n[[rigs]]\nname = \"alpha\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeCatalogFile(t, cityDir, ".gc/site.toml", "[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
	rigRoot := filepath.Join(cityDir, "rigs", "alpha")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rig root: %v", err)
	}
	legacyTracePath := filepath.Join(rigRoot, "control-dispatcher-trace.log")
	if err := os.WriteFile(legacyTracePath, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write legacy rig trace: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_RIG_ROOT", "")

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		return nil, nil
	}

	var stderr bytes.Buffer
	if err := runWorkflowServe("", false, io.Discard, &stderr); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	got := stderr.String()
	if !strings.Contains(got, legacyTracePath) {
		t.Fatalf("stderr = %q, want legacy rig trace path %q", got, legacyTracePath)
	}
	if !strings.Contains(got, "legacy control-dispatcher trace file") {
		t.Fatalf("stderr = %q, want legacy rig trace warning", got)
	}
}

func TestRunWorkflowServeWarnsWhenLegacyEnvRigTraceFileStillExistsOutsideConfiguredRigs(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n\n[[rigs]]\nname = \"alpha\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeCatalogFile(t, cityDir, ".gc/site.toml", "[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
	rigRoot := filepath.Join(cityDir, "rigs", "beta")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rig root: %v", err)
	}
	legacyTracePath := filepath.Join(rigRoot, "control-dispatcher-trace.log")
	if err := os.WriteFile(legacyTracePath, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write legacy env rig trace: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_RIG_ROOT", rigRoot)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		return nil, nil
	}

	var stderr bytes.Buffer
	if err := runWorkflowServe("", false, io.Discard, &stderr); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	got := stderr.String()
	if !strings.Contains(got, legacyTracePath) {
		t.Fatalf("stderr = %q, want undeclared rig trace path %q", got, legacyTracePath)
	}
	if !strings.Contains(got, "legacy control-dispatcher trace file") {
		t.Fatalf("stderr = %q, want undeclared rig trace warning", got)
	}
}

func TestRunControlDispatcherWithStoreRoutesRalphTraceWarningToStderr(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	checkPath := filepath.Join(cityDir, "pass-check.sh")
	if err := os.WriteFile(checkPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write pass-check.sh: %v", err)
	}
	t.Setenv("GC_WORKFLOW_TRACE", filepath.Join(t.TempDir(), "missing", "workflow-trace.log"))

	store := beads.NewMemStore()
	workflow, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("create workflow bead: %v", err)
	}
	logical, err := store.Create(beads.Bead{
		Title: "logical",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.step_id":      "implement",
			"gc.max_attempts": "1",
			"gc.root_bead_id": workflow.ID,
		},
	})
	if err != nil {
		t.Fatalf("create logical bead: %v", err)
	}
	run1, err := store.Create(beads.Bead{
		Title: "run 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "run",
			"gc.step_id":         "implement",
			"gc.ralph_step_id":   "implement",
			"gc.attempt":         "1",
			"gc.step_ref":        "implement.run.1",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})
	if err != nil {
		t.Fatalf("create run bead: %v", err)
	}
	check1, err := store.Create(beads.Bead{
		Title: "check 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "check",
			"gc.step_id":         "implement",
			"gc.ralph_step_id":   "implement",
			"gc.attempt":         "1",
			"gc.step_ref":        "implement.check.1",
			"gc.check_mode":      "exec",
			"gc.check_path":      "pass-check.sh",
			"gc.check_timeout":   "30s",
			"gc.max_attempts":    "1",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})
	if err != nil {
		t.Fatalf("create check bead: %v", err)
	}
	if err := store.DepAdd(check1.ID, run1.ID, "blocks"); err != nil {
		t.Fatalf("add check->run dep: %v", err)
	}
	if err := store.DepAdd(logical.ID, check1.ID, "blocks"); err != nil {
		t.Fatalf("add logical->check dep: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStore(cityDir, cityDir, store, check1, check1.ID, &stdout, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStore: %v", err)
	}

	gotStderr := stderr.String()
	if count := strings.Count(gotStderr, "opening workflow trace"); count != 1 {
		t.Fatalf("warning count = %d, want 1; stderr=%q", count, gotStderr)
	}
	if !strings.Contains(gotStderr, "gc convoy control --serve: warning: opening workflow trace") {
		t.Fatalf("stderr = %q, want workflow trace warning prefix", gotStderr)
	}
	if gotStdout := stdout.String(); !strings.Contains(gotStdout, "action=pass") {
		t.Fatalf("stdout = %q, want processed pass action", gotStdout)
	}
	checkAfter, err := store.Get(check1.ID)
	if err != nil {
		t.Fatalf("reload check bead: %v", err)
	}
	if checkAfter.Status != "closed" || checkAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("check bead = status %q outcome %q, want closed/pass", checkAfter.Status, checkAfter.Metadata["gc.outcome"])
	}
}

func TestRunControlDispatcherWithStoreWarnsOnLegacyTracePath(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	checkPath := filepath.Join(cityDir, "pass-check.sh")
	if err := os.WriteFile(checkPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write pass-check.sh: %v", err)
	}
	legacyTracePath := filepath.Join(cityDir, "control-dispatcher-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", legacyTracePath)

	store := beads.NewMemStore()
	workflow, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("create workflow bead: %v", err)
	}
	logical, err := store.Create(beads.Bead{
		Title: "logical",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.step_id":      "implement",
			"gc.max_attempts": "1",
			"gc.root_bead_id": workflow.ID,
		},
	})
	if err != nil {
		t.Fatalf("create logical bead: %v", err)
	}
	run1, err := store.Create(beads.Bead{
		Title: "run 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "run",
			"gc.step_id":         "implement",
			"gc.ralph_step_id":   "implement",
			"gc.attempt":         "1",
			"gc.step_ref":        "implement.run.1",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})
	if err != nil {
		t.Fatalf("create run bead: %v", err)
	}
	check1, err := store.Create(beads.Bead{
		Title: "check 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "check",
			"gc.step_id":         "implement",
			"gc.ralph_step_id":   "implement",
			"gc.attempt":         "1",
			"gc.step_ref":        "implement.check.1",
			"gc.check_mode":      "exec",
			"gc.check_path":      "pass-check.sh",
			"gc.check_timeout":   "30s",
			"gc.max_attempts":    "1",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})
	if err != nil {
		t.Fatalf("create check bead: %v", err)
	}
	if err := store.DepAdd(check1.ID, run1.ID, "blocks"); err != nil {
		t.Fatalf("add check->run dep: %v", err)
	}
	if err := store.DepAdd(logical.ID, check1.ID, "blocks"); err != nil {
		t.Fatalf("add logical->check dep: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStore(cityDir, cityDir, store, check1, check1.ID, &stdout, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStore: %v", err)
	}

	got := stderr.String()
	if !strings.Contains(got, legacyTracePath) {
		t.Fatalf("stderr = %q, want legacy trace path %q", got, legacyTracePath)
	}
	if !strings.Contains(got, "change or unset GC_WORKFLOW_TRACE") {
		t.Fatalf("stderr = %q, want explicit override guidance", got)
	}
}

func TestRunWorkflowServeDedupsTraceWarningsAcrossNestedControlDispatch(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	checkPath := filepath.Join(cityDir, "pass-check.sh")
	if err := os.WriteFile(checkPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write pass-check.sh: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_WORKFLOW_TRACE", filepath.Join(t.TempDir(), "missing", "workflow-trace.log"))

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	store := beads.NewMemStore()
	newCheckBead := func(stepID string) string {
		t.Helper()
		workflow, err := store.Create(beads.Bead{
			Title: "workflow " + stepID,
			Type:  "task",
			Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			},
		})
		if err != nil {
			t.Fatalf("create workflow bead for %s: %v", stepID, err)
		}
		logical, err := store.Create(beads.Bead{
			Title: "logical " + stepID,
			Type:  "task",
			Metadata: map[string]string{
				"gc.kind":         "ralph",
				"gc.step_id":      stepID,
				"gc.max_attempts": "1",
				"gc.root_bead_id": workflow.ID,
			},
		})
		if err != nil {
			t.Fatalf("create logical bead for %s: %v", stepID, err)
		}
		run, err := store.Create(beads.Bead{
			Title: "run " + stepID,
			Type:  "task",
			Metadata: map[string]string{
				"gc.kind":            "run",
				"gc.step_id":         stepID,
				"gc.ralph_step_id":   stepID,
				"gc.attempt":         "1",
				"gc.step_ref":        stepID + ".run.1",
				"gc.root_bead_id":    workflow.ID,
				"gc.logical_bead_id": logical.ID,
			},
		})
		if err != nil {
			t.Fatalf("create run bead for %s: %v", stepID, err)
		}
		check, err := store.Create(beads.Bead{
			Title: "check " + stepID,
			Type:  "task",
			Metadata: map[string]string{
				"gc.kind":            "check",
				"gc.step_id":         stepID,
				"gc.ralph_step_id":   stepID,
				"gc.attempt":         "1",
				"gc.step_ref":        stepID + ".check.1",
				"gc.check_mode":      "exec",
				"gc.check_path":      "pass-check.sh",
				"gc.check_timeout":   "30s",
				"gc.max_attempts":    "1",
				"gc.root_bead_id":    workflow.ID,
				"gc.logical_bead_id": logical.ID,
			},
		})
		if err != nil {
			t.Fatalf("create check bead for %s: %v", stepID, err)
		}
		if err := store.DepAdd(check.ID, run.ID, "blocks"); err != nil {
			t.Fatalf("add check->run dep for %s: %v", stepID, err)
		}
		if err := store.DepAdd(logical.ID, check.ID, "blocks"); err != nil {
			t.Fatalf("add logical->check dep for %s: %v", stepID, err)
		}
		return check.ID
	}

	checkOneID := newCheckBead("implement-a")
	checkTwoID := newCheckBead("implement-b")
	sequence := [][]hookBead{
		{{ID: checkOneID, Metadata: map[string]string{"gc.kind": "check"}}},
		{{ID: checkTwoID, Metadata: map[string]string{"gc.kind": "check"}}},
	}
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		if len(sequence) == 0 {
			return nil, nil
		}
		next := sequence[0]
		sequence = sequence[1:]
		return next, nil
	}
	controlDispatcherServe = func(cityPath, storePath, beadID string, stdout, stderr io.Writer) error {
		bead, err := store.Get(beadID)
		if err != nil {
			return err
		}
		return runControlDispatcherWithStore(cityPath, storePath, store, bead, beadID, stdout, stderr)
	}

	var stderr bytes.Buffer
	if err := runWorkflowServe("", false, io.Discard, &stderr); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	got := stderr.String()
	if count := strings.Count(got, "opening workflow trace"); count != 1 {
		t.Fatalf("warning count = %d, want 1 across nested control dispatch; stderr=%q", count, got)
	}
}

func TestRunWorkflowServeDedupsLegacyTraceWarningsAcrossNestedControlDispatch(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	checkPath := filepath.Join(cityDir, "pass-check.sh")
	if err := os.WriteFile(checkPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write pass-check.sh: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_WORKFLOW_TRACE", filepath.Join(cityDir, "control-dispatcher-trace.log"))

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	store := beads.NewMemStore()
	newCheckBead := func(stepID string) string {
		t.Helper()
		workflow, err := store.Create(beads.Bead{
			Title: "workflow " + stepID,
			Type:  "task",
			Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			},
		})
		if err != nil {
			t.Fatalf("create workflow bead for %s: %v", stepID, err)
		}
		logical, err := store.Create(beads.Bead{
			Title: "logical " + stepID,
			Type:  "task",
			Metadata: map[string]string{
				"gc.kind":         "ralph",
				"gc.step_id":      stepID,
				"gc.max_attempts": "1",
				"gc.root_bead_id": workflow.ID,
			},
		})
		if err != nil {
			t.Fatalf("create logical bead for %s: %v", stepID, err)
		}
		run, err := store.Create(beads.Bead{
			Title: "run " + stepID,
			Type:  "task",
			Metadata: map[string]string{
				"gc.kind":            "run",
				"gc.step_id":         stepID,
				"gc.ralph_step_id":   stepID,
				"gc.attempt":         "1",
				"gc.step_ref":        stepID + ".run.1",
				"gc.root_bead_id":    workflow.ID,
				"gc.logical_bead_id": logical.ID,
			},
		})
		if err != nil {
			t.Fatalf("create run bead for %s: %v", stepID, err)
		}
		check, err := store.Create(beads.Bead{
			Title: "check " + stepID,
			Type:  "task",
			Metadata: map[string]string{
				"gc.kind":            "check",
				"gc.step_id":         stepID,
				"gc.ralph_step_id":   stepID,
				"gc.attempt":         "1",
				"gc.step_ref":        stepID + ".check.1",
				"gc.check_mode":      "exec",
				"gc.check_path":      "pass-check.sh",
				"gc.check_timeout":   "30s",
				"gc.max_attempts":    "1",
				"gc.root_bead_id":    workflow.ID,
				"gc.logical_bead_id": logical.ID,
			},
		})
		if err != nil {
			t.Fatalf("create check bead for %s: %v", stepID, err)
		}
		if err := store.DepAdd(check.ID, run.ID, "blocks"); err != nil {
			t.Fatalf("add check->run dep for %s: %v", stepID, err)
		}
		if err := store.DepAdd(logical.ID, check.ID, "blocks"); err != nil {
			t.Fatalf("add logical->check dep for %s: %v", stepID, err)
		}
		return check.ID
	}

	checkOneID := newCheckBead("implement-a")
	checkTwoID := newCheckBead("implement-b")
	sequence := [][]hookBead{
		{{ID: checkOneID, Metadata: map[string]string{"gc.kind": "check"}}},
		{{ID: checkTwoID, Metadata: map[string]string{"gc.kind": "check"}}},
	}
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		if len(sequence) == 0 {
			return nil, nil
		}
		next := sequence[0]
		sequence = sequence[1:]
		return next, nil
	}
	controlDispatcherServe = func(cityPath, storePath, beadID string, stdout, stderr io.Writer) error {
		bead, err := store.Get(beadID)
		if err != nil {
			return err
		}
		return runControlDispatcherWithStore(cityPath, storePath, store, bead, beadID, stdout, stderr)
	}

	var stderr bytes.Buffer
	if err := runWorkflowServe("", false, io.Discard, &stderr); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	got := stderr.String()
	if count := strings.Count(got, "legacy control-dispatcher trace path"); count != 1 {
		t.Fatalf("warning count = %d, want 1 across nested control dispatch; stderr=%q", count, got)
	}
}

func TestWorkflowServeControlReadyQueryUsesControlTiers(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName})
	if strings.Contains(query, "GC_SESSION_ORIGIN") {
		t.Fatalf("workflowServeControlReadyQuery should not gate legacy routes on session origin: %q", query)
	}
	if strings.Contains(query, "bd list --status in_progress") {
		t.Fatalf("workflowServeControlReadyQuery should not return in-progress control beads: %q", query)
	}
	if !strings.Contains(query, "BD_EXPORT_AUTO=false") {
		t.Fatalf("workflowServeControlReadyQuery should disable bd auto-export: %q", query)
	}
	for _, want := range []string{
		`bd --readonly --sandbox ready --include-ephemeral --assignee="$cand" --exclude-type=epic --json --limit=20`,
		`bd --readonly --sandbox ready --include-ephemeral --metadata-field "gc.run_target=$route" --unassigned --exclude-type=epic --json --sort oldest --limit=20`,
		`bd --readonly --sandbox ready --include-ephemeral --metadata-field "gc.routed_to=$route" --unassigned --exclude-type=epic --json --sort oldest --limit=20`,
		`routed_ready "$GC_CONTROL_TARGET"`,
		`routed_ready "${GC_CONTROL_LEGACY_TARGET:-}"`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("workflowServeControlReadyQuery missing %q in %q", want, query)
		}
	}
	if !strings.Contains(query, `--limit=20`) {
		t.Fatalf("workflowServeControlReadyQuery missing scan limit: %q", query)
	}
}

func TestWorkflowServeControlReadyQueryIgnoresInProgressAssigned(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	out := runWorkflowServeShellQueryForTest(t, query, map[string]string{
		"GC_SESSION_NAME":   "gascity--control-dispatcher",
		"GC_ALIAS":          "gascity/control-dispatcher",
		"GC_SESSION_ORIGIN": "named",
	}, `#!/bin/sh
set -eu
case "$*" in
  "list --status in_progress --assignee=gascity--control-dispatcher --json --limit=20")
    printf '[{"id":"ga-in-progress"}]'
    ;;
  "--readonly --sandbox ready --assignee=gascity--control-dispatcher --json --limit=20")
    printf '[{"id":"ga-epic-leak"}]'
    ;;
  "--readonly --sandbox ready --include-ephemeral --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf '[{"id":"ga-ready"}]'
    ;;
  "--readonly --sandbox ready --include-ephemeral --metadata-field gc.run_target=gascity/control-dispatcher --unassigned --exclude-type=epic --json --sort oldest --limit=20")
    printf '[{"id":"ga-routed"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)
	assertJSONEqual(t, out, `[{"id":"ga-ready"},{"id":"ga-routed"}]`)
}

func TestWorkflowServeControlReadyQueryIncludesMetadataRoutedWorkAfterAssignedPending(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	out := runWorkflowServeShellQueryForTest(t, query, map[string]string{
		"GC_SESSION_NAME": "gascity--control-dispatcher",
		"GC_ALIAS":        "gascity/control-dispatcher",
	}, `#!/bin/sh
set -eu
case "$*" in
  "--readonly --sandbox ready --include-ephemeral --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf '[{"id":"ga-pending","metadata":{"gc.kind":"retry"}}]'
    ;;
  "--readonly --sandbox ready --include-ephemeral --metadata-field gc.run_target=gascity/control-dispatcher --unassigned --exclude-type=epic --json --sort oldest --limit=20")
    printf '[{"id":"ga-ready","metadata":{"gc.kind":"scope-check"}}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)
	assertJSONEqual(t, out, `[{"id":"ga-pending","metadata":{"gc.kind":"retry"}},{"id":"ga-ready","metadata":{"gc.kind":"scope-check"}}]`)
}

func TestWorkflowServeControlReadyQueryPreservesQueryPriorityWhenMerging(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	out := runWorkflowServeShellQueryForTest(t, query, map[string]string{
		"GC_SESSION_NAME": "gascity--control-dispatcher",
		"GC_ALIAS":        "gascity/control-dispatcher",
	}, `#!/bin/sh
set -eu
case "$*" in
  "--readonly --sandbox ready --include-ephemeral --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf '[{"id":"ga-z-assigned"},{"id":"ga-dup","source":"assigned"}]'
    ;;
  "--readonly --sandbox ready --include-ephemeral --metadata-field gc.run_target=gascity/control-dispatcher --unassigned --exclude-type=epic --json --sort oldest --limit=20")
    printf '[{"id":"ga-a-routed"},{"id":"ga-route-dup","source":"run-target"}]'
    ;;
  "--readonly --sandbox ready --include-ephemeral --metadata-field gc.routed_to=gascity/control-dispatcher --unassigned --exclude-type=epic --json --sort oldest --limit=20")
    printf '[{"id":"ga-route-dup","source":"routed-to"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)
	assertJSONEqual(t, out, `[{"id":"ga-z-assigned"},{"id":"ga-dup","source":"assigned"},{"id":"ga-a-routed"},{"id":"ga-route-dup","source":"run-target"}]`)
}

func TestWorkflowServeControlReadyQueryUsesConfiguredRuntimeNameWhenEnvIsManualSession(t *testing.T) {
	query := workflowServeControlReadyQuery(
		config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"},
		"gascity--control-dispatcher",
	)
	out := runWorkflowServeShellQueryForTest(t, query, map[string]string{
		"GC_SESSION_ID":     "mc-manual",
		"GC_SESSION_NAME":   "s-mc-manual",
		"GC_AGENT":          "s-mc-manual",
		"GC_SESSION_ORIGIN": "manual",
	}, `#!/bin/sh
set -eu
case "$*" in
  "--readonly --sandbox ready --include-ephemeral --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf '[{"id":"ga-control-ready"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)
	assertJSONEqual(t, out, `[{"id":"ga-control-ready"}]`)
}

func TestWorkflowServeControlReadyQueryFailsFastOnBDReadyError(t *testing.T) {
	query := workflowServeControlReadyQuery(
		config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"},
		"gascity--control-dispatcher",
	)
	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	logPath := filepath.Join(tmp, "bd.log")
	if err := os.WriteFile(bdPath, []byte(`#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$BD_LOG"
printf '[mysql] read tcp 127.0.0.1:1->127.0.0.1:3307: i/o timeout\n' >&2
printf '{"error":"failed to open database: invalid connection"}\n'
exit 1
`), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	_, err := shellWorkQueryWithEnv(query, t.TempDir(), []string{
		"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"BD_LOG=" + logPath,
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	})
	if err == nil {
		t.Fatal("workflow serve query succeeded after bd ready failed")
	}
	msg := err.Error()
	if !strings.Contains(msg, "i/o timeout") || !strings.Contains(msg, "failed to open database") {
		t.Fatalf("error = %q, want bd stderr/stdout details", msg)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read bd log: %v", readErr)
	}
	calls := strings.Split(strings.TrimSpace(string(logData)), "\n")
	if len(calls) != 1 {
		t.Fatalf("bd calls = %d, want fail-fast after first call; calls:\n%s", len(calls), string(logData))
	}
}

func TestWorkflowServeControlReadyQueryPrioritizesConfiguredRuntimeName(t *testing.T) {
	query := workflowServeControlReadyQuery(
		config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"},
		"gascity--control-dispatcher",
	)
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "bd.log")
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(`#!/bin/sh
set -eu
[ "${BD_EXPORT_AUTO:-}" = "false" ] || {
  echo "BD_EXPORT_AUTO=${BD_EXPORT_AUTO:-}" >&2
  exit 43
}
printf '%s\n' "$*" >> "$BD_LOG"
case "$*" in
  "--readonly --sandbox ready --include-ephemeral --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf '[{"id":"ga-control-ready"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	out, err := shellWorkQueryWithEnv(query, t.TempDir(), []string{
		"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"BD_LOG=" + logPath,
		"GC_SESSION_ID=mc-manual",
		"GC_SESSION_NAME=s-mc-manual",
		"GC_AGENT=s-mc-manual",
		"GC_SESSION_ORIGIN=manual",
	})
	if err != nil {
		t.Fatalf("run workflow serve query: %v", err)
	}
	assertJSONEqual(t, out, `[{"id":"ga-control-ready"}]`)
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	firstCall, _, _ := strings.Cut(strings.TrimSpace(string(logData)), "\n")
	if want := "--readonly --sandbox ready --include-ephemeral --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20"; firstCall != want {
		t.Fatalf("first bd call = %q, want %q; all calls:\n%s", firstCall, want, string(logData))
	}
}

func TestWorkflowServeControlReadyQueryDeduplicatesAssigneeProbes(t *testing.T) {
	query := workflowServeControlReadyQuery(
		config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"},
		"gascity--control-dispatcher",
	)
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "bd.log")
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(`#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$BD_LOG"
case "$*" in
  "--readonly --sandbox ready --include-ephemeral --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf '[{"id":"ga-control-ready"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	out, err := shellWorkQueryWithEnv(query, t.TempDir(), []string{
		"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"BD_LOG=" + logPath,
		"GC_SESSION_ID=gascity--control-dispatcher",
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	})
	if err != nil {
		t.Fatalf("run workflow serve query: %v", err)
	}
	assertJSONEqual(t, out, `[{"id":"ga-control-ready"}]`)
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	if got := strings.Count(string(logData), "--assignee=gascity--control-dispatcher "); got != 1 {
		t.Fatalf("gascity--control-dispatcher query count = %d, want 1; calls:\n%s", got, string(logData))
	}
	if got := strings.Count(string(logData), "--assignee=gascity--workflow-control "); got != 1 {
		t.Fatalf("gascity--workflow-control query count = %d, want 1; calls:\n%s", got, string(logData))
	}
}

func TestWorkflowServeControlReadyQueryQuotesMetadataFallbackTarget(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "my rig"})
	tmp := t.TempDir()
	argsPath := filepath.Join(tmp, "matched.args")
	out := runWorkflowServeShellQueryForTest(t, query, map[string]string{
		"BD_MATCHED_ARGS": argsPath,
	}, `#!/bin/sh
set -eu
if [ "$#" -eq 12 ] &&
   [ "$1" = "--readonly" ] &&
   [ "$2" = "--sandbox" ] &&
   [ "$3" = "ready" ] &&
   [ "$4" = "--include-ephemeral" ] &&
   [ "$5" = "--metadata-field" ] &&
   [ "$6" = "gc.run_target=my rig/control-dispatcher" ] &&
   [ "$7" = "--unassigned" ] &&
   [ "$8" = "--exclude-type=epic" ] &&
   [ "$9" = "--json" ] &&
   [ "${10}" = "--sort" ] &&
   [ "${11}" = "oldest" ] &&
   [ "${12}" = "--limit=20" ]; then
  printf '%s\n' "$@" > "$BD_MATCHED_ARGS"
  printf '[{"id":"ga-routed"}]'
  exit 0
fi
printf '[]'
`)
	assertJSONEqual(t, out, `[{"id":"ga-routed"}]`)
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read matched args: %v", err)
	}
	gotArgs := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	wantArgs := []string{"--readonly", "--sandbox", "ready", "--include-ephemeral", "--metadata-field", "gc.run_target=my rig/control-dispatcher", "--unassigned", "--exclude-type=epic", "--json", "--sort", "oldest", "--limit=20"}
	if !slices.Equal(gotArgs, wantArgs) {
		t.Fatalf("matched bd args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestWorkflowServeControlReadyQueryUsesLegacyRouteForNamedSessions(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	out := runWorkflowServeShellQueryForTest(t, query, map[string]string{
		"GC_SESSION_NAME":   "gascity--control-dispatcher",
		"GC_ALIAS":          "gascity/control-dispatcher",
		"GC_SESSION_ORIGIN": "named",
	}, `#!/bin/sh
set -eu
case "$*" in
  "--readonly --sandbox ready --include-ephemeral --metadata-field gc.run_target=gascity/workflow-control --unassigned --exclude-type=epic --json --sort oldest --limit=20")
    printf '[{"id":"ga-legacy-route"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)
	assertJSONEqual(t, out, `[{"id":"ga-legacy-route"}]`)
}

func runWorkflowServeShellQueryForTest(t *testing.T, query string, env map[string]string, bdScript string) string {
	t.Helper()

	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	queryEnv := []string{"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH")}
	for key, value := range env {
		queryEnv = append(queryEnv, key+"="+value)
	}
	out, err := shellWorkQueryWithEnv(query, t.TempDir(), queryEnv)
	if err != nil {
		t.Fatalf("run workflow serve query: %v", err)
	}
	return out
}

func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("unmarshal got JSON %q: %v", got, err)
	}
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("unmarshal want JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON output = %s, want %s", got, want)
	}
}

// TestRunWorkflowServeOverridesInheritedCityBeadsDir is a regression test for
// #514: the serve path must pass rig-scoped env to work query subprocesses,
// not inherit a city-scoped BEADS_DIR from the parent.
func TestRunWorkflowServeOverridesInheritedCityBeadsDir(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_TMUX_SESSION", "host-session")
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig-repo")

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n\n[[rigs]]\nname = \"myrig\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCatalogFile(t, cityDir, "pack.toml", "[pack]\nname = \"test-city\"\nschema = 2\n")
	writeCatalogFile(t, cityDir, ".gc/site.toml", fmt.Sprintf("[[rig]]\nname = \"myrig\"\npath = %q\n", rigDir))
	writeCatalogFile(t, cityDir, "agents/worker/agent.toml", "dir = \"myrig\"\n")

	t.Setenv("GC_CITY", cityDir)
	// Pollute parent env with a city-scoped BEADS_DIR. Without the fix,
	// this value leaks into work query subprocesses.
	cityBeads := filepath.Join(cityDir, ".beads")
	t.Setenv("BEADS_DIR", cityBeads)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	var capturedEnv map[string]string
	workflowServeList = func(_, _ string, env map[string]string) ([]hookBead, error) {
		capturedEnv = maps.Clone(env)
		return nil, nil // no work: exits immediately
	}
	controlDispatcherServe = func(_, _, _ string, _ io.Writer, _ io.Writer) error {
		return nil
	}

	if err := runWorkflowServe("worker", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	if capturedEnv == nil {
		t.Fatal("workflowServeList received nil env, want rig-scoped env")
	}
	wantBeads := filepath.Join(rigDir, ".beads")
	if got := capturedEnv["BEADS_DIR"]; got != wantBeads {
		t.Fatalf("BEADS_DIR = %q, want rig store %q", got, wantBeads)
	}
	if capturedEnv["BEADS_DIR"] == cityBeads {
		t.Fatalf("BEADS_DIR inherited city store %q", cityBeads)
	}
	if got := capturedEnv["GC_STORE_ROOT"]; got != rigDir {
		t.Fatalf("GC_STORE_ROOT = %q, want rig root %q", got, rigDir)
	}
	if got := capturedEnv["GC_STORE_SCOPE"]; got != "rig" {
		t.Fatalf("GC_STORE_SCOPE = %q, want rig", got)
	}
}

func TestRunWorkflowServeProcessesControlBeadsInAgentStoreScope(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig-repo")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[daemon]
formula_v2 = true

[[rigs]]
name = "myrig"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCatalogFile(t, cityDir, ".gc/site.toml", fmt.Sprintf("[[rig]]\nname = \"myrig\"\npath = %q\n", rigDir))
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	calls := 0
	var queryDir string
	workflowServeList = func(_, dir string, _ map[string]string) ([]hookBead, error) {
		calls++
		queryDir = dir
		if calls == 1 {
			return []hookBead{{ID: "gc-rig-control", Metadata: map[string]string{"gc.kind": "scope-check"}}}, nil
		}
		return nil, nil
	}

	var gotCityPath, gotStorePath, gotBeadID string
	controlDispatcherServe = func(cityPath, storePath, beadID string, _ io.Writer, _ io.Writer) error {
		gotCityPath = cityPath
		gotStorePath = storePath
		gotBeadID = beadID
		return nil
	}

	if err := runWorkflowServe("myrig/control-dispatcher", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}
	if canonicalTestPath(queryDir) != canonicalTestPath(rigDir) {
		t.Fatalf("query dir = %q, want rig root %q", queryDir, rigDir)
	}
	if canonicalTestPath(gotCityPath) != canonicalTestPath(cityDir) {
		t.Fatalf("control cityPath = %q, want %q", gotCityPath, cityDir)
	}
	if canonicalTestPath(gotStorePath) != canonicalTestPath(rigDir) {
		t.Fatalf("control storePath = %q, want rig root %q", gotStorePath, rigDir)
	}
	if gotBeadID != "gc-rig-control" {
		t.Fatalf("control beadID = %q, want gc-rig-control", gotBeadID)
	}
}

func TestOpenControlStoreDisablesAutoExportWithoutSandboxingWrites(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig-repo")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig", Path: rigDir}},
	}
	t.Setenv("GC_BEADS", "bd")

	var calls [][]string
	var envs []map[string]string
	prevRunner := beadsExecCommandRunnerWithEnv
	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		envs = append(envs, maps.Clone(env))
		return func(_ string, name string, args ...string) ([]byte, error) {
			if name != "bd" {
				return nil, fmt.Errorf("unexpected command %q", name)
			}
			calls = append(calls, append([]string(nil), args...))
			return []byte(`[]`), nil
		}
	}
	t.Cleanup(func() { beadsExecCommandRunnerWithEnv = prevRunner })

	status := "closed"
	cityStore, err := openControlStoreAtForCity(cityDir, cityDir, cfg)
	if err != nil {
		t.Fatalf("openControlStoreAtForCity(city): %v", err)
	}
	if err := cityStore.Update("ga-city-control", beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("city control update: %v", err)
	}
	rigStore, err := openControlStoreAtForCity(rigDir, cityDir, cfg)
	if err != nil {
		t.Fatalf("openControlStoreAtForCity(rig): %v", err)
	}
	if err := rigStore.Update("ga-rig-control", beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("rig control update: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("bd calls = %#v, want two update calls", calls)
	}
	if len(envs) != 2 {
		t.Fatalf("bd envs = %#v, want two command environments", envs)
	}
	for i, call := range calls {
		if len(call) < 1 || call[0] != "update" {
			t.Fatalf("bd call = %#v, want update ...", call)
		}
		if slices.Contains(call, "--sandbox") {
			t.Fatalf("bd call = %#v, write-capable control stores must not use --sandbox", call)
		}
		if got := envs[i]["BD_EXPORT_AUTO"]; got != "false" {
			t.Fatalf("bd env %d BD_EXPORT_AUTO = %q, want false", i, got)
		}
	}
}

func TestOpenControlStoreAtForCityPreservesFileAndExecProviderStores(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecStoreCityConfig(t, cityDir, "metro-city", "ct", []config.Rig{{
		Name:   "frontend",
		Path:   "rigs/frontend",
		Prefix: "fe",
	}})
	cfg := &config.City{
		Workspace: config.Workspace{Name: "metro-city", Prefix: "ct"},
		Rigs: []config.Rig{{
			Name:   "frontend",
			Path:   "rigs/frontend",
			Prefix: "fe",
		}},
	}

	t.Run("file", func(t *testing.T) {
		t.Setenv("GC_BEADS", "file")
		t.Setenv("GC_BEADS_SCOPE_ROOT", "")
		store, err := openControlStoreAtForCity(rigDir, cityDir, cfg)
		if err != nil {
			t.Fatalf("openControlStoreAtForCity(file): %v", err)
		}
		if _, ok := store.(*beads.FileStore); !ok {
			t.Fatalf("control store = %T, want *beads.FileStore for file provider", store)
		}
	})

	t.Run("exec", func(t *testing.T) {
		captureDir := t.TempDir()
		script := writeExecCaptureScript(t, captureDir)
		provider := "exec:" + script
		t.Setenv("GC_BEADS", provider)
		t.Setenv("GC_BEADS_SCOPE_ROOT", "")

		store, err := openControlStoreAtForCity(rigDir, cityDir, cfg)
		if err != nil {
			t.Fatalf("openControlStoreAtForCity(exec): %v", err)
		}
		if _, err := store.Create(beads.Bead{Title: "rig"}); err != nil {
			t.Fatalf("exec control Create: %v", err)
		}
		env := readExecCaptureEnv(t, filepath.Join(captureDir, "frontend.env"))
		if got := env["GC_PROVIDER"]; got != provider {
			t.Fatalf("exec GC_PROVIDER = %q, want %q", got, provider)
		}
		if got := env["GC_STORE_SCOPE"]; got != "rig" {
			t.Fatalf("exec GC_STORE_SCOPE = %q, want rig", got)
		}
	})
}

func TestOpenControlStoreAtForCityUsesControlRunnerForStaleBdScope(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	staleRigDir := filepath.Join(cityDir, "rigs", "removed")
	if err := os.MkdirAll(filepath.Join(staleRigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleRigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"removed"}`), 0o644); err != nil {
		t.Fatalf("write stale rig metadata: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "active", Path: "rigs/active"}},
	}
	t.Setenv("GC_BEADS", "bd")

	var calls [][]string
	var envs []map[string]string
	prevRunner := beadsExecCommandRunnerWithEnv
	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		envs = append(envs, maps.Clone(env))
		return func(_ string, name string, args ...string) ([]byte, error) {
			if name != "bd" {
				return nil, fmt.Errorf("unexpected command %q", name)
			}
			calls = append(calls, append([]string(nil), args...))
			return []byte(`[]`), nil
		}
	}
	t.Cleanup(func() { beadsExecCommandRunnerWithEnv = prevRunner })

	status := "closed"
	store, err := openControlStoreAtForCity(staleRigDir, cityDir, cfg)
	if err != nil {
		t.Fatalf("openControlStoreAtForCity(stale rig): %v", err)
	}
	if err := store.Update("ga-stale-control", beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("stale rig control update: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("bd calls = %#v, want one update call", calls)
	}
	if len(envs) != 1 {
		t.Fatalf("bd envs = %#v, want one command environment", envs)
	}
	if call := calls[0]; len(call) < 1 || call[0] != "update" {
		t.Fatalf("bd call = %#v, want update ...", calls[0])
	}
	if slices.Contains(calls[0], "--sandbox") {
		t.Fatalf("bd call = %#v, write-capable control stores must not use --sandbox", calls[0])
	}
	if got := envs[0]["BD_EXPORT_AUTO"]; got != "false" {
		t.Fatalf("BD_EXPORT_AUTO = %q, want false", got)
	}
	if got := envs[0]["BEADS_DIR"]; got != filepath.Join(staleRigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want stale rig store", got)
	}
	if got := envs[0]["GC_RIG_ROOT"]; got != staleRigDir {
		t.Fatalf("GC_RIG_ROOT = %q, want stale rig root", got)
	}
}

func TestRunWorkflowServeUsesGCTemplateForSessionContext(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigrepo")

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[rigs]]
name = "rigrepo"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCatalogFile(t, cityDir, "pack.toml", "[pack]\nname = \"test-city\"\nschema = 2\n")
	writeCatalogFile(t, cityDir, ".gc/site.toml", "[[rig]]\nname = \"rigrepo\"\npath = \"rigrepo\"\n")
	writeCatalogFile(t, cityDir, "agents/polecat/agent.toml", "dir = \"rigrepo\"\nmin_active_sessions = 0\nmax_active_sessions = 5\n")

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "rigrepo/furiosa")
	t.Setenv("GC_AGENT", "rigrepo/furiosa")
	t.Setenv("GC_TEMPLATE", "rigrepo/polecat")
	t.Setenv("GC_SESSION_NAME", "rigrepo--furiosa")

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	var gotQuery string
	var gotDir string
	workflowServeList = func(workQuery, dir string, _ map[string]string) ([]hookBead, error) {
		gotQuery = workQuery
		gotDir = dir
		return nil, nil
	}
	controlDispatcherServe = func(_, _, _ string, _ io.Writer, _ io.Writer) error {
		t.Fatal("controlDispatcherServe should not run when no control work is returned")
		return nil
	}

	if err := runWorkflowServe("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}
	if gotQuery == "" {
		t.Fatal("workflowServeList query was empty, want polecat work query")
	}
	if canonicalTestPath(gotDir) != canonicalTestPath(rigDir) {
		t.Fatalf("workflowServeList dir = %q, want rig root %q", gotDir, rigDir)
	}
}

func TestRunWorkflowServeRetriesBrieflyAfterProcessingBeforeIdleExit(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 2
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	var controlled []string
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		switch calls {
		case 1:
			return []hookBead{{ID: "gc-ctrl-1", Metadata: map[string]string{"gc.kind": "scope-check"}}}, nil
		case 2:
			return nil, nil
		case 3:
			return []hookBead{{ID: "gc-ctrl-2", Metadata: map[string]string{"gc.kind": "check"}}}, nil
		default:
			return nil, nil
		}
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		controlled = append(controlled, beadID)
		return nil
	}

	if err := runWorkflowServe("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	if !slices.Equal(controlled, []string{"gc-ctrl-1", "gc-ctrl-2"}) {
		t.Fatalf("controlled beads = %#v, want follow-on control bead after brief empty poll", controlled)
	}
}

func TestRunWorkflowServeSkipsPendingControlBeadAndProcessesLaterReady(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	var attempted []string
	var processed []string
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		switch calls {
		case 1:
			return []hookBead{
				{ID: "gc-pending", Metadata: map[string]string{"gc.kind": "retry-eval"}},
				{ID: "gc-ready", Metadata: map[string]string{"gc.kind": "scope-check"}},
			}, nil
		default:
			return nil, nil
		}
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		attempted = append(attempted, beadID)
		if beadID == "gc-pending" {
			return dispatch.ErrControlPending
		}
		processed = append(processed, beadID)
		return nil
	}

	if err := runWorkflowServe("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	if !slices.Equal(attempted, []string{"gc-pending", "gc-ready"}) {
		t.Fatalf("attempted beads = %#v, want pending bead skipped before ready bead is processed", attempted)
	}
	if !slices.Equal(processed, []string{"gc-ready"}) {
		t.Fatalf("processed beads = %#v, want only later ready bead to be processed", processed)
	}
}

func TestRunControlDispatcherReturnsPendingForOpenScopeSubject(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	store := beads.NewMemStore()
	workflow, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	body, err := store.Create(beads.Bead{
		Title: "scope body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_ref":    "pending-scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
		},
	})
	if err != nil {
		t.Fatalf("create scope body: %v", err)
	}
	subject, err := store.Create(beads.Bead{
		Title: "open subject",
		Type:  "task",
		Metadata: map[string]string{
			"gc.scope_ref":    "pending-scope",
			"gc.scope_role":   "member",
			"gc.root_bead_id": workflow.ID,
		},
	})
	if err != nil {
		t.Fatalf("create subject: %v", err)
	}
	control, err := store.Create(beads.Bead{
		Title: "Finalize pending scope",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "pending-scope",
			"gc.scope_role":   "control",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	if err := store.DepAdd(control.ID, subject.ID, "blocks"); err != nil {
		t.Fatalf("add control dependency: %v", err)
	}

	var stderr bytes.Buffer
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	err = runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, control, control.ID, cfg, io.Discard, &stderr)
	if !errors.Is(err, dispatch.ErrControlPending) {
		t.Fatalf("runControlDispatcherWithStoreAndConfig error = %v, want ErrControlPending", err)
	}

	after, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if after.Status != "open" {
		t.Fatalf("control status = %q, want open", after.Status)
	}
	if got := after.Metadata["gc.control_quarantined"]; got != "" {
		t.Fatalf("gc.control_quarantined = %q, want empty", got)
	}
	if slices.Contains(after.Labels, "gc:control-quarantined") {
		t.Fatalf("labels = %#v, want no gc:control-quarantined", after.Labels)
	}
	bodyAfter, err := store.Get(body.ID)
	if err != nil {
		t.Fatalf("get scope body: %v", err)
	}
	if bodyAfter.Status != "open" {
		t.Fatalf("body status = %q, want open", bodyAfter.Status)
	}
	if got := stderr.String(); strings.Contains(got, "control dispatch: quarantined bead="+control.ID) {
		t.Fatalf("stderr = %q, want no quarantine message", got)
	}
}

func TestRunWorkflowServeDispatchesUnexpectedNonControlBeadAndProcessesLaterReady(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	var controlled []string
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		switch calls {
		case 1:
			return []hookBead{
				{ID: "gc-task", Metadata: map[string]string{"gc.routed_to": "workflows.codex-max"}},
				{ID: "gc-ready", Metadata: map[string]string{"gc.kind": "scope-check"}},
			}, nil
		default:
			return nil, nil
		}
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		controlled = append(controlled, beadID)
		return nil
	}

	if err := runWorkflowServe("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	if !slices.Equal(controlled, []string{"gc-task", "gc-ready"}) {
		t.Fatalf("controlled beads = %#v, want unexpected bead dispatched before later ready bead", controlled)
	}
}

func TestRunWorkflowServeDispatchesUnexpectedNonControlOnly(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	var controlled []string
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		if calls > 1 {
			return nil, nil
		}
		return []hookBead{
			{ID: "gc-task", Metadata: map[string]string{"gc.routed_to": "workflows.codex-max"}},
		}, nil
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		controlled = append(controlled, beadID)
		return nil
	}

	if err := runWorkflowServe("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}
	if calls != 2 {
		t.Fatalf("workflowServeList calls = %d, want one processed pass and one empty requery", calls)
	}
	if !slices.Equal(controlled, []string{"gc-task"}) {
		t.Fatalf("controlled beads = %#v, want unexpected bead dispatched once", controlled)
	}
}

func TestRunWorkflowServeQuarantinesUnexpectedNonControlBead(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	store := beads.NewMemStore()
	nonControl, err := store.Create(beads.Bead{
		Title: "misrouted workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("create non-control bead: %v", err)
	}

	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		if calls > 1 {
			return nil, nil
		}
		return []hookBead{{ID: nonControl.ID, Metadata: map[string]string{"gc.kind": "workflow"}}}, nil
	}
	controlDispatcherServe = func(cityPath, storePath, beadID string, stdout, stderr io.Writer) error {
		bead, err := store.Get(beadID)
		if err != nil {
			return err
		}
		cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
		return runControlDispatcherWithStoreAndConfig(cityPath, storePath, store, bead, beadID, cfg, stdout, stderr)
	}

	var stderr bytes.Buffer
	if err := runWorkflowServe("", false, io.Discard, &stderr); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	after, err := store.Get(nonControl.ID)
	if err != nil {
		t.Fatalf("get non-control bead: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("status = %q, want closed", after.Status)
	}
	if got := after.Metadata["gc.control_quarantined"]; got != "true" {
		t.Fatalf("gc.control_quarantined = %q, want true", got)
	}
	if got := after.Metadata["gc.final_disposition"]; got != "control_quarantined" {
		t.Fatalf("gc.final_disposition = %q, want control_quarantined", got)
	}
	if got := stderr.String(); !strings.Contains(got, "control dispatch: quarantined bead="+nonControl.ID) {
		t.Fatalf("stderr = %q, want quarantine message", got)
	}
}

func TestRunWorkflowServeTreatsTransientControllerSpawnPendingAsNonFatal(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		if calls == 1 {
			return []hookBead{{ID: "gc-retry-control", Metadata: map[string]string{"gc.kind": "retry"}}}, nil
		}
		return nil, nil
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		if beadID != "gc-retry-control" {
			t.Fatalf("controlDispatcherServe beadID = %q, want gc-retry-control", beadID)
		}
		return fmt.Errorf("classified transient controller spawn: %w", dispatch.ErrControlPending)
	}

	if err := runWorkflowServe("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}
}

func TestRunWorkflowServeTreatsTransientControlErrorAsPending(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	var attempted []string
	var processed []string
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		if calls == 1 {
			return []hookBead{
				{ID: "gc-transient", Metadata: map[string]string{"gc.kind": "ralph"}},
				{ID: "gc-ready", Metadata: map[string]string{"gc.kind": "scope-check"}},
			}, nil
		}
		return nil, nil
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		attempted = append(attempted, beadID)
		if beadID == "gc-transient" {
			return fmt.Errorf("gc-transient: spawning iteration 2: adding dep: failed to check for dependency cycle: invalid connection: i/o timeout")
		}
		processed = append(processed, beadID)
		return nil
	}

	if err := runWorkflowServe("", false, io.Discard, io.Discard); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	if !slices.Equal(attempted, []string{"gc-transient", "gc-ready"}) {
		t.Fatalf("attempted beads = %#v, want transient bead skipped before ready bead is processed", attempted)
	}
	if !slices.Equal(processed, []string{"gc-ready"}) {
		t.Fatalf("processed beads = %#v, want only later ready bead to be processed", processed)
	}
}

func TestRunControlDispatcherQuarantinesMalformedControlGraph(t *testing.T) {
	clearGCEnv(t)

	store := beads.NewMemStore()
	workflow, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	subject, err := store.Create(beads.Bead{
		Title: "closed subject",
		Type:  "task",
		Metadata: map[string]string{
			"gc.scope_ref":    "missing-scope",
			"gc.scope_role":   "member",
			"gc.root_bead_id": workflow.ID,
			"gc.outcome":      "fail",
		},
	})
	if err != nil {
		t.Fatalf("create subject: %v", err)
	}
	if err := store.Close(subject.ID); err != nil {
		t.Fatalf("close subject: %v", err)
	}
	control, err := store.Create(beads.Bead{
		Title: "Finalize missing scope",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "missing-scope",
			"gc.scope_role":   "control",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	if err := store.DepAdd(control.ID, subject.ID, "blocks"); err != nil {
		t.Fatalf("add control dependency: %v", err)
	}

	var stderr bytes.Buffer
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if err := runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, control, control.ID, cfg, io.Discard, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStoreAndConfig: %v", err)
	}

	after, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("control status = %q, want closed", after.Status)
	}
	if got := after.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("gc.outcome = %q, want fail", got)
	}
	if got := after.Metadata["gc.failure_reason"]; got != "malformed_control_graph" {
		t.Fatalf("gc.failure_reason = %q, want malformed_control_graph", got)
	}
	if got := after.Metadata["gc.control_quarantined"]; got != "true" {
		t.Fatalf("gc.control_quarantined = %q, want true", got)
	}
	if got := after.Metadata["gc.control_quarantined_at"]; got == "" {
		t.Fatalf("gc.control_quarantined_at is empty")
	}
	if got := after.Metadata["gc.control_quarantine_reason"]; !strings.Contains(got, "scope body missing") {
		t.Fatalf("gc.control_quarantine_reason = %q, want scope body missing", got)
	}
	if !slices.Contains(after.Labels, "gc:control-quarantined") {
		t.Fatalf("labels = %#v, want gc:control-quarantined", after.Labels)
	}
	if got := stderr.String(); !strings.Contains(got, "control dispatch: quarantined bead="+control.ID) {
		t.Fatalf("stderr = %q, want quarantine message", got)
	}
}

func TestRunControlDispatcherQuarantinesMalformedFanoutScopeBody(t *testing.T) {
	clearGCEnv(t)

	store := beads.NewMemStore()
	workflow, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	fanout, err := store.Create(beads.Bead{
		Title: "Fan out missing scope",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.fanout_state": "spawned",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "missing-scope",
			"gc.scope_role":   "member",
		},
	})
	if err != nil {
		t.Fatalf("create fanout: %v", err)
	}

	var stderr bytes.Buffer
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if err := runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, fanout, fanout.ID, cfg, io.Discard, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStoreAndConfig: %v", err)
	}

	after, err := store.Get(fanout.ID)
	if err != nil {
		t.Fatalf("get fanout: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("fanout status = %q, want closed", after.Status)
	}
	if got := after.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("gc.outcome = %q, want fail", got)
	}
	if got := after.Metadata["gc.failure_reason"]; got != "malformed_control_graph" {
		t.Fatalf("gc.failure_reason = %q, want malformed_control_graph", got)
	}
	if got := after.Metadata["gc.control_quarantined"]; got != "true" {
		t.Fatalf("gc.control_quarantined = %q, want true", got)
	}
	if !slices.Contains(after.Labels, "gc:control-quarantined") {
		t.Fatalf("labels = %#v, want gc:control-quarantined", after.Labels)
	}
	if got := stderr.String(); !strings.Contains(got, "control dispatch: quarantined bead="+fanout.ID) {
		t.Fatalf("stderr = %q, want quarantine message", got)
	}
}

func TestRunControlDispatcherQuarantinesGenericControlFailure(t *testing.T) {
	clearGCEnv(t)

	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title: "Unsupported control",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind": "unknown-control-kind",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}

	var stderr bytes.Buffer
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if err := runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, control, control.ID, cfg, io.Discard, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStoreAndConfig: %v", err)
	}

	after, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("control status = %q, want closed", after.Status)
	}
	if got := after.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("gc.outcome = %q, want fail", got)
	}
	if got := after.Metadata["gc.failure_reason"]; got != "control_dispatch_error" {
		t.Fatalf("gc.failure_reason = %q, want control_dispatch_error", got)
	}
	if got := after.Metadata["gc.control_quarantined"]; got != "true" {
		t.Fatalf("gc.control_quarantined = %q, want true", got)
	}
	if got := after.Metadata["gc.controller_error"]; !strings.Contains(got, "unsupported control bead kind") {
		t.Fatalf("gc.controller_error = %q, want unsupported control bead kind", got)
	}
	if got := after.Metadata["gc.final_disposition"]; got != "control_quarantined" {
		t.Fatalf("gc.final_disposition = %q, want control_quarantined", got)
	}
	if !slices.Contains(after.Labels, "gc:control-quarantined") {
		t.Fatalf("labels = %#v, want gc:control-quarantined", after.Labels)
	}
	if got := stderr.String(); !strings.Contains(got, "control dispatch: quarantined bead="+control.ID) {
		t.Fatalf("stderr = %q, want quarantine message", got)
	}
}

func TestRunWorkflowServeReturnsLegacyOversizedControlError(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	var attempted []string
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		return []hookBead{
			{ID: "gc-legacy", Metadata: map[string]string{"gc.kind": "ralph"}},
		}, nil
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		attempted = append(attempted, beadID)
		if beadID == "gc-legacy" {
			return fmt.Errorf("gc-legacy: recording attempt log: setting metadata on %q: failed to record event: old_value is too large", beadID)
		}
		return nil
	}

	err := runWorkflowServe("", false, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("runWorkflowServe error = nil, want legacy oversized control error")
	}
	if !strings.Contains(err.Error(), "recording attempt log") ||
		!strings.Contains(err.Error(), "old_value is too large") {
		t.Fatalf("runWorkflowServe error = %v, want oversized attempt-log error surfaced", err)
	}

	if !slices.Equal(attempted, []string{"gc-legacy"}) {
		t.Fatalf("attempted beads = %#v, want serve to stop at surfaced stranded control", attempted)
	}
}

func TestRunWorkflowServeReturnsQueryError(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	cityFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
	})

	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		return nil, os.ErrDeadlineExceeded
	}
	controlDispatcherServe = func(_, _, _ string, _ io.Writer, _ io.Writer) error {
		t.Fatal("controlDispatcherServe should not be called on query failure")
		return nil
	}

	err := runWorkflowServe("", false, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("runWorkflowServe returned nil error, want query failure")
	}
	if !strings.Contains(err.Error(), "querying control work") {
		t.Fatalf("runWorkflowServe error = %q, want querying control work context", err)
	}
}

func TestRunWorkflowServeQueryKillEmitsCurrentSessionPayload(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "backend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[daemon]
formula_v2 = true

[[rigs]]
name = "backend"
path = %q

[[agent]]
name = "worker"
dir = "backend"
`, rigDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_SESSION_ID", "sess-control-123")
	t.Setenv("GC_TEMPLATE", "gascity/workflows.codex-min")

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	cityFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
	})

	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		return nil, errors.New("signal: killed")
	}
	controlDispatcherServe = func(_, _, _ string, _ io.Writer, _ io.Writer) error {
		t.Fatal("controlDispatcherServe should not be called on query failure")
		return nil
	}

	err := runWorkflowServe("backend/worker", false, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("runWorkflowServe returned nil error, want query failure")
	}
	evts, readErr := events.ReadFiltered(filepath.Join(cityDir, ".gc", "events.jsonl"), events.Filter{Type: events.SessionWorkQueryFailed})
	if readErr != nil {
		t.Fatalf("read work-query failure events: %v", readErr)
	}
	if len(evts) != 1 {
		t.Fatalf("work-query failure events = %d, want 1: %+v", len(evts), evts)
	}
	if evts[0].Subject != "gascity/workflows.codex-min" {
		t.Fatalf("event subject = %q, want current session template", evts[0].Subject)
	}
	payload := decodeSessionLifecyclePayload(t, evts[0])
	if payload.SessionID != "sess-control-123" {
		t.Fatalf("payload SessionID = %q, want sess-control-123", payload.SessionID)
	}
	if payload.Template != "gascity/workflows.codex-min" {
		t.Fatalf("payload Template = %q, want current session template", payload.Template)
	}
	if payload.Template == "backend/worker" {
		t.Fatalf("payload Template used target agent %q, want current session context", payload.Template)
	}
	if payload.Reason != "work query killed (signal: killed)" {
		t.Fatalf("payload Reason = %q, want work query killed (signal: killed)", payload.Reason)
	}
}

func TestRunWorkflowServeExpandsTemplateCommandsWithCityFallback(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := filepath.Join(t.TempDir(), "demo-city")
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[[rigs]]
name = "frontend"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCatalogFile(t, cityDir, "pack.toml", "[pack]\nname = \"demo-city\"\nschema = 2\n")
	writeCatalogFile(t, cityDir, ".gc/site.toml", fmt.Sprintf("[[rig]]\nname = \"frontend\"\npath = %q\n", rigDir))
	writeCatalogFile(t, cityDir, "agents/worker/agent.toml", "dir = \"frontend\"\nwork_query = \"bd {{.CityName}} {{.Rig}} {{.AgentBase}}\"\n")

	prevList := workflowServeList
	t.Cleanup(func() { workflowServeList = prevList })

	var gotQuery string
	workflowServeList = func(workQuery, _ string, _ map[string]string) ([]hookBead, error) {
		gotQuery = workQuery
		return nil, os.ErrDeadlineExceeded
	}

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", rigDir)

	err := runWorkflowServe("worker", false, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), os.ErrDeadlineExceeded.Error()) {
		t.Fatalf("runWorkflowServe error = %v, want wrapped %v", err, os.ErrDeadlineExceeded)
	}
	if gotQuery != "bd demo-city frontend worker" {
		t.Fatalf("workflowServe query = %q, want %q", gotQuery, "bd demo-city frontend worker")
	}
}

func TestRunWorkflowServeFollowUsesSweepFallback(t *testing.T) {
	eventsDir := t.TempDir()
	ep := newTestProvider(t, eventsDir)

	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevProvider := workflowServeOpenEventsProvider
	prevSweep := workflowServeWakeSweepInterval
	workflowServeWakeSweepInterval = time.Millisecond
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeOpenEventsProvider = prevProvider
		workflowServeWakeSweepInterval = prevSweep
	})

	workflowServeOpenEventsProvider = func(io.Writer) (events.Provider, error) {
		return ep, nil
	}

	var processed []string
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		switch calls {
		case 1:
			return nil, nil
		case 2:
			return []hookBead{{ID: "gc-ready", Metadata: map[string]string{"gc.kind": "scope-check"}}}, nil
		default:
			return nil, nil
		}
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		processed = append(processed, beadID)
		return errors.New("synthetic dispatch failure")
	}

	wfcAgent := config.Agent{Name: "control-dispatcher", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)}
	err := runWorkflowServeFollow(
		wfcAgent,
		t.TempDir(),
		t.TempDir(),
		wfcAgent.EffectiveWorkQuery(),
		nil,
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "synthetic dispatch failure") {
		t.Fatalf("runWorkflowServeFollow error = %v, want wrapped synthetic dispatch failure", err)
	}
	if !slices.Equal(processed, []string{"gc-ready"}) {
		t.Fatalf("processed beads = %#v, want sweep fallback to process gc-ready", processed)
	}
}

func TestRunWorkflowServeFollowResetsBackoffForProcessedEventAndPending(t *testing.T) {
	eventsDir := t.TempDir()
	ep := newTestProvider(t, eventsDir)

	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevProvider := workflowServeOpenEventsProvider
	prevWait := workflowServeWaitForWake
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeOpenEventsProvider = prevProvider
		workflowServeWaitForWake = prevWait
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	workflowServeOpenEventsProvider = func(io.Writer) (events.Provider, error) {
		return ep, nil
	}
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0

	type waitCall struct {
		idleSweeps int
		sleepDur   time.Duration
	}
	var waitCalls []waitCall
	stopErr := fmt.Errorf("stop after sequence")
	workflowServeWaitForWake = func(_ <-chan workflowWatchResult, sleepDur time.Duration, idleSweeps int) (bool, error) {
		waitCalls = append(waitCalls, waitCall{idleSweeps: idleSweeps, sleepDur: sleepDur})
		switch len(waitCalls) {
		case 1, 2, 3, 5:
			return false, nil
		case 4:
			return true, nil
		case 6:
			return false, stopErr
		default:
			t.Fatalf("unexpected wait call %d", len(waitCalls))
			return false, stopErr
		}
	}

	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		switch calls {
		case 1, 2, 4, 5, 7:
			return nil, nil
		case 3:
			return []hookBead{{ID: "gc-ready", Metadata: map[string]string{"gc.kind": "scope-check"}}}, nil
		case 6:
			return []hookBead{{ID: "gc-pending", Metadata: map[string]string{"gc.kind": "retry-eval"}}}, nil
		default:
			t.Fatalf("unexpected drain cycle %d", calls)
			return nil, nil
		}
	}
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		if beadID == "gc-pending" {
			return dispatch.ErrControlPending
		}
		return nil
	}

	agent := config.Agent{Name: "control-dispatcher"}
	err := runWorkflowServeFollow(agent, t.TempDir(), t.TempDir(), agent.EffectiveWorkQuery(), nil, io.Discard)
	if !errors.Is(err, stopErr) {
		t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, stopErr)
	}

	want := []waitCall{
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 1, sleepDur: 2 * time.Second},
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 0, sleepDur: 1 * time.Second},
	}
	if !slices.Equal(waitCalls, want) {
		t.Fatalf("wait calls = %#v, want %#v", waitCalls, want)
	}
}

// TestRunWorkflowServeFollowSurvivesTransientWorkQueryTimeout is the
// regression guard for the bug where a single transient work-query timeout
// (the bead store briefly saturated) killed the entire control-dispatcher
// --follow loop, leaving the rig un-dispatched while its session bead still
// reported "active". The loop must survive transient failures and only exit on
// genuinely fatal ones.
func TestRunWorkflowServeFollowSurvivesTransientWorkQueryTimeout(t *testing.T) {
	eventsDir := t.TempDir()
	ep := newTestProvider(t, eventsDir)

	prevList := workflowServeList
	prevProvider := workflowServeOpenEventsProvider
	prevWait := workflowServeWaitForWake
	t.Cleanup(func() {
		workflowServeList = prevList
		workflowServeOpenEventsProvider = prevProvider
		workflowServeWaitForWake = prevWait
	})

	workflowServeOpenEventsProvider = func(io.Writer) (events.Provider, error) { return ep, nil }
	workflowServeWaitForWake = func(_ <-chan workflowWatchResult, _ time.Duration, _ int) (bool, error) {
		return false, nil
	}

	// Drain 1 hits a transient work-query timeout (wraps DeadlineExceeded) — the
	// loop must survive it. Drain 2 returns a genuinely fatal error — the loop
	// must exit on that.
	transientErr := fmt.Errorf("querying control work: running work query %q: timed out after 30s: %w", "bd ready", context.DeadlineExceeded)
	fatalErr := errors.New("malformed work query: jq: command not found")
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		if calls == 1 {
			return nil, transientErr
		}
		return nil, fatalErr
	}

	agent := config.Agent{Name: "control-dispatcher"}
	err := runWorkflowServeFollow(agent, t.TempDir(), t.TempDir(), agent.EffectiveWorkQuery(), nil, io.Discard)
	if !errors.Is(err, fatalErr) {
		t.Fatalf("runWorkflowServeFollow err = %v, want fatal error after surviving the transient timeout", err)
	}
	if calls != 2 {
		t.Fatalf("workflowServeList calls = %d, want 2 (survive transient, then exit on fatal)", calls)
	}
}

func TestWorkflowEventRelevantAcceptsBeadLifecycleEvents(t *testing.T) {
	for _, evt := range []events.Event{
		{Type: events.BeadCreated},
		{Type: events.BeadClosed},
		{Type: events.BeadUpdated},
	} {
		if !workflowEventRelevant(evt) {
			t.Fatalf("workflowEventRelevant(%q) = false, want true", evt.Type)
		}
	}
}

func TestWorkflowEventRelevantRejectsNonBeadEvents(t *testing.T) {
	for _, evt := range []events.Event{
		{Type: events.SessionUpdated},
		{Type: events.ControllerStarted},
		{Type: events.CitySuspended},
	} {
		if workflowEventRelevant(evt) {
			t.Fatalf("workflowEventRelevant(%q) = true, want false", evt.Type)
		}
	}
}

func TestDecorateDynamicFragmentRecipeSynthesizesInheritedScopeChecks(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		Agents: []config.Agent{
			{Name: "reviewer", MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)

	source := beads.Bead{
		ID:    "gc-source",
		Title: "Source",
		Metadata: map[string]string{
			"gc.routed_to":     "reviewer",
			"gc.scope_ref":     "body",
			"gc.on_fail":       "abort_scope",
			"gc.step_id":       "review-loop",
			"gc.ralph_step_id": "review-loop",
			"gc.attempt":       "2",
		},
	}
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{
				ID:    "expansion-review.review",
				Title: "Review",
			},
			{
				ID:    "expansion-review.submit",
				Title: "Submit",
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "expansion-review.submit", DependsOnID: "expansion-review.review", Type: "blocks"},
		},
	}

	if err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, "", cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}

	steps := map[string]formula.RecipeStep{}
	for _, step := range fragment.Steps {
		steps[step.ID] = step
	}

	control, ok := steps["expansion-review.review-scope-check"]
	if !ok {
		t.Fatal("missing synthesized review scope-check")
	}
	if control.Metadata["gc.scope_ref"] != "body" {
		t.Fatalf("review scope-check gc.scope_ref = %q, want body", control.Metadata["gc.scope_ref"])
	}
	if control.Assignee != config.ControlDispatcherAgentName {
		t.Fatalf("review scope-check assignee = %q, want %q", control.Assignee, config.ControlDispatcherAgentName)
	}
	if got := control.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("review scope-check gc.routed_to = %q, want empty direct dispatcher assignee", got)
	}
	if control.Metadata[graphExecutionRouteMetaKey] != "reviewer" {
		t.Fatalf("review scope-check execution route = %q, want reviewer", control.Metadata[graphExecutionRouteMetaKey])
	}
	if control.Metadata["gc.attempt"] != "2" || control.Metadata["gc.ralph_step_id"] != "review-loop" || control.Metadata["gc.step_id"] != "review-loop" {
		t.Fatalf("review scope-check trace metadata = %#v, want inherited attempt/step ids", control.Metadata)
	}

	var sawRewritten bool
	for _, dep := range fragment.Deps {
		if dep.StepID == "expansion-review.submit" && dep.DependsOnID == "expansion-review.review-scope-check" && dep.Type == "blocks" {
			sawRewritten = true
			break
		}
	}
	if !sawRewritten {
		t.Fatal("submit dependency was not rewritten to synthesized scope-check")
	}
}

func TestResolveGraphStepBindingWorkflowFinalizeUsesFallback(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "reviewer", MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)

	stepByID := map[string]*formula.RecipeStep{
		"demo.owner": {
			ID:    "demo.owner",
			Title: "Owner step",
			Metadata: map[string]string{
				"gc.run_target": "control-dispatcher",
			},
		},
		"demo.review": {
			ID:    "demo.review",
			Title: "Review",
			Metadata: map[string]string{
				"gc.kind":       "retry-run",
				"gc.run_target": "reviewer",
			},
		},
		"demo.workflow-finalize": {
			ID:    "demo.workflow-finalize",
			Title: "Finalize workflow",
			Metadata: map[string]string{
				"gc.kind": "workflow-finalize",
			},
		},
	}
	depsByStep := map[string][]string{
		"demo.workflow-finalize": {"demo.review"},
	}
	fallback := graphRouteBinding{
		QualifiedName: "mayor",
		SessionName:   lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "mayor", cfg.Workspace.SessionTemplate),
	}

	binding, err := resolveGraphStepBinding("demo.workflow-finalize", stepByID, nil, depsByStep, map[string]graphRouteBinding{}, map[string]bool{}, fallback, "", store, cfg.Workspace.Name, "", cfg)
	if err != nil {
		t.Fatalf("resolveGraphStepBinding(workflow-finalize): %v", err)
	}
	if binding.QualifiedName != "mayor" || binding.SessionName != fallback.SessionName {
		t.Fatalf("binding = %+v, want fallback %+v", binding, fallback)
	}
}

func TestResolveGraphStepBindingCheckRejectsInconsistentDeps(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "reviewer-a"},
			{Name: "reviewer-b"},
		},
	}

	stepByID := map[string]*formula.RecipeStep{
		"demo.review-a": {
			ID:    "demo.review-a",
			Title: "Review A",
			Metadata: map[string]string{
				"gc.run_target": "reviewer-a",
			},
		},
		"demo.review-b": {
			ID:    "demo.review-b",
			Title: "Review B",
			Metadata: map[string]string{
				"gc.run_target": "reviewer-b",
			},
		},
		"demo.check": {
			ID:    "demo.check",
			Title: "Check",
			Metadata: map[string]string{
				"gc.kind": "check",
			},
		},
	}
	depsByStep := map[string][]string{
		"demo.check": {"demo.review-a", "demo.review-b"},
	}
	fallback := graphRouteBinding{
		QualifiedName: "reviewer-a",
		SessionName:   lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "reviewer-a", cfg.Workspace.SessionTemplate),
	}

	if _, err := resolveGraphStepBinding("demo.check", stepByID, nil, depsByStep, map[string]graphRouteBinding{}, map[string]bool{}, fallback, "", store, cfg.Workspace.Name, "", cfg); err == nil || !strings.Contains(err.Error(), "inconsistent control routing") {
		t.Fatalf("resolveGraphStepBinding(check) error = %v, want inconsistent control routing", err)
	}
}

func TestResolveGraphStepBindingRetryEvalUsesDependencyRoute(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		Agents: []config.Agent{
			{Name: "reviewer", MaxActiveSessions: intPtr(1)},
			{Name: "control-dispatcher"},
		},
	}
	config.InjectImplicitAgents(cfg)

	stepByID := map[string]*formula.RecipeStep{
		"demo.owner": {
			ID:    "demo.owner",
			Title: "Owner step",
			Metadata: map[string]string{
				"gc.run_target": "control-dispatcher",
			},
		},
		"demo.review": {
			ID:    "demo.review",
			Title: "Review",
			Metadata: map[string]string{
				"gc.kind":       "retry-run",
				"gc.run_target": "reviewer",
			},
		},
		"demo.review.eval.1": {
			ID:    "demo.review.eval.1",
			Title: "Evaluate review attempt",
			Metadata: map[string]string{
				"gc.kind": "retry-eval",
			},
		},
	}
	depsByStep := map[string][]string{
		"demo.review.eval.1": {"demo.owner", "demo.review"},
	}
	fallback := graphRouteBinding{
		QualifiedName: "control-dispatcher",
		SessionName:   lookupSessionNameOrLegacy(store, cfg.Workspace.Name, "control-dispatcher", cfg.Workspace.SessionTemplate),
	}

	binding, err := resolveGraphStepBinding("demo.review.eval.1", stepByID, nil, depsByStep, map[string]graphRouteBinding{}, map[string]bool{}, fallback, "", store, cfg.Workspace.Name, "", cfg)
	if err != nil {
		t.Fatalf("resolveGraphStepBinding(retry-eval): %v", err)
	}
	if binding.QualifiedName != "reviewer" {
		t.Fatalf("binding.QualifiedName = %q, want reviewer", binding.QualifiedName)
	}
}

func TestRunControlDispatcherRetryEvalRecyclesPooledSession(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "test-city"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeCatalogFile(t, cityPath, "pack.toml", "[pack]\nname = \"test-city\"\nschema = 2\n")
	writeCatalogFile(t, cityPath, "agents/control-dispatcher/agent.toml", "start_command = \"echo hello\"\n")
	t.Setenv("GC_CITY", cityPath)

	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}

	root, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	logical, err := store.Create(beads.Bead{
		Title: "review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "hard_fail",
		},
	})
	if err != nil {
		t.Fatalf("Create(logical): %v", err)
	}
	run1, err := store.Create(beads.Bead{
		Title:    "review attempt 1",
		Type:     "task",
		Assignee: "polecat-2",
		Labels:   []string{"pool:polecat"},
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
			"gc.outcome":         "fail",
			"gc.failure_class":   "transient",
			"gc.failure_reason":  "rate_limited",
		},
	})
	if err != nil {
		t.Fatalf("Create(run1): %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("Close(run1): %v", err)
	}
	eval1, err := store.Create(beads.Bead{
		Title: "review eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	if err != nil {
		t.Fatalf("Create(eval1): %v", err)
	}
	if err := store.DepAdd(logical.ID, eval1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(logical->eval1): %v", err)
	}
	if err := store.DepAdd(eval1.ID, run1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(eval1->run1): %v", err)
	}

	fakeProvider := runtime.NewFake()
	oldProvider := dispatchControlSessionProvider
	dispatchControlSessionProvider = func() runtime.Provider { return fakeProvider }
	t.Cleanup(func() { dispatchControlSessionProvider = oldProvider })

	var stdout bytes.Buffer
	if err := runControlDispatcher(eval1.ID, &stdout, io.Discard); err != nil {
		t.Fatalf("runControlDispatcher(retry-eval): %v", err)
	}

	stopCalls := 0
	for _, call := range fakeProvider.Calls {
		if call.Method == "Stop" && call.Name == "polecat-2" {
			stopCalls++
		}
	}
	if stopCalls != 1 {
		t.Fatalf("Stop(polecat-2) calls = %d, want 1; calls=%+v", stopCalls, fakeProvider.Calls)
	}

	reloadedStore, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(reload): %v", err)
	}
	evalAfter, err := reloadedStore.Get(eval1.ID)
	if err != nil {
		t.Fatalf("Get(eval1): %v", err)
	}
	if evalAfter.Metadata["gc.retry_session_recycled"] != "true" {
		t.Fatalf("eval1 gc.retry_session_recycled = %q, want true", evalAfter.Metadata["gc.retry_session_recycled"])
	}
}

func TestFindBeadAcrossStoresPropagatesCityStoreErrors(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "test-city"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_BEADS", "exec:/definitely/missing/provider")

	_, _, _, err := findBeadAcrossStores(cityPath, "gc-missing", io.Discard)
	if err == nil {
		t.Fatal("findBeadAcrossStores() error = nil, want provider failure")
	}
	if !strings.Contains(err.Error(), "getting bead \"gc-missing\" from "+cityPath) {
		t.Fatalf("findBeadAcrossStores() error = %v, want city store path context", err)
	}
	if strings.Contains(err.Error(), "bead not found") {
		t.Fatalf("findBeadAcrossStores() error = %v, want provider failure instead of masked not-found", err)
	}
}

func TestCmdWorkflowDeleteSourceAllowsStoreSelectorForAmbiguousSourceIDs(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "alpha")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rigDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"

[[rigs]]
name = "alpha"
prefix = "BL"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeCatalogFile(t, cityDir, ".gc/site.toml", "[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })
	if _, err := openStoreAtForCity(cityDir, cityDir); err != nil {
		t.Fatalf("openStoreAtForCity(city init): %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(city): %v", err)
	}
	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city scoped): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(rig .gc): %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(rig): %v", err)
	}
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	citySource, err := cityStore.Create(beads.Bead{Title: "City source", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create(city source): %v", err)
	}
	rigSource, err := rigStore.Create(beads.Bead{Title: "Rig source", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create(rig source): %v", err)
	}
	if citySource.ID != rigSource.ID {
		t.Fatalf("city source id = %q, rig source id = %q, want identical ids for ambiguity test", citySource.ID, rigSource.ID)
	}
	if err := cityStore.SetMetadata(citySource.ID, "workflow_id", "wf-city-stale"); err != nil {
		t.Fatalf("SetMetadata(city workflow_id): %v", err)
	}
	if err := rigStore.SetMetadata(rigSource.ID, "workflow_id", "wf-rig-stale"); err != nil {
		t.Fatalf("SetMetadata(rig workflow_id): %v", err)
	}
	root, err := cityStore.Create(beads.Bead{
		ID:     "wf-city",
		Title:  "Workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":                                "workflow",
			"gc.source_bead_id":                      citySource.ID,
			sourceworkflow.SourceStoreRefMetadataKey: "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	var stdout, stderr bytes.Buffer
	selector := sourceWorkflowStoreSelector{storeRef: "rig:alpha"}
	if code := cmdWorkflowDeleteSource(citySource.ID, selector, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWorkflowDeleteSource returned %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result=cleaned") {
		t.Fatalf("stdout = %q, want cleaned result", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	reloadedCity, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city reload): %v", err)
	}
	updatedRoot, err := reloadedCity.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if updatedRoot.Status != "closed" {
		t.Fatalf("root status = %q, want closed", updatedRoot.Status)
	}
	updatedCitySource, err := reloadedCity.Get(citySource.ID)
	if err != nil {
		t.Fatalf("Get(city source): %v", err)
	}
	if got := updatedCitySource.Metadata["workflow_id"]; got != "wf-city-stale" {
		t.Fatalf("city source workflow_id = %q, want wf-city-stale", got)
	}
	reloadedRig, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig reload): %v", err)
	}
	updatedRigSource, err := reloadedRig.Get(rigSource.ID)
	if err != nil {
		t.Fatalf("Get(rig source): %v", err)
	}
	if got := strings.TrimSpace(updatedRigSource.Metadata["workflow_id"]); got != "" {
		t.Fatalf("rig source workflow_id = %q, want cleared", got)
	}
}

func TestCmdWorkflowDeleteSourceStoreSelectorIgnoresLegacyRootInDifferentStore(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "alpha")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rigDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"

[[rigs]]
name = "alpha"
prefix = "BL"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeCatalogFile(t, cityDir, ".gc/site.toml", "[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(city): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(rig .gc): %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(rig): %v", err)
	}
	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}

	citySource, err := cityStore.Create(beads.Bead{Title: "City source", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create(city source): %v", err)
	}
	rigSource, err := rigStore.Create(beads.Bead{Title: "Rig source", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create(rig source): %v", err)
	}
	if citySource.ID != rigSource.ID {
		t.Fatalf("city source id = %q, rig source id = %q, want identical ids for ambiguity test", citySource.ID, rigSource.ID)
	}
	if err := cityStore.SetMetadata(citySource.ID, "workflow_id", "wf-city-stale"); err != nil {
		t.Fatalf("SetMetadata(city workflow_id): %v", err)
	}
	if err := rigStore.SetMetadata(rigSource.ID, "workflow_id", "wf-rig-stale"); err != nil {
		t.Fatalf("SetMetadata(rig workflow_id): %v", err)
	}
	root, err := cityStore.Create(beads.Bead{
		ID:     "wf-city-legacy",
		Title:  "Legacy city workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": citySource.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	var stdout, stderr bytes.Buffer
	selector := sourceWorkflowStoreSelector{storeRef: "rig:alpha"}
	if code := cmdWorkflowDeleteSource(citySource.ID, selector, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWorkflowDeleteSource returned %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result=already_clean") {
		t.Fatalf("stdout = %q, want already_clean result", stdout.String())
	}
	if !strings.Contains(stdout.String(), "metadata_cleared=true") {
		t.Fatalf("stdout = %q, want metadata_cleared=true", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	reloadedCity, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city reload): %v", err)
	}
	updatedRoot, err := reloadedCity.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if updatedRoot.Status != root.Status {
		t.Fatalf("root status = %q, want unchanged %q", updatedRoot.Status, root.Status)
	}
	updatedCitySource, err := reloadedCity.Get(citySource.ID)
	if err != nil {
		t.Fatalf("Get(city source): %v", err)
	}
	if got := updatedCitySource.Metadata["workflow_id"]; got != "wf-city-stale" {
		t.Fatalf("city source workflow_id = %q, want wf-city-stale", got)
	}

	reloadedRig, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig reload): %v", err)
	}
	updatedRigSource, err := reloadedRig.Get(rigSource.ID)
	if err != nil {
		t.Fatalf("Get(rig source): %v", err)
	}
	if got := strings.TrimSpace(updatedRigSource.Metadata["workflow_id"]); got != "" {
		t.Fatalf("rig source workflow_id = %q, want cleared", got)
	}
}

func TestCmdWorkflowReopenSourceRejectsLiveRootInDifferentStore(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "alpha")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rigDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"

[[rigs]]
name = "alpha"
prefix = "BL"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeCatalogFile(t, cityDir, ".gc/site.toml", "[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(city): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(rig .gc): %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(rig): %v", err)
	}
	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}

	if _, err := rigStore.Create(beads.Bead{Title: "Rig warmup", Type: "task", Status: "closed"}); err != nil {
		t.Fatalf("Create(rig warmup): %v", err)
	}
	rigSource, err := rigStore.Create(beads.Bead{Title: "Rig source", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("Create(rig source): %v", err)
	}
	initialStatus := rigSource.Status
	if err := rigStore.SetMetadata(rigSource.ID, "workflow_id", "wf-stale"); err != nil {
		t.Fatalf("SetMetadata(rig workflow_id): %v", err)
	}
	cityRoot, err := cityStore.Create(beads.Bead{
		ID:     "wf-city",
		Title:  "City workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":                                "workflow",
			"gc.source_bead_id":                      rigSource.ID,
			sourceworkflow.SourceStoreRefMetadataKey: "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(city root): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowReopenSource(rigSource.ID, sourceWorkflowStoreSelector{}, &stdout, &stderr); code != 3 {
		t.Fatalf("cmdWorkflowReopenSource returned %d, want 3; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "blocking_workflow_ids="+cityRoot.ID) {
		t.Fatalf("stderr = %q, want conflict with %s", stderr.String(), cityRoot.ID)
	}

	reloadedRig, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig reload): %v", err)
	}
	updatedSource, err := reloadedRig.Get(rigSource.ID)
	if err != nil {
		t.Fatalf("Get(rig source): %v", err)
	}
	if updatedSource.Status != initialStatus {
		t.Fatalf("rig source status = %q, want unchanged %q", updatedSource.Status, initialStatus)
	}
	if got := strings.TrimSpace(updatedSource.Metadata["workflow_id"]); got != "wf-stale" {
		t.Fatalf("rig source workflow_id = %q, want wf-stale", got)
	}

	reloadedCity, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city reload): %v", err)
	}
	updatedRoot, err := reloadedCity.Get(cityRoot.ID)
	if err != nil {
		t.Fatalf("Get(city root): %v", err)
	}
	if updatedRoot.Status != cityRoot.Status {
		t.Fatalf("city root status = %q, want unchanged %q", updatedRoot.Status, cityRoot.Status)
	}
}

func TestDeleteWorkflowBeadsRemovesDepsBeforeDelete(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "workflow root", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "workflow child", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	grandchild, err := store.Create(beads.Bead{Title: "workflow grandchild", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("Create(grandchild): %v", err)
	}
	if err := store.Close(root.ID); err != nil {
		t.Fatalf("Close(root): %v", err)
	}
	if err := store.Close(child.ID); err != nil {
		t.Fatalf("Close(child): %v", err)
	}
	if err := store.Close(grandchild.ID); err != nil {
		t.Fatalf("Close(grandchild): %v", err)
	}
	if err := store.DepAdd(child.ID, root.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(child->root): %v", err)
	}
	if err := store.DepAdd(grandchild.ID, child.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(grandchild->child): %v", err)
	}

	deleted, errs := deleteWorkflowBeads(store, []string{root.ID, child.ID, grandchild.ID})
	if len(errs) != 0 {
		t.Fatalf("deleteWorkflowBeads errs = %v, want none", errs)
	}
	if deleted != 3 {
		t.Fatalf("deleted = %d, want 3", deleted)
	}
	for _, id := range []string{root.ID, child.ID, grandchild.ID} {
		if _, err := store.Get(id); err == nil {
			t.Fatalf("Get(%s) succeeded after delete", id)
		}
		if down, err := store.DepList(id, "down"); err != nil {
			t.Fatalf("DepList(%s, down): %v", id, err)
		} else if len(down) != 0 {
			t.Fatalf("down deps for %s = %#v, want none", id, down)
		}
		if up, err := store.DepList(id, "up"); err != nil {
			t.Fatalf("DepList(%s, up): %v", id, err)
		} else if len(up) != 0 {
			t.Fatalf("up deps for %s = %#v, want none", id, up)
		}
	}
}

func TestApplySourceWorkflowMatchCleanupDeletesOnlyCollectedWorkflowBeads(t *testing.T) {
	store := beads.NewMemStore()
	first, err := store.Create(beads.Bead{Title: "workflow first", Type: "task"})
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	second, err := store.Create(beads.Bead{Title: "workflow second", Type: "task"})
	if err != nil {
		t.Fatalf("Create(second): %v", err)
	}
	outside, err := store.Create(beads.Bead{Title: "outside follow-up", Type: "task"})
	if err != nil {
		t.Fatalf("Create(outside): %v", err)
	}
	if err := store.DepAdd(first.ID, outside.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(first->outside): %v", err)
	}
	if err := store.DepAdd(outside.ID, second.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(outside->second): %v", err)
	}

	runnerCalled := false
	runner := func(_ string, _ string, _ ...string) ([]byte, error) {
		runnerCalled = true
		return []byte("ok"), nil
	}

	var stderr bytes.Buffer
	closed, deleted, incomplete := applySourceWorkflowMatchCleanup(sourceWorkflowStoreMatch{
		label:  "rig:gascity",
		store:  store,
		beads:  []beads.Bead{first, second},
		path:   "/repo",
		runner: runner,
	}, true, &stderr)
	if incomplete {
		t.Fatalf("cleanup incomplete; stderr=%s", stderr.String())
	}
	if closed != 2 || deleted != 2 {
		t.Fatalf("closed/deleted = %d/%d, want 2/2", closed, deleted)
	}
	if runnerCalled {
		t.Fatal("cleanup used bd cascade runner; want explicit in-process deletion of collected IDs")
	}
	for _, id := range []string{first.ID, second.ID} {
		if _, err := store.Get(id); err == nil {
			t.Fatalf("Get(%s) succeeded after delete", id)
		}
	}
	if got, err := store.Get(outside.ID); err != nil {
		t.Fatalf("Get(outside): %v", err)
	} else if got.Status != "open" {
		t.Fatalf("outside status = %q, want open", got.Status)
	}
	if down, err := store.DepList(outside.ID, "down"); err != nil {
		t.Fatalf("DepList(outside, down): %v", err)
	} else if len(down) != 0 {
		t.Fatalf("outside down deps = %#v, want none after collected bead deletion", down)
	}
	if up, err := store.DepList(outside.ID, "up"); err != nil {
		t.Fatalf("DepList(outside, up): %v", err)
	} else if len(up) != 0 {
		t.Fatalf("outside up deps = %#v, want none after collected bead deletion", up)
	}
}

type failingDeleteStore struct {
	*beads.MemStore
	failID       string
	failRestore  bool
	restoreCalls int
}

func (s *failingDeleteStore) Delete(id string) error {
	if id == s.failID {
		return fmt.Errorf("delete failed")
	}
	return s.MemStore.Delete(id)
}

func (s *failingDeleteStore) DepAdd(issueID, dependsOnID, depType string) error {
	if s.failRestore {
		s.restoreCalls++
		return fmt.Errorf("restore failed")
	}
	return s.MemStore.DepAdd(issueID, dependsOnID, depType)
}

func TestDeleteWorkflowBeadsRestoresDepsOnDeleteFailure(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "workflow root", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := base.Create(beads.Bead{Title: "workflow child", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	if err := base.Close(root.ID); err != nil {
		t.Fatalf("Close(root): %v", err)
	}
	if err := base.Close(child.ID); err != nil {
		t.Fatalf("Close(child): %v", err)
	}
	if err := base.DepAdd(child.ID, root.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(child->root): %v", err)
	}

	store := &failingDeleteStore{MemStore: base, failID: child.ID}
	deleted, errs := deleteWorkflowBeads(store, []string{child.ID})
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want 1 entry", errs)
	}
	if _, err := store.Get(child.ID); err != nil {
		t.Fatalf("Get(child) after failed delete: %v", err)
	}
	if down, err := store.DepList(child.ID, "down"); err != nil {
		t.Fatalf("DepList(child, down): %v", err)
	} else if len(down) != 1 || down[0].DependsOnID != root.ID {
		t.Fatalf("child down deps = %#v, want dependency on %s restored", down, root.ID)
	}
	if up, err := store.DepList(root.ID, "up"); err != nil {
		t.Fatalf("DepList(root, up): %v", err)
	} else if len(up) != 1 || up[0].IssueID != child.ID {
		t.Fatalf("root up deps = %#v, want dependency from %s restored", up, child.ID)
	}
}

func TestDeleteWorkflowBeadsReportsRollbackFailure(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "workflow root", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := base.Create(beads.Bead{Title: "workflow child", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	if err := base.Close(root.ID); err != nil {
		t.Fatalf("Close(root): %v", err)
	}
	if err := base.Close(child.ID); err != nil {
		t.Fatalf("Close(child): %v", err)
	}
	if err := base.DepAdd(child.ID, root.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(child->root): %v", err)
	}

	store := &failingDeleteStore{MemStore: base, failID: child.ID, failRestore: true}
	deleted, errs := deleteWorkflowBeads(store, []string{child.ID})
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want 1 entry", errs)
	}
	if !strings.Contains(errs[0].Error(), "delete failed") {
		t.Fatalf("error = %v, want delete failure", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "rollback failed") {
		t.Fatalf("error = %v, want rollback failure surfaced", errs[0])
	}
	if store.restoreCalls == 0 {
		t.Fatal("expected rollback DepAdd to be attempted")
	}
	if down, err := store.DepList(child.ID, "down"); err != nil {
		t.Fatalf("DepList(child, down): %v", err)
	} else if len(down) != 0 {
		t.Fatalf("child down deps = %#v, want none after failed rollback", down)
	}
}

func TestFollowSleepDurationBacksOffThenCaps(t *testing.T) {
	prevSweep := workflowServeWakeSweepInterval
	prevMax := workflowServeMaxIdleSleep
	workflowServeWakeSweepInterval = 1 * time.Second
	workflowServeMaxIdleSleep = 30 * time.Second
	t.Cleanup(func() {
		workflowServeWakeSweepInterval = prevSweep
		workflowServeMaxIdleSleep = prevMax
	})

	cases := []struct {
		idleSweeps int
		want       time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second},
		{6, 30 * time.Second},
		{20, 30 * time.Second},
	}
	for _, tc := range cases {
		if got := followSleepDuration(tc.idleSweeps); got != tc.want {
			t.Errorf("followSleepDuration(%d) = %v, want %v", tc.idleSweeps, got, tc.want)
		}
	}
}

func TestWaitForRelevantWorkflowWakeReturnsTrueOnRelevantEvent(t *testing.T) {
	eventCh := make(chan workflowWatchResult, 1)
	eventCh <- workflowWatchResult{evt: events.Event{Type: events.BeadCreated, Subject: "gc-1"}}

	eventWake, err := waitForRelevantWorkflowWake(eventCh, time.Second)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !eventWake {
		t.Fatal("eventWake = false, want true when relevant event arrives before timeout")
	}
}

func TestWaitForRelevantWorkflowWakeReturnsFalseOnTimer(t *testing.T) {
	eventCh := make(chan workflowWatchResult) // never receives

	start := time.Now()
	eventWake, err := waitForRelevantWorkflowWake(eventCh, 5*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if eventWake {
		t.Fatal("eventWake = true, want false when no event arrives and timer expires")
	}
	if elapsed < 5*time.Millisecond {
		t.Fatalf("returned after %v, want >= 5ms (timer must actually fire)", elapsed)
	}
}

func TestWaitForRelevantWorkflowWakeFallsThroughIrrelevantEventsToTimer(t *testing.T) {
	eventCh := make(chan workflowWatchResult, 1)
	eventCh <- workflowWatchResult{evt: events.Event{Type: events.SessionUpdated}}

	eventWake, err := waitForRelevantWorkflowWake(eventCh, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if eventWake {
		t.Fatal("eventWake = true, want false (irrelevant event must not wake the loop)")
	}
}

func TestWaitForRelevantWorkflowWakeReturnsWatcherErr(t *testing.T) {
	eventCh := make(chan workflowWatchResult, 1)
	eventCh <- workflowWatchResult{err: os.ErrDeadlineExceeded}

	eventWake, err := waitForRelevantWorkflowWake(eventCh, time.Second)
	if err == nil {
		t.Fatal("wait returned nil err, want watcher err surfaced")
	}
	if eventWake {
		t.Fatal("eventWake = true on error path, want false")
	}
}

func TestWaitForRelevantWorkflowWakeTraceIncludesBackoffState(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "workflow-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", tracePath)

	eventCh := make(chan workflowWatchResult) // never receives

	eventWake, err := waitForRelevantWorkflowWakeWithTrace(eventCh, 5*time.Millisecond, 3)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if eventWake {
		t.Fatal("eventWake = true, want false when timer expires")
	}

	traceBytes, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	trace := string(traceBytes)
	if !strings.Contains(trace, "serve wake-sweep idle_sweeps=3 sleep=5ms") {
		t.Fatalf("trace = %q, want wake-sweep line with idle_sweeps and sleep", trace)
	}
}

func TestWorkflowTracefWarnsOnceWhenTracePathCannotBeOpened(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "missing", "workflow-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", tracePath)

	var stderr bytes.Buffer
	restoreWarnings := useWorkflowTraceWarnings(&stderr)
	defer restoreWarnings()

	workflowTracef("first write")
	workflowTracef("second write")

	got := stderr.String()
	if count := strings.Count(got, "opening workflow trace"); count != 1 {
		t.Fatalf("warning count = %d, want 1; stderr=%q", count, got)
	}
	if !strings.Contains(got, tracePath) {
		t.Fatalf("stderr = %q, want missing trace path %q", got, tracePath)
	}
}

func TestWorkflowTracefFallsBackToSlingTrace(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "workflow-trace.log")
	t.Setenv("GC_SLING_TRACE", tracePath)

	workflowTracef("fallback trace")

	traceBytes, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if !strings.Contains(string(traceBytes), "fallback trace") {
		t.Fatalf("trace = %q, want fallback trace payload", traceBytes)
	}
}

func TestWorkflowTracefUsesRFC3339NanoTimestamp(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "workflow-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", tracePath)

	fixedNow := time.Date(2026, 5, 5, 22, 12, 34, 345678901, time.UTC)
	prevNow := workflowTraceNow
	workflowTraceNow = func() time.Time { return fixedNow }
	defer func() {
		workflowTraceNow = prevNow
	}()

	workflowTracef("precise trace")

	traceBytes, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	line := strings.TrimSpace(string(traceBytes))
	wantPrefix := fixedNow.Format(time.RFC3339Nano) + " "
	if !strings.HasPrefix(line, wantPrefix) {
		t.Fatalf("trace = %q, want prefix %q", line, wantPrefix)
	}
}

func TestWorkflowTraceWarningScopeResetsAcrossTopLevelInstalls(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "missing", "workflow-trace.log")
	var stderr bytes.Buffer

	restoreOne := useWorkflowTraceWarnings(&stderr)
	workflowTraceWarnOpenFailure(badPath, os.ErrNotExist)
	restoreOne()

	restoreTwo := useWorkflowTraceWarnings(&stderr)
	workflowTraceWarnOpenFailure(badPath, os.ErrNotExist)
	restoreTwo()

	if count := strings.Count(stderr.String(), "opening workflow trace"); count != 2 {
		t.Fatalf("warning count = %d, want 2 across separate top-level installs; stderr=%q", count, stderr.String())
	}
}

func TestWorkflowTraceWarningRestoreSupportsOutOfOrderRelease(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "missing", "workflow-trace.log")
	var outer bytes.Buffer
	var inner bytes.Buffer
	var fresh bytes.Buffer

	restoreOuter := useWorkflowTraceWarnings(&outer)
	restoreInner := useWorkflowTraceWarnings(&inner)

	restoreOuter()
	workflowTraceWarnOpenFailure(badPath, os.ErrNotExist)
	restoreInner()

	if outer.Len() != 0 {
		t.Fatalf("outer stderr = %q, want no warning after out-of-order outer restore", outer.String())
	}
	if count := strings.Count(inner.String(), "opening workflow trace"); count != 1 {
		t.Fatalf("inner warning count = %d, want 1 after out-of-order outer restore; stderr=%q", count, inner.String())
	}

	restoreFresh := useWorkflowTraceWarnings(&fresh)
	workflowTraceWarnOpenFailure(badPath, os.ErrNotExist)
	restoreFresh()
	if count := strings.Count(fresh.String(), "opening workflow trace"); count != 1 {
		t.Fatalf("fresh warning count = %d, want 1 after scopes reset; stderr=%q", count, fresh.String())
	}
}

func TestWorkflowTraceWarnfDedupsMatchingInactiveScopeWriter(t *testing.T) {
	var outer bytes.Buffer
	var inner bytes.Buffer

	restoreOuter := useWorkflowTraceWarnings(&outer)
	defer restoreOuter()
	restoreInner := useWorkflowTraceWarnings(&inner)
	defer restoreInner()

	workflowTraceWarnf(&outer, "duplicate", "outer warning\n")
	workflowTraceWarnf(&outer, "duplicate", "outer warning\n")

	if count := strings.Count(outer.String(), "outer warning"); count != 1 {
		t.Fatalf("outer warning count = %d, want 1; stderr=%q", count, outer.String())
	}
	if inner.Len() != 0 {
		t.Fatalf("inner stderr = %q, want no warning for outer-scope writer", inner.String())
	}
}

func TestFollowSleepDurationHandlesPathologicalInputs(t *testing.T) {
	prevSweep := workflowServeWakeSweepInterval
	prevMax := workflowServeMaxIdleSleep
	workflowServeWakeSweepInterval = 1 * time.Second
	workflowServeMaxIdleSleep = 30 * time.Second
	t.Cleanup(func() {
		workflowServeWakeSweepInterval = prevSweep
		workflowServeMaxIdleSleep = prevMax
	})

	if got := followSleepDuration(1000); got != 30*time.Second {
		t.Errorf("followSleepDuration(1000) = %v, want 30s (cap)", got)
	}
	if got := followSleepDuration(63); got != 30*time.Second {
		t.Errorf("followSleepDuration(63) = %v, want 30s (overflow-safe cap)", got)
	}
	if got := followSleepDuration(-1); got != 1*time.Second {
		t.Errorf("followSleepDuration(-1) = %v, want base 1s", got)
	}
}
