package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestAlwaysFreshWakeModeWarningIsNonFatalAndEmitted proves the always+fresh
// advisory behaves like a warning on both downstream re-classifiers of config
// warnings: strict mode — on by default for `gc start` — keeps it NON-FATAL,
// and the agent warning-emit path SURFACES it. The bundled gastown pack trips
// this warning, so without the config.IsAlwaysFreshWakeModeWarning wiring
// `gc start --foreground` / `--controller` / `--dry-run` exits 1 on the shipped
// example city, and `gc agent` drops the advisory silently.
//
// The warning text is derived from config.ValidateNamedSessions rather than
// hardcoded so this test cannot pass against a string the validator no longer
// emits.
func TestAlwaysFreshWakeModeWarningIsNonFatalAndEmitted(t *testing.T) {
	warnings, err := config.ValidateNamedSessions(&config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "watchdog", WakeMode: "fresh"}},
		NamedSessions: []config.NamedSession{{
			Template: "watchdog",
			Mode:     "always",
		}},
	})
	if err != nil {
		t.Fatalf("config.ValidateNamedSessions: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly the always+fresh advisory", warnings)
	}
	w := warnings[0]
	if !config.IsAlwaysFreshWakeModeWarning(w) {
		t.Fatalf("always+fresh warning not recognized by its own classifier: %q", w)
	}

	fatal, nonFatal := splitStrictConfigWarnings([]string{w})
	if len(fatal) != 0 || len(nonFatal) != 1 {
		t.Errorf("strict split: fatal=%v nonFatal=%v, want the always+fresh warning non-fatal", fatal, nonFatal)
	}
	if !shouldEmitLoadCityConfigWarning(w) {
		t.Error("an always+fresh warning must be emitted to the operator, not swallowed")
	}
}

func TestSplitStrictConfigWarnings_SiteBindingWarningsAreNonFatal(t *testing.T) {
	fatal, nonFatal := splitStrictConfigWarnings([]string{
		`rig "repo" still declares path in city.toml; move it to .gc/site.toml (run ` + "`gc doctor --fix`" + `)`,
		`.gc/site.toml declares a binding for unknown rig "stale"`,
		`city agent "mayor" shadows agent of the same name from import "gs"`,
	})

	if len(fatal) != 1 || fatal[0] != `city agent "mayor" shadows agent of the same name from import "gs"` {
		t.Fatalf("fatal = %v, want only non-site-binding warning", fatal)
	}
	if len(nonFatal) != 2 {
		t.Fatalf("nonFatal = %v, want 2 site-binding warnings", nonFatal)
	}
}

func TestSplitStrictConfigWarnings_LegacyV1SurfaceWarningsAreNonFatal(t *testing.T) {
	fatal, nonFatal := splitStrictConfigWarnings([]string{
		"city.toml: [[agent]] tables are deprecated in v2; use directory-based agents under agents/<name>/. Run `gc doctor` to inspect; `gc doctor --fix` handles the safe mechanical rewrites available in this wave.",
		"city.toml: [packs] is deprecated in v2; use [imports] + packs.lock. Run `gc doctor` to inspect; `gc doctor --fix` migrates entries referenced by legacy workspace include lists, then migrate or remove any remaining [packs] entries manually.",
		"city.toml: workspace.includes is deprecated in v2; use [imports]. Run `gc doctor` to inspect; `gc doctor --fix` handles the safe mechanical rewrites available in this wave.",
		"city.toml: workspace.default_rig_includes is deprecated in v2; use city.toml [defaults.rig.imports.<binding>]. Run `gc doctor` to inspect; `gc doctor --fix` handles the safe mechanical rewrites available in this wave.",
		`city agent "mayor" shadows agent of the same name from import "gs"`,
	})

	if len(fatal) != 1 || fatal[0] != `city agent "mayor" shadows agent of the same name from import "gs"` {
		t.Fatalf("fatal = %v, want only the shadow warning", fatal)
	}
	if len(nonFatal) != 4 {
		t.Fatalf("nonFatal = %v, want 4 v1-surface deprecations", nonFatal)
	}
}

func TestSplitStrictConfigWarnings_LegacyWorkspaceFieldWarningsAreNonFatal(t *testing.T) {
	fatal, nonFatal := splitStrictConfigWarnings([]string{
		"city.toml: workspace.start_command is deprecated: Use per-agent `start_command` in `agent.toml` instead.",
		"city.toml: workspace.suspended is deprecated: This will move to `.gc/site.toml` in a future release. No action is required now.",
		"city.toml: workspace.install_agent_hooks is deprecated: Set install_agent_hooks per agent in agents/<name>/agent.toml.",
		"city.toml: workspace.global_fragments is deprecated: Use `[agent_defaults] append_fragments` or explicit `{{ template }}` instead.",
		`city agent "mayor" shadows agent of the same name from import "gs"`,
	})

	if len(fatal) != 1 || fatal[0] != `city agent "mayor" shadows agent of the same name from import "gs"` {
		t.Fatalf("fatal = %v, want only the shadow warning", fatal)
	}
	if len(nonFatal) != 4 {
		t.Fatalf("nonFatal = %v, want 4 workspace field deprecations", nonFatal)
	}
}

func TestSplitStrictConfigWarnings_IdleSleepMaskingWarningIsNonFatal(t *testing.T) {
	fatal, nonFatal := splitStrictConfigWarnings([]string{
		`city.toml: agent "repo/refinery": idle_timeout and sleep_after_idle are both set; idle_timeout takes precedence and sleep_after_idle only applies when the session survives the idle_timeout check`,
		`city agent "mayor" shadows agent of the same name from import "gs"`,
	})

	if len(fatal) != 1 || fatal[0] != `city agent "mayor" shadows agent of the same name from import "gs"` {
		t.Fatalf("fatal = %v, want only the shadow warning", fatal)
	}
	if len(nonFatal) != 1 {
		t.Fatalf("nonFatal = %v, want idle sleep masking warning", nonFatal)
	}
}

func TestSplitStrictConfigWarnings_MissingSiteBindingRemainsFatal(t *testing.T) {
	fatal, nonFatal := splitStrictConfigWarnings([]string{
		`rig "repo" is declared in city.toml but has no path binding in .gc/site.toml; run ` + "`gc rig add <dir> --name repo`" + ` to bind it`,
	})

	if len(nonFatal) != 0 {
		t.Fatalf("nonFatal = %v, want none", nonFatal)
	}
	if len(fatal) != 1 {
		t.Fatalf("fatal = %v, want missing-binding warning to stay fatal", fatal)
	}
}
