package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
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

// The config load checks only the order layout, so a malformed order file no
// longer fails it. gc doctor's order checks scan the order roots themselves:
// order-outcome-healthy runs without the city store, so a malformed order is
// reported even when the store gate skips order-firing-current.
func TestDoctorReportsMalformedOrderThatConfigLoadAccepts(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_DOLT", "skip")
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, "[pack]\nname = \"demo\"\nschema = 1\n")
	if err := os.MkdirAll(filepath.Join(cityDir, "orders"), 0o755); err != nil {
		t.Fatalf("MkdirAll(orders): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "orders", "broken.toml"), []byte("[order\nformula = "), 0o644); err != nil {
		t.Fatalf("WriteFile(orders/broken.toml): %v", err)
	}

	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v, want a malformed order left to the order consumers", err)
	}

	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})
	var outcome doctor.Check
	for _, c := range checks {
		if c.Name() == "order-outcome-healthy" {
			outcome = c
		}
	}
	if outcome == nil {
		t.Fatalf("order-outcome-healthy not registered: %v", doctorCheckNames(checks))
	}
	result := outcome.Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusError || !strings.Contains(result.Message, "scan orders") || !strings.Contains(result.Message, "broken") {
		t.Fatalf("order-outcome-healthy = %v %q, want a scan error naming the malformed order", result.Status, result.Message)
	}
}
