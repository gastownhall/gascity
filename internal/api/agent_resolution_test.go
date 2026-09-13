package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestResolveSessionTemplateAgentAcceptsConfiguredTemplates(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "city-agent"},
			{Name: "worker", Dir: "rig-a"},
		},
	}

	a, ok := resolveSessionTemplateAgent(cfg, "rig-a/worker")
	if !ok {
		t.Fatal("expected qualified template lookup to succeed")
	}
	if got := a.QualifiedName(); got != "rig-a/worker" {
		t.Fatalf("QualifiedName = %q, want rig-a/worker", got)
	}

	a, ok = resolveSessionTemplateAgent(cfg, "city-agent")
	if !ok {
		t.Fatal("expected bare template lookup to succeed")
	}
	if got := a.QualifiedName(); got != "city-agent" {
		t.Fatalf("QualifiedName = %q, want city-agent", got)
	}
}

func TestResolveSessionTemplateAgentRejectsDerivedPoolMembers(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
		},
	}

	if _, ok := resolveSessionTemplateAgent(cfg, "worker-2"); ok {
		t.Fatal("expected pool member identity to be rejected")
	}
}

func TestResolveSessionTemplateAgentRejectsAmbiguousBareName(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "rig-a"},
			{Name: "worker", Dir: "rig-b"},
		},
	}

	if _, ok := resolveSessionTemplateAgent(cfg, "worker"); ok {
		t.Fatal("expected ambiguous bare name to fail")
	}
}

func TestFindAgentByQualifiedTemplateNilConfig(t *testing.T) {
	// The endpoint 503 path short-circuits before this helper, so the nil
	// guard is only reachable on a concurrent config swap-to-nil. Assert it
	// directly: a nil config must return no match without dereferencing.
	a, ok := findAgentByQualifiedTemplate(nil, "x")
	if ok {
		t.Fatal("expected nil config to yield no match")
	}
	if got := a.QualifiedName(); got != "" {
		t.Fatalf("QualifiedName = %q, want empty zero-value agent", got)
	}
}
