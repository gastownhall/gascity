package main

import (
	"bytes"
	"errors"
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

func TestPhase0WorkflowRouting_DirectSessionTargetDoesNotStampRoutedTo(t *testing.T) {
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
	codexBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:frontend/codex"},
		Metadata: map[string]string{
			"session_name": "s-gc-codex",
			"alias":        "frontend/codex",
			"template":     "frontend/codex",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create codex session bead: %v", err)
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
			{
				ID:    "demo.review",
				Title: "Review",
				Type:  "task",
				Metadata: map[string]string{
					"gc.run_target": "{{design_target}}",
				},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "demo.design", DependsOnID: "demo", Type: "parent-child"},
			{StepID: "demo.review", DependsOnID: "demo.design", Type: "blocks"},
		},
	}

	if err := decorateGraphWorkflowRecipe(recipe, graphWorkflowRouteVars(recipe, nil), "", "", "", "", "frontend/claude", claudeBead.Metadata["session_name"], store, cfg.Workspace.Name, cfg); err != nil {
		t.Fatalf("decorateGraphWorkflowRecipe: %v", err)
	}

	design := recipe.StepByID("demo.design")
	if design == nil {
		t.Fatal("design step missing after decorate")
	}
	if design.Assignee != codexBead.ID {
		t.Fatalf("design assignee = %q, want concrete session bead ID %q", design.Assignee, codexBead.ID)
	}
	if got := design.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("design gc.routed_to = %q, want empty for direct session target", got)
	}

	review := recipe.StepByID("demo.review")
	if review == nil {
		t.Fatal("review step missing after decorate")
	}
	if got := review.Metadata["gc.routed_to"]; got != "frontend/codex" {
		t.Fatalf("review gc.routed_to = %q, want frontend/codex for config-routed execution", got)
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
	if all[0].Status == "open" {
		t.Fatalf("removed named session remained open: metadata=%v", all[0].Metadata)
	}
}
