package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Surface matrix
// - Workflow routing and direct session delivery
// - Config evolution and re-adoption paths
// - Exit criteria around canonical alias ownership and old pool-era semantics

func TestPhase0WorkflowRouting_TemplateAssigneeRejected(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "claude", Dir: "frontend", MaxActiveSessions: intPtr(1)},
			{Name: "codex", Dir: "frontend", MaxActiveSessions: intPtr(1)},
			{Name: "control-dispatcher", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)

	claudeBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:frontend/claude"},
		Metadata: map[string]string{
			"session_name": "s-gc-claude",
			"alias":        "frontend/claude",
			"template":     "frontend/claude",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create claude session bead: %v", err)
	}
	defaultTarget := "codex"
	recipe := &formula.Recipe{
		Name: "demo",
		Vars: map[string]*formula.VarDef{
			"design_target": {Default: &defaultTarget},
		},
		Steps: []formula.RecipeStep{
			{
				ID:       "demo",
				Title:    "Root",
				Type:     "task",
				IsRoot:   true,
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
			},
			{
				ID:       "demo.design",
				Title:    "Design",
				Type:     "task",
				Assignee: "{{design_target}}",
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "demo.design", DependsOnID: "demo", Type: "parent-child"},
		},
	}

	err = decorateGraphWorkflowRecipe(recipe, graphWorkflowRouteVars(recipe, nil), "frontend/claude", claudeBead.Metadata["session_name"], store, cfg.Workspace.Name, "", cfg)
	if err == nil {
		t.Fatal("decorateGraphWorkflowRecipe unexpectedly succeeded for template assignee")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "use gc.run_target for config routing") {
		t.Fatalf("decorateGraphWorkflowRecipe error = %q, want gc.run_target guidance", got)
	}
}

func TestPhase0WorkflowRouting_DirectNamedSessionAssigneeMaterializesToConcreteBead(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Session:   config.SessionConfig{Provider: "fake"},
		Providers: map[string]config.ProviderSpec{
			"test-agent": {Command: "true"},
		},
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", Provider: "test-agent", MaxActiveSessions: intPtr(1)},
			{Name: "control-dispatcher", Dir: "frontend", Provider: "test-agent", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)},
		},
		NamedSessions: []config.NamedSession{
			{Name: "reviewer", Template: "worker", Dir: "frontend"},
		},
	}
	config.InjectImplicitAgents(cfg)

	recipe := &formula.Recipe{
		Name: "demo",
		Steps: []formula.RecipeStep{
			{
				ID:       "demo",
				Title:    "Root",
				Type:     "task",
				IsRoot:   true,
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
			},
			{
				ID:       "demo.review",
				Title:    "Review",
				Type:     "task",
				Assignee: "reviewer",
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "demo.review", DependsOnID: "demo", Type: "parent-child"},
		},
	}

	if err := decorateGraphWorkflowRecipe(recipe, graphWorkflowRouteVars(recipe, nil), "frontend/worker", "s-test-city-frontend-worker", store, cfg.Workspace.Name, cityPath, cfg); err != nil {
		t.Fatalf("decorateGraphWorkflowRecipe: %v", err)
	}

	review := recipe.StepByID("demo.review")
	if review == nil {
		t.Fatal("review step missing after decorate")
	}
	if review.Assignee == "" || review.Assignee == "reviewer" {
		t.Fatalf("review assignee = %q, want concrete materialized session bead ID", review.Assignee)
	}
	if got := review.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("review gc.routed_to = %q, want empty for direct session target", got)
	}
	bead, err := store.Get(review.Assignee)
	if err != nil {
		t.Fatalf("get materialized session bead %q: %v", review.Assignee, err)
	}
	if !session.IsSessionBeadOrRepairable(bead) {
		t.Fatalf("materialized bead type = %q labels=%v, want session bead", bead.Type, bead.Labels)
	}
	if got := bead.Metadata[namedSessionIdentityMetadata]; got != "frontend/reviewer" {
		t.Fatalf("configured named identity = %q, want frontend/reviewer", got)
	}
	if got := bead.Metadata["alias"]; got != "frontend/reviewer" {
		t.Fatalf("alias = %q, want frontend/reviewer", got)
	}
}

