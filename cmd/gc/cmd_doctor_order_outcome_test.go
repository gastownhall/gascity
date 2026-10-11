package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestBuildDoctorChecksRegistersOrderOutcomeHealthy(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	}))

	firing := doctorCheckIndex(names, "order-firing-current")
	if firing < 0 {
		t.Fatalf("order-firing-current missing: %v", names)
	}
	outcome := doctorCheckIndex(names, "order-outcome-healthy")
	if outcome != firing+1 {
		t.Fatalf("order-outcome-healthy index = %d, want immediately after order-firing-current at %d; names=%v", outcome, firing, names)
	}
}
