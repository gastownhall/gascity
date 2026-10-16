package main

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/resilience"
)

// openFleetGuardOn installs a tripped fleet guard on cr, on a fake clock that
// never reaches the probe window, so the runtime's own ensureFleetStartGuard
// returns it instead of building a real-clock one.
func openFleetGuardOn(t *testing.T, cr *CityRuntime) *fleetStartGuard {
	t.Helper()
	g, clk := newFleetTestGuard(t)
	tripFleet(t, g, clk)
	if got := fleetState(g); got != resilience.StateOpen {
		t.Fatalf("premise: guard state = %v, want open", got)
	}
	cr.fleetGuard = g
	cr.fleetGuardTimeout = cr.cfg.Session.StartupTimeoutDuration()
	return g
}

// fleetGuardTickCase is one tick entry point driven against an open fleet
// breaker.
type fleetGuardTickCase struct {
	name string
	// agent and command build the runtime's only agent.
	agent, command string
	tick           func(t *testing.T, cr *CityRuntime)
}

func fleetGuardTickCases() []fleetGuardTickCase {
	return []fleetGuardTickCase{
		{
			name:    "control dispatcher tick",
			agent:   config.ControlDispatcherAgentName,
			command: config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			tick: func(_ *testing.T, cr *CityRuntime) {
				cr.controlDispatcherTick(context.Background())
			},
		},
		{
			name:    "main tick",
			agent:   "worker",
			command: "worker-cmd",
			tick: func(t *testing.T, cr *CityRuntime) {
				dirty := &atomic.Bool{}
				lastProviderName := ""
				prevPoolRunning := make(map[string]bool)
				cr.tick(context.Background(), dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "poke")
				if !cr.waitForAsyncStarts() {
					t.Fatal("async starts did not settle after the main tick")
				}
			},
		},
	}
}

// Both reconcile entry points pass the runtime's fleet guard to their start
// executor: with the breaker open a due automatic start is deferred, and with
// the [daemon] kill switch off the same tick starts it. Removing
// withFleetStartGuard(cr.ensureFleetStartGuard()) from either site fails the
// first subtest of that entry point.
func TestControlDispatcherTick_UsesFleetStartGuard(t *testing.T) {
	testFleetGuardTick(t, fleetGuardTickCases()[0])
}

func TestCityRuntimeTick_UsesFleetStartGuard(t *testing.T) {
	testFleetGuardTick(t, fleetGuardTickCases()[1])
}

func testFleetGuardTick(t *testing.T, tc fleetGuardTickCase) {
	t.Run("open breaker defers the start", func(t *testing.T) {
		cr, sp := newCapacityRefusingRuntime(t, tc.agent, tc.command, nil)
		sp.refusing = false
		g := openFleetGuardOn(t, cr)

		tc.tick(t, cr)

		if got := sp.startCalls(); got != 0 {
			t.Fatalf("Start calls = %d, want 0: the %s did not honor the open fleet breaker", got, tc.name)
		}
		if got := fleetState(g); got != resilience.StateOpen {
			t.Errorf("fleet breaker state after the tick = %v, want it still open", got)
		}
	})
	t.Run("kill switch off starts it", func(t *testing.T) {
		cr, sp := newCapacityRefusingRuntime(t, tc.agent, tc.command, nil)
		sp.refusing = false
		openFleetGuardOn(t, cr)
		off := false
		cr.cfg.Daemon.FleetStartBreaker = &off

		tc.tick(t, cr)

		if got := sp.startCalls(); got != 1 {
			t.Fatalf("Start calls = %d, want 1 with fleet_start_breaker = false", got)
		}
		if cr.fleetGuard != nil {
			t.Error("the runtime kept its fleet guard with the kill switch off")
		}
	})
}
