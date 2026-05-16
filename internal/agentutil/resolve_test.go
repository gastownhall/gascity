package agentutil

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func intPtr(v int) *int { return &v }

func TestResolveAgentLiteralQualified(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig"},
			{Name: "mayor"},
		},
	}
	a, ok := ResolveAgent(cfg, "myrig/worker", ResolveOpts{})
	if !ok {
		t.Fatal("expected to resolve myrig/worker")
	}
	if a.QualifiedName() != "myrig/worker" {
		t.Errorf("got %q", a.QualifiedName())
	}
}

func TestResolveAgentLiteralBindingQualified(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "interface-lead", BindingName: "ar"},
			{Name: "interface-lead", BindingName: "ar", Dir: "demo"},
		},
	}
	a, ok := ResolveAgent(cfg, "ar.interface-lead", ResolveOpts{})
	if !ok {
		t.Fatal("expected to resolve ar.interface-lead")
	}
	if got := a.QualifiedName(); got != "ar.interface-lead" {
		t.Errorf("got %q, want ar.interface-lead", got)
	}

	a, ok = ResolveAgent(cfg, "demo/ar.interface-lead", ResolveOpts{})
	if !ok {
		t.Fatal("expected to resolve demo/ar.interface-lead")
	}
	if got := a.QualifiedName(); got != "demo/ar.interface-lead" {
		t.Errorf("got %q, want demo/ar.interface-lead", got)
	}
}

func TestResolveAgentBareName(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor"},
		},
	}
	a, ok := ResolveAgent(cfg, "mayor", ResolveOpts{})
	if !ok {
		t.Fatal("expected to resolve mayor")
	}
	if a.Name != "mayor" {
		t.Errorf("got %q", a.Name)
	}
}

func TestResolveAgentAmbiguousBareNameFails(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "claude", Dir: "rig1"},
			{Name: "claude", Dir: "rig2"},
		},
	}
	_, ok := ResolveAgent(cfg, "claude", ResolveOpts{})
	if ok {
		t.Error("expected ambiguous bare name to fail")
	}
}

func TestResolveAgentWithRigContext(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "claude", Dir: "rig1"},
			{Name: "claude", Dir: "rig2"},
		},
	}
	// With rig context, bare name should prefer the contextual rig.
	a, ok := ResolveAgent(cfg, "claude", ResolveOpts{
		UseAmbientRig: true,
		RigContext:    "rig1",
	})
	if !ok {
		t.Fatal("expected to resolve with rig context")
	}
	if a.QualifiedName() != "rig1/claude" {
		t.Errorf("got %q, want rig1/claude", a.QualifiedName())
	}
}

func TestResolveAgentTemplateOnlyRejectsPoolMember(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(3)},
		},
	}
	// Template mode: "myrig/polecat-2" should fail (pool member, not template).
	_, ok := ResolveAgent(cfg, "myrig/polecat-2", ResolveOpts{TemplateOnly: true})
	if ok {
		t.Error("expected TemplateOnly to reject pool member")
	}
}

func TestResolveAgentPoolMemberAllowed(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(3)},
		},
	}
	// Dispatch mode: pool members should resolve.
	a, ok := ResolveAgent(cfg, "myrig/polecat-2", ResolveOpts{AllowPoolMembers: true})
	if !ok {
		t.Fatal("expected pool member to resolve")
	}
	if a.Name != "polecat-2" {
		t.Errorf("got name %q, want polecat-2", a.Name)
	}
}

func TestResolveAgentCityScopedBindingQualifiedPoolMemberAllowed(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "witness", BindingName: "gastown", MaxActiveSessions: intPtr(-1)},
		},
	}
	a, ok := ResolveAgent(cfg, "gastown.witness-1", ResolveOpts{AllowPoolMembers: true})
	if !ok {
		t.Fatal("expected binding-qualified pool member to resolve")
	}
	if got := a.QualifiedName(); got != "gastown.witness-1" {
		t.Errorf("got %q, want gastown.witness-1", got)
	}
}

func TestResolveAgentTemplateOnlyAcceptsTemplate(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", MaxActiveSessions: intPtr(1)},
		},
	}
	a, ok := ResolveAgent(cfg, "myrig/worker", ResolveOpts{TemplateOnly: true})
	if !ok {
		t.Fatal("expected template to resolve")
	}
	if a.Name != "worker" {
		t.Errorf("got %q", a.Name)
	}
}

func TestResolveAgentNotFound(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor"},
		},
	}
	_, ok := ResolveAgent(cfg, "nonexistent", ResolveOpts{})
	if ok {
		t.Error("expected not found")
	}
}