func TestPhase0WorkflowRouting_ControlStepPreservesExecutionConfigLane(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "claude", Dir: "frontend", MaxActiveSessions: intPtr(1)},
			{Name: "codex", Dir: "frontend", MaxActiveSessions: intPtr(1)},
			{Name: "control-dispatcher", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)},
		},
	}
	config.InjectImplicitAgents(cfg)

	claudeBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:frontend/claude"},
		Metadata: map[string]string{
			"session_name": "s-gc-claude",
			"alias":        "frontend/claude",
			"template":     "frontend/claude",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create claude session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:frontend/codex"},
		Metadata: map[string]string{
			"session_name": "s-gc-codex",
			"alias":        "frontend/codex",
			"template":     "frontend/codex",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("create codex session bead: %v", err)
	}

	recipe := &formula.Recipe{
		Name: "demo",
		Steps: []formula.RecipeStep{
			{
				ID:       "demo",
				Title:    "Root",
				Type:     "task",
				IsRoot:   true,
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
			},
			{
				ID:       "demo.run",
				Title:    "Run",
				Type:     "task",
				Metadata: map[string]string{"gc.run_target": "codex"},
			},
			{
				ID:    "demo.run-scope-check",
				Title: "Run Scope Check",
				Type:  "task",
				Metadata: map[string]string{
					"gc.kind":        "scope-check",
					"gc.control_for": "demo.run",
				},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "demo.run", DependsOnID: "demo", Type: "parent-child"},
			{StepID: "demo.run-scope-check", DependsOnID: "demo.run", Type: "blocks"},
		},
	}

	if err := decorateGraphWorkflowRecipe(recipe, graphWorkflowRouteVars(recipe, nil), "frontend/claude", claudeBead.Metadata["session_name"], store, cfg.Workspace.Name, "", cfg); err != nil {
		t.Fatalf("decorateGraphWorkflowRecipe: %v", err)
	}

	check := recipe.StepByID("demo.run-scope-check")
	if check == nil {
		t.Fatal("scope-check step missing after decorate")
	}
	if got := check.Assignee; got != "frontend--control-dispatcher" {
		t.Fatalf("scope-check assignee = %q, want frontend--control-dispatcher", got)
	}
	if got := check.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("scope-check gc.routed_to = %q, want empty direct dispatcher assignee", got)
	}
	if got := check.Metadata[graphExecutionRouteMetaKey]; got != "frontend/codex" {
		t.Fatalf("scope-check execution route = %q, want frontend/codex", got)
	}
}

func TestPhase0ConfigEvolution_RemovedNamedSessionReleasesCanonicalAlias(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()

	cfgNamed := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "witness", Dir: "myrig"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "witness", Dir: "myrig"},
		},
	}
	cfgPlain := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "witness", Dir: "myrig"},
		},
	}

	identity := "myrig/witness"
	sessionName := config.NamedSessionRuntimeName(cfgNamed.Workspace.Name, cfgNamed.Workspace, identity)
	ds := map[string]TemplateParams{
		sessionName: {
			TemplateName:            identity,
			InstanceName:            identity,
			Alias:                   identity,
			Command:                 "claude",
			ConfiguredNamedIdentity: identity,
			ConfiguredNamedMode:     "on_demand",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, ds, sp, allConfiguredDS(ds), cfgNamed, clk, &stderr, false)
	clk.Advance(5 * time.Second)
	syncSessionBeads(cityPath, store, nil, sp, map[string]bool{}, cfgPlain, clk, &stderr, false)

	_, err := resolveSessionIDWithConfig(cityPath, cfgPlain, store, identity)
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDWithConfig(%q) error = %v, want ErrSessionNotFound after named-session removal", identity, err)
	}
}

func TestPhase0ConfigEvolution_RemovedNamedSessionDoesNotStayOpen(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()

	cfgNamed := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "refinery", StartCommand: "true", MaxActiveSessions: intPtr(2)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "refinery", Mode: "on_demand"},
		},
	}
	cfgPlain := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "refinery", StartCommand: "true", MaxActiveSessions: intPtr(2)},
		},
	}

	sessionName := config.NamedSessionRuntimeName(cfgNamed.Workspace.Name, cfgNamed.Workspace, "refinery")
	ds := map[string]TemplateParams{
		sessionName: {
			TemplateName:            "refinery",
			InstanceName:            "refinery",
			Alias:                   "refinery",
			Command:                 "true",
			ConfiguredNamedIdentity: "refinery",
			ConfiguredNamedMode:     "on_demand",
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, ds, sp, allConfiguredDS(ds), cfgNamed, clk, &stderr, false)
	clk.Advance(5 * time.Second)
	syncSessionBeads(cityPath, store, nil, sp, map[string]bool{}, cfgPlain, clk, &stderr, false)

	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(all))
	}
	if all[0].Status != "open" || all[0].Metadata["state"] != "archived" || all[0].Metadata["continuity_eligible"] != "false" {
		t.Fatalf("removed named session = status %q metadata=%v, want open archived continuity-ineligible history", all[0].Status, all[0].Metadata)
	}
}
