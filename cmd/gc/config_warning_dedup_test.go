package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// alwaysFreshWakeModeMarkerText mirrors internal/config's
// alwaysFreshWakeModeMarker so the fixtures below stay honest about the shape
// the config layer actually emits without importing the unexported constant.
const alwaysFreshWakeModeMarkerText = "starts a fresh provider session after every drain"

func alwaysFreshAdvisory(session string) string {
	return `named_session "` + session + `": mode "always" with wake_mode "fresh" on template "` +
		session + `" ` + alwaysFreshWakeModeMarkerText + `; use only for a deliberate restart-per-cycle actor`
}

const aliasCompatibilityWarning = `/city/pack.toml: [agents] is a deprecated compatibility alias for [agent_defaults]; rewrite the table name to [agent_defaults]`

func TestEmitLoadCityConfigWarningsCollapsesAlwaysFreshFamily(t *testing.T) {
	var stderr bytes.Buffer
	emitLoadCityConfigWarnings(&stderr, &config.Provenance{
		Warnings: []string{
			alwaysFreshAdvisory("gastown.mayor"),
			alwaysFreshAdvisory("gastown.deacon"),
			alwaysFreshAdvisory("gastown.boot"),
			alwaysFreshAdvisory("gk/gastown.witness"),
			aliasCompatibilityWarning,
		},
	})

	output := stderr.String()
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (first advisory + continuation + other warning), got %d: %q", len(lines), lines)
	}
	if lines[0] != alwaysFreshAdvisory("gastown.mayor") {
		t.Fatalf("first always+fresh advisory should print verbatim, got %q", lines[0])
	}
	wantContinuation := `  (x4 named sessions share this advisory: gastown.deacon, gastown.boot, gk/gastown.witness)`
	if lines[1] != wantContinuation {
		t.Fatalf("continuation = %q, want %q", lines[1], wantContinuation)
	}
	if lines[2] != aliasCompatibilityWarning {
		t.Fatalf("unrelated warning should print unchanged, got %q", lines[2])
	}
}

func TestEmitLoadCityConfigWarningsSingleAlwaysFreshStaysVerbatim(t *testing.T) {
	var stderr bytes.Buffer
	emitLoadCityConfigWarnings(&stderr, &config.Provenance{
		Warnings: []string{alwaysFreshAdvisory("gastown.mayor")},
	})

	output := stderr.String()
	if output != alwaysFreshAdvisory("gastown.mayor")+"\n" {
		t.Fatalf("single advisory should print verbatim with no continuation, got %q", output)
	}
}

func TestEmitLoadCityConfigWarningsExactDuplicatesStillCollapse(t *testing.T) {
	var stderr bytes.Buffer
	emitLoadCityConfigWarnings(&stderr, &config.Provenance{
		Warnings: []string{aliasCompatibilityWarning, aliasCompatibilityWarning},
	})

	if n := strings.Count(stderr.String(), aliasCompatibilityWarning); n != 1 {
		t.Fatalf("exact-duplicate collapse changed: printed %d times, want 1", n)
	}
}

func TestEmitLoadCityConfigWarningsInterleavedFamilyStaysAdjacent(t *testing.T) {
	var stderr bytes.Buffer
	emitLoadCityConfigWarnings(&stderr, &config.Provenance{
		Warnings: []string{
			alwaysFreshAdvisory("gastown.mayor"),
			aliasCompatibilityWarning,
			alwaysFreshAdvisory("gastown.deacon"),
			`both [agent_defaults] and [agents] are present; [agent_defaults] wins`,
			alwaysFreshAdvisory("gastown.boot"),
		},
	})

	output := stderr.String()
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (advisory + continuation + 2 others), got %d: %q", len(lines), lines)
	}
	if lines[0] != alwaysFreshAdvisory("gastown.mayor") {
		t.Fatalf("first advisory should print verbatim, got %q", lines[0])
	}
	wantContinuation := `  (x3 named sessions share this advisory: gastown.deacon, gastown.boot)`
	if lines[1] != wantContinuation {
		t.Fatalf("continuation = %q, want %q", lines[1], wantContinuation)
	}
	if !strings.Contains(output, `both [agent_defaults] and [agents] are present`) {
		t.Fatalf("mixed-table warning lost: %q", output)
	}
	if strings.Contains(output, alwaysFreshAdvisory("gastown.deacon")) || strings.Contains(output, alwaysFreshAdvisory("gastown.boot")) {
		t.Fatalf("later family advisories should be suppressed: %q", output)
	}
}

func TestEmitLoadCityConfigWarningsUnnamedAdvisoryCounts(t *testing.T) {
	unparseable := `session "weird": mode "always" with wake_mode "fresh" ` +
		alwaysFreshWakeModeMarkerText + `; use only for a deliberate restart-per-cycle actor`
	var stderr bytes.Buffer
	emitLoadCityConfigWarnings(&stderr, &config.Provenance{
		Warnings: []string{
			alwaysFreshAdvisory("gastown.mayor"),
			unparseable,
		},
	})

	output := stderr.String()
	if !strings.Contains(output, alwaysFreshAdvisory("gastown.mayor")+"\n") {
		t.Fatalf("first advisory should print verbatim: %q", output)
	}
	wantContinuation := "  (x2 named sessions share this advisory: 1 unnamed)\n"
	if !strings.Contains(output, wantContinuation) {
		t.Fatalf("continuation = %q, want it to contain %q", output, wantContinuation)
	}
	if strings.Contains(output, unparseable) {
		t.Fatalf("unparseable advisory should be suppressed into the count: %q", output)
	}
}

func TestEmitLoadCityConfigWarningsFilterUnchangedByCollapse(t *testing.T) {
	var stderr bytes.Buffer
	emitLoadCityConfigWarnings(&stderr, &config.Provenance{
		Warnings: []string{
			`workspace.name redefined by "/city/defaults.toml"`,
			alwaysFreshAdvisory("gastown.mayor"),
			alwaysFreshAdvisory("gastown.deacon"),
		},
	})

	output := stderr.String()
	if strings.Contains(output, `workspace.name redefined`) {
		t.Fatalf("non-emittable warning should stay filtered: %q", output)
	}
	if strings.Count(output, "named_session") != 1 {
		t.Fatalf("family should collapse to one advisory line: %q", output)
	}
}

func TestAlwaysFreshAdvisorySessionNameExtraction(t *testing.T) {
	name, ok := alwaysFreshAdvisorySessionName(alwaysFreshAdvisory("gk/gastown.witness"))
	if !ok || name != "gk/gastown.witness" {
		t.Fatalf("alwaysFreshAdvisorySessionName = (%q, %v), want (gk/gastown.witness, true)", name, ok)
	}
	if _, ok := alwaysFreshAdvisorySessionName(aliasCompatibilityWarning); ok {
		t.Fatal("non-advisory warnings must not yield a session name")
	}
	if _, ok := alwaysFreshAdvisorySessionName(`named_session "unterminated`); ok {
		t.Fatal("unterminated advisory must not yield a session name")
	}
}
