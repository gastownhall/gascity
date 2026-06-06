package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Config Namespace
// - Ambient rig resolution
// - template: scope

func TestPhase0FactoryResolution_BareNameRequiresQualificationWhenCityAndRigConfigBothVisible(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker"},
			{Name: "worker", Dir: "demo"},
		},
	}

	if _, ok := resolveSessionTemplate(cfg, "worker", "demo"); ok {
		t.Fatal("resolveSessionTemplate(worker, demo) succeeded, want qualification-required failure")
	}
}

func TestPhase0FactoryResolution_NoCrossRigUniqueBareFallbackWithoutAmbientRig(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "demo"},
		},
	}

	if _, ok := resolveSessionTemplate(cfg, "worker", ""); ok {
		t.Fatal("resolveSessionTemplate(worker, no-rig) succeeded, want qualification-required failure")
	}
}

func TestResolveSessionTemplate_BindingQualifiedExactMatch(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "interface-lead", BindingName: "ar"},
			{Name: "principal", BindingName: "ar"},
		},
	}
	a, ok := resolveSessionTemplate(cfg, "ar.interface-lead", "")
	if !ok {
		t.Fatal("resolveSessionTemplate(ar.interface-lead) failed; expected match on binding-qualified name")
	}
	if got := a.QualifiedName(); got != "ar.interface-lead" {
		t.Errorf("matched agent QualifiedName = %q, want ar.interface-lead", got)
	}
}

func TestResolveSessionTemplate_BindingQualifiedNoBareFallback(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "interface-lead", BindingName: "ar"},
		},
	}
	if _, ok := resolveSessionTemplate(cfg, "wrong.interface-lead", ""); ok {
		t.Fatal("resolveSessionTemplate(wrong.interface-lead) succeeded; binding-qualified miss must not fall through to bare-name")
	}
}

func TestResolveSessionTemplate_BareNameFallbackForBoundAgent(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "interface-lead", BindingName: "ar"},
		},
	}
	a, ok := resolveSessionTemplate(cfg, "interface-lead", "")
	if !ok {
		t.Fatal("resolveSessionTemplate(interface-lead) failed; bare local name must still resolve when unique")
	}
	if got := a.QualifiedName(); got != "ar.interface-lead" {
		t.Errorf("matched agent QualifiedName = %q, want ar.interface-lead", got)
	}
}

func TestResolveSessionTemplate_BindingQualifiedUsesCurrentRig(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "interface-lead", BindingName: "ar", Dir: "demo"},
			{Name: "interface-lead", BindingName: "ar", Dir: "other"},
		},
	}
	a, ok := resolveSessionTemplate(cfg, "ar.interface-lead", "demo")
	if !ok {
		t.Fatal("resolveSessionTemplate(ar.interface-lead, demo) failed; expected current-rig binding-qualified match")
	}
	if got := a.QualifiedName(); got != "demo/ar.interface-lead" {
		t.Errorf("matched agent QualifiedName = %q, want demo/ar.interface-lead", got)
	}
}

func TestResolveSessionTemplate_BindingQualifiedRequiresCurrentRig(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "interface-lead", BindingName: "ar", Dir: "demo"},
		},
	}
	if _, ok := resolveSessionTemplate(cfg, "ar.interface-lead", "other"); ok {
		t.Fatal("resolveSessionTemplate(ar.interface-lead, other) succeeded; wrong current rig must not match")
	}
}

func TestResolveSessionTemplate_RigBindingQualifiedExactMatch(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "interface-lead", BindingName: "ar", Dir: "demo"},
		},
	}
	a, ok := resolveSessionTemplate(cfg, "demo/ar.interface-lead", "")
	if !ok {
		t.Fatal("resolveSessionTemplate(demo/ar.interface-lead) failed; expected rig binding-qualified match")
	}
	if got := a.QualifiedName(); got != "demo/ar.interface-lead" {
		t.Errorf("matched agent QualifiedName = %q, want demo/ar.interface-lead", got)
	}
}

func TestPhase0SessionTargeting_RejectsTemplateToken(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "true",
		}},
	}

	_, err := resolveSessionIDMaterializingNamed(t.TempDir(), cfg, store, "template:worker")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveSessionIDMaterializingNamed(template:worker) error = %v, want ErrSessionNotFound on session-targeting surface", err)
	}

	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("session count = %d, want 0", len(all))
	}
}
