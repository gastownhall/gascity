package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Config Namespace
// - Ambient rig resolution
// - provider/create factory-targeting compatibility boundary

func TestPhase0APIFactoryResolution_NoCrossRigUniqueBareFallbackWithoutExplicitScope(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "demo"},
		},
	}

	if _, ok := resolveSessionTemplateAgent(cfg, "worker"); ok {
		t.Fatal("resolveSessionTemplateAgent(worker) succeeded, want qualification-required failure")
	}
}
