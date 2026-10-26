package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
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
// lookupEnv carries the developer override (latchReconcilerMode). It logs the
// latch's boot warnings (reconcilerBootWarnings) once, here, the one latch
// both controller entry points share.
func newControllerWiring(cfg *config.City, lookupEnv func(string) (string, bool), stderr io.Writer) (*controllerWiring, error) {
	return newControllerWiringLoggingTo(cfg, lookupEnv, stderr, stderr)
}

// newControllerWiringLoggingTo is newControllerWiring with the boot lines on
// bootLog, so the supervisor can name the city in them and log them once
// across init retries (logSupervisorReconcilerBoot).
func newControllerWiringLoggingTo(cfg *config.City, lookupEnv func(string) (string, bool), stderr, bootLog io.Writer) (*controllerWiring, error) {
	res, err := resolveReconcilerMode(cfg, lookupEnv)
	if err != nil {
		return nil, err
	}
	mode := res.Mode
	for _, w := range reconcilerBootWarnings(cfg, res) {
		fmt.Fprintf(bootLog, "warning: %s\n", w) //nolint:errcheck // best-effort log
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
	w.wake.mode = mode
	if mode == reconcilerV2 {
		w.v2 = newDefaultPlanner(stderr)
		w.wake.planner, w.wake.waitDepClosed, w.wake.wakeReads = w.v2.planner, w.v2.waitDependencyClosed, w.v2.wakeReads
		fmt.Fprintln(bootLog, "session reconciler: "+res.String()) //nolint:errcheck // best-effort log
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

// v2SoftReloadUnavailable answers a soft reload under a trace-only v2
// planner, which writes no row: legacy's drift acceptance (MAINT-025) is
// off, and A8 accepts only with effects on (acceptSoftReload). The reply
// says so instead of reporting zero accepted sessions.
func v2SoftReloadUnavailable(intent reloadIntent, r *reloadControlReply) {
	if intent.Soft {
		r.Warnings = append(r.Warnings, v2SoftReloadUnavailableWarning)
	}
}

const v2SoftReloadUnavailableWarning = "soft reload: config drift acceptance is not available under a trace-only v2 session reconciler; the config applied, and drifted sessions keep their accepted config hashes"

// supervisorReconcilerBootLogged holds, per city path, the reconciler boot
// lines the supervisor logged for the city's current boot: set by its first
// init attempt, kept across the init retries that follow a failure, and
// forgotten once the city starts (forgetSupervisorReconcilerBoot), so the next
// boot of the city logs them again.
var supervisorReconcilerBootLogged sync.Map

// logSupervisorReconcilerBoot logs a supervisor-hosted city's reconciler boot
// lines (newControllerWiringLoggingTo), each naming the city. An init retry
// of the same boot whose lines match the last ones logged logs nothing; a
// config edit that changes them logs again.
func logSupervisorReconcilerBoot(stderr io.Writer, path, cityName, lines string) {
	if prev, ok := supervisorReconcilerBootLogged.Swap(path, lines); ok && prev == lines {
		return
	}
	for _, line := range strings.Split(strings.TrimSuffix(lines, "\n"), "\n") {
		if line != "" {
			fmt.Fprintf(stderr, "gc supervisor: city '%s': %s\n", cityName, line) //nolint:errcheck // best-effort stderr
		}
	}
}

// forgetSupervisorReconcilerBoot ends the city's boot for the boot-log dedupe:
// the city at path started, so its next start is a new boot.
func forgetSupervisorReconcilerBoot(path string) {
	supervisorReconcilerBootLogged.Delete(path)
}
