package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestBuildDoctorChecks_SupervisorHTTPRegisteredAfterController verifies that
// the supervisor-http-api check is registered immediately after the controller
// check. The HTTP probe is only meaningful when the socket check passes, so
// the two checks must be adjacent and in that order.
func TestBuildDoctorChecks_SupervisorHTTPRegisteredAfterController(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SupervisorRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})

	controllerIdx, supervisorHTTPIdx := -1, -1
	for i, c := range checks {
		switch c.Name() {
		case "controller":
			controllerIdx = i
		case "supervisor-http-api":
			supervisorHTTPIdx = i
		}
	}

	if controllerIdx < 0 {
		t.Fatal("controller check not registered")
	}
	if supervisorHTTPIdx < 0 {
		t.Fatal("supervisor-http-api check not registered")
	}
	if supervisorHTTPIdx != controllerIdx+1 {
		t.Errorf("supervisor-http-api at index %d, want %d (immediately after controller at %d)",
			supervisorHTTPIdx, controllerIdx+1, controllerIdx)
	}
}
