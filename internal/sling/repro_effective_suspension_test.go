package sling

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// Regression coverage for gastownhall/gascity#8508: `gc sling` only warned
// on the DEPRECATED [[rigs]] suspended config field, never on a live
// `gc rig suspend`/`gc rig resume` runtime override, `suspended_on_start`,
// or a whole suspended city. These tests exercise the two previously-silent
// cases directly through DoSling's preflight: a rig suspended only via
// runtime state (not the deprecated field), and a whole suspended city.
//
// Each test uses its own isolated city directory rather than the package's
// sharedTestCityDir, since these tests write a real
// .gc/runtime/suspension-state.json and must not leak that write into other
// tests sharing the same directory.

func newIsolatedTestCityDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gc-sling-suspension-repro-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestDoSlingRuntimeSuspendedRigWarns covers case (2) from the issue: a rig
// suspended only through the live runtime-state file written by
// `gc rig suspend` (suspensionstate.SetRigSuspended), with the deprecated
// config field left false. Before the fix, rigSuspended read only
// cfg.Rigs[...].Suspended and never consulted runtime state, so this sling
// was silently routed with no warning at all.
func TestDoSlingRuntimeSuspendedRigWarns(t *testing.T) {
	cityPath := newIsolatedTestCityDir(t)
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, "myrig", boolPtr(true)); err != nil {
		t.Fatalf("SetRigSuspended: %v", err)
	}

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		// Deprecated field intentionally left false/unset: only the
		// runtime-state override marks this rig suspended.
		Rigs: []config.Rig{{Name: "myrig"}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.CityPath = cityPath
	deps.Store = seededStore("my-42")
	result, err := DoSling(testOpts(a, "my-42"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.SuspendedRig != "myrig" {
		t.Errorf("SuspendedRig = %q, want %q (runtime-state suspension must warn)", result.SuspendedRig, "myrig")
	}
}

// TestDoSlingSuspendedOnStartRigWarns covers case (3): the rig's
// authored suspended_on_start = true, the field `gc doctor --fix` migrates
// the deprecated `suspended` config field to. This must warn without any
// runtime-state override at all.
func TestDoSlingSuspendedOnStartRigWarns(t *testing.T) {
	cityPath := newIsolatedTestCityDir(t)

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig", SuspendedOnStart: true}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.CityPath = cityPath
	deps.Store = seededStore("my-43")
	result, err := DoSling(testOpts(a, "my-43"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.SuspendedRig != "myrig" {
		t.Errorf("SuspendedRig = %q, want %q (suspended_on_start must warn)", result.SuspendedRig, "myrig")
	}
}

// TestDoSlingSuspendedCityWarns covers case (4) from the issue: the whole
// city suspended via the live runtime override `gc suspend` writes
// (suspensionstate.SetCitySuspended). Before the fix there was no
// city-suspension check anywhere in sling -- the bead routed silently no
// matter what.
func TestDoSlingSuspendedCityWarns(t *testing.T) {
	cityPath := newIsolatedTestCityDir(t)
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, cityPath, boolPtr(true)); err != nil {
		t.Fatalf("SetCitySuspended: %v", err)
	}

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.CityPath = cityPath
	deps.Store = seededStore("my-44")
	result, err := DoSling(testOpts(a, "my-44"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if !result.SuspendedCity {
		t.Error("expected SuspendedCity=true when the city is suspended via runtime state")
	}
}

// TestDoSlingSuspendedOnStartCityWarns covers case (5): the city's authored
// workspace.suspended_on_start = true, with no runtime override at all.
func TestDoSlingSuspendedOnStartCityWarns(t *testing.T) {
	cityPath := newIsolatedTestCityDir(t)

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city", SuspendedOnStart: true}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.CityPath = cityPath
	deps.Store = seededStore("my-45")
	result, err := DoSling(testOpts(a, "my-45"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if !result.SuspendedCity {
		t.Error("expected SuspendedCity=true when workspace.suspended_on_start is true")
	}
}

// TestDoSlingSuspendedCityForce confirms --force suppresses the new
// city-suspension warning, mirroring the existing --force behavior for
// AgentSuspended/SuspendedRig.
func TestDoSlingSuspendedCityForce(t *testing.T) {
	cityPath := newIsolatedTestCityDir(t)
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, cityPath, boolPtr(true)); err != nil {
		t.Fatalf("SetCitySuspended: %v", err)
	}

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.CityPath = cityPath
	deps.Store = seededStore("my-46")
	opts := testOpts(a, "my-46")
	opts.Force = true
	result, err := DoSling(opts, deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.SuspendedCity {
		t.Error("expected SuspendedCity=false with --force")
	}
}
