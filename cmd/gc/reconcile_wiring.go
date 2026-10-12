package main

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
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
	// v2 is the constructed, unstarted v2 runtime when mode is v2, else nil.
	// Its router is already on wake, so a key the socket delivers before the
	// city runtime exists waits in its queue.
	v2               *v2Runtime
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
		w.v2 = newDefaultV2Runtime(v2SessionsLeg(cfg), stderr)
		w.wake.router = w.v2.router
		fmt.Fprintln(stderr, "session reconciler: v2 (skeleton: trace-only controllers)") //nolint:errcheck // best-effort stderr
	}
	return w, nil
}

// runtimeParams returns p with every field the wiring owns filled in: the
// latched mode and the environment it was latched with, the wake and the v2
// runtime, and the channels the socket and the API share with the city
// runtime. runController and startOneCity both build their city runtime's
// params through it, so neither can hand the runtime half of its wiring;
// checkReconcilerWiring refuses a v2 runtime handed in without its wake.
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

// newDefaultV2Runtime constructs this build's v2 runtime, unbound: the city
// runtime binds its host (bindHost). P2 registers the one reload hook the
// skeleton needs, which P4.4's soft drift rewrite replaces.
func newDefaultV2Runtime(sessionsLeg string, stderr io.Writer) *v2Runtime {
	metrics := newV2Metrics()
	rt := newV2Runtime(v2Host{sessionsLeg: sessionsLeg, stderr: stderr}, defaultV2Controllers(metrics), metrics)
	rt.barrier.afterPublish = append(rt.barrier.afterPublish, v2SoftReloadUnavailable)
	return rt
}

// v2SoftReloadUnavailable answers a soft reload under v2: drift acceptance
// (MAINT-025) is a legacy session phase, off under v2 until P4.4 rewrites the
// accepted hashes inside the barrier. The reply says so instead of reporting
// zero accepted sessions.
func v2SoftReloadUnavailable(_ context.Context, intent reloadIntent, _ *reconcileEnv, r *reloadControlReply) {
	if intent.Soft {
		r.Warnings = append(r.Warnings, v2SoftReloadUnavailableWarning)
	}
}

const v2SoftReloadUnavailableWarning = "soft reload: config drift acceptance is not available under session_reconciler=v2 yet; the config applied, and drifted sessions keep their accepted config hashes"

// v2SessionsLeg is the census label of the sessions-class store, the leg every
// v2 session key carries (rowKey). It reads only config, so the wiring knows
// it before the city runtime opens any store: the shared infrastructure
// binding's class ref when [storage] moves the infrastructure classes off the
// work store (the only split this runtime serves, storageSplitShapeOf), else
// the city work store's scoped label. [storage] is boot-latched, so it is
// stable for the process. The city label is spelled from Workspace.Name
// (censusCityName), not the resolved city name, and Workspace.Name is
// reloadable: the router keeps the leg latched here, while a census read
// after a reload that renames the workspace labels its rows city:<new name>.
// P3-7 must label the census's leading leg with the latched leg (L1).
func v2SessionsLeg(cfg *config.City) string {
	if shape, _ := storageSplitShapeOf(cfg.EffectiveStorage()); shape == storageSplitWhole {
		var infra []coordclass.Class
		for _, c := range coordclass.Classes() {
			if c.IsInfrastructure() {
				infra = append(infra, c)
			}
		}
		return string(storeref.ClassRef(infra))
	}
	return censusRef(cfg, storeref.WorkRef, censusRefScoped)
}
