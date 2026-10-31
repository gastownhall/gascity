package sling

// Effective-suspension coverage for the sling preflight (gastownhall/gascity
// #8508): the target's city and rig must be judged the way the reconciler,
// gc hook and gc session wake judge them — the runtime state file first,
// then the authored suspended_on_start defaults (aliasing the deprecated
// suspended fields). Every test uses its own city dir: the shared test city
// must never gain a suspension-state file.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// suspensionTestCityDir returns an isolated city dir with .gc/runtime in
// place, so runtime suspension-state writes never touch the shared test city.
func suspensionTestCityDir(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

// writeCitySuspension records an explicit city preference in the runtime
// state file — the write `gc suspend` / `gc resume` perform.
func writeCitySuspension(t *testing.T, cityPath string, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, cityPath, &suspended); err != nil {
		t.Fatal(err)
	}
}

func TestDoSlingWarnsCitySuspendedByRuntimeState(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.CityPath = suspensionTestCityDir(t)
	deps.Store = seededStore("my-1")
	writeCitySuspension(t, deps.CityPath, true)

	result, err := DoSling(testOpts(a, "my-1"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if !result.CitySuspended {
		t.Error("expected CitySuspended=true for a city suspended in runtime state")
	}
	if result.SuspendedRig != "" {
		t.Errorf("SuspendedRig = %q, want empty for a city-scoped agent", result.SuspendedRig)
	}
	if result.BeadID != "my-1" {
		t.Errorf("BeadID = %q, want my-1 (bead must still be routed)", result.BeadID)
	}
}

func TestDoSlingCitySuspendedWarnsEvenOnFailure(t *testing.T) {
	// Mirrors TestDoSlingSuspendedRigWarnsEvenOnFailure: the warning flag
	// must survive a routing failure so the CLI can still display it.
	runner := newFakeRunner()
	runner.on("bd update", fmt.Errorf("runner failed"))
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.CityPath = suspensionTestCityDir(t)
	deps.Store = seededStore("my-1")
	writeCitySuspension(t, deps.CityPath, true)

	result, err := DoSling(testOpts(a, "my-1"), deps, nil)
	if err == nil {
		t.Fatal("expected runner error")
	}
	if !result.CitySuspended {
		t.Error("expected CitySuspended=true even when routing fails")
	}
}
