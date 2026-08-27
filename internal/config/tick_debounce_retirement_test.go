package config

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestLoadWithIncludesWarnsAndIgnoresRetiredTickDebounce is WD.0's second
// negative: a city that still carries the retired [daemon].tick_debounce key
// must keep loading — with a surfaced warning, never a hard failure — and must
// tick at exactly the cadence it declares.
//
// The warning is the RETIRED-key rendering, not "unknown field": the debounce
// window is gone, and an operator who reads "unknown field" deletes the line
// believing nothing changed. The Note must therefore name the behavior.
func TestLoadWithIncludesWarnsAndIgnoresRetiredTickDebounce(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[daemon]
patrol_interval = "10s"
tick_debounce = "500ms"
`)

	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Daemon.PatrolIntervalDuration(); got != 10*time.Second {
		t.Fatalf("PatrolIntervalDuration() = %v, want 10s (cadence unchanged by the retired key)", got)
	}
	var got string
	for _, w := range prov.Warnings {
		if strings.Contains(w, "daemon.tick_debounce") {
			got = w
			break
		}
	}
	if got == "" {
		t.Fatalf("warnings = %v, want a retired tick-debounce warning", prov.Warnings)
	}
	if strings.Contains(got, "unknown field") || !IsRetiredKeyWarning(got) {
		t.Fatalf("warning = %q, want the retired-key rendering, not an unknown field", got)
	}
	if !strings.Contains(got, "debounce window is gone") {
		t.Fatalf("warning = %q, want it to say the BEHAVIOR is gone, not just the key", got)
	}
}
