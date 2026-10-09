package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"pgregory.net/rapid"
)

func testRosterCity() *config.City {
	return &config.City{
		Agents: []config.Agent{
			{Name: "polecat"},
			{Name: "city-infra-worker"},
		},
		NamedSessions: []config.NamedSession{
			{Name: "goal-4-context"},
			{Name: "mayor"},
		},
	}
}

func TestAssigneeRosterRejectsUnconfiguredTargets(t *testing.T) {
	roster := newAssigneeRosterAt(testRosterCity(), "")

	for _, assignee := range []string{
		"goal-5-temporal",
		"/srv/city/goal-5-temporal",
		"controller",
		"pr-pipeline-review-17",
	} {
		if roster.Resolves(assignee) {
			t.Errorf("assignee %q resolved, want unresolvable", assignee)
		}
	}
}

func TestAssigneeRosterAcceptsRoutableTargets(t *testing.T) {
	roster := newAssigneeRosterAt(testRosterCity(), "")

	for _, assignee := range []string{
		"",
		"goal-4-context",
		"polecat",
		"polecat-4",
		"city-infra-worker",
		"human",
		"mayor",
		"dr-toegp",
		"gc-4cyi0a",
		"claude-1-adhoc-6f649101fd",
		"claude-auto-2",
	} {
		if !roster.Resolves(assignee) {
			t.Errorf("assignee %q did not resolve, want routable", assignee)
		}
	}
}

func TestAssigneeRosterEmptyIsReported(t *testing.T) {
	roster := newAssigneeRosterAt(nil, "")
	if !roster.Empty() {
		t.Fatal("roster built from nil config does not report Empty")
	}
	if newAssigneeRosterAt(testRosterCity(), "").Empty() {
		t.Fatal("a populated roster must not report Empty")
	}
}

func rosterCityWithPrefixes() *config.City {
	c := testRosterCity()
	c.Workspace.Prefix = "gc"
	c.Rigs = []config.Rig{{Name: "research", Path: "research", Prefix: "dr"}}
	return c
}

func TestAssigneeRosterDistinguishesSessionIDsFromTypos(t *testing.T) {
	roster := newAssigneeRosterAt(rosterCityWithPrefixes(), "")

	for _, assignee := range []string{"dr-huhn", "gc-818bx", "dr-a95w9", "repo-adhoc-1a2b3c", "worker-auto-7"} {
		if !roster.Resolves(assignee) {
			t.Errorf("runtime identity %q was rejected; a static roster cannot confirm these", assignee)
		}
	}
	for _, assignee := range []string{"poly-cat1", "xy-1234", "abc-9999"} {
		if roster.Resolves(assignee) {
			t.Errorf("assignee %q resolved as a session identity, but no city prefix issues it", assignee)
		}
	}
}

func TestAssigneeRosterAcceptsAnyIDShapeWhenNoPrefixesDeclared(t *testing.T) {
	c := &config.City{Agents: []config.Agent{{Name: "polecat"}}}
	if !newAssigneeRosterAt(c, "").Resolves("poly-cat1") {
		t.Error("bead-shaped name rejected although the city declares no prefixes")
	}
}

func TestAssigneeRosterResolvesNamepoolInstances(t *testing.T) {
	c := testRosterCity()
	c.Agents = append(c.Agents, config.Agent{Name: "herder", NamepoolNames: []string{"rivet", "gasket"}})
	roster := newAssigneeRosterAt(c, "")

	for _, assignee := range []string{"rivet", "gasket", "herder-2"} {
		if !roster.Resolves(assignee) {
			t.Errorf("pool instance %q was rejected; it is routable", assignee)
		}
	}
}

func TestAssigneeRosterResolvesQualifiedSpellingsOfRuntimeIdentities(t *testing.T) {
	roster := newAssigneeRosterAt(rosterCityWithPrefixes(), "")

	for _, assignee := range []string{
		"research/dr-huhn",
		"research/repo-adhoc-1a2b3c",
	} {
		if !roster.Resolves(assignee) {
			t.Errorf("qualified runtime identity %q was rejected", assignee)
		}
	}
	for _, assignee := range []string{
		"/srv/city/goal-5-temporal",
		"research/poly-cat1",
		"wrongrig/dr-huhn",
	} {
		if roster.Resolves(assignee) {
			t.Errorf("qualified phantom %q resolved", assignee)
		}
	}
}

func TestAssigneeRosterResolvesSessionNameSpelling(t *testing.T) {
	c := testRosterCity()
	c.Agents = append(c.Agents, config.Agent{Name: "polecat-rig", Dir: "repo"})
	roster := newAssigneeRosterAt(c, "")

	if !roster.Resolves("repo--polecat-rig") {
		t.Error("session-name-spelled assignee \"repo--polecat-rig\" was rejected")
	}
	if roster.Resolves("repo--ghost") {
		t.Error("phantom \"repo--ghost\" resolved via the session-name decode")
	}
}

