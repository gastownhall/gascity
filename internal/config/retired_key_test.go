package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestClassifyUndecodedRetiredKey drives the classifier directly with a synthetic
// retired registration (no package-global mutation, so it is -race safe) across
// the three dispositions: retired warns-not-fatal, unknown stays fatal,
// specialized warns-not-fatal.
func TestClassifyUndecodedRetiredKey(t *testing.T) {
	t.Parallel()
	known := knownTOMLKeys()
	retired := map[string]retiredKey{
		"daemon.graph_workflows": {RemovedIn: "v9.9.9", Note: "use daemon.formula_v2"},
	}

	w, fatal := classifyUndecoded("city.toml", "daemon.graph_workflows", known, retired)
	if fatal {
		t.Error("a retired key must not be fatal")
	}
	if !strings.Contains(w, "retired in v9.9.9") || !strings.Contains(w, "use daemon.formula_v2") {
		t.Errorf("retired warning must carry RemovedIn and Note, got %q", w)
	}
	if strings.Contains(w, "unknown field") {
		t.Errorf("a retired key must not read as an unknown field, got %q", w)
	}

	w, fatal = classifyUndecoded("pack.toml", "daemon.totally_unknown", known, retired)
	if !fatal {
		t.Error("an unknown (non-retired) key must remain fatal")
	}
	if !strings.Contains(w, "unknown field") {
		t.Errorf("an unknown key should be an unknown-field warning, got %q", w)
	}

	if _, fatal := classifyUndecoded("city.toml", "agent_defaults.scope", known, retired); fatal {
		t.Error("a specialized release-wave key must not be fatal")
	}
}

// TestUndecodedPathsWithEmptyRetiredRegistry proves the SHIPPED registry (empty
// until S5-T7) leaves unknown-key fatality intact end-to-end through both paths.
func TestUndecodedPathsWithEmptyRetiredRegistry(t *testing.T) {
	t.Parallel()
	if len(retiredKeys) != 0 {
		t.Fatalf("retiredKeys ships empty (S5-T7 adds the first entry); got %v", retiredKeys)
	}
	var cfg City
	md, err := toml.Decode("[daemon]\ntotally_unknown_key = true\n", &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if fatal := fatalUndecodedWarnings(md, "pack.toml"); len(fatal) == 0 {
		t.Error("an unknown key must remain fatal through fatalUndecodedWarnings")
	}
	if joined := strings.Join(CheckUndecodedKeys(md, "city.toml"), "; "); !strings.Contains(joined, "totally_unknown_key") {
		t.Errorf("CheckUndecodedKeys should warn about the unknown key, got %q", joined)
	}
}

// TestIsRetiredKeyWarning pins the predicate downstream re-classifiers use to
// keep a retired key non-fatal: it recognizes the retiredKeyWarning rendering
// (with and without a Note) and rejects other warnings.
func TestIsRetiredKeyWarning(t *testing.T) {
	t.Parallel()
	withNote := retiredKeyWarning("city.toml", "daemon.graph_workflows", retiredKey{RemovedIn: "v1.4.0", Note: "use daemon.formula_v2"})
	if !IsRetiredKeyWarning(withNote) {
		t.Errorf("retiredKeyWarning output not recognized: %q", withNote)
	}
	if noNote := retiredKeyWarning("pack.toml", "x.y", retiredKey{RemovedIn: "v2"}); !IsRetiredKeyWarning(noNote) {
		t.Errorf("note-less retired warning not recognized: %q", noNote)
	}
	for _, other := range []string{
		`city.toml: unknown field "daemon.foo"`,
		`city.toml: "agents" is a deprecated compatibility alias for [agent_defaults]`,
		"an old feature was retired in a museum", // has "was retired in" but not the quoted-key + "and is ignored" shape
	} {
		if IsRetiredKeyWarning(other) {
			t.Errorf("false positive on %q", other)
		}
	}
}
