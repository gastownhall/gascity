package main

import (
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// promptQueryPinsFixture is a non-split city (no [storage] relocation, so
// cityQueryTopology answers FederatedReady=false) whose discovery placeholders
// pin the byte-for-byte behavior of every shape the prompt-federation swap must
// NOT reach.
type promptQueryPinsFixture struct {
	cityPath string
	cfg      *config.City
	agent    *config.Agent
	rigs     []config.Rig
}

func newPromptQueryPinsFixture(t *testing.T, agentScope string, withRigs bool) promptQueryPinsFixture {
	t.Helper()
	cityPath := t.TempDir()
	var rigs []config.Rig
	if withRigs {
		rigs = append(rigs, config.Rig{Name: "myrig", Path: filepath.Join(t.TempDir(), "myrig")})
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "bright-lights", Provider: "claude"},
		Rigs:      rigs,
	}
	agent := &config.Agent{
		Name:     "planner",
		Scope:    agentScope,
		Provider: "claude",
	}
	if agentScope != "" && agentScope != "city" {
		agent.Dir = "myrig"
	}
	return promptQueryPinsFixture{cityPath: cityPath, cfg: cfg, agent: agent, rigs: rigs}
}

// renderedQueries returns the four discovery placeholders as both render sites
// build them: gc prime's context builder and the controller's resolveTemplate.
func (f promptQueryPinsFixture) renderedQueries(t *testing.T) (prime, controller [4]string) {
	t.Helper()
	topo := cityQueryTopology(f.cityPath, f.cfg)
	ctx := buildAgentPromptContext(f.cityPath, f.cfg.Workspace.Name, f.agent, f.rigs, topo, io.Discard)
	prime = [4]string{ctx.WorkQuery, ctx.AssignedInProgressQuery, ctx.AssignedReadyQuery, ctx.RoutedPoolQuery}

	tmpl := writePromptQueryPinTemplate(t, f.cityPath)
	f.agent.PromptTemplate = tmpl
	tp, err := resolveTemplate(promptQueryTestParams(f.cityPath, f.cfg), f.agent, f.agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	controller = [4]string{
		extractPinnedPromptQuery(tp.Prompt, "WORK_QUERY"),
		extractPinnedPromptQuery(tp.Prompt, "ASSIGNED_IN_PROGRESS"),
		extractPinnedPromptQuery(tp.Prompt, "ASSIGNED_READY"),
		extractPinnedPromptQuery(tp.Prompt, "ROUTED_POOL"),
	}
	for i, got := range controller {
		if got == "" {
			t.Fatalf("controller render lost placeholder %d; prompt:\n%s", i, tp.Prompt)
		}
	}
	return prime, controller
}

var promptQueryPinNames = [4]string{"WorkQuery", "AssignedInProgressQuery", "AssignedReadyQuery", "RoutedPoolQuery"}

// assertQueriesEqualBase checks each placeholder against what base renders for
// the same agent: the value Effective*QueryFor builds from the caller's own
// topology, which is exactly the topology base's render sites passed through.
func assertQueriesEqualBase(t *testing.T, f promptQueryPinsFixture, site string, queries [4]string) {
	t.Helper()
	topo := cityQueryTopology(f.cityPath, f.cfg)
	base := [4]string{
		f.agent.EffectiveWorkQueryFor(topo),
		f.agent.EffectiveAssignedInProgressQueryFor(topo),
		f.agent.EffectiveAssignedReadyQueryFor(topo),
		f.agent.EffectiveRoutedPoolQueryFor(topo),
	}
	for i := range queries {
		if queries[i] != base[i] {
			t.Errorf("%s %s changed for a shape the federation swap must not reach:\ngot:  %q\nbase: %q", site, promptQueryPinNames[i], queries[i], base[i])
		}
	}
}

// TestPromptQueriesUnchangedOutsideCityScopedAgentsOnRigCities pins AC-4 of
// issue #1689: only a city-scoped agent on a city with rigs gets federated
// discovery placeholders. A rig-scoped agent, any agent on a rig-less city,
// and a city whose topology already federates render byte-for-byte what base
// rendered.
func TestPromptQueriesUnchangedOutsideCityScopedAgentsOnRigCities(t *testing.T) {
	t.Run("rig-scoped agent on a city with rigs", func(t *testing.T) {
		f := newPromptQueryPinsFixture(t, "rig", true)
		prime, controller := f.renderedQueries(t)
		assertQueriesEqualBase(t, f, "prime", prime)
		assertQueriesEqualBase(t, f, "controller", controller)
	})

	t.Run("city-scoped agent on a rig-less city", func(t *testing.T) {
		f := newPromptQueryPinsFixture(t, "city", false)
		prime, controller := f.renderedQueries(t)
		assertQueriesEqualBase(t, f, "prime", prime)
		assertQueriesEqualBase(t, f, "controller", controller)
	})

	t.Run("already-federated topology is never cleared", func(t *testing.T) {
		a := &config.Agent{Name: "planner", Scope: "city"}
		federated := config.QueryTopology{FederatedReady: true}
		rigs := []config.Rig{{Name: "myrig"}}
		if got := promptQueryTopology(federated, a, rigs); !got.FederatedReady {
			t.Error("promptQueryTopology cleared a FederatedReady the resolver answered")
		}
		empty := config.QueryTopology{}
		if got := promptQueryTopology(empty, nil, nil); got.FederatedReady {
			t.Error("promptQueryTopology federated a nil agent with no rigs")
		}
		if got := promptQueryTopology(empty, a, nil); got.FederatedReady {
			t.Error("promptQueryTopology federated a city-scoped agent on a rig-less city")
		}
		if got := promptQueryTopology(empty, &config.Agent{Name: "planner", Scope: "rig"}, rigs); got.FederatedReady {
			t.Error("promptQueryTopology federated a rig-scoped agent")
		}
		if got := promptQueryTopology(federated, nil, nil); !reflect.DeepEqual(got, federated) {
			t.Error("promptQueryTopology must pass the topology through unchanged apart from the federation bit")
		}
	})
}

// TestPromptQueriesUserWorkQueryOverridesVerbatim pins AC-5 of issue #1689: a
// user-authored work_query is the caller-owned discovery contract, so all four
// placeholders carry it verbatim — on a rig city, at both render sites,
// exactly as on a rig-less city.
func TestPromptQueriesUserWorkQueryOverridesVerbatim(t *testing.T) {
	f := newPromptQueryPinsFixture(t, "city", true)
	const custom = "echo my own discovery"
	f.agent.WorkQuery = custom
	prime, controller := f.renderedQueries(t)
	for i, queries := range [2][4]string{prime, controller} {
		for j, got := range queries {
			if got != custom {
				t.Errorf("%s %s = %q, want the user work_query verbatim %q", []string{"prime", "controller"}[i], promptQueryPinNames[j], got, custom)
			}
		}
	}
}

// TestHookQueryStaysSingleStorePerStoreForCityScopedAgents pins AC-6 of issue
// #1689: the prompt-side federation does not leak into `gc hook`. The query
// the hook builds (hookWorkQuery) comes from the city topology — still the
// single-store `bd` form on a city that relocates nothing — and its fan-out runs
// that one query once per store: the agent's own first, then one leg per
// serving rig.
func TestHookQueryStaysSingleStorePerStoreForCityScopedAgents(t *testing.T) {
	cityPath := t.TempDir()
	rigs := []config.Rig{
		{Name: "alpha", Path: filepath.Join(cityPath, "rigs", "alpha")},
		{Name: "beta", Path: filepath.Join(cityPath, "rigs", "beta")},
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "bright-lights"}, Rigs: rigs}
	agent := &config.Agent{Name: "planner", Scope: "city"}

	topo, query := hookWorkQuery(cityPath, "bright-lights", cfg, agent, io.Discard)
	if topo.FederatedReady {
		t.Fatalf("gc hook reads with a federated topology on a city that relocates no coordination class")
	}
	if !strings.Contains(query, "bd ready") {
		t.Errorf("the hook's work query left the single-store form: %q", query)
	}
	if !strings.Contains(query, "bd list --status in_progress") {
		t.Errorf("the hook's crash-recovery tier left the single-store form: %q", query)
	}
	if strings.Contains(query, "gc ready") {
		t.Errorf("the hook's work query federated; its fan-out already covers every store: %q", query)
	}
	if hookStoresScopeToPrimary(query, singleStoreHookWorkQuery(cityPath, "bright-lights", cfg, agent, topo, io.Discard)) {
		t.Fatal("gc hook pins a city-scoped agent's read to its primary store; it must fan out over every store")
	}

	prev := hookStoreQueryEnv
	hookStoreQueryEnv = func(_ string, _ *config.City, a *config.Agent) (map[string]string, error) {
		return map[string]string{"GC_STORE": "leg-" + a.Dir}, nil
	}
	t.Cleanup(func() { hookStoreQueryEnv = prev })

	stores := hookWorkQueryStores(cityPath, cfg, agent, "planner", cityPath, nil, nil, nil)
	if len(stores) == 0 {
		t.Fatal("gc hook built no fan-out legs")
	}
	if stores[0].dir != cityPath || stores[0].rig != "" {
		t.Errorf("primary leg = dir %q rig %q, want the agent's own store %q with no rig", stores[0].dir, stores[0].rig, cityPath)
	}
	var legs []string
	for _, st := range stores {
		legs = append(legs, st.rig)
		if got := hookStoreCommand(st, query); got != query {
			t.Errorf("store %q runs its own command %q, want the one shared work query", st.rig, got)
		}
	}
	if want := []string{"", "alpha", "beta"}; !slices.Equal(legs, want) {
		t.Errorf("fan-out legs = %q, want the agent's own store then each rig %q", legs, want)
	}
}
