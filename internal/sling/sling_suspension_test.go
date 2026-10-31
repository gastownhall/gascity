package sling

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// suspensionTestDeps returns sling deps rooted at a private city directory so
// runtime suspension state written by a test never leaks into other tests.
// suspensionTestBead is the bead every suspension test slings.
const suspensionTestBead = "my-1"

func suspensionTestDeps(t *testing.T, cfg *config.City, runner *fakeRunner) SlingDeps {
	t.Helper()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.CityPath = t.TempDir()
	deps.Store = seededStore(suspensionTestBead)
	return deps
}

func setCitySuspendedState(t *testing.T, cityPath string, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, cityPath, &suspended); err != nil {
		t.Fatalf("SetCitySuspended(%v): %v", suspended, err)
	}
}

func assertSlingRouted(t *testing.T, result SlingResult, runner *fakeRunner) {
	t.Helper()
	if result.BeadID != suspensionTestBead {
		t.Errorf("BeadID = %q, want %q (bead must still be routed)", result.BeadID, suspensionTestBead)
	}
	if len(runner.calls) != 1 {
		t.Errorf("got %d runner calls, want 1 routing call", len(runner.calls))
	}
}

func TestDoSlingWarnsCitySuspendedByRuntimeState(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	deps := suspensionTestDeps(t, cfg, runner)
	setCitySuspendedState(t, deps.CityPath, true)

	result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if !result.CitySuspended {
		t.Error("CitySuspended = false, want true for a city suspended in runtime state")
	}
	if result.SuspendedRig != "" || result.AgentSuspended {
		t.Errorf("SuspendedRig = %q, AgentSuspended = %v; want only the city flagged", result.SuspendedRig, result.AgentSuspended)
	}
	assertSlingRouted(t, result, runner)
}

func TestDoSlingCitySuspendedWarnsEvenOnFailure(t *testing.T) {
	runner := newFakeRunner()
	runner.on("bd update", fmt.Errorf("runner failed"))
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	deps := suspensionTestDeps(t, cfg, runner)
	setCitySuspendedState(t, deps.CityPath, true)

	result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
	if err == nil {
		t.Fatal("expected runner error")
	}
	if !result.CitySuspended {
		t.Error("CitySuspended = false, want true even when routing fails")
	}
}

func TestDoSlingLiveCityNoCitySuspendedWarning(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	deps := suspensionTestDeps(t, cfg, runner)

	result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.CitySuspended {
		t.Error("CitySuspended = true, want false for a live city")
	}
}

func TestDoSlingWarnsCitySuspendedOnStart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace config.Workspace
	}{
		{name: "SuspendedOnStart", workspace: config.Workspace{Name: "test-city", SuspendedOnStart: true}},
		{name: "deprecated Suspended", workspace: config.Workspace{Name: "test-city", Suspended: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeRunner()
			cfg := &config.City{Workspace: tc.workspace}
			a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
			deps := suspensionTestDeps(t, cfg, runner)

			result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
			if err != nil {
				t.Fatalf("DoSling error: %v", err)
			}
			if !result.CitySuspended {
				t.Error("CitySuspended = false, want true for an authored city suspension with no runtime entry")
			}
			assertSlingRouted(t, result, runner)
		})
	}
}

func TestDoSlingCitySuspendedOnStartWithoutCityPath(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city", SuspendedOnStart: true}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	deps := suspensionTestDeps(t, cfg, runner)
	deps.CityPath = ""

	result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if !result.CitySuspended {
		t.Error("CitySuspended = false, want true from the authored default when CityPath is empty")
	}
}

func TestDoSlingCorruptSuspensionStateKeepsAuthoredCitySuspension(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city", SuspendedOnStart: true}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	deps := suspensionTestDeps(t, cfg, runner)
	writeCorruptSuspensionState(t, deps.CityPath)

	result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v (a corrupt suspension state must not fail the route)", err)
	}
	if !result.CitySuspended {
		t.Error("CitySuspended = false, want true from the authored default when the state file is corrupt")
	}
	assertSlingRouted(t, result, runner)
}

func writeCorruptSuspensionState(t *testing.T, cityPath string) {
	t.Helper()
	p := citylayout.SuspensionStateFile(cityPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setRigSuspendedState(t *testing.T, cityPath string, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, "myrig", &suspended); err != nil {
		t.Fatalf("SetRigSuspended(myrig, %v): %v", suspended, err)
	}
}

func TestDoSlingWarnsRigSuspendedByRuntimeState(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig"}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}
	deps := suspensionTestDeps(t, cfg, runner)
	setRigSuspendedState(t, deps.CityPath, true)

	result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.SuspendedRig != "myrig" {
		t.Errorf("SuspendedRig = %q, want %q for a rig suspended in runtime state", result.SuspendedRig, "myrig")
	}
	if result.CitySuspended {
		t.Error("CitySuspended = true, want false: only the rig is suspended")
	}
	assertSlingRouted(t, result, runner)
}

func TestDoSlingWarnsCityAndRigBothSuspended(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig"}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}
	deps := suspensionTestDeps(t, cfg, runner)
	setCitySuspendedState(t, deps.CityPath, true)
	setRigSuspendedState(t, deps.CityPath, true)

	result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if !result.CitySuspended || result.SuspendedRig != "myrig" {
		t.Errorf("CitySuspended = %v, SuspendedRig = %q; want both the city and %q reported", result.CitySuspended, result.SuspendedRig, "myrig")
	}
}

func TestDoSlingCityTargetIgnoresOtherSuspendedRig(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig", SuspendedOnStart: true}},
	}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	deps := suspensionTestDeps(t, cfg, runner)
	setRigSuspendedState(t, deps.CityPath, true)

	result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.SuspendedRig != "" {
		t.Errorf("SuspendedRig = %q, want empty for a city-scoped target", result.SuspendedRig)
	}
}

