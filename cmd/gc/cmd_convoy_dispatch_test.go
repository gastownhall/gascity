package main

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/runtime"
)

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
	if control.Metadata["gc.routed_to"] != config.ControlDispatcherAgentName {
		t.Fatalf("review scope-check gc.routed_to = %q, want %q", control.Metadata["gc.routed_to"], config.ControlDispatcherAgentName)
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
	if control.Metadata["gc.routed_to"] != config.ControlDispatcherAgentName {
		t.Fatalf("control gc.routed_to = %q, want %q", control.Metadata["gc.routed_to"], config.ControlDispatcherAgentName)
	}
	if control.Metadata[graphExecutionRouteMetaKey] != "frontend/reviewer" {
		t.Fatalf("control execution route = %q, want frontend/reviewer", control.Metadata[graphExecutionRouteMetaKey])
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

	// The tiered query has sh -c wrapper; workflowServeQuery replaces the
	// first --limit=1 with --limit=20 for scan width.
	cdAgent := config.Agent{Name: config.ControlDispatcherAgentName}
	wantQuery := workflowServeQuery(cdAgent.EffectiveWorkQuery())
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
	controlDispatcherServe = func(beadID string, _ io.Writer, _ io.Writer) error {
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

func TestRunWorkflowServeUsesGCTemplateForSessionContext(t *testing.T) {
	clearGCEnv(t)
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
path = "rigrepo"

[[agent]]
name = "polecat"
dir = "rigrepo"

[agent.pool]
min = 0
max = 5
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

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
	controlDispatcherServe = func(_ string, _ io.Writer, _ io.Writer) error {
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
	controlDispatcherServe = func(beadID string, _ io.Writer, _ io.Writer) error {
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
	controlDispatcherServe = func(beadID string, _ io.Writer, _ io.Writer) error {
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

func TestRunWorkflowServeReturnsQueryError(t *testing.T) {
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
	controlDispatcherServe = func(string, io.Writer, io.Writer) error {
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
	controlDispatcherServe = func(beadID string, _ io.Writer, _ io.Writer) error {
		processed = append(processed, beadID)
		return os.ErrDeadlineExceeded
	}

	wfcAgent := config.Agent{Name: "control-dispatcher", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)}
	err := runWorkflowServeFollow(
		wfcAgent,
		t.TempDir(),
		nil,
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), os.ErrDeadlineExceeded.Error()) {
		t.Fatalf("runWorkflowServeFollow error = %v, want wrapped %v", err, os.ErrDeadlineExceeded)
	}
	if !slices.Equal(processed, []string{"gc-ready"}) {
		t.Fatalf("processed beads = %#v, want sweep fallback to process gc-ready", processed)
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
	if control.Metadata["gc.routed_to"] != config.ControlDispatcherAgentName {
		t.Fatalf("review scope-check gc.routed_to = %q, want %q", control.Metadata["gc.routed_to"], config.ControlDispatcherAgentName)
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

[[agent]]
name = "control-dispatcher"
start_command = "echo hello"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
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

	_, _, err := findBeadAcrossStores(cityPath, "gc-missing")
	if err == nil {
		t.Fatal("findBeadAcrossStores() error = nil, want provider failure")
	}
	if !strings.Contains(err.Error(), "getting bead \"gc-missing\" from city store") {
		t.Fatalf("findBeadAcrossStores() error = %v, want city store context", err)
	}
	if strings.Contains(err.Error(), "bead not found") {
		t.Fatalf("findBeadAcrossStores() error = %v, want provider failure instead of masked not-found", err)
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
