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

func TestPhase0SessionTargeting_RejectsTemplateToken(t *testing.T) {
	t.Setenv("GC_SESSION", "phase0")

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
