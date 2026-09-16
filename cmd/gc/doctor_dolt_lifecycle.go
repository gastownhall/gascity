package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/doctor"
)

// gc doctor is a read-only diagnostic, but its beads checks shell out to bd,
// whose provider script starts the managed dolt server on demand
// (examples/bd/assets/scripts/gc-beads-bd.sh -> `gc dolt-state start-managed`).
// That server and the scope watchdog supervising it outlive the command,
// reparent to PID 1, and are invisible to `gc cities` — so running `gc doctor`
// against a stopped, unregistered city leaves a daemon pair behind that the
// operator has no reason to know they now owe a teardown for.
//
// The guard below records whether a managed dolt was already running before the
// checks and, if doctor's own checks caused one to start, stops it on the way
// out. See gastownhall/gascity#4685. The persistence mechanism (why the watchdog
// never self-exits once started) is gastownhall/gascity#4679.

// doctorShouldStopManagedDolt reports whether doctor should stop the managed
// dolt server it observed after running its checks.
//
// Split out as a pure function so the policy is testable without a live server:
// the whole risk of this change is stopping a server we did not start, so the
// decision must be provable in isolation.
//
//   - cityIsLive: a controller or supervisor is running, so the city owns its
//     dolt server and doctor must never stop it — even if the probe raced and
//     reported it down beforehand.
//   - wasRunning: a server was already up before doctor's checks. Someone else
//     started it; leaving it is the caller's expectation.
//   - nowRunning: a server is up after the checks. Nothing to stop otherwise.
func doctorShouldStopManagedDolt(cityIsLive, wasRunning, nowRunning bool) bool {
	if cityIsLive {
		return false
	}
	if wasRunning {
		return false
	}
	return nowRunning
}

// doctorManagedDoltDeps are the impure managed-dolt operations the guard needs.
// They are injected so release's stop wiring — resolve the port, probe the
// server, re-check liveness, stop — is testable without a live dolt server.
// The pure doctorShouldStopManagedDolt cannot cover that wiring: a mutant that
// dead-coded the real stop call left every table-test green, i.e. the guard
// would silently leak every server doctor started (#4685) with no test failing
// (gastownhall/gascity#4827 review finding 1).
type doctorManagedDoltDeps struct {
	resolvePort func(cityPath string) (string, managedDoltPortResolution)
	probe       func(cityPath, host, port string) (managedDoltProbeReport, error)
	liveNow     func(cityPath string) bool
	stop        func(cityPath, port string) (managedDoltStopReport, error)
}

// defaultDoctorManagedDoltDeps wires the guard to the real managed-dolt helpers.
func defaultDoctorManagedDoltDeps() doctorManagedDoltDeps {
	return doctorManagedDoltDeps{
		resolvePort: resolveManagedDoltPort,
		probe:       probeManagedDolt,
		liveNow: func(cityPath string) bool {
			return doctor.IsControllerRunning(cityPath) || supervisorAliveHook() != 0
		},
		stop: stopManagedDoltProcess,
	}
}

// doctorManagedDoltGuard captures managed-dolt state before doctor's checks so
// release can undo a start that doctor itself caused.
//
// The port is deliberately NOT cached from the snapshot. On a stopped city no
// port is published yet, so resolution returns empty until the server doctor
// triggers actually starts — caching the empty value there would disarm the
// guard in exactly the case it exists for. Both ends resolve the port fresh.
type doctorManagedDoltGuard struct {
	cityPath   string
	cityIsLive bool
	wasRunning bool
	// armed is false whenever we could not establish a trustworthy "before"
	// picture (no city path, live city, port-resolution or probe error). An
	// unarmed guard never stops anything — failing closed keeps a diagnostic
	// from killing a server it cannot prove it started.
	armed bool
	deps  doctorManagedDoltDeps
}

// newDoctorManagedDoltGuard snapshots managed-dolt state for cityPath using the
// real managed-dolt helpers.
//
// cityIsLive should be true when a controller or supervisor is running.
func newDoctorManagedDoltGuard(cityPath string, cityIsLive bool) *doctorManagedDoltGuard {
	return newDoctorManagedDoltGuardWithDeps(cityPath, cityIsLive, defaultDoctorManagedDoltDeps())
}

// newDoctorManagedDoltGuardWithDeps is newDoctorManagedDoltGuard with injectable
// dependencies, so the snapshot and release paths can be exercised without a
// live dolt server.
func newDoctorManagedDoltGuardWithDeps(cityPath string, cityIsLive bool, deps doctorManagedDoltDeps) *doctorManagedDoltGuard {
	guard := &doctorManagedDoltGuard{
		cityPath:   cityPath,
		cityIsLive: cityIsLive,
		deps:       deps,
	}
	if cityIsLive || strings.TrimSpace(cityPath) == "" {
		return guard
	}
	guard.armed = true

	port, resolution := deps.resolvePort(cityPath)
	switch resolution {
	case managedDoltPortError:
		// Presence is genuinely unknown (a transient read/parse error). Fail
		// closed: an unknown "before" picture must never be read as "nothing
		// running" and then let release stop a server someone else owns
		// (gastownhall/gascity#4827 review finding 3).
		guard.armed = false
	case managedDoltPortFound:
		report, err := deps.probe(cityPath, defaultManagedDoltBindHost, strings.TrimSpace(port))
		if err != nil {
			// A port resolved but its live state could not be read — fail closed
			// rather than risk stopping a server that was already up.
			guard.armed = false
			return guard
		}
		guard.wasRunning = report.Running
	default: // managedDoltPortAbsent
		// Nothing published: wasRunning stays false and the guard stays armed so
		// a server doctor's own checks start is still stopped on the way out.
	}
	return guard
}

// release stops the managed dolt server when doctor's own checks started it.
//
// Best effort: a failure to stop is reported on stderr but never changes the
// doctor exit code, because the checks themselves already succeeded or failed
// on their own terms.
func (g *doctorManagedDoltGuard) release(stderr io.Writer) {
	if g == nil || !g.armed {
		return
	}
	port, resolution := g.deps.resolvePort(g.cityPath)
	if resolution != managedDoltPortFound {
		// Nothing to stop, or presence could not be trusted — fail closed.
		return
	}
	port = strings.TrimSpace(port)
	if port == "" {
		return
	}
	report, err := g.deps.probe(g.cityPath, defaultManagedDoltBindHost, port)
	if err != nil {
		return
	}
	// Re-check liveness immediately before the stop decision: a city that went
	// live mid-run (gc start racing doctor) owns its server now, even though the
	// entry snapshot saw it stopped (gastownhall/gascity#4827 review finding 2 —
	// the cityIsLive TOCTOU). This narrows the window; fully closing it needs an
	// ownership lease (tracked as a linked follow-up), so treat it as defense in
	// depth, not a complete fix.
	cityIsLive := g.cityIsLive || g.deps.liveNow(g.cityPath)
	if !doctorShouldStopManagedDolt(cityIsLive, g.wasRunning, report.Running) {
		return
	}
	if _, stopErr := g.deps.stop(g.cityPath, port); stopErr != nil {
		fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
			"gc doctor: started a managed dolt server on port %s for its checks but could not stop it: %v\n",
			port, stopErr)
	}
}
