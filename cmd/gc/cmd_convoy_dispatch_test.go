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

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/storeref"
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

type sourceWorkflowScanFailStore struct {
	beads.Store
	err error
}

func (s sourceWorkflowScanFailStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.err
}

type sourceWorkflowDescendantScanFailStore struct {
	beads.Store
	err error
}

func (s sourceWorkflowDescendantScanFailStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Metadata[beadmeta.RootBeadIDMetadataKey] != "" {
		return nil, s.err
	}
	return s.Store.List(query)
}

func TestCollectSourceWorkflowMatchesSkipsNonSourceListFailure(t *testing.T) {
	cityPath := "/city"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "healthy", Path: "rigs/healthy"},
			{Name: "stale", Path: "rigs/stale"},
		},
	}
	cityStore := beads.NewMemStore()
	healthyStore := beads.NewMemStore()
	root, err := healthyStore.Create(beads.Bead{
		ID:     "wf-existing",
		Title:  "existing workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:           beadmeta.KindWorkflow,
			beadmeta.SourceBeadIDMetadataKey:   "mc-source",
			beadmeta.SourceStoreRefMetadataKey: "city:test",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	staleErr := errors.New("issues.revision is missing")
	stores := []convoyStoreView{
		{path: cityPath, store: cityStore},
		{path: filepath.Join(cityPath, "rigs/stale"), store: sourceWorkflowScanFailStore{Store: beads.NewMemStore(), err: staleErr}},
		{path: filepath.Join(cityPath, "rigs/healthy"), store: healthyStore},
	}

	matches, skips, err := collectSourceWorkflowMatchesFromStores(cfg, cityPath, "mc-source", "city:test", stores, nil)
	if err != nil {
		t.Fatalf("collectSourceWorkflowMatchesFromStores: %v", err)
	}
	if len(matches) != 1 || len(matches[0].roots) != 1 || matches[0].roots[0].ID != root.ID {
		t.Fatalf("matches = %#v, want healthy root %s", matches, root.ID)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].path, "rigs/stale") || !errors.Is(skips[0].err, staleErr) {
		t.Fatalf("skips = %#v, want stale rig list failure", skips)
	}
	if warning := formatSourceWorkflowStoreSkips(skips); !strings.Contains(warning, "revision") || !strings.Contains(warning, "invisible") {
		t.Fatalf("warning = %q, want scan failure and degraded-coverage context", warning)
	}
}

func TestCollectSourceWorkflowMatchesSurfacesDescendantScanFailure(t *testing.T) {
	cityPath := "/city"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs:      []config.Rig{{Name: "stale", Path: "rigs/stale"}},
	}
	staleBacking := beads.NewMemStore()
	root, err := staleBacking.Create(beads.Bead{
		ID:     "wf-stale",
		Title:  "stale workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:           beadmeta.KindWorkflow,
			beadmeta.SourceBeadIDMetadataKey:   "mc-source",
			beadmeta.SourceStoreRefMetadataKey: "city:test",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	descendantErr := errors.New("descendant query failed")
	stores := []convoyStoreView{
		{path: cityPath, store: beads.NewMemStore()},
		{path: filepath.Join(cityPath, "rigs/stale"), store: sourceWorkflowDescendantScanFailStore{Store: staleBacking, err: descendantErr}},
	}

	matches, skips, err := collectSourceWorkflowMatchesFromStores(cfg, cityPath, "mc-source", "city:test", stores, nil)
	if err != nil {
		t.Fatalf("collectSourceWorkflowMatchesFromStores: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("matches = %#v, want incomplete store %s excluded", matches, root.ID)
	}
	if len(skips) != 1 || !errors.Is(skips[0].err, descendantErr) {
		t.Fatalf("skips = %#v, want descendant query failure", skips)
	}
}

func TestCollectSourceWorkflowMatchesKeepsSelectedStoreListFailureStrict(t *testing.T) {
	cityPath := "/city"
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	selectedErr := errors.New("selected store read failed")
	stores := []convoyStoreView{
		{path: cityPath, store: sourceWorkflowScanFailStore{Store: beads.NewMemStore(), err: selectedErr}},
		{path: filepath.Join(cityPath, "rigs/healthy"), store: beads.NewMemStore()},
	}

	_, skips, err := collectSourceWorkflowMatchesFromStores(cfg, cityPath, "mc-source", "city:test", stores, nil)
	if !errors.Is(err, selectedErr) {
		t.Fatalf("collectSourceWorkflowMatchesFromStores error = %v, want selected store error %v", err, selectedErr)
	}
	if len(skips) != 1 || !errors.Is(skips[0].err, selectedErr) {
		t.Fatalf("skips = %#v, want selected store failure recorded", skips)
	}
}

func TestCollectSourceWorkflowMatchesFailsWhenSelectedStoreIsMissing(t *testing.T) {
	cityPath := "/city"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs:      []config.Rig{{Name: "healthy", Path: "rigs/healthy"}},
	}
	stores := []convoyStoreView{
		{path: filepath.Join(cityPath, "rigs/healthy"), store: beads.NewMemStore()},
	}
	selectedErr := errors.New("selected store reopen failed")
	skips := []sourceWorkflowStoreSkip{{path: cityPath, err: selectedErr}}

	_, _, err := collectSourceWorkflowMatchesFromStores(cfg, cityPath, "mc-source", "city:test", stores, skips)
	if err == nil || !strings.Contains(err.Error(), "city:test") {
		t.Fatalf("collectSourceWorkflowMatchesFromStores error = %v, want missing selected-store failure", err)
	}
	if !errors.Is(err, selectedErr) {
		t.Fatalf("collectSourceWorkflowMatchesFromStores error = %v, want wrapped selected open error %v", err, selectedErr)
	}
}

func TestUnscannedSourceWorkflowStoreSkipsExcludesRecoveredSelectedStore(t *testing.T) {
	cityPath := "/city"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs:      []config.Rig{{Name: "stale", Path: "rigs/stale"}},
	}
	skips := []sourceWorkflowStoreSkip{
		{path: cityPath, err: errors.New("selected reopen failed")},
		{path: filepath.Join(cityPath, "rigs/stale"), err: errors.New("stale rig failed")},
	}

	unscanned, selectedRecovered := unscannedSourceWorkflowStoreSkips(cfg, cityPath, "city:test", skips)
	if !selectedRecovered {
		t.Fatal("selectedRecovered = false, want already-open selected store to repair its reopen skip")
	}
	if len(unscanned) != 1 || !strings.Contains(unscanned[0].path, "rigs/stale") {
		t.Fatalf("unscanned skips = %#v, want only stale non-selected rig", unscanned)
	}
}

func TestCollectSourceWorkflowMatchesFailsWhenNoStoreCanBeScanned(t *testing.T) {
	cityPath := "/city"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "stale-a", Path: "rigs/stale-a"},
			{Name: "stale-b", Path: "rigs/stale-b"},
		},
	}
	firstErr := errors.New("first store failed")
	stores := []convoyStoreView{
		{path: filepath.Join(cityPath, "rigs/stale-a"), store: sourceWorkflowScanFailStore{Store: beads.NewMemStore(), err: firstErr}},
		{path: filepath.Join(cityPath, "rigs/stale-b"), store: sourceWorkflowScanFailStore{Store: beads.NewMemStore(), err: errors.New("second store failed")}},
	}

	_, skips, err := collectSourceWorkflowMatchesFromStores(cfg, cityPath, "mc-source", "", stores, nil)
	if !errors.Is(err, firstErr) {
		t.Fatalf("collectSourceWorkflowMatchesFromStores error = %v, want first scan error %v", err, firstErr)
	}
	if len(skips) != 2 {
		t.Fatalf("len(skips) = %d, want both failed stores recorded: %#v", len(skips), skips)
	}
}

func TestCollectSourceWorkflowMatchesFailsWhenNoStoreIsAvailable(t *testing.T) {
	_, _, err := collectSourceWorkflowMatchesFromStores(
		&config.City{Workspace: config.Workspace{Name: "test"}},
		"/city",
		"mc-source",
		"",
		[]convoyStoreView{{path: "/city/rigs/nil"}},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "no source workflow stores") {
		t.Fatalf("collectSourceWorkflowMatchesFromStores error = %v, want no-usable-store failure", err)
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
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "reviewer", MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend", "myrig")

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
	if control.Assignee != "" {
		t.Fatalf("review scope-check assignee = %q, want empty routed control-dispatcher queue", control.Assignee)
	}
	if got := control.Metadata["gc.routed_to"]; got != config.ControlDispatcherAgentName {
		t.Fatalf("review scope-check gc.routed_to = %q, want %q", got, config.ControlDispatcherAgentName)
	}
	if control.Metadata[graphroute.GraphExecutionRouteMetaKey] != "reviewer" {
		t.Fatalf("review scope-check execution route = %q, want reviewer", control.Metadata[graphroute.GraphExecutionRouteMetaKey])
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
			"gc.routed_to":                        config.ControlDispatcherAgentName,
			graphroute.GraphExecutionRouteMetaKey: "frontend/reviewer",
		},
	})
	if len(control) != 2 || control[1] != "/rig/frontend/formulas" {
		t.Fatalf("workflowFormulaSearchPaths(control frontend) = %#v, want rig-specific layers", control)
	}

	directControl := workflowFormulaSearchPaths(cfg, beads.Bead{
		Metadata: map[string]string{
			"gc.routed_to":                             config.ControlDispatcherAgentName,
			graphroute.GraphExecutionRouteMetaKey:      "session-123",
			graphroute.GraphExecutionRigContextMetaKey: "frontend",
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
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend", "myrig")
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
			graphroute.GraphExecutionRouteMetaKey:      direct.ID,
			graphroute.GraphExecutionRigContextMetaKey: "frontend",
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
	if got := check.Metadata[graphroute.GraphExecutionRouteMetaKey]; got != direct.ID {
		t.Fatalf("item.check execution route = %q, want direct session %s", got, direct.ID)
	}
	if got := check.Metadata[graphroute.GraphExecutionRigContextMetaKey]; got != "frontend" {
		t.Fatalf("item.check execution rig context = %q, want frontend", got)
	}
}

func TestDecorateDrainItemRecipeDoesNotFallbackToControllerAssignee(t *testing.T) {
	store := beads.NewMemStore()
	maxSessions := 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Agents: []config.Agent{{
			Name:              "worker",
			MaxActiveSessions: &maxSessions,
		}},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend", "myrig")
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

	found, err := findWorkflowBeads(store, root.ID)
	if err != nil {
		t.Fatalf("findWorkflowBeads(...): %v", err)
	}
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

	found, err := findWorkflowBeads(store, "wf-delete-logical")
	if err != nil {
		t.Fatalf("findWorkflowBeads(logical): %v", err)
	}
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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

[daemon]
formula_v2 = true

[[rigs]]
name = "alpha"
prefix = "BL"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core")
	writeCatalogFile(t, cityDir, ".gc/site.toml", "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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

func TestCmdWorkflowReopenSourcePreservesRouteWithoutRunTarget(t *testing.T) {
	// ga-20zd: when gc.run_target is absent, reopen-source must fall back to
	// the route the bead already carries instead of blanking it. Blanking made
	// the reopen destructive and order-dependent: the refinery's rejection path
	// writes the pool route with `gc bd update` and calls reopen-source as a
	// separate command, so a reopen that landed after the metadata write
	// silently erased the route. The bead then looked correctly re-pooled
	// (rejection_reason set, branch intact) but was invisible to pool-demand
	// dispatch, which filters on gc.routed_to.
	//
	// Preserving is safe for the caller's follow-up re-sling: a re-sling to a
	// different target overwrites the route, and a re-sling to the same target
	// hits resolveConvoyRecovery, which detects the just-deleted workflow and
	// re-runs finalize rather than short-circuiting as idempotent.
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
	if err := store.SetMetadata(source.ID, "gc.routed_to", "myrig/voxist.executor"); err != nil {
		t.Fatalf("SetMetadata(gc.routed_to): %v", err)
	}
	if err := store.SetMetadata(source.ID, "gc.session_affinity", "require"); err != nil {
		t.Fatalf("SetMetadata(gc.session_affinity): %v", err)
	}
	if err := store.SetMetadata(source.ID, "gc.continuation_group", "main"); err != nil {
		t.Fatalf("SetMetadata(gc.continuation_group): %v", err)
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
	const wantRoute = "myrig/voxist.executor"
	if got := strings.TrimSpace(updated.Metadata["gc.routed_to"]); got != wantRoute {
		t.Fatalf("gc.routed_to = %q, want %q preserved (no gc.run_target → keep existing route)", got, wantRoute)
	}
	if got := strings.TrimSpace(updated.Metadata["gc.session_affinity"]); got != "" {
		t.Fatalf("gc.session_affinity = %q, want cleared with unassigned reopen", got)
	}
	if got := strings.TrimSpace(updated.Metadata["gc.continuation_group"]); got != "" {
		t.Fatalf("gc.continuation_group = %q, want cleared with unassigned reopen", got)
	}
	if updated.Status != "open" {
		t.Fatalf("status = %q, want open", updated.Status)
	}
	if updated.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", updated.Assignee)
	}
}

func TestCmdWorkflowReopenSourceLeavesRouteBlankWhenNoRouteAvailable(t *testing.T) {
	// ga-20zd: preserving an existing route must not invent one. A bead
	// carrying neither gc.run_target nor gc.routed_to still reopens blank —
	// the pre-existing behavior for that class is unchanged.
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
	if err := store.SetMetadata(source.ID, "workflow_id", "wf-gone"); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowReopenSource(source.ID, sourceWorkflowStoreSelector{}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWorkflowReopenSource returned %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	reloaded, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(reload): %v", err)
	}
	updated, err := reloaded.Get(source.ID)
	if err != nil {
		t.Fatalf("Get(source): %v", err)
	}
	if got := strings.TrimSpace(updated.Metadata["gc.routed_to"]); got != "" {
		t.Fatalf("gc.routed_to = %q, want blank (no run_target, no prior route)", got)
	}
	if updated.Status != "open" {
		t.Fatalf("status = %q, want open", updated.Status)
	}
	if updated.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", updated.Assignee)
	}
}

func TestCmdWorkflowReopenSourcePreRoutesToRunTarget(t *testing.T) {
	// FR-C0.1 (vp-nq8): when gc.run_target is set, reopen-source must write
	// gc.routed_to = gc.run_target atomically with the status/assignee reset.
	// This eliminates the orphan window where a blank gc.routed_to is
	// invisible to route-reclaim (which skips blank routes) and causes
	// unrouted-feeder to mis-route to the rig planner.
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
	if err := store.SetMetadata(source.ID, "gc.kind", "workflow"); err != nil {
		t.Fatalf("SetMetadata(gc.kind): %v", err)
	}
	if err := store.SetMetadata(source.ID, "workflow_id", "wf-old"); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}
	if err := store.SetMetadata(source.ID, "gc.run_target", "myrig/voxist.reviewer"); err != nil {
		t.Fatalf("SetMetadata(gc.run_target): %v", err)
	}
	if err := store.SetMetadata(source.ID, "gc.routed_to", "myrig/voxist.executor"); err != nil {
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
	const wantRoute = "myrig/voxist.reviewer"
	if got := strings.TrimSpace(updated.Metadata["gc.routed_to"]); got != wantRoute {
		t.Fatalf("gc.routed_to = %q, want %q (pre-routed to gc.run_target)", got, wantRoute)
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
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Rigs:      []config.Rig{{Name: "frontend", Path: "frontend"}},
		Agents: []config.Agent{
			{Name: "reviewer", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)},
		},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend", "myrig")

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
	if control.Assignee != "" {
		t.Fatalf("control assignee = %q, want empty routed control-dispatcher queue", control.Assignee)
	}
	if got := control.Metadata["gc.routed_to"]; got != "frontend/control-dispatcher" {
		t.Fatalf("control gc.routed_to = %q, want frontend/control-dispatcher", got)
	}
	if control.Metadata[graphroute.GraphExecutionRouteMetaKey] != "frontend/reviewer" {
		t.Fatalf("control execution route = %q, want frontend/reviewer", control.Metadata[graphroute.GraphExecutionRouteMetaKey])
	}
}

func TestDecorateDynamicFragmentRecipeControlRouteUsesOwningStoreScope(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Rigs:      []config.Rig{{Name: "frontend", Path: "frontend"}},
		Agents:    []config.Agent{{Name: "reviewer", Scope: "city", MaxActiveSessions: intPtr(1)}},
	}
	addTestControlDispatcherAgents(cfg, "", "frontend")
	source := beads.Bead{
		ID: "gc-source",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:     "reviewer",
			beadmeta.RootStoreRefMetadataKey: "rig:frontend",
		},
	}
	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{ID: "expansion-review.review", Title: "Review"},
			{ID: "expansion-review.check", Title: "Check", Metadata: map[string]string{
				beadmeta.KindMetadataKey:         beadmeta.KindCheck,
				beadmeta.RootStoreRefMetadataKey: "rig:stale",
			}},
		},
		Deps: []formula.RecipeDep{{
			StepID: "expansion-review.check", DependsOnID: "expansion-review.review", Type: "blocks",
		}},
	}

	if err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, "", cfg); err != nil {
		t.Fatalf("decorateDynamicFragmentRecipe: %v", err)
	}
	check := fragment.Steps[1]
	if got := check.Metadata[beadmeta.RoutedToMetadataKey]; got != "frontend/control-dispatcher" {
		t.Fatalf("check gc.routed_to = %q, want owning-store route frontend/control-dispatcher", got)
	}
	if got := check.Metadata[graphroute.GraphExecutionRouteMetaKey]; got != "reviewer" {
		t.Fatalf("check gc.execution_routed_to = %q, want reviewer", got)
	}
	if got := check.Metadata[beadmeta.RootStoreRefMetadataKey]; got != "rig:frontend" {
		t.Fatalf("check gc.root_store_ref = %q, want authoritative source store rig:frontend", got)
	}
}

