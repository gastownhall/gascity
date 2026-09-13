package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestRetiredKeyWarningIsNonFatalAndEmitted proves the retirement contract holds
// on the two downstream re-classifiers of config warnings: strict mode keeps a
// retired-key warning NON-FATAL, and the agent warning-emit path SURFACES it.
// Without the config.IsRetiredKeyWarning wiring, a retired key (once S5-T7
// registers daemon.graph_workflows) would make `gc start` — strict by default —
// exit 1 on a city that still carries the key, or drop the warning silently.
func TestRetiredKeyWarningIsNonFatalAndEmitted(t *testing.T) {
	w := `city.toml: "daemon.graph_workflows" was retired in v1.4.0 and is ignored; use daemon.formula_v2`
	if !config.IsRetiredKeyWarning(w) {
		t.Fatalf("test warning not recognized as retired: %q", w)
	}

	fatal, nonFatal := splitStrictConfigWarnings([]string{w})
	if len(fatal) != 0 || len(nonFatal) != 1 {
		t.Errorf("strict split: fatal=%v nonFatal=%v, want the retired warning non-fatal", fatal, nonFatal)
	}
	if !shouldEmitLoadCityConfigWarning(w) {
		t.Error("a retired-key warning must be emitted to the operator, not swallowed")
	}
	if got := strictFatalLoadConfigWarnings([]string{w}); len(got) != 0 {
		t.Errorf("a retired-key warning must not be a strict-fatal load warning, got %v", got)
	}
}