func TestDoSlingWarnsRigSuspendedOnStart(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig", SuspendedOnStart: true}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}
	deps := suspensionTestDeps(t, cfg, runner)

	result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.SuspendedRig != "myrig" {
		t.Errorf("SuspendedRig = %q, want %q for a rig with suspended_on_start and no runtime entry", result.SuspendedRig, "myrig")
	}
	assertSlingRouted(t, result, runner)
}

func TestTargetScopeSuspensionSkipsStateReadWithoutCityPath(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig", SuspendedOnStart: true}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig"}
	fs := fsys.NewFake()

	city, rig := targetScopeSuspension(fs, SlingDeps{Cfg: cfg}, a)
	if city || rig != "myrig" {
		t.Errorf("targetScopeSuspension = (%v, %q), want (false, %q) from authored defaults", city, rig, "myrig")
	}
	if len(fs.Calls) != 0 {
		t.Errorf("filesystem calls = %v, want none when CityPath is empty", fs.Calls)
	}
}

func TestDoSlingRuntimeResumeOverridesAuthoredSuspend(t *testing.T) {
	for _, tc := range []struct {
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
			name:      "deprecated suspended",
			workspace: config.Workspace{Name: "test-city", Suspended: true},
			rig:       config.Rig{Name: "myrig", Suspended: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeRunner()
			cfg := &config.City{Workspace: tc.workspace, Rigs: []config.Rig{tc.rig}}
			a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}
			deps := suspensionTestDeps(t, cfg, runner)
			setCitySuspendedState(t, deps.CityPath, false)
			setRigSuspendedState(t, deps.CityPath, false)

			result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
			if err != nil {
				t.Fatalf("DoSling error: %v", err)
			}
			if result.CitySuspended || result.SuspendedRig != "" {
				t.Errorf("CitySuspended = %v, SuspendedRig = %q; want neither after an explicit runtime resume", result.CitySuspended, result.SuspendedRig)
			}
			assertSlingRouted(t, result, runner)
		})
	}
}

func TestDoSlingWarnsPathBoundAgentInSuspendedRig(t *testing.T) {
	for _, dirForm := range []string{"absolute", "relative"} {
		for _, suspension := range []string{"runtime", "authored"} {
			t.Run(dirForm+"/"+suspension, func(t *testing.T) {
				runner := newFakeRunner()
				cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
				deps := suspensionTestDeps(t, cfg, runner)
				rig := config.Rig{Name: "myrig", Path: filepath.Join(deps.CityPath, "repos", "myrig")}
				if suspension == "authored" {
					rig.SuspendedOnStart = true
				} else {
					setRigSuspendedState(t, deps.CityPath, true)
				}
				cfg.Rigs = []config.Rig{rig}
				dir := rig.Path
				if dirForm == "relative" {
					dir = filepath.Join("repos", "myrig")
				}
				a := config.Agent{Name: "polecat", Dir: dir, MaxActiveSessions: intPtr(1)}

				result, err := DoSling(testOpts(a, suspensionTestBead), deps, nil)
				if err != nil {
					t.Fatalf("DoSling error: %v", err)
				}
				if result.SuspendedRig != "myrig" {
					t.Errorf("SuspendedRig = %q, want %q for an agent bound to the rig by path %q", result.SuspendedRig, "myrig", dir)
				}
				assertSlingRouted(t, result, runner)
			})
		}
	}
}

func TestDoSlingForceSuppressesEffectiveSuspensionWarnings(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", SuspendedOnStart: true},
		Rigs:      []config.Rig{{Name: "myrig", SuspendedOnStart: true}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1), Suspended: true}
	deps := suspensionTestDeps(t, cfg, runner)
	setCitySuspendedState(t, deps.CityPath, true)
	setRigSuspendedState(t, deps.CityPath, true)
	opts := testOpts(a, suspensionTestBead)
	opts.Force = true

	result, err := DoSling(opts, deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.CitySuspended || result.AgentSuspended || result.SuspendedRig != "" {
		t.Errorf("CitySuspended = %v, AgentSuspended = %v, SuspendedRig = %q; want all zero with --force",
			result.CitySuspended, result.AgentSuspended, result.SuspendedRig)
	}
	assertSlingRouted(t, result, runner)
}
