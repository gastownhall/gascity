package graphroute

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// TestZeroSessionStoreRefusesDirectSessionResolution pins graphroute's absent
// sessions store (#8436): a direct-session target names a session row, and a
// zero handle is no store to resolve it in. The lookup fails with an error
// naming the target instead of reporting "no binding", which would let a
// caller fall back to config routing or materialize the row in whatever store
// it holds; the resolver never runs. An empty target needs no session access
// and still reports no binding.
func TestZeroSessionStoreRefusesDirectSessionResolution(t *testing.T) {
	resolved := false
	deps := Deps{DirectSessionResolver: func(beads.SessionStore, string, string, *config.City, string, string) (string, bool, error) {
		resolved = true
		return "materialized", true, nil
	}}
	binding, ok, err := ResolveGraphDirectSessionBinding(beads.SessionStore{}, "city", &config.City{}, "worker", "", deps)
	if !errors.Is(err, ErrNoSessionStore) || ok || binding != (GraphRouteBinding{}) {
		t.Errorf("ResolveGraphDirectSessionBinding over the zero handle = %+v, %v, %v; want ErrNoSessionStore and no binding", binding, ok, err)
	}
	if err != nil && !strings.Contains(err.Error(), `"worker"`) {
		t.Errorf("error %q does not name the target", err)
	}
	if resolved {
		t.Error("the direct-session resolver ran over the zero handle; an absent store resolves nothing")
	}
	if _, ok, err := ResolveGraphDirectSessionBinding(beads.SessionStore{}, "city", &config.City{}, "  ", "", deps); err != nil || ok {
		t.Errorf("ResolveGraphDirectSessionBinding(empty target) over the zero handle = %v, %v; want no binding and no error", ok, err)
	}
	if got, want := agentutil.LookupSessionName(beads.SessionStore{}, "city", "rig/worker", ""), agent.SessionNameFor("city", "rig/worker", ""); got != want {
		t.Errorf("LookupSessionName over the zero handle = %q, want the configured name %q", got, want)
	}
}

// TestZeroSessionStoreAssigneeStepFailsWithContext proves a graph step whose
// assignee names a session fails decoration with the absent-store cause and
// the step, rather than the generic "did not resolve" refusal.
func TestZeroSessionStoreAssigneeStepFailsWithContext(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker"},
			{Name: config.ControlDispatcherAgentName},
		},
	}
	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}},
			{ID: "wf.work", Assignee: "worker"},
		},
	}
	called := false
	deps := Deps{
		Resolver: testAgentResolver{},
		DirectSessionResolver: func(beads.SessionStore, string, string, *config.City, string, string) (string, bool, error) {
			called = true
			return "materialized", true, nil
		},
	}
	err := DecorateGraphWorkflowRecipe(recipe, nil, "", "city", "test-city", "city:test-city", "worker", "", beads.SessionStore{}, "test-city", cfg, deps)
	if !errors.Is(err, ErrNoSessionStore) {
		t.Fatalf("DecorateGraphWorkflowRecipe(assignee step, zero handle) = %v; want ErrNoSessionStore", err)
	}
	if !strings.Contains(err.Error(), "wf.work") {
		t.Errorf("error %q does not name the step", err)
	}
	if called {
		t.Error("the direct-session resolver ran over the zero handle")
	}
}
