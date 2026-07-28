package workdir

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func demoRigs(cityPath string) []config.Rig {
	return []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}
}

func TestValidatePoolWorkDirIsolationRejectsUnsetWorkDirForPooledAgent(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name:              "polecat",
		Dir:               "demo",
		MaxActiveSessions: intPtr(2),
	}}

	err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath))
	if err == nil {
		t.Fatal("expected error for pooled agent with unset work_dir, got nil")
	}
	if !strings.Contains(err.Error(), "polecat") {
		t.Fatalf("error %q does not identify the affected agent", err.Error())
	}
}

func TestValidatePoolWorkDirIsolationRejectsConstantWorkDirForPooledAgent(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name:              "polecat",
		Dir:               "demo",
		WorkDir:           ".gc/worktrees/demo/polecat",
		MaxActiveSessions: intPtr(3),
	}}

	err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath))
	if err == nil {
		t.Fatal("expected error for pooled agent with constant work_dir, got nil")
	}
}

func TestValidatePoolWorkDirIsolationRejectsTemplateThatDoesNotVaryByInstance(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name:              "polecat",
		Dir:               "demo",
		WorkDir:           ".gc/worktrees/{{.Rig}}/shared",
		MaxActiveSessions: intPtr(2),
	}}

	err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath))
	if err == nil {
		t.Fatal("expected error for a templated work_dir that does not vary by instance, got nil")
	}
}

func TestValidatePoolWorkDirIsolationAcceptsPerInstanceTemplate(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name:              "polecat",
		Dir:               "demo",
		WorkDir:           ".gc/worktrees/{{.Rig}}/{{.AgentBase}}",
		MaxActiveSessions: intPtr(3),
	}}

	if err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath)); err != nil {
		t.Fatalf("expected no error for a per-instance work_dir template, got %v", err)
	}
}

func TestValidatePoolWorkDirIsolationRejectsMalformedTemplate(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name:              "polecat",
		Dir:               "demo",
		WorkDir:           "{{.NoSuchField}}",
		MaxActiveSessions: intPtr(2),
	}}

	err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath))
	if err == nil {
		t.Fatal("expected error for an unresolvable work_dir template, got nil")
	}
}

func TestValidatePoolWorkDirIsolationAcceptsExplicitSingleton(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name:              "mayor",
		Dir:               "demo",
		MaxActiveSessions: intPtr(1),
	}}

	if err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath)); err != nil {
		t.Fatalf("expected no error for an explicit singleton agent, got %v", err)
	}
}

func TestValidatePoolWorkDirIsolationAcceptsZeroMaxActiveSessions(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name:              "dormant",
		Dir:               "demo",
		MaxActiveSessions: intPtr(0),
	}}

	if err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath)); err != nil {
		t.Fatalf("expected no error for max_active_sessions=0, got %v", err)
	}
}

func TestValidatePoolWorkDirIsolationRejectsUnlimitedMaxActiveSessionsWithSharedWorkDir(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name: "drifter",
		Dir:  "demo",
		// MaxActiveSessions left nil ("unlimited" per EffectiveMaxActiveSessions),
		// but MinActiveSessions is an explicit pool-flavor marker, so this is
		// an opted-in pool (not a bare default) and must still be checked.
		MinActiveSessions: intPtr(0),
	}}

	err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath))
	if err == nil {
		t.Fatal("expected error for an explicitly-signaled unlimited pool with a shared work_dir, got nil")
	}
}

// TestValidatePoolWorkDirIsolationAcceptsUnsetMaxActiveSessionsWithoutExplicitPoolSignal
// locks in a regression found via make test-fast-parallel: writeCityTOML (and
// the equivalent minimal "[[agent]] name = \"mayor\"" shape used throughout
// cmd/gc's controller tests and tutorials) sets no max_active_sessions,
// min_active_sessions, scale_check, namepool, or work_dir at all. Nothing in
// today's system spontaneously creates a second concurrent "mayor" instance
// for that shape, so it must not be hard-rejected merely because
// max_active_sessions was never set (see requiresPoolWorkDirIsolationCheck).
func TestValidatePoolWorkDirIsolationAcceptsUnsetMaxActiveSessionsWithoutExplicitPoolSignal(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name: "mayor",
		Dir:  "demo",
	}}

	if err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath)); err != nil {
		t.Fatalf("expected no error for a default agent with no explicit pool signal, got %v", err)
	}
}

