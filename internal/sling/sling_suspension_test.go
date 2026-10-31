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

// writeRigSuspension records an explicit rig preference in the runtime state
// file — the write `gc rig suspend` / `gc rig resume` perform.
func writeRigSuspension(t *testing.T, cityPath, rig string, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, rig, &suspended); err != nil {
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

func TestDoSlingWarnsCitySuspendedOnStart(t *testing.T) {
	cases := []struct {
		name      string
		workspace config.Workspace
	}{
		{name: "suspended_on_start", workspace: config.Workspace{Name: "test-city", SuspendedOnStart: true}},
		{name: "deprecated_suspended", workspace: config.Workspace{Name: "test-city", Suspended: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeRunner()
			cfg := &config.City{Workspace: tc.workspace}
			a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}

			deps := testDeps(cfg, runtime.NewFake(), runner.run)
			// No runtime city entry: the authored default must apply on its own.
			deps.CityPath = suspensionTestCityDir(t)
			deps.Store = seededStore("my-1")

			result, err := DoSling(testOpts(a, "my-1"), deps, nil)
			if err != nil {
				t.Fatalf("DoSling error: %v", err)
			}
			if !result.CitySuspended {
				t.Error("expected CitySuspended=true from the authored workspace default")
			}
		})
	}
}

func TestDoSlingCorruptSuspensionStateStillHonorsAuthoredCitySuspend(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city", SuspendedOnStart: true}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.CityPath = suspensionTestCityDir(t)
	if err := os.WriteFile(filepath.Join(deps.CityPath, ".gc", "runtime", "suspension-state.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.Store = seededStore("my-1")

	result, err := DoSling(testOpts(a, "my-1"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v (a corrupt state file must not fail the route)", err)
	}
	if !result.CitySuspended {
		t.Error("expected CitySuspended=true: a corrupt state file degrades to the zero state so the authored default still applies")
	}
}

func TestDoSlingWarnsRigSuspendedByRuntimeState(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig"}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.CityPath = suspensionTestCityDir(t)
	deps.Store = seededStore("my-1")
	writeRigSuspension(t, deps.CityPath, "myrig", true)

	result, err := DoSling(testOpts(a, "my-1"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.SuspendedRig != "myrig" {
		t.Errorf("SuspendedRig = %q, want myrig for a rig suspended in runtime state", result.SuspendedRig)
	}
	if result.CitySuspended {
		t.Error("expected CitySuspended=false: only the rig is suspended")
	}
	if result.BeadID != "my-1" {
		t.Errorf("BeadID = %q, want my-1 (bead must still be routed)", result.BeadID)
	}
}

func TestDoSlingWarnsRigSuspendedOnStart(t *testing.T) {
	cases := []struct {
		name string
		rig  config.Rig
	}{
		{name: "suspended_on_start", rig: config.Rig{Name: "myrig", SuspendedOnStart: true}},
		{name: "deprecated_suspended", rig: config.Rig{Name: "myrig", Suspended: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeRunner()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Rigs:      []config.Rig{tc.rig},
			}
			a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

			deps := testDeps(cfg, runtime.NewFake(), runner.run)
			// No runtime rig entry: the authored default must apply on its own.
			deps.CityPath = suspensionTestCityDir(t)
			deps.Store = seededStore("my-1")

			result, err := DoSling(testOpts(a, "my-1"), deps, nil)
			if err != nil {
				t.Fatalf("DoSling error: %v", err)
			}
			if result.SuspendedRig != "myrig" {
				t.Errorf("SuspendedRig = %q, want myrig from the authored rig default", result.SuspendedRig)
			}
		})
	}
}

func TestDoSlingRuntimeResumeOverridesAuthoredSuspend(t *testing.T) {
	cases := []struct {
		name      string
		workspace config.Workspace
		rig       config.Rig
	}{
		{
			name:      "suspended_on_start",
			workspace: config.Workspace{Name: "test-city", SuspendedOnStart: true},
			rig:       config.Rig{Name: "myrig", SuspendedOnStart: true},
		},
		{
			name:      "deprecated_suspended",
			workspace: config.Workspace{Name: "test-city", Suspended: true},
			rig:       config.Rig{Name: "myrig", Suspended: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeRunner()
			cfg := &config.City{Workspace: tc.workspace, Rigs: []config.Rig{tc.rig}}
			a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

			deps := testDeps(cfg, runtime.NewFake(), runner.run)
			deps.CityPath = suspensionTestCityDir(t)
			deps.Store = seededStore("my-1")
			// Explicit resumes recorded by `gc resume` / `gc rig resume myrig`:
			// the runtime override must win over the authored default.
			writeCitySuspension(t, deps.CityPath, false)
			writeRigSuspension(t, deps.CityPath, "myrig", false)

			result, err := DoSling(testOpts(a, "my-1"), deps, nil)
			if err != nil {
				t.Fatalf("DoSling error: %v", err)
			}
			if result.CitySuspended {
				t.Error("expected CitySuspended=false: explicit runtime resume wins over the authored default")
			}
			if result.SuspendedRig != "" {
				t.Errorf("SuspendedRig = %q, want empty: explicit runtime resume wins over the authored default", result.SuspendedRig)
			}
		})
	}
}

func TestDoSlingWarnsPathBoundAgentInSuspendedRig(t *testing.T) {
	cityPath := suspensionTestCityDir(t)
	rigPath := filepath.Join(cityPath, "repos", "myrig")
	writeRigSuspension(t, cityPath, "myrig", true)

	cases := []struct {
		name string
		dir  string
	}{
		{name: "absolute_dir", dir: rigPath},
		{name: "relative_dir", dir: filepath.Join("repos", "myrig")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeRunner()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Rigs:      []config.Rig{{Name: "myrig", Path: rigPath}},
			}
			a := config.Agent{Name: "polecat", Dir: tc.dir, MaxActiveSessions: intPtr(1)}

			deps := testDeps(cfg, runtime.NewFake(), runner.run)
			deps.CityPath = cityPath
			deps.Store = seededStore("my-1")

			result, err := DoSling(testOpts(a, "my-1"), deps, nil)
			if err != nil {
				t.Fatalf("DoSling error: %v", err)
			}
			// The same path-aware resolution gc session wake / gc hook use:
			// the rig is matched by configured path, not by bare name.
			if result.SuspendedRig != "myrig" {
				t.Errorf("SuspendedRig = %q, want myrig for a path-bound agent in a suspended rig", result.SuspendedRig)
			}
		})
	}
}

func TestDoSlingForceSuppressesEffectiveSuspensionWarnings(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig"}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.CityPath = suspensionTestCityDir(t)
	deps.Store = seededStore("my-1")
	writeCitySuspension(t, deps.CityPath, true)
	writeRigSuspension(t, deps.CityPath, "myrig", true)

	opts := testOpts(a, "my-1")
	opts.Force = true
	result, err := DoSling(opts, deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.CitySuspended {
		t.Error("expected CitySuspended=false with --force")
	}
	if result.SuspendedRig != "" {
		t.Errorf("SuspendedRig = %q, want empty with --force", result.SuspendedRig)
	}
	if result.AgentSuspended {
		t.Error("expected AgentSuspended=false with --force")
	}
}

func TestDoSlingCityAndRigBothSuspendedSetBothFields(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig"}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.CityPath = suspensionTestCityDir(t)
	deps.Store = seededStore("my-1")
	writeCitySuspension(t, deps.CityPath, true)
	writeRigSuspension(t, deps.CityPath, "myrig", true)

	result, err := DoSling(testOpts(a, "my-1"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if !result.CitySuspended {
		t.Error("expected CitySuspended=true: a suspended city does not hide a suspended rig")
	}
	if result.SuspendedRig != "myrig" {
		t.Errorf("SuspendedRig = %q, want myrig: the scopes stay independent", result.SuspendedRig)
	}
}

func TestDoSlingOtherRigSuspendedLeavesCityTargetClean(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "otherrig"}},
	}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.CityPath = suspensionTestCityDir(t)
	deps.Store = seededStore("my-1")
	writeRigSuspension(t, deps.CityPath, "otherrig", true)

	result, err := DoSling(testOpts(a, "my-1"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.CitySuspended {
		t.Error("expected CitySuspended=false: only an unrelated rig is suspended")
	}
	if result.SuspendedRig != "" {
		t.Errorf("SuspendedRig = %q, want empty: the target belongs to no suspended rig", result.SuspendedRig)
	}
}

func TestDoSlingEmptyCityPathStillWarnsAuthoredRigSuspension(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig", SuspendedOnStart: true}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.CityPath = ""
	deps.Store = seededStore("my-1")

	result, err := DoSling(testOpts(a, "my-1"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.SuspendedRig != "myrig" {
		t.Errorf("SuspendedRig = %q, want myrig: the authored name-bound default applies without a city path", result.SuspendedRig)
	}
}
