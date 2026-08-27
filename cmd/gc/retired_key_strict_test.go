package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestRetiredKeyWarningIsNonFatalAndEmitted proves the retirement contract holds
// on the two downstream re-classifiers of config warnings: strict mode keeps a
// retired-key warning NON-FATAL, and the agent warning-emit path SURFACES it.
// Without the config.IsRetiredKeyWarning wiring, a city still carrying a key the
// registry has retired would make `gc start` — strict by default — exit 1, or
// drop the warning silently. Both are wrong: the point of retiring a key
// warn-and-ignore is that the city keeps running and the operator still hears
// about the behavior that went with it.
//
// The warning below is synthetic on purpose. The predicate is anchored to the
// RENDERING, not to any particular key, and spelling the shipped entry here would
// trip the retirement source-reference guard in cmd/gc, which keeps retired
// spellings out of Go sources.
func TestRetiredKeyWarningIsNonFatalAndEmitted(t *testing.T) {
	w := `city.toml: "daemon.graph_workflows" was retired in v9.9.9 and is ignored; use daemon.formula_v2`
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