func TestAssigneeRosterRejectsDotQualifiedBeadIDShapes(t *testing.T) {
	r := newAssigneeRosterAt(rosterCityWithPrefixes(), "")
	for _, name := range []string{
		"someone.gc-818bx",
		"totally-bogus-typo.gc-818bx",
	} {
		if r.Resolves(name) {
			t.Errorf("Resolves(%q) = true, want false: a dot-qualified tail is not a session identity", name)
		}
	}
	if !r.Resolves("research/gc-818bx") {
		t.Error(`Resolves("research/gc-818bx") = false, want true`)
	}
}

func TestAssigneeRosterResolvesClassStoreSessionIDs(t *testing.T) {
	roster := newAssigneeRosterAt(rosterCityWithPrefixes(), "")

	for _, assignee := range []string{"gcs-ab12", "gcs-7", "gc-7", "dr-42"} {
		if !roster.Resolves(assignee) {
			t.Errorf("session id %q was rejected; the city or its class stores mint it", assignee)
		}
	}
	for _, assignee := range []string{"xy-7", "gcs-ab"} {
		if roster.Resolves(assignee) {
			t.Errorf("assignee %q resolved as a session id, want unresolvable", assignee)
		}
	}
}

func TestAssigneeRosterResolvesHyphenatedPrefixesCaseInsensitively(t *testing.T) {
	c := rosterCityWithPrefixes()
	c.Rigs = append(c.Rigs,
		config.Rig{Name: "repo", Path: "repo", Prefix: "my-repo"},
		config.Rig{Name: "zero", Path: "zero", Prefix: "repo-0"},
	)
	roster := newAssigneeRosterAt(c, "")

	for _, assignee := range []string{"my-repo-ab12", "MY-REPO-ab12", "My-RePo-7", "REPO-0-0000"} {
		if !roster.Resolves(assignee) {
			t.Errorf("session id %q was rejected", assignee)
		}
	}
	for _, assignee := range []string{"my-repo-ab", "my-other-ab12"} {
		if roster.Resolves(assignee) {
			t.Errorf("assignee %q resolved, want unresolvable", assignee)
		}
	}
}

func TestAssigneeRosterResolvesEveryConfiguredHyphenatedPrefixCaseInsensitively(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		prefixTail := rapid.StringMatching(`[a-z0-9]{1,8}`).Draw(rt, "prefixTail")
		suffix := rapid.StringMatching(`[a-z0-9]{4,12}`).Draw(rt, "suffix")
		prefix := "repo-" + prefixTail
		c := rosterCityWithPrefixes()
		c.Rigs = append(c.Rigs, config.Rig{Name: "repo", Path: "repo", Prefix: prefix})
		assignee := strings.ToUpper(prefix) + "-" + suffix
		if !newAssigneeRosterAt(c, "").Resolves(assignee) {
			rt.Fatalf("configured prefix %q did not resolve assignee %q", prefix, assignee)
		}
	})
}

func TestAssigneeRosterResolvesTemplatedRuntimeNames(t *testing.T) {
	c := testRosterCity()
	c.Workspace.Name = "acme"
	c.Workspace.SessionTemplate = "{{.City}}-{{.Agent}}"
	roster := newAssigneeRosterAt(c, "")

	for _, assignee := range []string{
		"acme-polecat",
		"acme-polecat-3",
		"acme-goal-4-context",
		"polecat-pool",
		"acme-polecat-pool",
	} {
		if !roster.Resolves(assignee) {
			t.Errorf("runtime session name %q was rejected; the session template produces it", assignee)
		}
	}
	for _, assignee := range []string{"acme-goal-5-temporal", "other-polecat"} {
		if roster.Resolves(assignee) {
			t.Errorf("assignee %q resolved, want unresolvable", assignee)
		}
	}
}

func TestAssigneeRosterResolvesBeadScopedPoolRuntimeNames(t *testing.T) {
	c := rosterCityWithPrefixes()
	c.Agents = append(c.Agents, config.Agent{Name: "worker", Dir: "research"})
	roster := newAssigneeRosterAt(c, "")

	for _, assignee := range []string{
		PoolSessionName("research/worker", "gc-123456"),
		PoolSessionName("worker", "dr-7"),
		PoolSessionName("city-infra-worker", "gcs-ab12"),
	} {
		if !roster.Resolves(assignee) {
			t.Errorf("bead-scoped pool runtime name %q was rejected; PoolSessionName produces it", assignee)
		}
	}
	for _, assignee := range []string{"worker-xy-123456", "ghost-gc-123456"} {
		if roster.Resolves(assignee) {
			t.Errorf("assignee %q resolved, want unresolvable", assignee)
		}
	}
}

func TestAssigneeRosterResolvesBindingQualifiedPoolRuntimeNames(t *testing.T) {
	c := rosterCityWithPrefixes()
	c.Agents = append(c.Agents, config.Agent{Name: "worker", Dir: "research", BindingName: "pack"})
	roster := newAssigneeRosterAt(c, "")

	assignee := PoolSessionName(c.Agents[len(c.Agents)-1].QualifiedName(), "gc-123456")
	if !roster.Resolves(assignee) {
		t.Errorf("bead-scoped pool runtime name %q of a bound agent was rejected", assignee)
	}
	if roster.Resolves("other__worker-gc-123456") {
		t.Error("pool runtime name under an undeclared binding resolved, want unresolvable")
	}
}

