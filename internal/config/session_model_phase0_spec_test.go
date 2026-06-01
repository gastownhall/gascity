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
// - Cap Accounting for mode=always named sessions
// Keep these cases unique; a prior rebase duplicated the trailing block and
// broke CI typechecking.

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

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("LoadWithIncludes(city.toml): %v", err)
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
	if !strings.Contains(got, "for key in gc.run_target gc.routed_to") || !strings.Contains(got, "-- myrig/worker") {
		t.Fatalf("EffectiveWorkQuery() = %q, want qualified config route argument", got)
	}
}

func TestPhase0ConfigDefaults_OnBootUnclaimsRoutedWorkByDefault(t *testing.T) {
	a := Agent{Name: "worker", Dir: "myrig"}

	got := a.EffectiveOnBoot()
	for _, want := range []string{
		"template='myrig/worker'",
		"for key in gc.run_target gc.routed_to",
		`--metadata-field "$key=$template"`,
		"--status=in_progress",
		"--no-assignee",
		"--status open",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("EffectiveOnBoot() = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, `--assignee ""`) {
		t.Fatalf("EffectiveOnBoot() = %q, want to target only ownerless work instead of bulk-unassigning routed work", got)
	}
}

func TestPhase0ConfigDefaults_OnDeathUnclaimsAssignedWorkByDefault(t *testing.T) {
	a := Agent{Name: "worker", Dir: "myrig"}

	got := a.EffectiveOnDeath()
	for _, want := range []string{
		"bd list --include-ephemeral --assignee=myrig/worker",
		"--status=in_progress",
		"--assignee \"\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("EffectiveOnDeath() = %q, want %q", got, want)
		}
	}
}

func TestPhase0NamedSessionConfig_DuplicateExplicitNamesRejectedAcrossTemplates(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city.toml")
	configText := `[workspace]
name = "test-city"

[[agent]]
name = "reviewer"
start_command = "true"

[[agent]]
name = "coder"
start_command = "true"

[[named_session]]
name = "mayor"
template = "reviewer"

[[named_session]]
name = "mayor"
template = "coder"
`
	if err := os.WriteFile(cityPath, []byte(configText), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	if _, err := Load(fsys.OSFS{}, cityPath); err == nil {
		t.Fatal("Load(city.toml) error = nil, want duplicate configured named-session identity rejection")
	}
}

func TestPhase0NamedSessionConfig_AlwaysModeCannotExceedBackingConfigCapacity(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city.toml")
	configText := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1

[[named_session]]
name = "one"
template = "worker"
mode = "always"

[[named_session]]
name = "two"
template = "worker"
mode = "always"
`
	if err := os.WriteFile(cityPath, []byte(configText), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	_, err := Load(fsys.OSFS{}, cityPath)
	if err == nil {
		t.Fatal("Load(city.toml) error = nil, want mode=always named-session capacity rejection")
	}
	if !strings.Contains(err.Error(), "max_active_sessions") && !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("Load(city.toml) error = %v, want explicit capacity/max_active_sessions rejection", err)
	}
}

func TestPhase0NamedSessionConfig_OmittedNameDefaultsToTemplateIdentity(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city.toml")
	configText := `[workspace]
name = "test-city"

[[agent]]
name = "reviewer"
start_command = "true"

[[named_session]]
template = "reviewer"
`
	if err := os.WriteFile(cityPath, []byte(configText), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("LoadWithIncludes(city.toml): %v", err)
	}
	if len(cfg.NamedSessions) != 1 {
		t.Fatalf("len(NamedSessions) = %d, want 1", len(cfg.NamedSessions))
	}
	if got := cfg.NamedSessions[0].QualifiedName(); got != "reviewer" {
		t.Fatalf("QualifiedName = %q, want compatibility default reviewer", got)
	}
}

func TestPhase0NamedSessionConfig_ExpandsGenericRigScopedNamedSessionsPerRig(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city.toml")
	configText := `[workspace]
name = "test-city"

[[agent]]
name = "reviewer"
scope = "rig"
start_command = "true"

[[named_session]]
template = "reviewer"
scope = "rig"

[[rigs]]
name = "alpha"
path = "/tmp/alpha"

[[rigs]]
name = "beta"
path = "/tmp/beta"
`
	if err := os.WriteFile(cityPath, []byte(configText), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("LoadWithIncludes(city.toml): %v", err)
	}
	if len(cfg.NamedSessions) != 2 {
		t.Fatalf("len(NamedSessions) = %d, want 2", len(cfg.NamedSessions))
	}
	if got := cfg.NamedSessions[0].QualifiedName(); got != "alpha/reviewer" {
		t.Fatalf("NamedSessions[0] = %q, want alpha/reviewer", got)
	}
	if got := cfg.NamedSessions[1].QualifiedName(); got != "beta/reviewer" {
		t.Fatalf("NamedSessions[1] = %q, want beta/reviewer", got)
	}
	if got := cfg.NamedSessions[0].TemplateQualifiedName(); got != "alpha/reviewer" {
		t.Fatalf("TemplateQualifiedName() = %q, want alpha/reviewer", got)
	}
	if agent := FindAgent(cfg, "alpha/reviewer"); agent == nil {
		t.Fatal("FindAgent(alpha/reviewer) = nil, want backing template")
	}
}
