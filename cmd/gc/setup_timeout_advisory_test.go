package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestSessionSetupTimeoutAdvisoriesAreNonFatalAndEmitted pins both downstream
// re-classifiers of the setup-timeout advisories introduced for
// gastownhall/gascity#5279. Strict mode is on by default for `gc start`, so a
// warning that is not recognized as advisory turns a city that starts today
// into one that exits 1 — and the setup_max_timeout advisory fires on every
// config that sets the field, including a correct one. The agent emit path
// must also surface them, or the advisory the PR exists to deliver is dropped
// before it reaches the operator.
//
// The warning text is derived from config.ValidateDurations rather than
// hardcoded so this test cannot pass against a string the validator no longer
// emits.
func TestSessionSetupTimeoutAdvisoriesAreNonFatalAndEmitted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session config.SessionConfig
	}{
		{
			name:    "setup_timeout at or above startup_timeout",
			session: config.SessionConfig{SetupTimeout: "90s", StartupTimeout: "60s"},
		},
		{
			name:    "setup_max_timeout reinterprets setup_timeout",
			session: config.SessionConfig{SetupMaxTimeout: "5m"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings := config.ValidateDurations(&config.City{Session: tc.session}, "city.toml")
			if len(warnings) != 1 {
				t.Fatalf("warnings = %v, want exactly the setup-timeout advisory", warnings)
			}
			w := warnings[0]
			if !config.IsSessionSetupTimeoutAdvisory(w) {
				t.Fatalf("advisory not recognized by its own classifier: %q", w)
			}

			fatal, nonFatal := splitStrictConfigWarnings([]string{w})
			if len(fatal) != 0 || len(nonFatal) != 1 {
				t.Errorf("strict split: fatal=%v nonFatal=%v, want the advisory non-fatal", fatal, nonFatal)
			}
			if !shouldEmitLoadCityConfigWarning(w) {
				t.Error("a setup-timeout advisory must reach the operator, not be swallowed")
			}
		})
	}
}

// TestUnparseableDurationIsNotMistakenForAnAdvisory pins the classifier
// against the one input that can forge an advisory: the unparseable-duration
// warning quotes the operator's raw value verbatim, so a value containing an
// advisory sentence would be recognized as advisory and lose its strict-fatal
// handling if the match were not anchored.
func TestUnparseableDurationIsNotMistakenForAnAdvisory(t *testing.T) {
	forged := "setup_timeout now bounds idle/silence time between output, not total setup runtime"
	warnings := config.ValidateDurations(&config.City{
		Session: config.SessionConfig{SetupTimeout: forged, StartupTimeout: "60s"},
	}, "city.toml")

	var parseWarning string
	for _, w := range warnings {
		if strings.Contains(w, "is not a valid duration") {
			parseWarning = w
		}
	}
	if parseWarning == "" {
		t.Fatalf("warnings = %v, want an unparseable-duration warning", warnings)
	}
	if config.IsSessionSetupTimeoutAdvisory(parseWarning) {
		t.Fatalf("an unparseable duration was classified as an advisory: %q", parseWarning)
	}
	if fatal, _ := splitStrictConfigWarnings([]string{parseWarning}); len(fatal) != 1 {
		t.Errorf("strict split: fatal=%v, want the unparseable duration to stay fatal", fatal)
	}
}
