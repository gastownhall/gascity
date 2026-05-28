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

func TestResolveAgentPoolMemberRejectsCanonicalSingletonPoolSuffix(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
		},
	}
	if a, ok := ResolveAgent(cfg, "myrig/worker", ResolveOpts{AllowPoolMembers: true}); !ok || a.QualifiedName() != "myrig/worker" {
		t.Fatalf("ResolveAgent(myrig/worker) = (%q, %v), want canonical template", a.QualifiedName(), ok)
	}
	if _, ok := ResolveAgent(cfg, "myrig/worker-1", ResolveOpts{AllowPoolMembers: true}); ok {
		t.Fatal("ResolveAgent(myrig/worker-1) = true, want false for canonical singleton pool")
	}
	if _, ok := ResolveAgent(cfg, "worker-1", ResolveOpts{AllowPoolMembers: true}); ok {
		t.Fatal("ResolveAgent(worker-1) = true, want false for canonical singleton pool")
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

func TestResolveAgentQualifiedGenericRigScopedTemplate(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "reviewer", Scope: "rig"},
		},
		Rigs: []config.Rig{
			{Name: "alpha"},
			{Name: "beta"},
		},
	}

	a, ok := ResolveAgent(cfg, "alpha/reviewer", ResolveOpts{AllowPoolMembers: true})
	if !ok {
		t.Fatal("expected to resolve alpha/reviewer")
	}
	if got := a.QualifiedName(); got != "alpha/reviewer" {
		t.Fatalf("QualifiedName() = %q, want %q", got, "alpha/reviewer")
	}

	template, ok := ResolveAgent(cfg, "beta/reviewer", ResolveOpts{TemplateOnly: true})
	if !ok {
		t.Fatal("expected TemplateOnly to resolve beta/reviewer")
	}
	if got := template.QualifiedName(); got != "beta/reviewer" {
		t.Fatalf("TemplateOnly QualifiedName() = %q, want %q", got, "beta/reviewer")
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

func TestNormalizePoolRouteTarget(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(3)},
			{Name: "solo", Dir: "myrig", MaxActiveSessions: intPtr(1)},
			{Name: "witness", BindingName: "gastown", MaxActiveSessions: intPtr(-1)},
			{Name: "mayor"},
		},
	}
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{"qualified slot instance collapses to base", "myrig/polecat-2", "myrig/polecat"},
		{"highest in-range slot collapses", "myrig/polecat-3", "myrig/polecat"},
		{"binding slot instance collapses to base", "gastown.witness-7", "gastown.witness"},
		{"base qualified name is left unchanged", "myrig/polecat", "myrig/polecat"},
		{"out-of-range slot is left unchanged", "myrig/polecat-9", "myrig/polecat-9"},
		{"zero slot is left unchanged", "myrig/polecat-0", "myrig/polecat-0"},
		{"non-numeric suffix is left unchanged", "myrig/polecat-foo", "myrig/polecat-foo"},
		{"singleton pool is left unchanged", "myrig/solo-1", "myrig/solo-1"},
		{"non-pool agent is left unchanged", "mayor", "mayor"},
		{"unknown target is left unchanged", "myrig/ghost-2", "myrig/ghost-2"},
		{"empty target is left unchanged", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizePoolRouteTarget(cfg, tt.target); got != tt.want {
				t.Errorf("NormalizePoolRouteTarget(%q) = %q, want %q", tt.target, got, tt.want)
			}
		})
	}
}

func TestNormalizePoolRouteTargetNilConfig(t *testing.T) {
	if got := NormalizePoolRouteTarget(nil, "myrig/polecat-2"); got != "myrig/polecat-2" {
		t.Errorf("NormalizePoolRouteTarget(nil) = %q, want unchanged", got)
	}
}
