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
// whose gc.run_target is the BARE name "run-operator" (config routing). A bare
// target is exactly what a rig formula authored inside the dip rig writes; it
// only resolves when the decorate step is given the "dip" rig context.
func cookReproRecipe() *formula.Recipe {
	return cookReproRecipeWithTarget("run-operator")
}

// cookReproRecipeWithTarget builds the cookReproRecipe shape with an explicit
// step target, so the same fixture covers both the bare rig-scoped target and
// the gc.<role> alias that falls back to it.
func cookReproRecipeWithTarget(target string) *formula.Recipe {
	return &formula.Recipe{
		Name: "wf-cook",
		Steps: []formula.RecipeStep{
			{ID: "wf-cook.root", IsRoot: true, Metadata: map[string]string{
				"gc.kind": "workflow", "gc.formula_contract": "graph.v2",
			}},
			{ID: "wf-cook.work", Metadata: map[string]string{
				"gc.run_target": target,
			}},
		},
	}
}

// TestCookRigContext_StoreRefFallbackResolvesBareTarget documents that on the
// current base cook's decorate path already resolves a bare rig-scoped step
// target WITHOUT an explicit rig context: with a rig-scoped rootStoreRef
// ("rig:dip") and no default execution binding, the store-scope fallback in
// DecorateGraphWorkflowRecipeWithDefaultBinding (added by #4175) derives the
// execution rig context from the store ref. This is why the explicit
// rig-context binding cook now threads (next test) is defense-in-depth rather
// than a load-bearing fix for the original #3944 report, which #4175 already
// resolved.
func TestCookRigContext_StoreRefFallbackResolvesBareTarget(t *testing.T) {
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
		t.Fatalf("store-scope fallback should resolve bare rig target, got: %v", err)
	}
	if got := recipe.Steps[1].Metadata["gc.routed_to"]; got != "dip/run-operator" {
		t.Fatalf("work step gc.routed_to = %q, want dip/run-operator", got)
	}
	if got := recipe.Steps[0].Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("root gc.routed_to = %q, want empty (cook must not route the root)", got)
	}
}

// TestCookRigContext_StoreRefFallbackResolvesFormulaRoleAlias proves that an
// unbound gc.<role> alias falls back to the bare role within its owning rig:
// the unbound pack configuration exposes the role as a bare rig-scoped
// "run-operator", and the store-scope fallback above supplies the "dip" rig
// context the bare retry needs. The alias therefore routes exactly where the
// bare target does, without changing the default route for any rig-scoped
// workflow.
func TestCookRigContext_StoreRefFallbackResolvesFormulaRoleAlias(t *testing.T) {
	cfg := cookReproConfig()
	deps := Deps{Resolver: rigAwareResolver{}}

	recipe := cookReproRecipeWithTarget("gc.run-operator")
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

// boundRoleAliasConfig builds a city where BOTH resolution paths for the
// gc.<role> alias succeed but land on DIFFERENT agents, so the precedence rule
// is observable rather than incidental:
//
//   - the bound "gc.run-operator" (BindingName "gc", city-scoped) has that
//     string as its literal qualified identity, so only the exact lookup can
//     reach it — AgentMatchesIdentity refuses the V1 bare-name fallback for an
//     agent carrying a BindingName;
//   - the unbound "run-operator" is what formulaRoleTarget's bare retry finds.
//
// Exact-first therefore routes to "gc.run-operator" and a fallback-first
// resolver would route to "run-operator": swapping the two ResolveAgent calls
// in ResolveGraphStepBindingWithVars fails the test below. withExact=false
// drops the bound agent so only the fallback can resolve.
func boundRoleAliasConfig(withExact bool) *config.City {
	three := 3
	one := 1
	agents := []config.Agent{{Name: "mayor", MaxActiveSessions: &three}}
	if withExact {
		agents = append(agents, config.Agent{Name: "run-operator", BindingName: "gc", MaxActiveSessions: &three})
	}
	agents = append(agents,
		config.Agent{Name: "run-operator", MaxActiveSessions: &three},
		config.Agent{Name: "control-dispatcher", MaxActiveSessions: &one},
	)
	return &config.City{Agents: agents}
}

// TestApplyGraphRoutingPreservesBoundFormulaRoleTarget pins the first half of
// the precedence rule: when an agent is actually bound as "gc.<role>", the
// exact-identity lookup wins and the bare fallback never runs. Swapping the two
// ResolveAgent calls in ResolveGraphStepBindingWithVars must fail this test —
// the bare scan is ambiguous here, so a fallback-first resolver errors out.
func TestApplyGraphRoutingPreservesBoundFormulaRoleTarget(t *testing.T) {
	cfg := boundRoleAliasConfig(true)
	recipe := cookReproRecipeWithTarget("gc.run-operator")

	err := ApplyGraphRouting(recipe, &cfg.Agents[0], "mayor", nil, "", "rig", "my-project", "", nil, "test-city", cfg, Deps{Resolver: rigAwareResolver{}})
	if err != nil {
		t.Fatalf("ApplyGraphRouting: %v", err)
	}
	if got := recipe.Steps[1].Metadata["gc.routed_to"]; got != "gc.run-operator" {
		t.Fatalf("gc.routed_to = %q, want gc.run-operator (exact bound identity, not the bare fallback)", got)
	}
}

// TestApplyGraphRoutingFallsBackToBareRoleWhenUnbound pins the other half: with
// no agent bound as "gc.<role>", the same target falls back to the bare role
// and resolves to the unbound pack's agent.
func TestApplyGraphRoutingFallsBackToBareRoleWhenUnbound(t *testing.T) {
	cfg := boundRoleAliasConfig(false)
	recipe := cookReproRecipeWithTarget("gc.run-operator")

	err := ApplyGraphRouting(recipe, &cfg.Agents[0], "mayor", nil, "", "rig", "my-project", "", nil, "test-city", cfg, Deps{Resolver: rigAwareResolver{}})
	if err != nil {
		t.Fatalf("ApplyGraphRouting: %v", err)
	}
	if got := recipe.Steps[1].Metadata["gc.routed_to"]; got != "run-operator" {
		t.Fatalf("gc.routed_to = %q, want run-operator (bare fallback)", got)
	}
}
