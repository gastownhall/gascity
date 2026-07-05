package graphroute

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// rigAwareResolver mirrors the CLI's resolveAgentIdentity / cliAgentResolver:
// bare names prefer the rig-scoped agent when a rig context is supplied. The
// package's own testAgentResolver ignores rigContext, so it CANNOT reproduce
// this bug — the whole point is that rigContext is load-bearing.
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
// "ce/run-operator"), plus a city control-dispatcher. The bare name
// "run-operator" is therefore NOT city-unique: it is resolvable only when a rig
// context disambiguates it. This mirrors a real multi-rig city where an
// imported pack (e.g. compound-engineering) contributes same-named agents to
// several rigs — the shape that makes cook's dropped rig context a hard failure
// rather than silent mis-routing.
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
			// City control-dispatcher, required by ControlDispatcherBinding.
			{Name: config.ControlDispatcherAgentName, MaxActiveSessions: &one},
		},
	}
}

// cookReproRecipe is a minimal graph.v2 workflow: a root plus one work step
// whose gc.run_target is the BARE name "run-operator" (config routing). A bare
// target is exactly what a rig formula authored inside the dip rig writes; it
// only resolves when the decorate step is given the "dip" rig context.
func cookReproRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "wf-cook",
		Steps: []formula.RecipeStep{
			{ID: "wf-cook.root", IsRoot: true, Metadata: map[string]string{
				"gc.kind": "workflow", "gc.formula_contract": "graph.v2",
			}},
			{ID: "wf-cook.work", Metadata: map[string]string{
				"gc.run_target": "run-operator",
			}},
		},
	}
}

// TestCookDropsRigContext_ReproducesResolutionFailure demonstrates the bug:
// the cook decorate path (cmd_formula.go:889) passes routedTo="" so the
// derived routing rig context is empty, and a bare rig-scoped step target is
// unresolvable — exactly what the mayor reported.
func TestCookDropsRigContext_ReproducesResolutionFailure(t *testing.T) {
	cfg := cookReproConfig()
	deps := Deps{Resolver: rigAwareResolver{}}

	// (A) COOK path, verbatim argument shape from cmd_formula.go:889
	//     decorateFormulaCookGraphV2Recipe: routedTo="" and sessionName="".
	cookRecipe := cookReproRecipe()
	cookErr := DecorateGraphWorkflowRecipe(
		cookRecipe, GraphWorkflowRouteVars(cookRecipe, nil),
		"",             // sourceBeadID
		"formula-cook", // scopeKind
		"",             // scopeRef
		"rig:dip",      // rootStoreRef
		"",             // routedTo  <-- empty: no rig context reaches decorate
		"",             // sessionName
		nil, "test-city", cfg, deps,
	)
	if cookErr == nil {
		t.Fatalf("cook path unexpectedly succeeded; expected rig-scoped target to be unresolvable")
	}
	if !strings.Contains(cookErr.Error(), `unknown formulas v2 target "run-operator"`) {
		t.Fatalf("cook error = %q; want it to fail resolving bare rig target run-operator", cookErr.Error())
	}
	t.Logf("COOK (routedTo=\"\") failed as reported: %v", cookErr)

	// (B) SLING path contrast: sling passes a.QualifiedName() as routedTo
	//     (internal/sling/sling.go:1252 -> ApplyGraphRouting). The "dip/"
	//     prefix yields routingRigContext="dip", so the same bare target
	//     resolves.
	slingRecipe := cookReproRecipe()
	slingErr := DecorateGraphWorkflowRecipe(
		slingRecipe, GraphWorkflowRouteVars(slingRecipe, nil),
		"", "formula-cook", "", "rig:dip",
		"dip/run-operator", // routedTo carries the rig context
		"",
		nil, "test-city", cfg, deps,
	)
	if slingErr != nil {
		t.Fatalf("sling-style path (routedTo carries rig) should succeed, got: %v", slingErr)
	}
	if got := slingRecipe.Steps[1].Metadata["gc.routed_to"]; got != "dip/run-operator" {
		t.Fatalf("sling work step gc.routed_to = %q, want dip/run-operator", got)
	}
	t.Logf("SLING (routedTo=dip/run-operator) resolved work step to %q", slingRecipe.Steps[1].Metadata["gc.routed_to"])
}

// TestCookRigContextFix_ViaDefaultBinding verifies the proposed fix: cook
// threads its already-resolved rig context in through a rig-context-only
// default binding (QualifiedName empty so the root is NOT falsely routed to an
// agent, MetadataOnly set). This makes the bare rig target resolve while
// preserving cook's "instantiate without routing the root" contract.
func TestCookRigContextFix_ViaDefaultBinding(t *testing.T) {
	cfg := cookReproConfig()
	deps := Deps{Resolver: rigAwareResolver{}}

	recipe := cookReproRecipe()
	// Proposed fix shape: pass a default binding carrying only the rig context.
	fixErr := DecorateGraphWorkflowRecipeWithDefaultBinding(
		recipe, GraphWorkflowRouteVars(recipe, nil),
		"", "formula-cook", "", "rig:dip",
		GraphRouteBinding{RigContext: "dip", MetadataOnly: true}, // rig context, no route
		nil, "test-city", cfg, deps,
	)
	if fixErr != nil {
		t.Fatalf("fix path should resolve bare rig target, got: %v", fixErr)
	}
	// Work step resolved to the rig-scoped agent.
	if got := recipe.Steps[1].Metadata["gc.routed_to"]; got != "dip/run-operator" {
		t.Fatalf("fixed work step gc.routed_to = %q, want dip/run-operator", got)
	}
	// Root remains unrouted (empty), preserving cook's no-routing-of-root contract.
	if got := recipe.Steps[0].Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("root gc.routed_to = %q, want empty (cook must not route the root to an agent)", got)
	}
	t.Logf("FIX (rig-context default binding) resolved work step to %q, root unrouted", recipe.Steps[1].Metadata["gc.routed_to"])
}
