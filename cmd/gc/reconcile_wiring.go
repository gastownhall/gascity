package main

import (
	"fmt"
	"io"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/config"
)

// controllerWiring is what a controller entry point builds before its socket
// can deliver anything: the latched reconciler mode, the reconciler's wake
// signals and the wake over them, and the channels the socket, the API and
// the city runtime share. runController and the supervisor's startOneCity
// both take it from newControllerWiring, so neither can wire a channel the
// other forgets.
type controllerWiring struct {
	mode                        reconcilerMode
	lookupEnv                   func(string) (string, bool)
	pokeCh, controlDispatcherCh chan struct{}
	wake                        *controllerWake
	// v2 is the constructed, unstarted planner when mode is v2, else nil. It
	// is already on wake, so a key the socket delivers before the city
	// runtime exists leaves the planner dirty for its first pass.
	v2               *plannerRuntime
	reloadReqCh      chan reloadRequest
	convergenceReqCh chan convergenceRequest
	configDirty      *atomic.Bool
}

// newControllerWiring latches the session reconciler and builds the wiring.
// A refused mode is a start failure: the caller must not start the city.
// lookupEnv carries the developer override (latchReconcilerMode).
func newControllerWiring(cfg *config.City, lookupEnv func(string) (string, bool), stderr io.Writer) (*controllerWiring, error) {
	mode, err := latchReconcilerMode(cfg, lookupEnv)
	if err != nil {
		return nil, err
	}
	w := &controllerWiring{
		mode:                mode,
		lookupEnv:           lookupEnv,
		pokeCh:              make(chan struct{}, 1),
		controlDispatcherCh: make(chan struct{}, 1),
		reloadReqCh:         make(chan reloadRequest),
		convergenceReqCh:    make(chan convergenceRequest, 16),
		configDirty:         &atomic.Bool{},
	}
	w.wake = newLegacyWake(w.pokeCh, w.controlDispatcherCh)
	if mode == reconcilerV2 {
		w.v2 = newDefaultPlanner(stderr)
		w.wake.planner, w.wake.waitDepClosed = w.v2.planner, w.v2.waitDependencyClosed
		fmt.Fprintln(stderr, "session reconciler: v2 (planner: trace-only)") //nolint:errcheck // best-effort stderr
	}
	return w, nil
}

// runtimeParams returns p with every field the wiring owns filled in: the
// latched mode and the environment it was latched with, the wake and the
// planner, and the channels the socket and the API share with the city
// runtime. runController and startOneCity both build their city runtime's
// params through it, so neither can hand the runtime half of its wiring;
// checkReconcilerWiring refuses a planner handed in without its wake.
func (w *controllerWiring) runtimeParams(p CityRuntimeParams) CityRuntimeParams {
	p.ReconcilerMode = w.mode
	p.ReconcilerLookupEnv = w.lookupEnv
	p.Wake = w.wake
	p.V2 = w.v2
	p.ConfigDirty = w.configDirty
	p.ReloadReqCh = w.reloadReqCh
	p.ConvergenceReqCh = w.convergenceReqCh
	p.PokeCh = w.pokeCh
	p.ControlDispatcherCh = w.controlDispatcherCh
	return p
}

// reloadIntent is what the tick knows about a reload: where it came from,
// and whether it is soft (accept config drift instead of draining,
// city_runtime.go's soft-acceptance guard).
type reloadIntent struct {
	Source reloadSource
	Soft   bool
}

// v2SoftReloadUnavailable answers a soft reload under v2: drift acceptance
// (MAINT-025) is a legacy session phase, off under v2 until C7c hands the
// planner a soft-reload request and deletes this hook (v5 M2). The reply says
// so instead of reporting zero accepted sessions.
func v2SoftReloadUnavailable(intent reloadIntent, r *reloadControlReply) {
	if intent.Soft {
		r.Warnings = append(r.Warnings, v2SoftReloadUnavailableWarning)
	}
}

const v2SoftReloadUnavailableWarning = "soft reload: config drift acceptance is not available under session_reconciler=v2 yet; the config applied, and drifted sessions keep their accepted config hashes"
