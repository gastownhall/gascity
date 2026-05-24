package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestBuildDoctorChecks_NameSetUnchanged(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})
	var names []string
	for _, check := range checks {
		names = append(names, check.Name())
	}

	data, err := os.ReadFile(filepath.Join("testdata", "doctor_check_names.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	want := strings.TrimSpace(string(data))
	got := strings.Join(names, "\n")
	if got != want {
		t.Fatalf("doctor check names changed\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