func TestValidatePoolWorkDirIsolationRejectsExplicitUnlimitedNegativeOne(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name:              "drifter",
		Dir:               "demo",
		MaxActiveSessions: intPtr(-1),
	}}

	err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath))
	if err == nil {
		t.Fatal("expected error for max_active_sessions=-1 (unlimited) with a shared work_dir, got nil")
	}
}

func TestValidatePoolWorkDirIsolationRejectsNamepoolAgentWithSharedWorkDir(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name:          "ant",
		Dir:           "demo",
		WorkDir:       ".gc/worktrees/demo/ants",
		Namepool:      "names.txt",
		NamepoolNames: []string{"fenrir", "grendel"},
	}}

	err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath))
	if err == nil {
		t.Fatal("expected error for a namepool agent with a shared work_dir, got nil")
	}
}

func TestValidatePoolWorkDirIsolationAcceptsNamepoolAgentWithPerInstanceTemplate(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name:          "ant",
		Dir:           "demo",
		WorkDir:       ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		Namepool:      "names.txt",
		NamepoolNames: []string{"fenrir", "grendel"},
	}}

	if err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath)); err != nil {
		t.Fatalf("expected no error for a namepool agent with a per-instance work_dir, got %v", err)
	}
}

// TestValidatePoolWorkDirIsolationAcceptsRealGascityAndTincanBuilderPools mirrors
// the live packs/actual/builder/pack.toml work_dir template plus the city.toml
// rig-patch overrides for the gascity (max=3) and tincan (max=2) builder pools
// (ga-ighomh.2 acceptance criterion #7): neither rig patch overrides work_dir,
// so both inherit the AgentBase-templated default and must resolve cleanly.
func TestValidatePoolWorkDirIsolationAcceptsRealGascityAndTincanBuilderPools(t *testing.T) {
	cityPath := t.TempDir()
	const packWorkDirTemplate = ".gc/worktrees/{{.Rig}}/{{.AgentBase}}"

	cases := []struct {
		rig string
		max int
	}{
		{rig: "gascity", max: 3},
		{rig: "tincan", max: 2},
	}
	for _, tc := range cases {
		t.Run(tc.rig, func(t *testing.T) {
			rigs := []config.Rig{{Name: tc.rig, Path: filepath.Join(cityPath, "rigs", tc.rig)}}
			agents := []config.Agent{{
				Name:              "builder",
				Dir:               tc.rig,
				WorkDir:           packWorkDirTemplate,
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(tc.max),
			}}
			if err := ValidatePoolWorkDirIsolation(cityPath, "gascity-management", agents, rigs); err != nil {
				t.Fatalf("expected no error for the real %s builder pool shape, got %v", tc.rig, err)
			}
		})
	}
}

func TestValidatePoolWorkDirIsolationOnlyFlagsTheOffendingAgent(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{
		{
			Name:              "good",
			Dir:               "demo",
			WorkDir:           ".gc/worktrees/{{.Rig}}/{{.AgentBase}}",
			MaxActiveSessions: intPtr(2),
		},
		{
			Name:              "bad",
			Dir:               "demo",
			WorkDir:           ".gc/worktrees/demo/bad",
			MaxActiveSessions: intPtr(2),
		},
	}

	err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath))
	if err == nil {
		t.Fatal("expected error identifying the offending agent, got nil")
	}
	if strings.Contains(err.Error(), "\"demo/good\"") {
		t.Fatalf("error incorrectly implicates the well-configured agent: %v", err)
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Fatalf("error %q does not identify the offending agent", err.Error())
	}
}

func TestValidatePoolWorkDirIsolationErrorDoesNotHardcodeARoleName(t *testing.T) {
	cityPath := t.TempDir()
	agents := []config.Agent{{
		Name:              "widget-runner",
		Dir:               "demo",
		MaxActiveSessions: intPtr(2),
	}}

	err := ValidatePoolWorkDirIsolation(cityPath, "gastown", agents, demoRigs(cityPath))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, role := range []string{"builder", "mayor", "deacon", "polecat", "supervisor"} {
		if strings.Contains(strings.ToLower(err.Error()), role) {
			t.Fatalf("error message hardcodes role name %q: %v", role, err.Error())
		}
	}
}
