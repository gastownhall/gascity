package graphroute

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

func rigNamespaceGraphCity(guarded bool) *config.City {
	maxActive := 1
	beta := config.Rig{Name: "beta", Prefix: "bt"}
	if guarded {
		beta.Federation = config.RigFederationRig
	}
	return &config.City{
		Rigs: []config.Rig{beta},
		Agents: []config.Agent{
			{Name: "city-worker", Scope: "city"},
			{Name: "crew.worker", Dir: "beta", MaxActiveSessions: &maxActive},
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				Dir:               "beta",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
		},
	}
}

func rigNamespaceGraphRecipe(workTarget string) *formula.Recipe {
	return &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}},
			{ID: "wf.work", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: workTarget}},
			{ID: "wf.finalize", Metadata: map[string]string{"gc.kind": "workflow-finalize"}},
		},
	}
}

func decorateRigNamespace(cfg *config.City, recipe *formula.Recipe, routedTo string) error {
	return DecorateGraphWorkflowRecipe(recipe, nil, "", "rig", "beta", "rig:beta", routedTo, "", beads.SessionStore{}, "test-city", cfg, Deps{Resolver: testAgentResolver{}})
}

// The rig's own agents and its own control dispatcher are admitted.
func TestDecorateGraphIntoGuardedRigAllowsOwnRoutes(t *testing.T) {
	cfg := rigNamespaceGraphCity(true)
	recipe := rigNamespaceGraphRecipe("beta/crew.worker")
	if err := decorateRigNamespace(cfg, recipe, "beta/crew.worker"); err != nil {
		t.Fatalf("decorate: %v", err)
	}
	if got := recipe.Steps[2].Metadata[beadmeta.RoutedToMetadataKey]; got != "beta/core.control-dispatcher" {
		t.Fatalf("finalize route = %q, want the rig's dispatcher", got)
	}
}

// A graph step routed to a city-scoped agent is refused, whichever step
// carries it.
func TestDecorateGraphIntoGuardedRigRefusesOutsideRoutes(t *testing.T) {
	cfg := rigNamespaceGraphCity(true)
	for _, tc := range []struct{ name, step, root string }{
		{"step route", "city-worker", "beta/crew.worker"},
		{"root route", "beta/crew.worker", "city-worker"},
	} {
		err := decorateRigNamespace(cfg, rigNamespaceGraphRecipe(tc.step), tc.root)
		if !errors.Is(err, agentutil.ErrRigNamespaceRoute) {
			t.Errorf("%s: err = %v, want a rig namespace refusal", tc.name, err)
		}
	}
	// Unguarded, the same recipe is decorated as before.
	if err := decorateRigNamespace(rigNamespaceGraphCity(false), rigNamespaceGraphRecipe("city-worker"), "city-worker"); err != nil {
		t.Fatalf("unguarded decorate: %v", err)
	}
}

func TestApplyGraphRoutingLegacyIntoGuardedRig(t *testing.T) {
	cfg := rigNamespaceGraphCity(true)
	legacy := func(perStep string) *formula.Recipe {
		return &formula.Recipe{Name: "mol", Steps: []formula.RecipeStep{
			{ID: "mol", IsRoot: true, Metadata: map[string]string{}},
			{ID: "mol.s1", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: perStep}},
		}}
	}
	if err := ApplyGraphRouting(legacy("beta/crew.worker"), nil, "beta/crew.worker", nil, "", "", "", "rig:beta", beads.SessionStore{}, "city", cfg, Deps{}); err != nil {
		t.Fatalf("own legacy route: %v", err)
	}
	if err := ApplyGraphRouting(legacy("crew.worker"), nil, "beta/crew.worker", nil, "", "", "", "rig:beta", beads.SessionStore{}, "city", cfg, Deps{}); !errors.Is(err, agentutil.ErrRigNamespaceRoute) {
		t.Fatalf("bare legacy per-step route: err = %v, want a refusal", err)
	}
}

func TestValidateRigNamespaceRoutes(t *testing.T) {
	cfg := rigNamespaceGraphCity(true)
	ok := []formula.RecipeStep{
		{ID: "a", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "beta/crew.worker"}},
		{ID: "b", Metadata: map[string]string{beadmeta.ExecutionRoutedToMetadataKey: "beta/crew.worker", beadmeta.RoutedToMetadataKey: "beta/core.control-dispatcher"}},
		{ID: "c", Metadata: map[string]string{}},
		{ID: "d", Assignee: "beta/crew.lead"},
	}
	if err := ValidateRigNamespaceRoutes(ok, "rig:beta", cfg); err != nil {
		t.Fatalf("admitted steps refused: %v", err)
	}
	for _, step := range []formula.RecipeStep{
		{ID: "x", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "crew.worker"}},
		{ID: "x", Metadata: map[string]string{beadmeta.ExecutionRoutedToMetadataKey: "alpha/crew.worker"}},
		{ID: "x", Metadata: map[string]string{beadmeta.DeferredRoutedToMetadataKey: "beta/"}},
		{ID: "x", Assignee: "gcx-17"},
		{ID: "x", Assignee: "gcx-session-0123456789abcdef0123456789abcdef"},
	} {
		if err := ValidateRigNamespaceRoutes([]formula.RecipeStep{step}, "rig:beta", cfg); !errors.Is(err, agentutil.ErrRigNamespaceRoute) {
			t.Errorf("%+v: err = %v, want a refusal", step, err)
		}
	}
	// Other store refs check nothing.
	bad := []formula.RecipeStep{{ID: "x", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "crew.worker"}}}
	for _, ref := range []string{"", "city:test", "rig:alpha"} {
		if err := ValidateRigNamespaceRoutes(bad, ref, cfg); err != nil {
			t.Errorf("store ref %q: %v", ref, err)
		}
	}
}
