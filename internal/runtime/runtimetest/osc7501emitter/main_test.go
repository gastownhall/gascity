package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/runtimetest/osc7501emitter/emit"
)

// Kills a loader that accepts both a script file and inline steps, neither,
// or two files, or that does not parse what it loads.
func TestLoadTakesAScriptFileOrInlineSteps(t *testing.T) {
	script := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(script, []byte("report state=idle\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load([]string{script}, nil); err != nil {
		t.Fatalf("script file: %v", err)
	}
	if _, err := load(nil, steps{"report state=idle", "hang"}); err != nil {
		t.Fatalf("inline steps: %v", err)
	}
	for name, c := range map[string]struct {
		args   []string
		inline steps
	}{
		"neither":       {},
		"both":          {[]string{script}, steps{"hang"}},
		"two files":     {[]string{script, script}, nil},
		"missing file":  {[]string{filepath.Join(t.TempDir(), "absent")}, nil},
		"bad inline":    {nil, steps{"bogus"}},
		"bad file line": {[]string{writeScript(t, "term crlf\n")}, nil},
	} {
		if _, err := load(c.args, c.inline); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	if _, err := load(nil, steps{"bogus"}); !errors.Is(err, emit.ErrScript) {
		t.Errorf("bad inline step: %v, want emit.ErrScript", err)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
