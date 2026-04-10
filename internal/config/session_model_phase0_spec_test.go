package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Named Sessions / explicit name distinct from template
// - Default work_query contract
// - Default on_boot / on_death hooks

func TestPhase0NamedSessionConfig_ExplicitNameCreatesDistinctIdentityFromTemplate(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city.toml")
	configText := `[workspace]
name = "test-city"

[[agent]]
name = "reviewer"
start_command = "true"
max_active_sessions = 2

[[named_session]]
name = "mayor"
template = "reviewer"

[[named_session]]
name = "triage"
template = "reviewer"
`
	if err := os.WriteFile(cityPath, []byte(configText), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	cfg, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load(city.toml): %v", err)
	}
	if len(cfg.NamedSessions) != 2 {
		t.Fatalf("len(NamedSessions) = %d, want 2", len(cfg.NamedSessions))
	}
	if got := cfg.NamedSessions[0].QualifiedName(); got != "mayor" {
		t.Fatalf("first QualifiedName = %q, want mayor", got)
	}
	if got := cfg.NamedSessions[1].QualifiedName(); got != "triage" {
		t.Fatalf("second QualifiedName = %q, want triage", got)
	}
	if got := cfg.NamedSessions[0].Template; got != "reviewer" {
		t.Fatalf("first Template = %q, want reviewer", got)
	}
	if got := cfg.NamedSessions[1].Template; got != "reviewer" {
		t.Fatalf("second Template = %q, want reviewer", got)
	}
	if FindNamedSession(cfg, "mayor") == nil {
		t.Fatal("FindNamedSession(cfg, mayor) = nil, want named identity mayor")
	}
	if FindNamedSession(cfg, "triage") == nil {
		t.Fatal("FindNamedSession(cfg, triage) = nil, want named identity triage")
	}
	if FindAgent(cfg, "reviewer") == nil {
		t.Fatal("FindAgent(cfg, reviewer) = nil, want backing config reviewer")
	}
}

func TestPhase0ConfigDefaults_WorkQueryIsOriginAware(t *testing.T) {
	a := Agent{Name: "worker", Dir: "myrig"}

	got := a.EffectiveWorkQuery()

	if !strings.Contains(got, "GC_SESSION_ORIGIN") {
		t.Fatalf("EffectiveWorkQuery() = %q, want origin-aware GC_SESSION_ORIGIN branch", got)
	}
	if !strings.Contains(got, "ephemeral") {
		t.Fatalf("EffectiveWorkQuery() = %q, want origin-specific ephemeral generic queue tier", got)
	}
	if !strings.Contains(got, "gc.routed_to=myrig/worker") {
		t.Fatalf("EffectiveWorkQuery() = %q, want qualified config route", got)
	}
}

func TestPhase0ConfigDefaults_OnBootIsNoOpByDefault(t *testing.T) {
	a := Agent{Name: "worker", Dir: "myrig"}

	if got := a.EffectiveOnBoot(); got != "" {
		t.Fatalf("EffectiveOnBoot() = %q, want empty default", got)
	}
}

func TestPhase0ConfigDefaults_OnDeathIsNoOpByDefault(t *testing.T) {
	a := Agent{Name: "worker", Dir: "myrig"}

	if got := a.EffectiveOnDeath(); got != "" {
		t.Fatalf("EffectiveOnDeath() = %q, want empty default", got)
	}
}
