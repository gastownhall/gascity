package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestBuildDoctorChecksRegistersPoolWorkDirIsolation is AC-7: `gc doctor`
// carries a pool-workdir-isolation line, so a city whose pool agents would
// share a working directory — or whose slot-isolated rig cannot mint a
// worktree — says so instead of failing at session start.
func TestBuildDoctorChecksRegistersPoolWorkDirIsolation(t *testing.T) {
	clearInheritedBeadsEnv(t)

	cityDir := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}
	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    true,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})

	names := doctorCheckNames(checks)
	if doctorCheckIndex(names, "pool-workdir-isolation") < 0 {
		t.Errorf("check %q not registered; names=%v", "pool-workdir-isolation", names)
	}
}
