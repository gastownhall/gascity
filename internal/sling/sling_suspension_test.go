package sling

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// suspensionTestDeps returns sling deps rooted at a private city directory so
// runtime suspension state written by a test never leaks into other tests.
func suspensionTestDeps(t *testing.T, cfg *config.City, runner *fakeRunner, beadID string) SlingDeps {
	t.Helper()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.CityPath = t.TempDir()
	deps.Store = seededStore(beadID)
	return deps
}

func setCitySuspendedState(t *testing.T, cityPath string, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, cityPath, &suspended); err != nil {
		t.Fatalf("SetCitySuspended(%v): %v", suspended, err)
	}
}

func assertSlingRouted(t *testing.T, result SlingResult, runner *fakeRunner, beadID string) {
	t.Helper()
	if result.BeadID != beadID {
		t.Errorf("BeadID = %q, want %q (bead must still be routed)", result.BeadID, beadID)
	}
	if len(runner.calls) != 1 {
		t.Errorf("got %d runner calls, want 1 routing call", len(runner.calls))
	}
}

func TestDoSlingWarnsCitySuspendedByRuntimeState(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	deps := suspensionTestDeps(t, cfg, runner, "BL-1")
	setCitySuspendedState(t, deps.CityPath, true)

	result, err := DoSling(testOpts(a, "BL-1"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if !result.CitySuspended {
		t.Error("CitySuspended = false, want true for a city suspended in runtime state")
	}
	if result.SuspendedRig != "" || result.AgentSuspended {
		t.Errorf("SuspendedRig = %q, AgentSuspended = %v; want only the city flagged", result.SuspendedRig, result.AgentSuspended)
	}
	assertSlingRouted(t, result, runner, "BL-1")
}

func TestDoSlingCitySuspendedWarnsEvenOnFailure(t *testing.T) {
	runner := newFakeRunner()
	runner.on("bd update", fmt.Errorf("runner failed"))
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	deps := suspensionTestDeps(t, cfg, runner, "BL-1")
	setCitySuspendedState(t, deps.CityPath, true)

	result, err := DoSling(testOpts(a, "BL-1"), deps, nil)
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
	deps := suspensionTestDeps(t, cfg, runner, "BL-1")

	result, err := DoSling(testOpts(a, "BL-1"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.CitySuspended {
		t.Error("CitySuspended = true, want false for a live city")
	}
}
