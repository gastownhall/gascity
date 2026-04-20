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