func TestPropagateDynamicScopeMetadataClassifiesEveryControlKind(t *testing.T) {
	source := beads.Bead{
		ID: "gc-source",
		Metadata: map[string]string{
			beadmeta.ScopeRefMetadataKey: "body",
		},
	}
	for _, kind := range beadmeta.ControlKinds {
		t.Run(kind, func(t *testing.T) {
			step := &formula.RecipeStep{
				ID: "frag.step",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey: kind,
				},
			}
			propagateDynamicScopeMetadata(step, source)
			if got := step.Metadata[beadmeta.ScopeRefMetadataKey]; got != "body" {
				t.Fatalf("kind %q: gc.scope_ref = %q, want body", kind, got)
			}
			if got := step.Metadata[beadmeta.ScopeRoleMetadataKey]; got != beadmeta.ScopeRoleControl {
				t.Fatalf("kind %q: gc.scope_role = %q, want %q", kind, got, beadmeta.ScopeRoleControl)
			}
		})
	}
}

func TestPropagateDynamicScopeMetadataNonControlRoles(t *testing.T) {
	source := beads.Bead{
		ID: "gc-source",
		Metadata: map[string]string{
			beadmeta.ScopeRefMetadataKey: "body",
		},
	}

	t.Run("plain work defaults to member", func(t *testing.T) {
		step := &formula.RecipeStep{ID: "frag.step"}
		propagateDynamicScopeMetadata(step, source)
		if got := step.Metadata[beadmeta.ScopeRoleMetadataKey]; got != beadmeta.ScopeRoleMember {
			t.Fatalf("gc.scope_role = %q, want %q", got, beadmeta.ScopeRoleMember)
		}
	})

	t.Run("scope kind gets no role", func(t *testing.T) {
		step := &formula.RecipeStep{
			ID: "frag.step",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey: beadmeta.KindScope,
			},
		}
		propagateDynamicScopeMetadata(step, source)
		if got := step.Metadata[beadmeta.ScopeRoleMetadataKey]; got != "" {
			t.Fatalf("gc.scope_role = %q, want empty for scope kind", got)
		}
	})

	t.Run("explicit role is preserved", func(t *testing.T) {
		step := &formula.RecipeStep{
			ID: "frag.step",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:      beadmeta.KindDrain,
				beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
			},
		}
		propagateDynamicScopeMetadata(step, source)
		if got := step.Metadata[beadmeta.ScopeRoleMetadataKey]; got != beadmeta.ScopeRoleTeardown {
			t.Fatalf("gc.scope_role = %q, want preserved %q", got, beadmeta.ScopeRoleTeardown)
		}
	})

	t.Run("no scope_ref means no role", func(t *testing.T) {
		step := &formula.RecipeStep{
			ID: "frag.step",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey: beadmeta.KindDrain,
			},
		}
		propagateDynamicScopeMetadata(step, beads.Bead{ID: "gc-source"})
		if got := step.Metadata[beadmeta.ScopeRoleMetadataKey]; got != "" {
			t.Fatalf("gc.scope_role = %q, want empty without scope_ref", got)
		}
	})
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
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Rigs:      []config.Rig{{Name: "frontend", Path: "frontend"}},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend", "myrig")
	source := beads.Bead{
		ID:    "gc-source",
		Title: "Source",
		Metadata: map[string]string{
			graphroute.GraphExecutionRouteMetaKey:      direct.ID,
			graphroute.GraphExecutionRigContextMetaKey: "frontend",
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
	if check.Assignee != "" {
		t.Fatalf("check assignee = %q, want empty routed control-dispatcher queue", check.Assignee)
	}
	if got := check.Metadata["gc.routed_to"]; got != "frontend/control-dispatcher" {
		t.Fatalf("check gc.routed_to = %q, want frontend/control-dispatcher", got)
	}
	if got := check.Metadata[graphroute.GraphExecutionRouteMetaKey]; got != direct.ID {
		t.Fatalf("check execution route = %q, want direct session %s", got, direct.ID)
	}
	if got := check.Metadata[graphroute.GraphExecutionRigContextMetaKey]; got != "frontend" {
		t.Fatalf("check execution rig context = %q, want frontend", got)
	}
}

func TestDecorateDynamicFragmentRecipeUsesSourceRouteRigContextForBareTargets(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Agents: []config.Agent{
			{Name: "reviewer", Dir: "frontend", MaxActiveSessions: intPtr(1)},
			{Name: "reviewer", Dir: "backend", MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend", "myrig")

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
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Agents: []config.Agent{
			{Name: "reviewer", Dir: "frontend", MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend", "myrig")

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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	skipSlowCmdGCTest(t, "starts real Dolt lifecycle")
	clearInheritedBeadsEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	cleanupManagedDoltTestCity(t, cityDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	err = runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, control.ID, cfg, io.Discard, &stderr)
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

func TestRunControlDispatcherReprojectsCurrentExecutionFactsAfterControl(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city config: %v", err)
	}
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "expand.formula.toml"), []byte(`
formula = "expand"
type = "expansion"
version = 2
contract = "graph.v2"

[vars.reviewer]
required = true

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
`), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}
	store := beads.NewMemStore()
	root, source, control := createFanoutControl(t, store)
	before, err := store.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}, 0, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("list workflow beads before fanout: %v", err)
	}
	for _, workflowBead := range before {
		if workflowBead.Metadata[beadmeta.StepIDMetadataKey] != "" {
			t.Fatalf("pre-control workflow bead %s already has a step id", workflowBead.ID)
		}
	}

	var stderr bytes.Buffer
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{City: []string{formulaDir}},
	}
	if err := runControlDispatcherWithStoreAndConfig(cityPath, cityPath, store, control.ID, cfg, io.Discard, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStoreAndConfig: %v", err)
	}

	after, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if after.Metadata[beadmeta.FanoutStateMetadataKey] != beadmeta.SpawnStateSpawned {
		t.Fatalf("fanout state = %q, want spawned", after.Metadata[beadmeta.FanoutStateMetadataKey])
	}
	recorded, err := events.ReadAll(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("read execution events: %v", err)
	}
	childIDs := map[string]struct{}{}
	workflowBeads, err := store.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}, 0, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("list workflow beads: %v", err)
	}
	for _, workflowBead := range workflowBeads {
		if workflowBead.ID != source.ID && workflowBead.Metadata[beadmeta.StepIDMetadataKey] != "" {
			childIDs[workflowBead.ID] = struct{}{}
		}
	}
	if len(childIDs) == 0 {
		t.Fatal("fanout did not create a graph step")
	}
	if len(recorded) == 0 {
		t.Fatal("no execution facts recorded after fanout")
	}
	foundNewStep := false
	for _, event := range recorded {
		if event.Type == events.ExecutionStepDefined && event.RunID == root.ID {
			if _, ok := childIDs[event.Subject]; ok {
				foundNewStep = true
			}
		}
	}
	if !foundNewStep {
		t.Fatalf("execution events = %#v, want a fact for post-control graph steps %v", recorded, childIDs)
	}
}

func TestRunControlDispatcherPreservesSuccessfulControlWhenReprojectionFails(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	_, _, control := createProcessedScopeCheckControl(t, store, false)

	var stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, cityPath, store, control.ID, &config.City{Workspace: config.Workspace{Name: "test-city"}}, io.Discard, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStoreAndConfig: %v", err)
	}

	after, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("control status = %q, want closed despite projection failure", after.Status)
	}
	if !strings.Contains(stderr.String(), "projecting execution facts") {
		t.Fatalf("stderr = %q, want observable projection failure", stderr.String())
	}
}

func createProcessedScopeCheckControl(t *testing.T, store beads.Store, graphV2 bool) (beads.Bead, beads.Bead, beads.Bead) {
	t.Helper()
	rootMetadata := map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}
	if graphV2 {
		rootMetadata[beadmeta.FormulaContractMetadataKey] = beadmeta.FormulaContractGraphV2
	}
	root, err := store.Create(beads.Bead{Title: "workflow", Type: "task", Metadata: rootMetadata})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	body, err := store.Create(beads.Bead{Title: "scope body", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindScope,
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.ScopeRefMetadataKey:   "scope",
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleBody,
	}})
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	subject, err := store.Create(beads.Bead{Title: "subject", Type: "task", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.ScopeRefMetadataKey:   "scope",
		beadmeta.ScopeRoleMetadataKey:  "member",
		beadmeta.StepIDMetadataKey:     "workflow.subject",
	}})
	if err != nil {
		t.Fatalf("create subject: %v", err)
	}
	if err := store.Close(subject.ID); err != nil {
		t.Fatalf("close subject: %v", err)
	}
	control, err := store.Create(beads.Bead{Title: "scope check", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindScopeCheck,
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.ScopeRefMetadataKey:   "scope",
		beadmeta.ScopeRoleMetadataKey:  "control",
	}})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	if err := store.DepAdd(control.ID, subject.ID, "blocks"); err != nil {
		t.Fatalf("add control dependency: %v", err)
	}
	if err := store.DepAdd(body.ID, control.ID, "blocks"); err != nil {
		t.Fatalf("add body dependency: %v", err)
	}
	return root, subject, control
}

func createFanoutControl(t *testing.T, store beads.Store) (beads.Bead, beads.Bead, beads.Bead) {
	t.Helper()
	root, err := store.Create(beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	source, err := store.Create(beads.Bead{Title: "prepare items", Type: "task", Status: "closed", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.StepRefMetadataKey:    "source",
		beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
		beadmeta.OutputJSONMetadataKey: `{"items":[{"name":"reviewer"}]}`,
	}})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	control, err := store.Create(beads.Bead{Title: "fan out items", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:       beadmeta.KindFanout,
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.ControlForMetadataKey: "source",
		beadmeta.ForEachMetadataKey:    "output.items",
		beadmeta.BondMetadataKey:       "expand",
		beadmeta.BondVarsMetadataKey:   `{"reviewer":"{item.name}"}`,
		beadmeta.FanoutModeMetadataKey: "parallel",
	}})
	if err != nil {
		t.Fatalf("create fanout: %v", err)
	}
	if err := store.DepAdd(control.ID, source.ID, "blocks"); err != nil {
		t.Fatalf("add fanout dependency: %v", err)
	}
	return root, source, control
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
	if err := runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, control.ID, cfg, io.Discard, &stderr); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n\n[[rigs]]\nname = \"alpha\"\n"+testControlDispatcherAgentTOML("")+testControlDispatcherAgentTOML("alpha")), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeCatalogFile(t, cityDir, ".gc/site.toml", "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n\n[[rigs]]\nname = \"alpha\"\n"+testControlDispatcherAgentTOML("")+testControlDispatcherAgentTOML("alpha")), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeCatalogFile(t, cityDir, ".gc/site.toml", "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := runControlDispatcherWithStore(cityDir, cityDir, store, check1.ID, &stdout, &stderr); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := runControlDispatcherWithStore(cityDir, cityDir, store, check1.ID, &stdout, &stderr); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
		return runControlDispatcherWithStore(cityPath, storePath, store, beadID, stdout, stderr)
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
		return runControlDispatcherWithStore(cityPath, storePath, store, beadID, stdout, stderr)
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
		`bd --readonly --sandbox ready --assignee="$cand" --exclude-type=epic --json --limit=20`,
		`bd --readonly --sandbox ready --metadata-field "gc.run_target=$route" --unassigned --exclude-type=epic --exclude-label "hold:mayor" --exclude-label "hold:external" --json --sort oldest --limit=20`,
		`bd --readonly --sandbox ready --metadata-field "gc.routed_to=$route" --unassigned --exclude-type=epic --exclude-label "hold:mayor" --exclude-label "hold:external" --json --sort oldest --limit=20`,
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
	if strings.Contains(query, "--include-ephemeral") {
		t.Fatalf("workflowServeControlReadyQuery default must stay bd 1.0.4-compatible: %q", query)
	}
}