func TestAssigneeRosterResolvesConfiguredTmuxAliases(t *testing.T) {
	c := rosterCityWithPrefixes()
	c.Agents = append(c.Agents,
		config.Agent{Name: "worker", TmuxAlias: "chair"},
		config.Agent{Name: "scout", Dir: "research", TmuxAlias: "{{.Rig}}-crew"},
	)
	roster := newAssigneeRosterAt(c, "")

	for _, assignee := range []string{"chair", "chair-2", "chair-gc-123456", "research-crew", "research-crew-3"} {
		if !roster.Resolves(assignee) {
			t.Errorf("tmux_alias session name %q was rejected", assignee)
		}
	}
	if roster.Resolves("stool") {
		t.Error("undeclared alias resolved, want unresolvable")
	}
}

func TestAssigneeRosterResolvesTmuxAliasWithCityPath(t *testing.T) {
	c := rosterCityWithPrefixes()
	c.Agents = append(c.Agents, config.Agent{Name: "worker", TmuxAlias: "{{.CityRoot}}"})
	roster := newAssigneeRosterAt(c, "/srv/city")

	if !roster.Resolves("--srv--city") {
		t.Error("tmux_alias using CityRoot did not resolve from the roster city path")
	}
}

func TestAssigneeRosterSurfacesTmuxAliasResolutionErrors(t *testing.T) {
	c := rosterCityWithPrefixes()
	c.Agents = append(c.Agents, config.Agent{Name: "worker", TmuxAlias: "{{.Unknown}}"})
	roster := newAssigneeRosterAt(c, "/srv/city")

	if len(roster.problemsList()) != 1 || !strings.Contains(roster.problemsList()[0], "worker") {
		t.Fatalf("problemsList() = %v, want the worker tmux_alias error", roster.problemsList())
	}
}

func TestAssigneeRosterKeepsExactNamesEndingInPoolSuffix(t *testing.T) {
	c := rosterCityWithPrefixes()
	c.NamedSessions = append(c.NamedSessions, config.NamedSession{Name: "intake-pool"})
	roster := newAssigneeRosterAt(c, "")

	if !roster.Resolves("intake-pool") {
		t.Error("configured named session intake-pool was rejected after the -pool suffix was trimmed")
	}
}

func TestAssigneeRosterIgnoresUndeclaredRigQualifier(t *testing.T) {
	c := rosterCityWithPrefixes()
	c.Agents = append(c.Agents, config.Agent{Name: "worker", Dir: "research"})
	roster := newAssigneeRosterAt(c, "")

	for _, assignee := range []string{"research/worker", "research/worker-3"} {
		if !roster.Resolves(assignee) {
			t.Errorf("assignee %q was rejected; research is a declared rig", assignee)
		}
	}
	for _, assignee := range []string{"wrongrig/worker", "wrongrig/polecat", "wrongrig/worker-3", "/worker"} {
		if roster.Resolves(assignee) {
			t.Errorf("assignee %q resolved on its trailing segment although its rig qualifier is undeclared", assignee)
		}
	}
}

func TestAssigneeRosterRejectsAgentUnderDifferentDeclaredRig(t *testing.T) {
	c := rosterCityWithPrefixes()
	c.Rigs = append(c.Rigs, config.Rig{Name: "delivery", Path: "delivery", Prefix: "dl"})
	c.Agents = append(c.Agents, config.Agent{Name: "worker", Dir: "research"})
	roster := newAssigneeRosterAt(c, "")

	if roster.Resolves("delivery/worker") {
		t.Error("agent resolved under a different declared rig")
	}
}

func TestAssigneeRosterNeverCrossesDeclaredRigBoundaries(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		agentRig := rapid.StringMatching(`[a-z]{1,8}`).Draw(rt, "agentRig")
		otherRig := agentRig + "x"
		c := &config.City{
			Rigs: []config.Rig{
				{Name: agentRig, Path: agentRig, Prefix: "ar"},
				{Name: otherRig, Path: otherRig, Prefix: "or"},
			},
			Agents: []config.Agent{{Name: "worker", Dir: agentRig}},
		}
		if newAssigneeRosterAt(c, "").Resolves(otherRig + "/worker") {
			rt.Fatalf("agent in rig %q resolved under rig %q", agentRig, otherRig)
		}
	})
}

func TestAssigneeRosterIgnoresUndeclaredBindingQualifier(t *testing.T) {
	c := rosterCityWithPrefixes()
	c.Agents = append(c.Agents, config.Agent{Name: "worker", BindingName: "pack"})
	roster := newAssigneeRosterAt(c, "")

	for _, assignee := range []string{"pack.worker", "pack.worker-2"} {
		if !roster.Resolves(assignee) {
			t.Errorf("assignee %q was rejected; pack is a declared binding", assignee)
		}
	}
	for _, assignee := range []string{"other.worker", "other.polecat", "research/pack.worker-2", "research/other.worker"} {
		if roster.Resolves(assignee) {
			t.Errorf("assignee %q resolved on its trailing segment although its binding qualifier is undeclared", assignee)
		}
	}
}
