package graphroute

import (
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// rigAwareResolver mirrors the CLI's resolveAgentIdentity / cliAgentResolver:
// bare names prefer the rig-scoped agent when a rig context is supplied. A
// rig-agnostic resolver cannot exercise the rig-context-dependent resolution
// these tests turn on.
type rigAwareResolver struct{}

func (rigAwareResolver) ResolveAgent(cfg *config.City, name, rigContext string) (config.Agent, bool) {
	return agentutil.ResolveAgent(cfg, name, agentutil.ResolveOpts{
		UseAmbientRig:    true,
		RigContext:       rigContext,
		AllowPoolMembers: true,
	})
}

// cookReproConfig builds a city with TWO rigs ("dip" and "ce"), each owning a
// pool agent named "run-operator" (qualified "dip/run-operator" and
// "ce/run-operator") plus its own rig-scoped control-dispatcher. The bare name
// "run-operator" is therefore NOT city-unique: it resolves only when a rig
// context disambiguates it. This mirrors a real multi-rig city where an
// imported pack (e.g. compound-engineering) contributes same-named agents to
// several rigs — the shape where cook must supply the invocation's rig context
// for bare rig-scoped step targets to resolve.
//
// Each rig owns a control-dispatcher because a rig-scoped rootStoreRef
// ("rig:dip") routes control beads through that rig's store scope (#4175), so
// DecorateGraphWorkflowRecipeWithDefaultBinding requires a dispatcher whose Dir
// matches the store-scoped rig rather than a city dispatcher.
func cookReproConfig() *config.City {
	two := 2
	one := 1
	return &config.City{
		Rigs: []config.Rig{{Name: "dip", Path: "/tmp/dip"}, {Name: "ce", Path: "/tmp/ce"}},
		Agents: []config.Agent{
			// Rig-scoped pool agents. MaxActiveSessions>1 => SupportsInstanceExpansion,
			// so resolution yields a MetadataOnly binding and needs no store/session.
			{Name: "run-operator", Dir: "dip", MaxActiveSessions: &two},
			{Name: "run-operator", Dir: "ce", MaxActiveSessions: &two},
			// Rig-scoped control-dispatchers, required by ControlDispatcherBinding
			// for a rig-scoped graph store ref (#4175 store-scope routing).
			{Name: config.ControlDispatcherAgentName, Dir: "dip", MaxActiveSessions: &one},
			{Name: config.ControlDispatcherAgentName, Dir: "ce", MaxActiveSessions: &one},
		},
	}
}

// cookReproRecipe is a minimal graph.v2 workflow: a root plus one work step
// whose gc.run_target is the semantic role alias "gc.run-operator". The
// unbound pack configuration exposes that role as a bare rig-scoped
// "run-operator", so it only resolves when decorate derives the "dip" rig
// context from the scoped store.
func cookReproRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "wf-cook",
		Steps: []formula.RecipeStep{
			{ID: "wf-cook.root", IsRoot: true, Metadata: map[string]string{
				"gc.kind": "workflow", "gc.formula_contract": "graph.v2",
			}},
			{ID: "wf-cook.work", Metadata: map[string]string{
				"gc.run_target": "gc.run-operator",
			}},
		},
	}
}

// TestCookRigContext_StoreRefFallbackResolvesFormulaRoleAlias proves that an
// unbound gc.<role> alias falls back to the bare role within its owning rig.
// With a rig-scoped rootStoreRef ("rig:dip") and no default execution binding,
// DecorateGraphWorkflowRecipeWithDefaultBinding derives the execution context
// from the store ref without changing the default route for every rig-scoped
// workflow.
func TestCookRigContext_StoreRefFallbackResolvesFormulaRoleAlias(t *testing.T) {
	cfg := cookReproConfig()
	deps := Deps{Resolver: rigAwareResolver{}}

	// COOK decorate path with the pre-change argument shape: routedTo="" and
	// sessionName="", so no rig context reaches decorate via the default route.
	recipe := cookReproRecipe()
	err := DecorateGraphWorkflowRecipe(
		recipe, GraphWorkflowRouteVars(recipe, nil),
		"",             // sourceBeadID
		"formula-cook", // scopeKind
		"",             // scopeRef
		"rig:dip",      // rootStoreRef supplies the rig context via store-scope fallback
		"",             // routedTo
		"",             // sessionName
		nil, "test-city", cfg, deps,
	)
	if err != nil {
		t.Fatalf("store-scope fallback should resolve gc role alias, got: %v", err)
	}
	if got := recipe.Steps[1].Metadata["gc.routed_to"]; got != "dip/run-operator" {
		t.Fatalf("work step gc.routed_to = %q, want dip/run-operator", got)
	}
	if got := recipe.Steps[0].Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("root gc.routed_to = %q, want empty (cook must not route the root)", got)
	}
}

// TestCookRigContext_ExplicitDefaultBindingResolvesBareTarget covers the change
// this PR makes: cook threads its already-resolved rig context in explicitly
// through a rig-context-only default binding (QualifiedName empty so the root is
// NOT routed to an agent, MetadataOnly set). The bare rig target resolves and
// cook's "instantiate without routing the root" contract is preserved. The
// routing is identical to the store-scope fallback above; the explicit binding
// states the rig context at the cook call site instead of relying solely on the
// rootStoreRef encoding.
func TestCookRigContext_ExplicitDefaultBindingResolvesBareTarget(t *testing.T) {
	cfg := cookReproConfig()
	deps := Deps{Resolver: rigAwareResolver{}}

	recipe := cookReproRecipe()
	err := DecorateGraphWorkflowRecipeWithDefaultBinding(
		recipe, GraphWorkflowRouteVars(recipe, nil),
		"", "formula-cook", "", "rig:dip",
		GraphRouteBinding{RigContext: "dip", MetadataOnly: true}, // rig context, no route
		nil, "test-city", cfg, deps,
	)
	if err != nil {
		t.Fatalf("explicit rig-context binding should resolve bare rig target, got: %v", err)
	}
	if got := recipe.Steps[1].Metadata["gc.routed_to"]; got != "dip/run-operator" {
		t.Fatalf("work step gc.routed_to = %q, want dip/run-operator", got)
	}
	if got := recipe.Steps[0].Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("root gc.routed_to = %q, want empty (cook must not route the root)", got)
	}
}

func TestApplyGraphRoutingPreservesBoundFormulaRoleTarget(t *testing.T) {
	three := 3
	one := 1
	cfg := &config.City{Agents: []config.Agent{
		{Name: "mayor", MaxActiveSessions: &three},
		{Name: "run-operator", BindingName: "gc", Dir: "my-project", MaxActiveSessions: &three},
		{Name: "control-dispatcher", MaxActiveSessions: &one},
		{Name: "control-dispatcher", Dir: "my-project", MaxActiveSessions: &one},
	}}
	recipe := cookReproRecipe()
	recipe.Steps[1].Metadata["gc.run_target"] = "gc.run-operator"

	err := ApplyGraphRouting(recipe, &cfg.Agents[0], "mayor", nil, "", "rig", "my-project", "", nil, "test-city", cfg, Deps{Resolver: rigAwareResolver{}})
	if err != nil {
		t.Fatalf("ApplyGraphRouting: %v", err)
	}
	if got := recipe.Steps[1].Metadata["gc.routed_to"]; got != "my-project/gc.run-operator" {
		t.Fatalf("gc.routed_to = %q, want my-project/gc.run-operator", got)
	}
}