// TestWorkflowServeControlReadyQueryPassesThroughAmbientDoltPort guards
// against gc-74rxa: the ready-query subprocess env is otherwise rebuilt via
// mergeRuntimeEnv/controllerWorkQueryEnv, which can transiently resolve
// without a Dolt port and silently drop GC_DOLT_PORT/BEADS_DOLT_SERVER_PORT,
// causing `bd --sandbox` to fall back to port 0. The dispatcher process's own
// environment already carries the correct connection coordinates it was
// spawned with, so the query string must carry them through explicitly.
func TestWorkflowServeControlReadyQueryPassesThroughAmbientDoltPort(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "127.0.0.1")
	t.Setenv("GC_DOLT_PORT", "29620")
	unsetTestEnv(t, "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT")

	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName})

	for _, want := range []string{
		"GC_DOLT_HOST='127.0.0.1'",
		"BEADS_DOLT_SERVER_HOST='127.0.0.1'",
		"GC_DOLT_PORT='29620'",
		"BEADS_DOLT_SERVER_PORT='29620'",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("workflowServeControlReadyQuery missing %q in %q", want, query)
		}
	}
}

// TestWorkflowServeControlReadyQueryOmitsDoltEnvWhenAmbientUnset ensures the
// query stays clean (no bare "KEY=" assignments) when the current process has
// no Dolt connection env at all (e.g. a doltlite-backed scope).
func TestWorkflowServeControlReadyQueryOmitsDoltEnvWhenAmbientUnset(t *testing.T) {
	unsetTestEnv(t, "GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT")

	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName})

	for _, unwanted := range []string{"GC_DOLT_HOST=", "GC_DOLT_PORT=", "BEADS_DOLT_SERVER_HOST=", "BEADS_DOLT_SERVER_PORT="} {
		if strings.Contains(query, unwanted) {
			t.Fatalf("workflowServeControlReadyQuery should omit %q when ambient env is unset: %q", unwanted, query)
		}
	}
}

// TestWorkflowServeControlReadyQueryDoesNotMixDoltNamespaces guards against a
// correctness gap found in cross-provider review of gc-74rxa: host and port
// must resolve as a matched pair from one env-var namespace, never as a host
// from GC_DOLT_* combined with a port from BEADS_DOLT_SERVER_* (or vice
// versa) -- a combination that may never have described the same server.
// Here GC_DOLT_PORT is set (so the GC_DOLT_* namespace is "in use" for this
// process) while only BEADS_DOLT_SERVER_HOST carries a value; the stale
// BEADS host must NOT leak into the query paired with the GC port.
func TestWorkflowServeControlReadyQueryDoesNotMixDoltNamespaces(t *testing.T) {
	unsetTestEnv(t, "GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "29999")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "9.9.9.9")
	unsetTestEnv(t, "BEADS_DOLT_SERVER_PORT")

	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName})

	for _, want := range []string{"GC_DOLT_PORT='29999'", "BEADS_DOLT_SERVER_PORT='29999'"} {
		if !strings.Contains(query, want) {
			t.Fatalf("workflowServeControlReadyQuery missing %q in %q", want, query)
		}
	}
	if strings.Contains(query, "9.9.9.9") {
		t.Fatalf("workflowServeControlReadyQuery must not mix BEADS_DOLT_SERVER_HOST from a different namespace than the resolved port: %q", query)
	}
}

// unsetTestEnv unsets the given env vars for the duration of the test,
// restoring the original values (or absence) afterward.
func unsetTestEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
}

// TestWorkflowServeControlReadyQueryDeliversAmbientDoltPortAtExecution is the
// execution-level companion to TestWorkflowServeControlReadyQueryPassesThroughAmbientDoltPort:
// cross-provider review of gc-74rxa noted that a pure string-assertion test
// can pass while the real runtime path (shellWorkQueryWithEnv running the
// query via `sh -c`, cmd/gc/cmd_hook.go:555) stays broken, since it never
// crosses the process boundary. This test runs the built query through a
// fake `bd` with an OUTER env that deliberately carries no Dolt connection
// vars at all -- reproducing the exact failure mode (mergeRuntimeEnv having
// stripped them) -- and asserts bd still receives the ambient port via the
// query string's own shell-prefix assignment.
func TestWorkflowServeControlReadyQueryDeliversAmbientDoltPortAtExecution(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "127.0.0.1")
	t.Setenv("GC_DOLT_PORT", "29620")
	unsetTestEnv(t, "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT")

	query := workflowServeControlReadyQuery(
		config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"},
		"gascity--control-dispatcher",
	)

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "bd.log")
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(`#!/bin/sh
set -eu
printf 'GC_DOLT_PORT=%s BEADS_DOLT_SERVER_PORT=%s\n' "${GC_DOLT_PORT:-}" "${BEADS_DOLT_SERVER_PORT:-}" >> "$BD_LOG"
printf '[]'
`), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	// The outer env passed to shellWorkQueryWithEnv has no GC_DOLT_*/
	// BEADS_DOLT_SERVER_* at all -- simulating mergeRuntimeEnv/
	// controllerWorkQueryEnv having dropped them. Without the fix, bd would
	// see an empty port here and resolve :0.
	_, err := shellWorkQueryWithEnv(query, t.TempDir(), []string{
		"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"BD_LOG=" + logPath,
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	})
	if err != nil {
		t.Fatalf("run workflow serve query: %v", err)
	}

	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read bd log: %v", readErr)
	}
	if !strings.Contains(string(logData), "GC_DOLT_PORT=29620") || !strings.Contains(string(logData), "BEADS_DOLT_SERVER_PORT=29620") {
		t.Fatalf("bd did not see the ambient Dolt port despite a stripped outer env; log:\n%s", string(logData))
	}
}

func TestWorkflowServeWorkQueryRecognizesCoreControlDispatcher(t *testing.T) {
	query := workflowServeWorkQuery(config.Agent{Name: "core.control-dispatcher", Dir: "fixture"})

	if strings.Contains(query, "bd query --json") {
		t.Fatalf("core control-dispatcher serve query should avoid generic ephemeral scans: %q", query)
	}
	if !strings.Contains(query, "BD_EXPORT_AUTO=false") {
		t.Fatalf("core control-dispatcher serve query should use the specialized control query: %q", query)
	}
	if !strings.Contains(query, "GC_CONTROL_TARGET='fixture/core.control-dispatcher'") {
		t.Fatalf("core control-dispatcher serve query missing scoped target: %q", query)
	}
}

func TestWorkflowServeControlReadyQueryDoesNotCrossScope(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{
		Name:        config.ControlDispatcherAgentName,
		BindingName: "core",
		Dir:         "fixture",
	})

	for _, want := range []string{
		"GC_CONTROL_TARGET='fixture/core.control-dispatcher'",
		"GC_CONTROL_BARE_TARGET='fixture/control-dispatcher'",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("rig control query missing %q: %q", want, query)
		}
	}
	for _, forbidden := range []string{
		"GC_CONTROL_CITY_TARGET=",
		"GC_CONTROL_TARGET='core.control-dispatcher'",
		"GC_CONTROL_BARE_TARGET='control-dispatcher'",
	} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("rig control query contains cross-scope target %q: %q", forbidden, query)
		}
	}
}

func TestWorkflowServeControlReadyQueryBD105IncludesEphemeral(t *testing.T) {
	query := workflowServeControlReadyQueryForBeads(
		config.Agent{Name: config.ControlDispatcherAgentName},
		config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105},
	)
	for _, want := range []string{
		`bd --readonly --sandbox ready --include-ephemeral --assignee="$cand" --exclude-type=epic --json --limit=20`,
		`bd --readonly --sandbox ready --include-ephemeral --metadata-field "gc.run_target=$route" --unassigned --exclude-type=epic --exclude-label "hold:mayor" --exclude-label "hold:external" --json --sort oldest --limit=20`,
		`bd --readonly --sandbox ready --include-ephemeral --metadata-field "gc.routed_to=$route" --unassigned --exclude-type=epic --exclude-label "hold:mayor" --exclude-label "hold:external" --json --sort oldest --limit=20`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("workflowServeControlReadyQueryForBeads(bd-1.0.5) missing %q in %q", want, query)
		}
	}
}

// TestWorkflowServeControlReadyQueryHonorsBareLegacyRoute guards the upgrade
// gap: a qualified "core.control-dispatcher" serve loop must still claim
// control beads that pre-1.3 builds routed to the binding-stripped bare name
// "control-dispatcher". The bare alias is queried alongside the qualified
// target so persisted in-flight work is not stranded after upgrade.
func TestWorkflowServeControlReadyQueryHonorsBareLegacyRoute(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, BindingName: "core"})
	if !strings.Contains(query, "GC_CONTROL_TARGET='core.control-dispatcher'") {
		t.Fatalf("serve query missing qualified target: %q", query)
	}
	if !strings.Contains(query, "GC_CONTROL_BARE_TARGET='control-dispatcher'") {
		t.Fatalf("serve query missing bare legacy target: %q", query)
	}
	if !strings.Contains(query, `routed_ready "${GC_CONTROL_BARE_TARGET:-}"`) {
		t.Fatalf("serve query missing bare routed_ready scan: %q", query)
	}
}

// TestControlDispatcherBareRoute pins the binding-stripping alias derivation.
func TestControlDispatcherBareRoute(t *testing.T) {
	cases := []struct{ in, want string }{
		{"core.control-dispatcher", "control-dispatcher"},
		{"rig/core.control-dispatcher", "rig/control-dispatcher"},
		{"control-dispatcher", ""},     // already bare: no distinct alias
		{"rig/control-dispatcher", ""}, // already bare (rig-scoped)
		{"gascity.polecat", ""},        // not a control dispatcher
		{"", ""},
	}
	for _, tc := range cases {
		if got := controlDispatcherBareRoute(tc.in); got != tc.want {
			t.Errorf("controlDispatcherBareRoute(%q) = %q, want %q", tc.in, got, tc.want)
		}
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
  "--readonly --sandbox ready --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf '[{"id":"ga-ready"}]'
    ;;
  "--readonly --sandbox ready --metadata-field gc.run_target=gascity/control-dispatcher --unassigned --exclude-type=epic --exclude-label hold:mayor --exclude-label hold:external --json --sort oldest --limit=20")
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
  "--readonly --sandbox ready --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf '[{"id":"ga-pending","metadata":{"gc.kind":"retry"}}]'
    ;;
  "--readonly --sandbox ready --metadata-field gc.run_target=gascity/control-dispatcher --unassigned --exclude-type=epic --exclude-label hold:mayor --exclude-label hold:external --json --sort oldest --limit=20")
    printf '[{"id":"ga-ready","metadata":{"gc.kind":"scope-check"}}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)
	assertJSONEqual(t, out, `[{"id":"ga-pending","metadata":{"gc.kind":"retry"}},{"id":"ga-ready","metadata":{"gc.kind":"scope-check"}}]`)
}

func TestWorkflowServeControlReadyQueryIncludesCanonicalRoutedControlWork(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	out := runWorkflowServeShellQueryForTest(t, query, map[string]string{
		"GC_SESSION_NAME": "gascity--control-dispatcher",
		"GC_ALIAS":        "gascity/control-dispatcher",
	}, `#!/bin/sh
set -eu
case "$*" in
  "--readonly --sandbox ready --metadata-field gc.routed_to=gascity/control-dispatcher --unassigned --exclude-type=epic --exclude-label hold:mayor --exclude-label hold:external --json --sort oldest --limit=20")
    printf '[{"id":"ga-control-routed","metadata":{"gc.routed_to":"gascity/control-dispatcher","gc.kind":"workflow-finalize"}}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)
	assertJSONEqual(t, out, `[{"id":"ga-control-routed","metadata":{"gc.routed_to":"gascity/control-dispatcher","gc.kind":"workflow-finalize"}}]`)
}

func TestWorkflowServeControlReadyQuerySkipsInstantiatingBeads(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	out := runWorkflowServeShellQueryForTest(t, query, map[string]string{
		"GC_SESSION_NAME": "gascity--control-dispatcher",
		"GC_ALIAS":        "gascity/control-dispatcher",
	}, fmt.Sprintf(`#!/bin/sh
set -eu
case "$*" in
  "--readonly --sandbox ready --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf '[{"id":"ga-instantiating-assigned","metadata":{"%s":"true"}},{"id":"ga-assigned","metadata":{"gc.kind":"retry"}}]'
    ;;
  "--readonly --sandbox ready --metadata-field gc.run_target=gascity/control-dispatcher --unassigned --exclude-type=epic --exclude-label hold:mayor --exclude-label hold:external --json --sort oldest --limit=20")
    printf '[{"id":"ga-instantiating-routed","metadata":{"%s":"true"}},{"id":"ga-routed","metadata":{"gc.kind":"scope-check"}}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`, beadmeta.InstantiatingMetadataKey, beadmeta.InstantiatingMetadataKey))
	assertJSONEqual(t, out, `[{"id":"ga-assigned","metadata":{"gc.kind":"retry"}},{"id":"ga-routed","metadata":{"gc.kind":"scope-check"}}]`)
}

