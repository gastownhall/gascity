package sling

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// suspensionTestBead is the bead every suspension test slings.
const suspensionTestBead = "my-1"

// suspensionTestDeps returns sling deps rooted at a private city directory so
// runtime suspension state written by a test never leaks into other tests.
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

// suspendedScopesTarget is a target every target warning applies to: it is
// suspended, its pool is empty, and it belongs to rig myrig, which
// suspendedScopesDeps suspends together with the city.
func suspendedScopesTarget() config.Agent {
	return config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(0), Suspended: true}
}

// suspendedScopesDeps returns deps for a city whose runtime state suspends the
// city and rig myrig, with a v1 formula (v1-work) and a graph.v2 formula
// (graph-work) to launch. The store mints ids under the rig's prefix so the
// cross-rig guard lets myrig's agents route them.
func suspendedScopesDeps(t *testing.T) SlingDeps {
	t.Helper()
	formulaDir := t.TempDir()
	writeSuspensionTestFormula(t, formulaDir, "v1-work", `
formula = "v1-work"
version = 1
phase = "vapor"

[[steps]]
id = "work"
title = "Work"
`)
	writeSuspensionTestFormula(t, formulaDir, "graph-work", `
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`)
	cfg := graphV2SlingTestConfig(t, formulaDir)
	cfg.Rigs = []config.Rig{{Name: "myrig"}}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	store := beads.NewMemStore()
	store.IDPrefix = "my"
	deps.Store = store
	deps.CityPath = t.TempDir()
	setCitySuspendedState(t, deps.CityPath, true)
	setRigSuspendedState(t, deps.CityPath, true)
	return deps
}

func writeSuspensionTestFormula(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func createSuspensionTestTask(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	task, err := store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// createSuspensionTestConvoy creates a convoy tracking one open task.
func createSuspensionTestConvoy(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	if err := convoycore.TrackItem(store, convoy.ID, createSuspensionTestTask(t, store).ID); err != nil {
		t.Fatalf("track task: %v", err)
	}
	return convoy
}

// TestDispatchCarriesTargetWarningsOnEveryLaunchPath pins the target warnings
// on every result Dispatch returns, not only on the plain-bead route: a
// --formula launch, a graph.v2 --on or default-formula attach and a convoy
// each build a result of their own, and work launched into a suspended city or
// rig stalls as silently as a routed bead.
func TestDispatchCarriesTargetWarningsOnEveryLaunchPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		// opts builds the sling to target, creating what it slings in store.
		opts func(t *testing.T, store beads.Store, target config.Agent) SlingOpts
		// launched reports whether result comes from the path the case names.
		launched func(result SlingResult) bool
	}{
		{
			name: "v1 formula",
			opts: func(_ *testing.T, _ beads.Store, target config.Agent) SlingOpts {
				return SlingOpts{Target: target, BeadOrFormula: "v1-work", IsFormula: true}
			},
			launched: func(r SlingResult) bool { return r.Method == "formula" && r.BeadID != "" && r.WorkflowID == "" },
		},
		{
			name: "graph.v2 formula",
			opts: func(_ *testing.T, _ beads.Store, target config.Agent) SlingOpts {
				return SlingOpts{Target: target, BeadOrFormula: "graph-work", IsFormula: true}
			},
			launched: func(r SlingResult) bool { return r.Method == "formula" && r.WorkflowID != "" },
		},
		{
			name: "graph.v2 --on attach",
			opts: func(t *testing.T, store beads.Store, target config.Agent) SlingOpts {
				return SlingOpts{Target: target, BeadOrFormula: createSuspensionTestTask(t, store).ID, OnFormula: "graph-work"}
			},
			launched: func(r SlingResult) bool { return r.Method == "on-formula" && r.WorkflowID != "" },
		},
		{
			name: "graph.v2 default formula attach",
			opts: func(t *testing.T, store beads.Store, target config.Agent) SlingOpts {
				target.DefaultSlingFormula = stringPtr("graph-work")
				return SlingOpts{Target: target, BeadOrFormula: createSuspensionTestTask(t, store).ID}
			},
			launched: func(r SlingResult) bool { return r.Method == "default-on-formula" && r.WorkflowID != "" },
		},
		{
			name: "convoy",
			opts: func(t *testing.T, store beads.Store, target config.Agent) SlingOpts {
				return SlingOpts{Target: target, BeadOrFormula: createSuspensionTestConvoy(t, store).ID}
			},
			launched: func(r SlingResult) bool { return r.Method == "batch" && r.Routed == 1 },
		},
		{
			name: "convoy with a v1 formula per child",
			opts: func(t *testing.T, store beads.Store, target config.Agent) SlingOpts {
				return SlingOpts{Target: target, BeadOrFormula: createSuspensionTestConvoy(t, store).ID, OnFormula: "v1-work"}
			},
			launched: func(r SlingResult) bool { return r.Method == "batch-on" && r.Routed == 1 },
		},
		{
			name: "convoy dry-run",
			opts: func(t *testing.T, store beads.Store, target config.Agent) SlingOpts {
				return SlingOpts{Target: target, BeadOrFormula: createSuspensionTestConvoy(t, store).ID, DryRun: true}
			},
			launched: func(r SlingResult) bool { return r.DryRun && r.Method == "batch" && r.Routed == 1 },
		},
		{
			name: "convoy as one graph.v2 input",
			opts: func(t *testing.T, store beads.Store, target config.Agent) SlingOpts {
				return SlingOpts{Target: target, BeadOrFormula: createSuspensionTestConvoy(t, store).ID, OnFormula: "graph-work"}
			},
			launched: func(r SlingResult) bool { return r.Method == "on-formula" && r.WorkflowID != "" },
		},
	} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force=%v", tc.name, force), func(t *testing.T) {
				deps := suspendedScopesDeps(t)
				s, err := New(deps)
				if err != nil {
					t.Fatal(err)
				}
				opts := tc.opts(t, deps.Store, suspendedScopesTarget())
				opts.Force = force
				result, err := s.Dispatch(context.Background(), opts, deps.Store)
				if err != nil {
					t.Fatalf("Dispatch: %v", err)
				}
				if !tc.launched(result) {
					t.Fatalf("result = %+v, want the %s path's result", result, tc.name)
				}
				if result.Target != "myrig/polecat" {
					t.Errorf("Target = %q, want myrig/polecat (the agent and pool warnings name it)", result.Target)
				}
				// --force suppresses every target warning; without it the
				// city, rig myrig, the agent and its empty pool are all reported.
				wantRig := "myrig"
				if force {
					wantRig = ""
				}
				if result.CitySuspended == force || result.SuspendedRig != wantRig || result.AgentSuspended == force || result.PoolEmpty == force {
					t.Errorf("CitySuspended = %v, SuspendedRig = %q, AgentSuspended = %v, PoolEmpty = %v; want %v, %q, %v, %v",
						result.CitySuspended, result.SuspendedRig, result.AgentSuspended, result.PoolEmpty, !force, wantRig, !force, !force)
				}
			})
		}
	}
}
