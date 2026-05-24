package main

import "testing"

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
		"city.toml: workspace.default_rig_includes is deprecated in v2; use root pack.toml [defaults.rig.imports.<binding>]. Run `gc doctor` to inspect; `gc doctor --fix` handles the safe mechanical rewrites available in this wave.",
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
		"city.toml: workspace.provider is deprecated: Set provider per agent in agents/<name>/agent.toml.",
		"city.toml: workspace.start_command is deprecated: Use per-agent `start_command` in `agent.toml` instead.",
		"city.toml: workspace.suspended is deprecated: This will move to `.gc/site.toml` in a future release. No action is required now.",
		"city.toml: workspace.install_agent_hooks is deprecated: Set install_agent_hooks per agent in agents/<name>/agent.toml.",
		"city.toml: workspace.global_fragments is deprecated: Use `[agent_defaults] append_fragments` or explicit `{{ template }}` instead.",
		`city agent "mayor" shadows agent of the same name from import "gs"`,
	})

	if len(fatal) != 1 || fatal[0] != `city agent "mayor" shadows agent of the same name from import "gs"` {
		t.Fatalf("fatal = %v, want only the shadow warning", fatal)
	}
	if len(nonFatal) != 5 {
		t.Fatalf("nonFatal = %v, want 5 workspace field deprecations", nonFatal)
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
