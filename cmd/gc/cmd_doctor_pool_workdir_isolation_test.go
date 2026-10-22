package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// AC-1: gc doctor runs the pool-workdir-isolation check with the other
// static agent-config checks.
func TestBuildDoctorChecksRegistersPoolWorkDirIsolation(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	}))

	preStart := doctorCheckIndex(names, "pre-start-scripts")
	if preStart < 0 {
		t.Fatalf("pre-start-scripts missing: %v", names)
	}
	if got := doctorCheckIndex(names, "pool-workdir-isolation"); got != preStart+1 {
		t.Fatalf("pool-workdir-isolation index = %d, want immediately after pre-start-scripts at %d; names=%v", got, preStart, names)
	}
}
