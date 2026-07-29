package main

import (
	"fmt"
	"io"
	"strings"
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
	// picture (no city path, live city, probe error). An unarmed guard never
	// stops anything — failing closed keeps a diagnostic from killing a server
	// it cannot prove it started.
	armed bool
}

// newDoctorManagedDoltGuard snapshots managed-dolt state for cityPath.
//
// cityIsLive should be true when a controller or supervisor is running.
func newDoctorManagedDoltGuard(cityPath string, cityIsLive bool) *doctorManagedDoltGuard {
	guard := &doctorManagedDoltGuard{
		cityPath:   cityPath,
		cityIsLive: cityIsLive,
	}
	if cityIsLive || strings.TrimSpace(cityPath) == "" {
		return guard
	}
	guard.armed = true

	port := strings.TrimSpace(currentManagedDoltPort(cityPath))
	if port == "" {
		// Nothing published means nothing running that we could later mistake
		// for a server someone else owns; wasRunning stays false.
		return guard
	}
	report, err := probeManagedDolt(cityPath, defaultManagedDoltBindHost, port)
	if err != nil {
		// A port exists but we cannot read its state — fail closed rather than
		// risk stopping a server that was already up.
		guard.armed = false
		return guard
	}
	guard.wasRunning = report.Running
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
	port := strings.TrimSpace(currentManagedDoltPort(g.cityPath))
	if port == "" {
		return
	}
	report, err := probeManagedDolt(g.cityPath, defaultManagedDoltBindHost, port)
	if err != nil {
		return
	}
	if !doctorShouldStopManagedDolt(g.cityIsLive, g.wasRunning, report.Running) {
		return
	}
	if _, stopErr := stopManagedDoltProcess(g.cityPath, port); stopErr != nil {
		fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
			"gc doctor: started a managed dolt server on port %s for its checks but could not stop it: %v\n",
			port, stopErr)
	}
}