func TestWorkflowServeControlReadyQueryPreservesQueryPriorityWhenMerging(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	out := runWorkflowServeShellQueryForTest(t, query, map[string]string{
		"GC_SESSION_NAME": "gascity--control-dispatcher",
		"GC_ALIAS":        "gascity/control-dispatcher",
	}, `#!/bin/sh
set -eu
case "$*" in
  "--readonly --sandbox ready --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf '[{"id":"ga-z-assigned"},{"id":"ga-dup","source":"assigned"}]'
    ;;
  "--readonly --sandbox ready --metadata-field gc.run_target=gascity/control-dispatcher --unassigned --exclude-type=epic --exclude-label hold:mayor --exclude-label hold:external --json --sort oldest --limit=20")
    printf '[{"id":"ga-a-routed"},{"id":"ga-route-dup","source":"run-target"}]'
    ;;
  "--readonly --sandbox ready --metadata-field gc.routed_to=gascity/control-dispatcher --unassigned --exclude-type=epic --exclude-label hold:mayor --exclude-label hold:external --json --sort oldest --limit=20")
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
  "--readonly --sandbox ready --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
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

func TestWorkflowServeControlReadyQueryKeepsSuccessfulBDStderrOutOfJSON(t *testing.T) {
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
  "--readonly --sandbox ready --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf '[{"id":"ga-control-ready"}]'
    printf 'notice: refreshed export metadata\n' >&2
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
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	})
	if err != nil {
		t.Fatalf("run workflow serve query: %v", err)
	}
	assertJSONEqual(t, out, `[{"id":"ga-control-ready"}]`)
}

func TestWorkflowServeControlReadyQueryFailsOnMalformedBDJSON(t *testing.T) {
	query := workflowServeControlReadyQuery(
		config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"},
		"gascity--control-dispatcher",
	)
	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(`#!/bin/sh
set -eu
case "$*" in
  "--readonly --sandbox ready --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
    printf 'not-json'
    ;;
  *)
    printf '[]'
    ;;
esac
`), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	_, err := shellWorkQueryWithEnv(query, t.TempDir(), []string{
		"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	})
	if err == nil {
		t.Fatal("workflow serve query succeeded with malformed bd JSON")
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
  "--readonly --sandbox ready --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
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
	if want := "--readonly --sandbox ready --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20"; firstCall != want {
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
  "--readonly --sandbox ready --assignee=gascity--control-dispatcher --exclude-type=epic --json --limit=20")
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
if [ "$#" -eq 15 ] &&
   [ "$1" = "--readonly" ] &&
   [ "$2" = "--sandbox" ] &&
   [ "$3" = "ready" ] &&
   [ "$4" = "--metadata-field" ] &&
   [ "$5" = "gc.run_target=my rig/control-dispatcher" ] &&
   [ "$6" = "--unassigned" ] &&
   [ "$7" = "--exclude-type=epic" ] &&
   [ "$8" = "--exclude-label" ] &&
   [ "$9" = "hold:mayor" ] &&
   [ "${10}" = "--exclude-label" ] &&
   [ "${11}" = "hold:external" ] &&
   [ "${12}" = "--json" ] &&
   [ "${13}" = "--sort" ] &&
   [ "${14}" = "oldest" ] &&
   [ "${15}" = "--limit=20" ]; then
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
	wantArgs := []string{"--readonly", "--sandbox", "ready", "--metadata-field", "gc.run_target=my rig/control-dispatcher", "--unassigned", "--exclude-type=epic", "--exclude-label", "hold:mayor", "--exclude-label", "hold:external", "--json", "--sort", "oldest", "--limit=20"}
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
  "--readonly --sandbox ready --metadata-field gc.run_target=gascity/workflow-control --unassigned --exclude-type=epic --exclude-label hold:mayor --exclude-label hold:external --json --sort oldest --limit=20")
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
` + testControlDispatcherAgentTOML("") + testControlDispatcherAgentTOML("myrig")
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
		store = underlyingPolicyStoreForTest(store)
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	err = runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, control.ID, cfg, io.Discard, &stderr)
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
		cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
		return runControlDispatcherWithStoreAndConfig(cityPath, storePath, store, beadID, cfg, stdout, stderr)
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, control.ID, cfg, io.Discard, &stderr); err != nil {
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
	if err := runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, fanout.ID, cfg, io.Discard, &stderr); err != nil {
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

func TestRunControlDispatcherQuarantinesRalphControlMissingIteration(t *testing.T) {
	clearGCEnv(t)

	// A ralph control bead with no iteration sub-DAG — the unprocessable
	// shape left behind by the pre-seed-fix ralph-in-ralph re-spawn gap.
	// The dispatcher must quarantine it as a malformed control graph and
	// return nil so the serve loop keeps draining the rest of the queue.
	// Regression for gastownhall/gascity#2798.
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
	control, err := store.Create(beads.Bead{
		Title: "inner loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":          "ralph",
			"gc.root_bead_id":  workflow.ID,
			"gc.step_ref":      "mol-test.outer.iteration.2.inner",
			"gc.step_id":       "inner",
			"gc.max_attempts":  "3",
			"gc.control_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}

	var stderr bytes.Buffer
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if err := runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, control.ID, cfg, io.Discard, &stderr); err != nil {
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
	if got := after.Metadata["gc.control_quarantine_reason"]; !strings.Contains(got, "no iteration found") {
		t.Fatalf("gc.control_quarantine_reason = %q, want no iteration found", got)
	}
	if !slices.Contains(after.Labels, "gc:control-quarantined") {
		t.Fatalf("labels = %#v, want gc:control-quarantined", after.Labels)
	}
	if got := stderr.String(); !strings.Contains(got, "control dispatch: quarantined bead="+control.ID) {
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
	if err := runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, control.ID, cfg, io.Discard, &stderr); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
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
`, rigDir) + testControlDispatcherAgentTOML("") + testControlDispatcherAgentTOML("backend")
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

// TestRunWorkflowServeFollowDrainsObservedWakeBeforeSurfacingWatcherErr is the
// regression guard for the coalescing bug where a relevant event observed just
// before a fatal watcher error was consumed without its promised re-scan. When
// the wait reports a relevant wake AND a pending fatal stream error (the error
// arrived inside the same coalescing window), the loop must perform the one
// drain that wake scheduled before surfacing the error, so newly-ready work is
// serviced in-process rather than stranded until an external dispatcher
// restart re-scans.
func TestRunWorkflowServeFollowDrainsObservedWakeBeforeSurfacingWatcherErr(t *testing.T) {
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

	workflowServeOpenEventsProvider = func(io.Writer) (events.Provider, error) { return ep, nil }
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0

	watcherErr := errors.New("event stream closed")
	waitCalls := 0
	workflowServeWaitForWake = func(_ <-chan workflowWatchResult, _ time.Duration, _ int) (bool, error) {
		waitCalls++
		if waitCalls == 1 {
			// A relevant event was observed, then a fatal watcher error arrived
			// inside the coalescing window: report the wake AND the error.
			return true, watcherErr
		}
		t.Fatalf("unexpected wait call %d: the loop must drain the observed wake and exit, not wait again", waitCalls)
		return false, watcherErr
	}

	listCalls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		listCalls++
		switch listCalls {
		case 1:
			// Initial drain before the first wait: nothing ready yet.
			return nil, nil
		case 2:
			// The re-scan the observed wake promised finds newly-ready work that
			// must be processed before the loop surfaces the watcher error.
			return []hookBead{{ID: "gc-woke", Metadata: map[string]string{"gc.kind": "scope-check"}}}, nil
		case 3:
			// Drain's internal exit poll after processing gc-woke.
			return nil, nil
		default:
			t.Fatalf("unexpected list call %d: the loop must perform exactly one re-scan for the observed wake before exiting", listCalls)
			return nil, nil
		}
	}
	processedAfterWake := false
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		if beadID == "gc-woke" {
			processedAfterWake = true
		}
		return nil
	}

	agent := config.Agent{Name: "control-dispatcher"}
	err := runWorkflowServeFollow(agent, t.TempDir(), t.TempDir(), agent.EffectiveWorkQuery(), nil, io.Discard)
	if !errors.Is(err, watcherErr) {
		t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, watcherErr)
	}
	if waitCalls != 1 {
		t.Fatalf("wait calls = %d, want 1 (loop must drain the observed wake then exit, not wait again)", waitCalls)
	}
	if !processedAfterWake {
		t.Fatal("observed wake's newly-ready bead was not processed before the watcher error surfaced")
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

func TestRunWorkflowServeFollowSurvivesDoltCircuitBreakerOutage(t *testing.T) {
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

	trippedErr := fmt.Errorf(`querying control work: running work query %q: exit status 1: begin read tx: dial tcp 127.0.0.1:52022: connect: connection refused (circuit breaker tripped)`, "bd ready")
	breakerOpenErr := fmt.Errorf(`querying control work: running work query %q: exit status 1: Error: failed to open database: dolt circuit breaker is open: server appears down, failing fast (cooldown 5s)`, "bd ready")
	fatalErr := errors.New("malformed work query: jq: command not found")
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		switch calls {
		case 1:
			return nil, trippedErr
		case 2:
			return nil, breakerOpenErr
		default:
			return nil, fatalErr
		}
	}

	agent := config.Agent{Name: config.ControlDispatcherAgentName}
	err := runWorkflowServeFollow(agent, t.TempDir(), t.TempDir(), agent.EffectiveWorkQuery(), nil, io.Discard)
	if !errors.Is(err, fatalErr) {
		t.Fatalf("runWorkflowServeFollow err = %v, want fatal error after surviving the breaker outage", err)
	}
	if calls != 3 {
		t.Fatalf("workflowServeList calls = %d, want 3 (survive tripped and open breaker errors, then exit on fatal)", calls)
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
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Agents: []config.Agent{
			{Name: "reviewer", MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend", "myrig")

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
	if control.Assignee != "" {
		t.Fatalf("review scope-check assignee = %q, want empty routed control-dispatcher queue", control.Assignee)
	}
	if got := control.Metadata["gc.routed_to"]; got != config.ControlDispatcherAgentName {
		t.Fatalf("review scope-check gc.routed_to = %q, want %q", got, config.ControlDispatcherAgentName)
	}
	if control.Metadata[graphroute.GraphExecutionRouteMetaKey] != "reviewer" {
		t.Fatalf("review scope-check execution route = %q, want reviewer", control.Metadata[graphroute.GraphExecutionRouteMetaKey])
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
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "reviewer", MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend", "myrig")

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
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Agents: []config.Agent{
			{Name: "reviewer", MaxActiveSessions: intPtr(1)},
			{Name: "control-dispatcher"},
		},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend", "myrig")

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
	dispatchControlSessionProvider = func() (runtime.Provider, error) { return fakeProvider, nil }
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

	_, _, err := findBeadScopeAcrossStores(cityPath, "gc-missing", io.Discard)
	if err == nil {
		t.Fatal("findBeadScopeAcrossStores() error = nil, want provider failure")
	}
	if !strings.Contains(err.Error(), "getting bead \"gc-missing\" from "+cityPath) {
		t.Fatalf("findBeadScopeAcrossStores() error = %v, want city store path context", err)
	}
	if strings.Contains(err.Error(), "bead not found") {
		t.Fatalf("findBeadScopeAcrossStores() error = %v, want provider failure instead of masked not-found", err)
	}
}

func TestCmdWorkflowDeleteSourceAllowsStoreSelectorForAmbiguousSourceIDs(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "alpha")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rigDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]

[[rigs]]
name = "alpha"
prefix = "BL"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core")
	writeCatalogFile(t, cityDir, ".gc/site.toml", "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
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

[[rigs]]
name = "alpha"
prefix = "BL"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core")
	writeCatalogFile(t, cityDir, ".gc/site.toml", "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
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

[[rigs]]
name = "alpha"
prefix = "BL"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core")
	writeCatalogFile(t, cityDir, ".gc/site.toml", "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"alpha\"\npath = \"rigs/alpha\"\n")
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

	// The match carries no bd runner, which is the point: delete-source deletes
	// exactly the ids it collected, in process. A `bd delete --cascade` would
	// walk the dependency edges out of the workflow and take `outside` with it.
	var stderr bytes.Buffer
	closed, deleted, incomplete := applySourceWorkflowMatchCleanup(sourceWorkflowStoreMatch{
		label: "rig:gascity",
		store: store,
		beads: []beads.Bead{first, second},
		path:  "/repo",
	}, true, &stderr)
	if incomplete {
		t.Fatalf("cleanup incomplete; stderr=%s", stderr.String())
	}
	if closed != 2 || deleted != 2 {
		t.Fatalf("closed/deleted = %d/%d, want 2/2", closed, deleted)
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
	// Set the debounce explicitly so a lone relevant wake is fast and
	// intentional rather than silently inheriting the package default.
	prevDebounce := workflowServeWakeDebounce
	workflowServeWakeDebounce = 5 * time.Millisecond
	defer func() { workflowServeWakeDebounce = prevDebounce }()

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

func TestWaitForRelevantWorkflowWakeCoalescesBurst(t *testing.T) {
	prevDebounce := workflowServeWakeDebounce
	workflowServeWakeDebounce = 50 * time.Millisecond
	defer func() { workflowServeWakeDebounce = prevDebounce }()

	const burst = 8
	eventCh := make(chan workflowWatchResult, burst)
	for i := 0; i < burst; i++ {
		eventCh <- workflowWatchResult{evt: events.Event{Type: events.BeadUpdated, Subject: fmt.Sprintf("mc-wisp-%d", i)}}
	}

	// A single wait call must drain the whole buffered burst and return exactly
	// one wake, so runWorkflowServeFollow performs one re-scan for the burst
	// rather than one per event.
	eventWake, err := waitForRelevantWorkflowWake(eventCh, time.Second)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !eventWake {
		t.Fatal("eventWake = false, want true when a relevant burst arrives")
	}
	if leftover := len(eventCh); leftover != 0 {
		t.Fatalf("eventCh still has %d buffered events after one wake; want 0 (burst not coalesced)", leftover)
	}
}

func TestWaitForRelevantWorkflowWakeBurstSurfacesWatcherErr(t *testing.T) {
	prevDebounce := workflowServeWakeDebounce
	workflowServeWakeDebounce = 50 * time.Millisecond
	defer func() { workflowServeWakeDebounce = prevDebounce }()

	eventCh := make(chan workflowWatchResult, 2)
	eventCh <- workflowWatchResult{evt: events.Event{Type: events.BeadUpdated, Subject: "gc-1"}}
	eventCh <- workflowWatchResult{err: os.ErrDeadlineExceeded}

	// A fatal stream error that arrives during the coalescing window must still
	// terminate the serve loop, but the relevant event observed just before it
	// must still report a wake so runWorkflowServeFollow performs the one
	// promised re-scan for that wake before surfacing the error. Returning
	// (false, err) here would strand the just-observed work until a dispatcher
	// restart re-scans.
	eventWake, err := waitForRelevantWorkflowWake(eventCh, time.Second)
	if err == nil {
		t.Fatal("wait returned nil err, want watcher err surfaced from coalescing window")
	}
	if !eventWake {
		t.Fatal("eventWake = false, want true: the relevant event observed before the error must still wake the loop for its re-scan")
	}
}

func TestWaitForRelevantWorkflowWakeDisabledDebounceDoesNotCoalesce(t *testing.T) {
	prevDebounce := workflowServeWakeDebounce
	workflowServeWakeDebounce = 0
	defer func() { workflowServeWakeDebounce = prevDebounce }()

	// With coalescing disabled (debounce <= 0) the escape hatch restores
	// one-event-one-drain: a relevant event still wakes the loop, but the helper
	// must not consume any trailing buffered events.
	eventCh := make(chan workflowWatchResult, 2)
	eventCh <- workflowWatchResult{evt: events.Event{Type: events.BeadCreated, Subject: "gc-1"}}
	eventCh <- workflowWatchResult{evt: events.Event{Type: events.BeadCreated, Subject: "gc-2"}}

	start := time.Now()
	eventWake, err := waitForRelevantWorkflowWake(eventCh, time.Second)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !eventWake {
		t.Fatal("eventWake = false, want true for a relevant event with coalescing disabled")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("returned after %v, want near-immediate when debounce is disabled", elapsed)
	}
	if leftover := len(eventCh); leftover != 1 {
		t.Fatalf("eventCh has %d buffered events, want 1 (disabled debounce must not drain the trailing event)", leftover)
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

// TestAssertDrainRootScopeMatchesDispatch pins the invariant a drain's member
// resolution depends on: the convoy lives in the root's work scope, so a drain
// dispatched from a different one would resolve an empty convoy and report
// success. The matching and unstamped arms are what keep it off today's shapes.
func TestAssertDrainRootScopeMatchesDispatch(t *testing.T) {
	drain := func(rootRef string) beads.Bead {
		b := beads.Bead{ID: "gcg-drain-1", Metadata: map[string]string{}}
		if rootRef != "" {
			b.Metadata[beadmeta.RootStoreRefMetadataKey] = rootRef
		}
		return b
	}
	for _, tc := range []struct {
		name       string
		rootRef    string
		dispatchTo string
		wantErr    bool
	}{
		{name: "rig-rooted drain dispatched at its own rig", rootRef: "rig:gascity", dispatchTo: "rig:gascity"},
		{name: "city-rooted drain dispatched at the city", rootRef: "city:mc", dispatchTo: "city:mc"},
		{name: "city-rooted drain dispatched at a rig", rootRef: "city:mc", dispatchTo: "rig:gascity", wantErr: true},
		{name: "rig-rooted drain dispatched at another rig", rootRef: "rig:beads", dispatchTo: "rig:gascity", wantErr: true},
		{name: "unstamped root ref is not a mismatch", rootRef: "", dispatchTo: "rig:gascity"},
		{name: "unrecognized dispatch directory is not a mismatch", rootRef: "city:mc", dispatchTo: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := assertDrainRootScopeMatchesDispatch(drain(tc.rootRef), tc.dispatchTo)
			if tc.wantErr && err == nil {
				t.Fatalf("root %q dispatched from %q was accepted; the drain would read the wrong work store and resolve an empty convoy", tc.rootRef, tc.dispatchTo)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("root %q dispatched from %q was rejected: %v", tc.rootRef, tc.dispatchTo, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.rootRef) {
				t.Errorf("the error must name the root scope so an operator can see which store the convoy is in; got %v", err)
			}
		})
	}
}

// TestRunControlDispatcherRejectsCrossScopeDrain pins the guard's call site: the
// unit test above proves the predicate, this proves the drain arm consults it.
func TestRunControlDispatcherRejectsCrossScopeDrain(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title:  "Drain",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         "drain",
			beadmeta.RootStoreRefMetadataKey: "rig:elsewhere",
		},
	})
	if err != nil {
		t.Fatalf("Create(control): %v", err)
	}

	var stderr bytes.Buffer
	err = runControlDispatcherWithStoreAndConfig(cityPath, cityPath, store, control.ID, &config.City{Workspace: config.Workspace{Name: "test-city"}}, io.Discard, &stderr)
	if err == nil {
		t.Fatal("a drain rooted in rig:elsewhere dispatched from city:test-city was accepted; it would drain the wrong store's convoy and report success")
	}
	if !strings.Contains(err.Error(), "rig:elsewhere") || !strings.Contains(err.Error(), "city:test-city") {
		t.Fatalf("error = %v, want both the root and dispatch scopes named", err)
	}
}

// relocatedWorkflowCity builds the shape `gc storage migrate` leaves behind for
// a workflow tree: control beads copied into the class binding with their ids
// preserved, and the copies the migration retained still sitting in the work
// ledger.
//
// The two rows differ in the one way that makes a wrong sweep visible. The
// binding carries a step minted AFTER the cutover, so a sweep that never opened
// the binding cannot reach it at all; the work ledger's copy shares its ids with
// the binding's, so a sweep that folded duplicates away would leave it open.
// Both belong to the operator's "erase this workflow", which is why these arms
// take the union rather than the merge the read fan-outs take.
func relocatedWorkflowCity(t *testing.T) (cityPath, rootID, bindingOnlyID string, work, binding beads.Store) {
	t.Helper()
	cityPath, _ = foreignProviderCity(t)
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })
	work = workStoreFor(t, cityPath)

	rootShape := beads.Bead{
		Title:  "the retained frozen workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
	}
	root, err := work.Create(rootShape)
	if err != nil {
		t.Fatalf("seeding the retained workflow root in the work store: %v", err)
	}
	step, err := work.Create(beads.Bead{
		Title:    "the retained frozen step",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("seeding the retained workflow step in the work store: %v", err)
	}

	binding = soleClassBindingStore(t, cityPath)
	carried := rootShape
	carried.ID = root.ID
	carried.Title = "the binding's live workflow"
	if _, err := migrationSeed(binding, carried); err != nil {
		t.Fatalf("carrying the workflow root across to the class binding: %v", err)
	}
	if _, err := migrationSeed(binding, beads.Bead{
		ID:       step.ID,
		Title:    "the binding's live step",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	}); err != nil {
		t.Fatalf("carrying the workflow step across to the class binding: %v", err)
	}
	binding = recensusAfterSeedingARelic(t, cityPath)

	later, err := binding.Create(beads.Bead{
		Title:    "minted in the binding after the migration",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("seeding the binding's post-migration step: %v", err)
	}
	return cityPath, root.ID, later.ID, work, binding
}

// TestWorkflowDeleteSweepsTheRelocatedTreeAndTheRetainedCopy is the ga-gqc9e
// regression on `gc workflow delete`.
//
// The command enumerates the city's and rigs' DIRECTORIES, and a relocated class
// binding is not one of them. On a converged city that leaves the sweep working
// entirely on the copies the migration retained: it closes the frozen twin,
// reports the count and exits 0, while the tree the city is actually running
// stays live in the binding. An operator who ran delete to stop a workflow has
// been told it stopped.
func TestWorkflowDeleteSweepsTheRelocatedTreeAndTheRetainedCopy(t *testing.T) {
	_, rootID, bindingOnlyID, work, binding := relocatedWorkflowCity(t)

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDelete(rootID, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc workflow delete exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), convoyBindingViewPath) {
		t.Errorf("the sweep never named the class binding, so the tree the city works was not in it:\n%s", stdout.String())
	}
	for _, id := range []string{rootID, bindingOnlyID} {
		swept, err := binding.Get(id)
		if err != nil {
			t.Fatalf("reading %s back from the binding: %v", id, err)
		}
		if swept.Status != "closed" {
			t.Errorf("the binding's %s is %q after the sweep, want closed", id, swept.Status)
		}
	}
	retained, err := work.Get(rootID)
	if err != nil {
		t.Fatalf("reading %s back from the work store: %v", rootID, err)
	}
	if retained.Status != "closed" {
		t.Errorf("the retained work copy is %q after the sweep, want closed; delete erases the workflow everywhere, and a frozen twin left open is the copy an operator later finds still listed", retained.Status)
	}
}

// TestWorkflowDeleteRefusesToSweepPastABindingThatStandsRefused pins the arm
// that separates a sweep from a read.
//
// `gc beads list` prints what it can reach and says what it could not. A
// destructive one-shot cannot: "I could not see the binding's tree" and "the
// binding's tree is gone" produce the same exit code and the same operator
// belief, and only one of them is true. So the refusal stops the sweep before it
// touches anything.
//
// The fault here is a STANDING refusal — a verdict about storage config that
// arrives at federation time, before a row is read. A binding that resolves and
// then fails mid-read is a different arrival with the same consequence, and it
// is pinned separately by the ...FaultsMidRead rows below.
func TestWorkflowDeleteRefusesToSweepPastABindingThatStandsRefused(t *testing.T) {
	cityPath, rootID, _, work, _ := relocatedWorkflowCity(t)
	failClassBindingReads(t, cityPath, errors.New("the class binding is having a bad day"))

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDelete(rootID, true, false, &stdout, &stderr); code != 1 {
		t.Fatalf("gc workflow delete exited %d, want 1: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "bad day") {
		t.Errorf("the refusal does not carry the binding's cause: %q", stderr.String())
	}
	retained, err := work.Get(rootID)
	if err != nil {
		t.Fatalf("reading %s back from the work store: %v", rootID, err)
	}
	if retained.Status == "closed" {
		t.Errorf("the sweep closed the copy it could reach and then stopped; a partial sweep is exactly what refusing is for")
	}
}

// TestWorkflowDeleteSourceSweepsTheRelocatedRoots is the same regression on the
// arm operators actually reach for: delete-source names the SOURCE bead and lets
// gc find the workflow it spawned.
//
// The roots here carry no gc.source_store_ref, which is the legacy shape
// WorkflowMatchesSource resolves against the store the root physically lives in.
// That makes the row a pin on more than the extra view: the binding is the city's
// own store after relocation, so its rows have to answer to the CITY's store ref.
// A binding view that reported a ref of its own would match no legacy root at
// all, and would also read as a second store to the multi-store guard — which
// would refuse every converged city instead of sweeping it.
func TestWorkflowDeleteSourceSweepsTheRelocatedRoots(t *testing.T) {
	_, rootID, bindingOnlyID, work, binding := relocatedWorkflowCity(t)

	source, err := work.Create(beads.Bead{Title: "the source bead", Type: "task", Status: "in_progress"})
	if err != nil {
		t.Fatalf("seeding the source bead: %v", err)
	}
	if err := work.SetMetadata(source.ID, "workflow_id", rootID); err != nil {
		t.Fatalf("stamping the source bead's workflow_id: %v", err)
	}
	for _, store := range []beads.Store{work, binding} {
		if err := store.SetMetadata(rootID, beadmeta.SourceBeadIDMetadataKey, source.ID); err != nil {
			t.Fatalf("stamping the root's source bead id: %v", err)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(source.ID, sourceWorkflowStoreSelector{}, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc workflow delete-source exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result=cleaned") {
		t.Errorf("delete-source did not report a clean sweep:\n%s", stdout.String())
	}
	for _, id := range []string{rootID, bindingOnlyID} {
		swept, err := binding.Get(id)
		if err != nil {
			t.Fatalf("reading %s back from the binding: %v", id, err)
		}
		if swept.Status != "closed" {
			t.Errorf("the binding's %s is %q after delete-source, want closed", id, swept.Status)
		}
	}
	retained, err := work.Get(rootID)
	if err != nil {
		t.Fatalf("reading %s back from the work store: %v", rootID, err)
	}
	if retained.Status != "closed" {
		t.Errorf("the retained work copy is %q after delete-source, want closed", retained.Status)
	}
	cleared, err := work.Get(source.ID)
	if err != nil {
		t.Fatalf("reading the source bead back: %v", err)
	}
	if got := strings.TrimSpace(cleared.Metadata["workflow_id"]); got != "" {
		t.Errorf("the source bead's workflow_id = %q, want empty", got)
	}
}

// TestWorkflowDeleteSourceRefusesToSweepPastABindingThatStandsRefused is the
// delete-source half of the standing refusal. It runs a different collector from
// `gc workflow delete`, so the policy has to be stated in both.
//
// The selector is explicit on purpose. Without one, delete-source resolves the
// source bead by id first, and that resolution leads with the same binding —
// so an unreadable binding aborts the command before the sweep is ever planned,
// and the exit code proves nothing about the sweep. Naming the store skips the
// by-id leg and puts the collector's own refusal on the only path to failure.
func TestWorkflowDeleteSourceRefusesToSweepPastABindingThatStandsRefused(t *testing.T) {
	cityPath, rootID, _, work, _ := relocatedWorkflowCity(t)
	source, err := work.Create(beads.Bead{Title: "the source bead", Type: "task", Status: "in_progress"})
	if err != nil {
		t.Fatalf("seeding the source bead: %v", err)
	}
	if err := work.SetMetadata(rootID, beadmeta.SourceBeadIDMetadataKey, source.ID); err != nil {
		t.Fatalf("stamping the root's source bead id: %v", err)
	}
	failClassBindingReads(t, cityPath, errors.New("the class binding is having a bad day"))

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(source.ID, sourceWorkflowStoreSelector{storeRef: "city"}, true, false, &stdout, &stderr); code != 1 {
		t.Fatalf("gc workflow delete-source exited %d, want 1: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "bad day") {
		t.Errorf("the refusal does not carry the binding's cause: %q", stderr.String())
	}
	retained, err := work.Get(rootID)
	if err != nil {
		t.Fatalf("reading %s back from the work store: %v", rootID, err)
	}
	if retained.Status == "closed" {
		t.Errorf("delete-source swept the copy it could reach past an unreadable binding")
	}
}

// TestWorkflowReopenSourceSeesTheRelocatedRoot is the gating half of ga-gqc9e.
//
// reopen-source refuses to re-open a source bead whose workflow is still live,
// and it decides that from the roots the same candidate walk finds. On a
// converged city the live root is in the binding, so the walk finds nothing,
// the guard passes, and the bead is re-slung underneath a workflow that never
// stopped — two runs of the same work against the same branch.
func TestWorkflowReopenSourceSeesTheRelocatedRoot(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	work := workStoreFor(t, cityPath)
	source, err := work.Create(beads.Bead{Title: "the source bead", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the source bead: %v", err)
	}
	// Create normalizes a new bead to open, so the closed state this test's
	// mutation assertion rests on has to be a transition. Assert it landed —
	// a source bead that was never closed makes "still closed" vacuous.
	if _, err := work.CloseAll([]string{source.ID}, nil); err != nil {
		t.Fatalf("closing the source bead: %v", err)
	}
	if seeded, err := work.Get(source.ID); err != nil {
		t.Fatalf("reading the seeded source bead: %v", err)
	} else if seeded.Status != "closed" {
		t.Fatalf("the seeded source bead is %q, want closed", seeded.Status)
	}

	binding := soleClassBindingStore(t, cityPath)
	root, err := binding.Create(beads.Bead{
		Title:  "the binding's live workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.SourceBeadIDMetadataKey:    source.ID,
		},
	})
	if err != nil {
		t.Fatalf("seeding the binding's live workflow root: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowReopenSource(source.ID, sourceWorkflowStoreSelector{}, &stdout, &stderr); code != 3 {
		t.Fatalf("gc workflow reopen-source exited %d, want 3 (conflict): %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), root.ID) {
		t.Errorf("the conflict does not name the binding's live root %s: %q", root.ID, stderr.String())
	}
	unchanged, err := work.Get(source.ID)
	if err != nil {
		t.Fatalf("reading the source bead back: %v", err)
	}
	if unchanged.Status != "closed" {
		t.Errorf("the source bead is %q, want closed; reopen-source ran past a live workflow it could not see", unchanged.Status)
	}
}

// faultingClassStore wraps a city's real class binding and makes chosen
// operations fail at RUNTIME, without the binding becoming a standing refusal.
//
// That distinction is the whole point of the rows below. refusedClassStore is a
// verdict about the city's storage configuration, taken before any work starts,
// and it is the one shape the sweep's federation guard was written to intercept.
// A binding that resolves normally and then drops its connection mid-sweep — a
// sqlite I/O error, a dolt server going away, a permission change — announces
// nothing: it contributes no rows, which is indistinguishable from holding none,
// and a sweep that reads that silence as "nothing here" reports a completed
// erase over a live tree.
//
// Pointer-typed because the binding grouping keys a map on store identity, and
// IDPrefix is delegated explicitly for the reason countingClassStore delegates
// it: beads.Store does not carry it, so embedding the interface alone would hide
// the leaf's declaration and the binding's mint bit would read false.
type faultingClassStore struct {
	beads.Store
	readErr  error
	writeErr error
}

func (s *faultingClassStore) Get(id string) (beads.Bead, error) {
	if s.readErr != nil {
		return beads.Bead{}, s.readErr
	}
	return s.Store.Get(id)
}

func (s *faultingClassStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.Store.List(q)
}

func (s *faultingClassStore) CloseAll(ids []string, meta map[string]string) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.Store.CloseAll(ids, meta)
}

// SetMetadata faults on writeErr for CloseAll's reason, one verb over. A sweep
// closes and then STAMPS the source bead, and a binding that drops its
// connection does not do so between the two; a wrapper that faulted only the
// close would leave the metadata arm exercising a healthy store.
func (s *faultingClassStore) SetMetadata(id, key, value string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	return s.Store.SetMetadata(id, key, value)
}

func (s *faultingClassStore) IDPrefix() string {
	declaring, ok := s.Store.(storeref.HasIDPrefix)
	if !ok {
		return ""
	}
	return declaring.IDPrefix()
}

// installFaultingClassBinding swaps this city's class stores for one that faults
// on the given operations, and restates the census verdict for the store it
// installed so the binding still derives as relocated and relic-bearing.
//
// Both halves, for installCountedClassBindingWrapped's reason: the verdict is
// keyed by store identity and the swap installs a store the census never saw.
// Without the restatement the derivation would re-probe through a store that is
// now failing, and the row would be asserting on a binding that stopped
// resolving rather than on one that resolves and cannot answer.
func installFaultingClassBinding(t *testing.T, cityPath string, readErr, writeErr error) {
	t.Helper()
	installWrappedClassBinding(t, cityPath, func(previous beads.Store) beads.Store {
		return &faultingClassStore{Store: previous, readErr: readErr, writeErr: writeErr}
	})
}

// installWrappedClassBinding swaps this city's class stores for wrap's store and
// restates the census verdict for it, so the binding still derives as relocated
// and relic-bearing.
//
// Both halves, for installCountedClassBindingWrapped's reason: the verdict is
// keyed by store identity and the swap installs a store the census never saw.
// Without the restatement the derivation would re-probe through a store that is
// now failing, and the row would be asserting on a binding that stopped
// resolving rather than on one that resolves and cannot answer.
//
// wrap is called exactly once, on the first relocated class store, and the store
// it returns fronts every class — the fixtures here relocate one class, and a
// wrapper installed for some of them would leave the row asserting on whichever
// leg the command happened to take.
func installWrappedClassBinding(t *testing.T, cityPath string, wrap func(beads.Store) beads.Store) {
	t.Helper()
	routes := cliStorageRoutes(cityPath)
	if routes == nil {
		t.Fatal("the city resolved no routes to wrap")
	}
	var installed beads.Store
	restore := make(map[coordclass.Class]beads.Store, len(routes.stores))
	for class, previous := range routes.stores {
		restore[class] = previous
		if installed == nil {
			installed = wrap(previous)
		}
		routes.stores[class] = installed
	}
	if installed == nil {
		t.Fatal("the city relocated no class store to wrap")
	}
	previousRelics := routes.relics
	routes.relics = map[beads.Store]bool{installed: true}
	dropDerivedResidencyMemo(t, cityPath)
	t.Cleanup(func() {
		routes.relics = previousRelics
		for class, previous := range restore {
			routes.stores[class] = previous
		}
	})

	bindings, err := cliResidencyBindings(cityPath)
	if err != nil {
		t.Fatalf("re-deriving the bindings this row will use: %v", err)
	}
	if len(bindings) != 1 || bindings[0].Leg.Store != installed {
		t.Fatalf("the binding resolves to %d bindings not fronted by the installed store; this row would exercise a healthy one", len(bindings))
	}
}

// TestWorkflowDeleteRefusesToSweepPastABindingThatFaultsMidRead is the ga-gqc9e
// regression on the fault a standing refusal does not cover.
//
// A binding that resolves as relocated and then FAILS its reads contributes zero
// rows to the sweep. Zero rows is what an empty store contributes, so the sweep
// drops it, closes the retained frozen copies it could reach, prints a count and
// exits 0 — never naming the binding whose live tree it did not touch. That is
// the partial sweep reported as success, arrived at without any refusal for the
// federation guard to intercept.
func TestWorkflowDeleteRefusesToSweepPastABindingThatFaultsMidRead(t *testing.T) {
	cityPath, rootID, _, work, _ := relocatedWorkflowCity(t)
	installFaultingClassBinding(t, cityPath, errors.New("connection reset mid-sweep"), nil)

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDelete(rootID, true, false, &stdout, &stderr); code != 1 {
		t.Fatalf("gc workflow delete exited %d, want 1: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "connection reset mid-sweep") {
		t.Errorf("the refusal does not carry the fault that caused it: %q", stderr.String())
	}
	retained, err := work.Get(rootID)
	if err != nil {
		t.Fatalf("reading %s back from the work store: %v", rootID, err)
	}
	if retained.Status == "closed" {
		t.Errorf("the sweep closed the copies it could reach and reported success over an unread binding")
	}
}

// TestWorkflowDeleteReportsAFailedCloseInTheBinding is the ga-gqc9e regression on
// the default mode's WRITE half.
//
// `gc workflow delete` without --delete closes; that close is the destructive
// act, and its error was dropped on the floor. Ordinary views are swept before
// the binding, so a binding whose writes fail loses nothing of its own and takes
// the retained copies with it: the frozen twins close, the live tree stays open,
// and the command prints the count of what it managed and exits 0. Delete mode
// and delete-source both fail loud here; the default mode of the same command
// must too.
func TestWorkflowDeleteReportsAFailedCloseInTheBinding(t *testing.T) {
	cityPath, rootID, bindingOnlyID, _, binding := relocatedWorkflowCity(t)
	installFaultingClassBinding(t, cityPath, nil, errors.New("database is locked mid-close"))

	var stdout, stderr bytes.Buffer
	code := cmdWorkflowDelete(rootID, true, false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc workflow delete exited 0 after the binding refused every close: %s%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "database is locked mid-close") {
		t.Errorf("the failure does not carry the store's cause: %q", stderr.String())
	}
	for _, id := range []string{rootID, bindingOnlyID} {
		live, err := binding.Get(id)
		if err != nil {
			t.Fatalf("reading %s back from the binding: %v", id, err)
		}
		if live.Status == "closed" {
			t.Fatalf("the fixture's binding closed %s after all; this row cannot distinguish a reported failure from a real sweep", id)
		}
	}
}

// TestWorkflowDeleteSourceRefusesWhenTheBindingFaultsOnARigLeg is the ga-gqc9e
// regression on the gap the selected-store rule leaves open.
//
// The source-workflow collector tolerates a per-store scan failure so one sick
// rig cannot take the whole walk down, and it makes an exception only for the
// store the walk was told to work in. The class binding is never that store on a
// rig-selected sweep: the binding's rows are the city's and carry the CITY's
// ref, so `--rig frontend` puts it permanently outside the strict set. The sweep
// then warns, closes the retained frozen copy it could reach, prints
// result=cleaned and exits 0 while the tree the city is running stays live in
// the store it could not read.
//
// The topology here is the ordinary one, not a contrivance: the source bead
// lives in a rig, the workflow's control beads live in the city's coordination
// class, and relocation moved that class into the binding. So the leg that
// carries the rig's ref is the same leg that has to reach the binding.
//
// A binding is not a rig whose absence merely degrades coverage — on a converged
// city it IS the city's store. Whether it can answer is not a question about
// which leg the walk is on.
func TestWorkflowDeleteSourceRefusesWhenTheBindingFaultsOnARigLeg(t *testing.T) {
	cityPath, rootID, _, work, _ := relocatedWorkflowCity(t)
	binding := soleClassBindingStore(t, cityPath)
	const sourceID = "rig-src-1"
	rig := rigHoldingID(t, cityPath, sourceID, "the rig's source bead", "task")
	for _, store := range []beads.Store{work, binding} {
		if err := store.SetMetadataBatch(rootID, map[string]string{
			beadmeta.SourceBeadIDMetadataKey:   sourceID,
			beadmeta.SourceStoreRefMetadataKey: "rig:frontend",
		}); err != nil {
			t.Fatalf("stamping the root's source identity: %v", err)
		}
	}
	if err := rig.SetMetadata(sourceID, "workflow_id", rootID); err != nil {
		t.Fatalf("stamping the rig source bead's workflow_id: %v", err)
	}
	installFaultingClassBinding(t, cityPath, errors.New("connection reset mid-sweep"), nil)

	var stdout, stderr bytes.Buffer
	code := cmdWorkflowDeleteSource(sourceID, sourceWorkflowStoreSelector{storeRef: "rig:frontend"}, true, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("gc workflow delete-source exited %d, want 1: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "connection reset mid-sweep") {
		t.Errorf("the refusal does not carry the fault that caused it: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "result=cleaned") {
		t.Errorf("delete-source reported a clean sweep over a binding it could not read:\n%s", stdout.String())
	}
	retained, err := work.Get(rootID)
	if err != nil {
		t.Fatalf("reading %s back from the work store: %v", rootID, err)
	}
	if retained.Status == "closed" {
		t.Errorf("delete-source closed the copies it could reach and reported success over an unread binding")
	}
	source, err := rig.Get(sourceID)
	if err != nil {
		t.Fatalf("reading the rig's source bead back: %v", err)
	}
	if strings.TrimSpace(source.Metadata["workflow_id"]) == "" {
		t.Errorf("the source bead's workflow_id was cleared, which tells the next sling the workflow is gone")
	}
}

// TestDeleteWorkflowMatchesErasesTheBindingThroughItsStoreHandle covers the arm
// `gc workflow delete --delete` takes on a converged city.
//
// A relocated class binding has no directory, so the `bd delete` invocation every
// other view is erased through has nowhere to run. The binding is erased through
// its store handle instead, and that branch had no test at all: it could have
// deleted nothing, or deleted through the wrong store, and the command would
// still have printed a count and exited 0.
//
// The row does NOT pin sweepOrder's ordering: with every store healthy the sweep
// reaches all of them whichever way round it goes, so this row passes against
// the identity order too. The order is falsified by the refusal row below, where
// the binding's fault is what the retained copies have to be spared by.
func TestDeleteWorkflowMatchesErasesTheBindingThroughItsStoreHandle(t *testing.T) {
	binding := beads.NewMemStore()
	live, err := binding.Create(beads.Bead{Title: "the binding's live root", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the binding's root: %v", err)
	}
	retained := beads.NewMemStore()
	frozen, err := retained.Create(beads.Bead{Title: "the retained frozen root", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the retained root: %v", err)
	}

	var bdInvokedFor []string
	deleted, err := deleteWorkflowMatches([]workflowStoreMatch{
		{
			store: retained,
			beads: []beads.Bead{frozen},
			label: "city",
			path:  "/city",
			role:  convoyViewMigrationSource,
			runner: func(string, string, ...string) ([]byte, error) {
				bdInvokedFor = append(bdInvokedFor, "city")
				return nil, nil
			},
		},
		{
			store: binding,
			beads: []beads.Bead{live},
			label: convoyBindingViewPath,
			path:  convoyBindingViewPath,
			role:  convoyViewClassBinding,
			runner: func(string, string, ...string) ([]byte, error) {
				t.Error("the binding was erased through a bd invocation; it has no directory to run one in")
				return nil, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("deleteWorkflowMatches: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (one row in each store)", deleted)
	}
	if _, err := binding.Get(live.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("the binding's live root is still there after --delete: %v", err)
	}
	if want := []string{"city"}; !slices.Equal(bdInvokedFor, want) {
		t.Errorf("bd was invoked for %v, want %v", bdInvokedFor, want)
	}
}

// TestDeleteWorkflowMatchesRefusesABindingThatWillNotDelete pins the fault half
// of the same arm: the binding's store handle is the only access path to it, so
// a delete it rejects has to stop the sweep before the frozen twins go.
//
// This is also the row that falsifies sweepOrder on the delete arm. The binding
// is listed second, as the federation appends it, and its delete fails — so the
// retained copy survives only if the sweep put the binding first. Neutered to
// the identity order, the `bd delete` guard on the retained view fires.
func TestDeleteWorkflowMatchesRefusesABindingThatWillNotDelete(t *testing.T) {
	binding := beads.NewMemStore()
	live, err := binding.Create(beads.Bead{Title: "the binding's live root", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the binding's root: %v", err)
	}
	retained := beads.NewMemStore()
	frozen, err := retained.Create(beads.Bead{Title: "the retained frozen root", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the retained root: %v", err)
	}
	// An id the binding does not hold: the per-bead delete reports it, which is
	// the shape a binding that cannot answer for its rows produces.
	live.ID = "gc-does-not-exist"

	_, err = deleteWorkflowMatches([]workflowStoreMatch{
		{
			store: retained,
			beads: []beads.Bead{frozen},
			label: "city",
			path:  "/city",
			role:  convoyViewMigrationSource,
			runner: func(string, string, ...string) ([]byte, error) {
				t.Error("the sweep erased the retained copy after the binding refused; that is the partial sweep")
				return nil, nil
			},
		},
		{
			store: binding,
			beads: []beads.Bead{live},
			label: convoyBindingViewPath,
			path:  convoyBindingViewPath,
			role:  convoyViewClassBinding,
		},
	})
	if err == nil {
		t.Fatal("deleteWorkflowMatches returned nil after the binding could not delete its rows")
	}
	if !strings.Contains(err.Error(), convoyBindingViewPath) {
		t.Errorf("the refusal does not name the store that failed: %v", err)
	}
}

// unhonoredCloseStore reports a close count it did not apply, which is the one
// store shape the verification pass exists for.
//
// A store that accepts CloseAll and leaves the rows open returns exactly what a
// store that honored it returns — same count, nil error — so the sweep cannot
// tell them apart from the write's own answer. effect is what the close ACTUALLY
// does to the rows; nil means nothing at all. getErr faults the re-read instead,
// which is the other way the verification can fail to get an answer.
type unhonoredCloseStore struct {
	beads.Store
	effect func(ids []string)
	getErr error
}

func (s *unhonoredCloseStore) CloseAll(ids []string, _ map[string]string) (int, error) {
	if s.effect != nil {
		s.effect(ids)
	}
	return len(ids), nil
}

func (s *unhonoredCloseStore) Get(id string) (beads.Bead, error) {
	if s.getErr != nil {
		return beads.Bead{}, s.getErr
	}
	return s.Store.Get(id)
}

// TestCloseWorkflowMatchesVerifiesTheRowsItWasToldItClosed pins every arm of the
// re-read that follows the default mode's close.
//
// The close IS the destructive act of `gc workflow delete`, and its only report
// is a count the store chooses. A store that accepts the write and does not
// apply it — a stale view, a rejected transaction retried into a no-op, a
// binding fronting a class it can no longer write — returns the identical count
// and nil error as one that honored it, so the command would print "Closed 2
// open beads" over a workflow that is still running. The re-read is the only
// thing that can tell those apart, and without a store that lies there is
// nothing in the suite it can be told apart FROM.
func TestCloseWorkflowMatchesVerifiesTheRowsItWasToldItClosed(t *testing.T) {
	readFailed := errors.New("the store went away after the write")
	tests := []struct {
		name    string
		effect  func(store beads.Store) func(ids []string)
		getErr  error
		wantErr string
	}{
		{
			name: "a close the store did not apply is refused",
			// The rows stay exactly as they were: open, and reported closed.
			wantErr: "is still open after the sweep",
		},
		{
			name: "a close the store honored is accepted",
			effect: func(store beads.Store) func([]string) {
				return func(ids []string) {
					for _, id := range ids {
						if err := store.Close(id); err != nil {
							panic(err)
						}
					}
				}
			},
		},
		{
			name: "a row the store deleted instead counts as swept",
			// Gone is what the sweep wanted; --delete reaches this shape, and
			// so does a store that erases on close.
			effect: func(store beads.Store) func([]string) {
				return func(ids []string) {
					for _, id := range ids {
						if err := store.Delete(id); err != nil {
							panic(err)
						}
					}
				}
			},
		},
		{
			name:    "a re-read that faults is refused rather than assumed clean",
			getErr:  readFailed,
			wantErr: readFailed.Error(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backing := beads.NewMemStore()
			row, err := backing.Create(beads.Bead{Title: "a matched workflow bead", Type: "task"})
			if err != nil {
				t.Fatalf("seeding the matched bead: %v", err)
			}
			store := &unhonoredCloseStore{Store: backing, getErr: tc.getErr}
			if tc.effect != nil {
				store.effect = tc.effect(backing)
			}

			closed, err := closeWorkflowMatches([]workflowStoreMatch{{
				store: store,
				beads: []beads.Bead{row},
				label: "city",
				path:  "/city",
				role:  convoyViewMigrationSource,
			}})
			if closed != 1 {
				t.Errorf("closed = %d, want the 1 the store reported; the count is what the command prints either way", closed)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("closeWorkflowMatches refused a sweep the store honored: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("closeWorkflowMatches returned nil; the command would print %d closed over rows it did not close", closed)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("the refusal is %v, want it to name %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "city") {
				t.Errorf("the refusal does not name the store that failed: %v", err)
			}
		})
	}
}

// TestCloseWorkflowMatchesClosesTheBindingBeforeTheRetainedCopies pins
// sweepOrder on the CLOSE arm, which is the arm `gc workflow delete` takes by
// default.
//
// sweepOrder's promise is stated for mutations generally, not for --delete
// alone: a fault before the binding is touched sweeps nothing, and a fault after
// it means the workflow really did stop. The close arm needs that at least as
// much as the delete arm does, because closing the frozen twins while the live
// tree keeps running is the partial sweep that LOOKS finished — the retained
// copies are what a reader sees.
//
// So the binding is listed second here, as the federation appends it, and the
// retained view faults. Swept binding-first, the live tree is closed before the
// fault; swept in the order given, the fault lands first and the binding is
// never reached.
func TestCloseWorkflowMatchesClosesTheBindingBeforeTheRetainedCopies(t *testing.T) {
	binding := beads.NewMemStore()
	live, err := binding.Create(beads.Bead{Title: "the binding's live root", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the binding's root: %v", err)
	}
	retained := beads.NewMemStore()
	frozen, err := retained.Create(beads.Bead{Title: "the retained frozen root", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the retained root: %v", err)
	}

	_, err = closeWorkflowMatches([]workflowStoreMatch{
		{
			store: &faultingClassStore{Store: retained, writeErr: errors.New("database is locked mid-close")},
			beads: []beads.Bead{frozen},
			label: "city",
			path:  "/city",
			role:  convoyViewMigrationSource,
		},
		{
			store: binding,
			beads: []beads.Bead{live},
			label: convoyBindingViewPath,
			path:  convoyBindingViewPath,
			role:  convoyViewClassBinding,
		},
	})
	if err == nil {
		t.Fatal("closeWorkflowMatches returned nil after the retained view refused every close")
	}
	current, err := binding.Get(live.ID)
	if err != nil {
		t.Fatalf("reading the binding's root back: %v", err)
	}
	if current.Status != "closed" {
		t.Errorf("the binding's live root is still %s; the sweep faulted on the retained copies before it reached the tree the city is running", current.Status)
	}
}

// TestWorkflowDeleteWithDeleteBeadsErasesTheRelocatedTree is the end-to-end half:
// `gc workflow delete --delete --force` on a converged city, which no test
// reached at all.
//
// The city view is erased through a `bd delete` this fixture's foreign-provider
// city cannot serve, so the command reports that store as a failure. That is
// what makes the row worth having: the binding is swept FIRST, so its live tree
// is gone before the failing store is ever touched.
func TestWorkflowDeleteWithDeleteBeadsErasesTheRelocatedTree(t *testing.T) {
	_, rootID, bindingOnlyID, _, binding := relocatedWorkflowCity(t)

	var stdout, stderr bytes.Buffer
	cmdWorkflowDelete(rootID, true, true, &stdout, &stderr)
	if !strings.Contains(stdout.String(), convoyBindingViewPath) {
		t.Fatalf("the sweep never named the class binding:\n%s%s", stdout.String(), stderr.String())
	}
	for _, id := range []string{rootID, bindingOnlyID} {
		if _, err := binding.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("the binding still holds %s after --delete: %v", id, err)
		}
	}
}

// TestFindUniqueBeadAcrossStoresViewRefusesABindingRigCollision pins the
// uniqueness refusal on the SOURCE-workflow resolver's binding leg.
//
// The refusal itself is pinned for the convoy resolver, but this resolver is the
// one delete-source and reopen-source WRITE through: it returns the view whose
// store the source bead's workflow_id is cleared in. Resolving a rig collision
// silently from the binding here clears the metadata on one ledger's row and
// leaves the other still pointing at a workflow that no longer exists.
func TestFindUniqueBeadAcrossStoresViewRefusesABindingRigCollision(t *testing.T) {
	cityPath, rootID, _, _, _ := relocatedWorkflowCity(t)
	rigHoldingID(t, cityPath, rootID, "a rig row minted under the same id", "task")

	view, _, err := findUniqueBeadAcrossStoresView(cityPath, rootID)
	if err == nil {
		t.Fatalf("a binding/rig collision resolved to %s; the write that follows lands on one ledger and not the other", view.path)
	}
	if !strings.Contains(err.Error(), "exists in multiple stores") {
		t.Errorf("the refusal reads %v, want the uniqueness wording", err)
	}
}

// TestFindUniqueBeadAcrossStoresViewKeepsTheBindingOverTheRetainedCopy is the
// control for the test above, and the reason the collision probe skips the city
// store: the city is where the migration RETAINED its copies, so it holds the
// same id on every converged city. A probe that counted it would refuse every
// source-workflow command on exactly the cities that finished migrating.
func TestFindUniqueBeadAcrossStoresViewKeepsTheBindingOverTheRetainedCopy(t *testing.T) {
	cityPath, rootID, _, _, _ := relocatedWorkflowCity(t)

	view, bead, err := findUniqueBeadAcrossStoresView(cityPath, rootID)
	if err != nil {
		t.Fatalf("a dual-resident id resolved to %v; the retained city copy is the migration working, not a collision", err)
	}
	if view.role != convoyViewClassBinding {
		t.Errorf("the resolver returned the %v view at %s, want the class binding", view.role, view.path)
	}
	if bead.Title != "the binding's live workflow" {
		t.Errorf("the resolver answered with %q, want the binding's live row", bead.Title)
	}
}

// orphanedSourceRoots points the fixture city's relocated roots at a source
// bead id no store holds, which is the only shape that reaches the multi-store
// guard: with the source bead gone there is no store to resolve a selector
// from, so delete-source runs unscoped and has to decide from the matches alone.
func orphanedSourceRoots(t *testing.T, rootID string, stores ...beads.Store) string {
	t.Helper()
	const orphaned = "spent-source-1"
	for _, store := range stores {
		if err := store.SetMetadata(rootID, beadmeta.SourceBeadIDMetadataKey, orphaned); err != nil {
			t.Fatalf("stamping the root's source bead id: %v", err)
		}
	}
	return orphaned
}

// TestWorkflowDeleteSourceSweepsAConvergedCityAsOneScope pins the difference
// between "matched two STORES" and "matched two SCOPES".
//
// The multi-store guard refuses when it cannot tell which workflow the operator
// means. A converged city is never that case: the binding and the city's work
// ledger hold one workflow in two copies, deliberately, and the binding's rows
// are the city's. Counting matches instead of scopes takes delete-source away
// from every city that has migrated — and it does so only on the unscoped path,
// which is exactly the path an operator lands on once the source bead is gone.
func TestWorkflowDeleteSourceSweepsAConvergedCityAsOneScope(t *testing.T) {
	_, rootID, bindingOnlyID, work, binding := relocatedWorkflowCity(t)
	orphaned := orphanedSourceRoots(t, rootID, work, binding)

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(orphaned, sourceWorkflowStoreSelector{}, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc workflow delete-source exited %d on one scope in two copies: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result=cleaned") {
		t.Errorf("delete-source did not report a clean sweep:\n%s", stdout.String())
	}
	for _, id := range []string{rootID, bindingOnlyID} {
		swept, err := binding.Get(id)
		if err != nil {
			t.Fatalf("reading %s back from the binding: %v", id, err)
		}
		if swept.Status != "closed" {
			t.Errorf("the binding's %s is %q after delete-source, want closed", id, swept.Status)
		}
	}
}

// TestWorkflowDeleteSourceRefusesRootsInTwoScopes is the control for the test
// above: the guard it relaxes for the binding still has to fire for a rig, which
// is never a migration target and so is a genuinely different workflow.
func TestWorkflowDeleteSourceRefusesRootsInTwoScopes(t *testing.T) {
	cityPath, rootID, _, work, binding := relocatedWorkflowCity(t)
	orphaned := orphanedSourceRoots(t, rootID, work, binding)

	const rigRootID = "rig-root-1"
	rig := rigHoldingID(t, cityPath, rigRootID, "the rig's own live workflow", "task")
	if err := rig.SetMetadataBatch(rigRootID, map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.SourceBeadIDMetadataKey:    orphaned,
	}); err != nil {
		t.Fatalf("making the rig's row a workflow root: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(orphaned, sourceWorkflowStoreSelector{}, true, false, &stdout, &stderr); code != 1 {
		t.Fatalf("gc workflow delete-source exited %d across two scopes, want 1: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "live roots in multiple stores") {
		t.Errorf("the refusal does not say which ambiguity it hit:\n%s", stderr.String())
	}
	live, err := binding.Get(rootID)
	if err != nil {
		t.Fatalf("reading %s back from the binding: %v", rootID, err)
	}
	if live.Status == "closed" {
		t.Errorf("the refusal still closed the city's workflow; the operator was never asked which one they meant")
	}
}

// relocatedSourceBead plants a source bead the way `gc storage migrate` leaves
// one: the live row in the class binding, and the copy the migration retained
// still sitting in the work ledger under the same id.
//
// Both copies name the workflow, which is what makes a wrong clear visible.
// Clearing either alone leaves the pair disagreeing, and only the binding's
// answer is the one the next resolve of this id reads.
//
// The residency verdict is asserted here rather than in each row, because a
// fixture whose binding does not own the id has built an ordinary city and every
// assertion below it would pass for the wrong reason.
func relocatedSourceBead(t *testing.T, cityPath string, work, binding beads.Store, workflowID string) string {
	t.Helper()
	shape := beads.Bead{Title: "the retained frozen source bead", Type: "task", Status: "in_progress"}
	twin, err := work.Create(shape)
	if err != nil {
		t.Fatalf("seeding the retained source bead in the work store: %v", err)
	}
	carried := shape
	carried.ID = twin.ID
	carried.Title = "the binding's live source bead"
	if _, err := migrationSeed(binding, carried); err != nil {
		t.Fatalf("carrying the source bead across to the class binding: %v", err)
	}
	for _, store := range []beads.Store{work, binding} {
		if err := store.SetMetadata(twin.ID, "workflow_id", workflowID); err != nil {
			t.Fatalf("stamping the source bead's workflow_id: %v", err)
		}
	}
	if _, ownedByBinding, err := cliByIDBindingOwner(cityPath, twin.ID); err != nil || !ownedByBinding {
		t.Fatalf("the residency contract answers %s from the work ledger (err=%v); this fixture says nothing about the owning copy", twin.ID, err)
	}
	return twin.ID
}

// stampSourceIdentity points the fixture's relocated roots at a source bead, in
// every copy of them, so the sweep has something to close in both stores.
func stampSourceIdentity(t *testing.T, rootID, sourceBeadID string, stores ...beads.Store) {
	t.Helper()
	for _, store := range stores {
		if err := store.SetMetadata(rootID, beadmeta.SourceBeadIDMetadataKey, sourceBeadID); err != nil {
			t.Fatalf("stamping the root's source bead id: %v", err)
		}
	}
}

// sourceWorkflowID reads one copy's workflow_id, so a row can say which copy it
// is talking about.
func sourceWorkflowID(t *testing.T, store beads.Store, sourceBeadID string) string {
	t.Helper()
	bead, err := store.Get(sourceBeadID)
	if err != nil {
		t.Fatalf("reading %s back: %v", sourceBeadID, err)
	}
	return strings.TrimSpace(bead.Metadata["workflow_id"])
}

// TestWorkflowDeleteSourceClearsTheOwningCopyForACitySelector is the ga-4kivg
// regression.
//
// The selector says which store's workflow is being SWEPT. It does not say which
// copy of the source bead the next reader consults — that is the residency
// contract's answer, and on a converged city the two are different stores. A
// clear that follows the selector writes the frozen twin while the binding's
// live row goes on naming a workflow that was just swept, so the next resolve
// answers from the binding, still sees a workflow, and refuses the re-sling as
// already-running against a tree that no longer exists.
func TestWorkflowDeleteSourceClearsTheOwningCopyForACitySelector(t *testing.T) {
	cityPath, rootID, _, work, binding := relocatedWorkflowCity(t)
	sourceID := relocatedSourceBead(t, cityPath, work, binding, rootID)
	stampSourceIdentity(t, rootID, sourceID, work, binding)

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(sourceID, sourceWorkflowStoreSelector{storeRef: "city"}, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc workflow delete-source exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "metadata_cleared=true") {
		t.Errorf("delete-source did not report clearing the source bead's metadata:\n%s", stdout.String())
	}
	if got := sourceWorkflowID(t, binding, sourceID); got != "" {
		t.Errorf("the binding's live source row still names workflow %q, so the next sling is refused against a workflow this command just swept", got)
	}
	if got := sourceWorkflowID(t, work, sourceID); got != "" {
		t.Errorf("the retained frozen copy still names workflow %q; the two copies disagree", got)
	}
	_, resolved, err := findUniqueBeadAcrossStoresView(cityPath, sourceID)
	if err != nil {
		t.Fatalf("resolving %s after the sweep: %v", sourceID, err)
	}
	if got := strings.TrimSpace(resolved.Metadata["workflow_id"]); got != "" {
		t.Errorf("the next resolve of %s reports workflow %q, want none", sourceID, got)
	}
}

// TestWorkflowDeleteSourceClearsTheSelectedStoreOnAnUnsplitCity is the control.
//
// A city that relocates nothing has one copy of the source bead, so the copy the
// residency contract owns IS the store the selector named and the command must
// behave exactly as it did before — same store written, same line printed,
// nothing on stderr. Without this row the fix could reroute every city's clear
// through a binding lane and only the converged rows would notice.
func TestWorkflowDeleteSourceClearsTheSelectedStoreOnAnUnsplitCity(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core")
	writeCatalogFile(t, cityDir, ".gc/site.toml", "workspace_name = \"test-city\"\n")
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
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.SourceBeadIDMetadataKey:    source.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	if err := store.SetMetadata(source.ID, "workflow_id", root.ID); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(source.ID, sourceWorkflowStoreSelector{storeRef: "city"}, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc workflow delete-source exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	want := fmt.Sprintf("result=cleaned source_bead_id=%s matched_roots=1 matched_beads=1 closed=1 deleted=0 metadata_cleared=true\n", source.ID)
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	if got := sourceWorkflowID(t, store, source.ID); got != "" {
		t.Errorf("the source bead's workflow_id = %q, want empty", got)
	}
}

// TestWorkflowDeleteSourceClearsTheRigsCopyForARigSelector is the other control.
//
// The by-id residency walk carries no rig legs on purpose, so a rig-owned source
// bead has no binding answer and the copy the contract owns is the one the
// selector named. Routing the clear through the resolver must not move a rig's
// write anywhere.
func TestWorkflowDeleteSourceClearsTheRigsCopyForARigSelector(t *testing.T) {
	cityPath, rootID, _, work, binding := relocatedWorkflowCity(t)
	const sourceID = "rig-src-1"
	rig := rigHoldingID(t, cityPath, sourceID, "the rig's source bead", "task")
	for _, store := range []beads.Store{work, binding} {
		if err := store.SetMetadataBatch(rootID, map[string]string{
			beadmeta.SourceBeadIDMetadataKey:   sourceID,
			beadmeta.SourceStoreRefMetadataKey: "rig:frontend",
		}); err != nil {
			t.Fatalf("stamping the root's source identity: %v", err)
		}
	}
	if err := rig.SetMetadata(sourceID, "workflow_id", rootID); err != nil {
		t.Fatalf("stamping the rig source bead's workflow_id: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(sourceID, sourceWorkflowStoreSelector{storeRef: "rig:frontend"}, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc workflow delete-source exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "metadata_cleared=true") {
		t.Errorf("delete-source did not report clearing the rig's source bead:\n%s", stdout.String())
	}
	if got := sourceWorkflowID(t, rig, sourceID); got != "" {
		t.Errorf("the rig's source bead still names workflow %q; the clear went somewhere else", got)
	}
}

// TestWorkflowDeleteSourceFailsWhenTheOwningCopyCannotBeCleared is the fault row.
//
// The owning copy is the one every later reader consults, so failing to clear it
// is not a degraded success. The command has to say so and leave the operator
// with a source bead that still names its workflow in BOTH copies — a run that
// cleared the frozen twin alone would report a fault while having already made
// the two copies disagree.
//
// The roots carry no source identity, so the sweep matches nothing and the
// already_clean arm runs the clear on its own. That is what keeps this row about
// the metadata write rather than about a close that failed first.
func TestWorkflowDeleteSourceFailsWhenTheOwningCopyCannotBeCleared(t *testing.T) {
	cityPath, rootID, _, work, binding := relocatedWorkflowCity(t)
	sourceID := relocatedSourceBead(t, cityPath, work, binding, rootID)
	installFaultingClassBinding(t, cityPath, nil, errors.New("attempt to write a readonly database"))

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(sourceID, sourceWorkflowStoreSelector{storeRef: "city"}, true, false, &stdout, &stderr); code != 1 {
		t.Fatalf("gc workflow delete-source exited %d over an unwritable owning copy, want 1: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "readonly database") {
		t.Errorf("the failure does not carry the fault that caused it: %q", stderr.String())
	}
	if got := sourceWorkflowID(t, work, sourceID); got != rootID {
		t.Errorf("the retained frozen copy's workflow_id = %q, want %q; the command cleared the twin alone and then failed", got, rootID)
	}
	if got := sourceWorkflowID(t, binding, sourceID); got != rootID {
		t.Errorf("the binding's live copy's workflow_id = %q, want %q", got, rootID)
	}
}

// TestWorkflowDeleteSourceReportsAnUnopenableTwinWithoutFailing pins the twin's
// OPEN as best-effort, the same as its write.
//
// The retained ledger of a converged city going unreadable — read-only, moved
// aside, dropped once the migration was believed done — is a normal end state,
// not a fault the operator can act on from here. The copy the residency contract
// owns is the binding, and it answered; failing the run because the frozen twin
// could not be opened to clear a value no reader consults would take
// delete-source away from exactly the city the migration produced.
//
// The rig is what makes the sweep survive the same fault: the store scan skips
// what it cannot open and errors only when NOTHING opened, so a converged city
// with no second store fails before the resolution is ever reached. With one
// openable store the run gets as far as the clear, which is the arm this row is
// about.
func TestWorkflowDeleteSourceReportsAnUnopenableTwinWithoutFailing(t *testing.T) {
	cityPath, rootID, _, work, binding := relocatedWorkflowCity(t)
	sourceID := relocatedSourceBead(t, cityPath, work, binding, rootID)
	rigHoldingID(t, cityPath, "rig-src-1", "the rig's unrelated bead", "task")
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "beads.json"), []byte("the retained ledger is no longer readable"), 0o644); err != nil {
		t.Fatalf("faulting the retained ledger's open: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(sourceID, sourceWorkflowStoreSelector{}, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc workflow delete-source exited %d over an unopenable retained twin, want 0: %s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "metadata_cleared=true") {
		t.Errorf("delete-source did not clear the owning copy it could reach:\n%s", stdout.String())
	}
	if got := sourceWorkflowID(t, binding, sourceID); got != "" {
		t.Errorf("the binding's live source row still names workflow %q; the twin's open took the owning copy's clear down with it", got)
	}
	if !strings.Contains(stderr.String(), "store=city workflow_id_clear_error=") {
		t.Errorf("the run said nothing about the twin it could not reach: %q", stderr.String())
	}
}

// TestWorkflowReopenSourceReopensTheOwningCopyForACitySelector is ga-4kivg on
// the other writer.
//
// delete-source and reopen-source are the two commands that WRITE the source
// bead's metadata, and they resolved the store to write through the same way.
// Reopening the frozen twin leaves the binding's live row closed and still bound
// to its workflow, so the bead the city actually reads is neither reopened nor
// released — the operator is told it was.
func TestWorkflowReopenSourceReopensTheOwningCopyForACitySelector(t *testing.T) {
	cityPath, rootID, _, work, binding := relocatedWorkflowCity(t)
	sourceID := relocatedSourceBead(t, cityPath, work, binding, rootID)
	for _, store := range []beads.Store{work, binding} {
		closed := "closed"
		if err := store.Update(sourceID, beads.UpdateOpts{Status: &closed}); err != nil {
			t.Fatalf("closing the source bead: %v", err)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowReopenSource(sourceID, sourceWorkflowStoreSelector{storeRef: "city"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc workflow reopen-source exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	live, err := binding.Get(sourceID)
	if err != nil {
		t.Fatalf("reading the binding's source row back: %v", err)
	}
	if live.Status != "open" {
		t.Errorf("the binding's live source row is %q after reopen-source, want open", live.Status)
	}
	if got := strings.TrimSpace(live.Metadata["workflow_id"]); got != "" {
		t.Errorf("the binding's live source row still names workflow %q after being reopened", got)
	}
	if got := sourceWorkflowID(t, work, sourceID); got != "" {
		t.Errorf("the retained frozen copy still names workflow %q; the two copies disagree", got)
	}
}

// sourceWorkflowLockFiles lists the lock files the source-workflow lock has
// created under a city. Every scope's lock lives in this one directory and is
// named for a hash of the scope, so the set of files IS the set of scopes that
// have been locked.
func sourceWorkflowLockFiles(t *testing.T, cityPath string) []string {
	t.Helper()
	dir := filepath.Join(citylayout.RuntimeDataDir(cityPath), "sling-source-locks")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("listing the source-workflow lock dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// takeSourceWorkflowLock takes and releases the lock for one scope, so the test
// learns which file that scope hashes to without reimplementing the hash.
func takeSourceWorkflowLock(t *testing.T, cityPath, scope, sourceBeadID string) {
	t.Helper()
	if err := sourceworkflow.WithLock(context.Background(), cityPath, scope, sourceBeadID, func() error { return nil }); err != nil {
		t.Fatalf("taking the source-workflow lock on scope %q: %v", scope, err)
	}
}

// TestWorkflowDeleteSourceLocksTheCityScopeForABindingTarget pins WHICH scope a
// binding-resolved delete-source excludes on.
//
// The scope is hashed into a lock filename, so the binding's own view path is a
// valid scope that simply hashes somewhere else. Locking it fails nothing and
// prints nothing: a binding-resolved run and a city-resolved run over the same
// workflow each take a lock, neither sees the other, and the mutual exclusion
// the lock exists for is gone with no symptom until two of them interleave.
//
// So the assertion is on the lock file itself. The command's lock must be the
// one the CITY scope hashes to, and the binding's view path must hash to a
// different file — which is what makes the first half mean anything.
func TestWorkflowDeleteSourceLocksTheCityScopeForABindingTarget(t *testing.T) {
	cityPath, rootID, _, work, binding := relocatedWorkflowCity(t)

	source, err := binding.Create(beads.Bead{Title: "the source bead, relocated with its class", Type: "task", Status: "in_progress"})
	if err != nil {
		t.Fatalf("seeding the source bead in the binding: %v", err)
	}
	if err := binding.SetMetadata(source.ID, "workflow_id", rootID); err != nil {
		t.Fatalf("stamping the source bead's workflow_id: %v", err)
	}
	for _, store := range []beads.Store{work, binding} {
		if err := store.SetMetadata(rootID, beadmeta.SourceBeadIDMetadataKey, source.ID); err != nil {
			t.Fatalf("stamping the root's source bead id: %v", err)
		}
	}
	view, _, err := findUniqueBeadAcrossStoresView(cityPath, source.ID)
	if err != nil {
		t.Fatalf("resolving the source bead: %v", err)
	}
	if view.role != convoyViewClassBinding {
		t.Fatalf("the source bead resolved to the %v view at %s; this test only says anything about a binding target", view.role, view.path)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(source.ID, sourceWorkflowStoreSelector{}, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc workflow delete-source exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	locked := sourceWorkflowLockFiles(t, cityPath)
	if len(locked) != 1 {
		t.Fatalf("the command left %d lock files (%v), want exactly the one it took", len(locked), locked)
	}

	takeSourceWorkflowLock(t, cityPath, cityPath, source.ID)
	if after := sourceWorkflowLockFiles(t, cityPath); !slices.Equal(after, locked) {
		t.Errorf("the city scope hashes to %v but the command locked %v; a city-resolved run would not exclude this one", after, locked)
	}
	takeSourceWorkflowLock(t, cityPath, convoyBindingViewPath, source.ID)
	if after := sourceWorkflowLockFiles(t, cityPath); len(after) != 2 {
		t.Errorf("the binding's view path hashes to the same file as the city scope (%v), so this test cannot tell them apart", after)
	}
}
