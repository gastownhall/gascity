package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Runtime Environment
// - GC_TEMPLATE/GC_AGENT/GC_SESSION_ORIGIN contracts

func TestPhase0RuntimeEnv_TemplateResolutionSetsOriginAndPublicHandle(t *testing.T) {
	params := &agentBuildParams{
		cityName:   "phase0-city",
		cityPath:   t.TempDir(),
		workspace:  &config.Workspace{Provider: "test-agent"},
		providers:  map[string]config.ProviderSpec{"test-agent": {DisplayName: "Test Agent", Command: "true"}},
		lookPath:   func(string) (string, error) { return filepath.Join("/usr/bin", "true"), nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agentCfg := &config.Agent{
		Name:     "worker",
		Provider: "test-agent",
		WorkDir:  filepath.Join(".gc", "agents", "phase0"),
	}

	tp, err := resolveTemplate(params, agentCfg, agentCfg.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(worker): %v", err)
	}
	if got := tp.Env["GC_TEMPLATE"]; got != "worker" {
		t.Fatalf("GC_TEMPLATE = %q, want worker", got)
	}
	if got := tp.Env["GC_SESSION_ORIGIN"]; got == "" {
		t.Fatal("GC_SESSION_ORIGIN = empty, want explicit origin")
	}
	// The template layer's GC_AGENT == GC_SESSION_NAME stamp is PROVISIONAL, not
	// the contract: GC_AGENT's contract is to mirror BEADS_ACTOR (the
	// session.AssigneeIdentifier selector), and for an unaliased pool worker that
	// is the session bead ID. This row is load-bearing only because spawn merges
	// the session runtime projection AFTER the template env, which overwrites
	// both keys with the selected identity. Assert the pre-merge stamp here, and
	// see TestRuntimeEnvWithSessionContextAlignsAgentAndBeadsActor for the value
	// a running session actually receives.
	if got := tp.Env["GC_AGENT"]; got != tp.Env["GC_SESSION_NAME"] {
		t.Fatalf("GC_AGENT = %q, want the provisional pre-projection stamp %q", got, tp.Env["GC_SESSION_NAME"])
	}
}

func TestPhase0RuntimeEnv_TemplateResolutionDoesNotPublishLifecycleBeadsWrapper(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	params := &agentBuildParams{
		cityName:   "phase0-city",
		cityPath:   t.TempDir(),
		workspace:  &config.Workspace{Provider: "test-agent"},
		providers:  map[string]config.ProviderSpec{"test-agent": {DisplayName: "Test Agent", Command: "true"}},
		lookPath:   func(string) (string, error) { return filepath.Join("/usr/bin", "true"), nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agentCfg := &config.Agent{
		Name:     "mayor",
		Provider: "test-agent",
	}

	tp, err := resolveTemplate(params, agentCfg, agentCfg.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(mayor): %v", err)
	}
	if got := tp.Env["GC_BEADS"]; strings.Contains(got, "gc-beads-bd") {
		t.Fatalf("GC_BEADS = %q, want data-path provider value, not lifecycle wrapper", got)
	}
}
