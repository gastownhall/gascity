package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestBuildDoctorChecksRegistersRigSSHKeepalive(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "gascity", Path: cityDir, Prefix: "ga"}},
	}

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
		SkipRigDoltChecks:    true,
	}))
	if doctorCheckIndex(names, "rig:gascity:ssh-keepalive") < 0 {
		t.Fatalf("rig:gascity:ssh-keepalive missing; names=%v", names)
	}
}
