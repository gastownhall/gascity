package main

import (
	"os"
	"strings"
	"sync/atomic"
)

// supervisorAmbientDoltPortExportEnabled gates process-env export of the
// managed Dolt port. Only `gc supervisor run` (doSupervisorRun) enables it:
// the supervisor is the long-lived process whose ambient env children
// inherit, and test binaries / standalone invocations must not have their
// process env mutated by a city-runtime tick.
var supervisorAmbientDoltPortExportEnabled atomic.Bool

func enableSupervisorAmbientDoltPortExport() {
	supervisorAmbientDoltPortExportEnabled.Store(true)
}

// envBeadsDoltServerPort is the beads-side spelling of the managed Dolt port.
// The GC_DOLT_PORT / BEADS_DOLT_SERVER_PORT pairing rule also exists, with
// different semantics, in mirrorBeadsDoltServerEnv (bd_env.go) — that one
// blanks the pair on empty, this one must never unset. Keep them in agreement
// by hand; a third port alias would need adding to both.
const envBeadsDoltServerPort = "BEADS_DOLT_SERVER_PORT"

// ambientDoltEnvGet and ambientDoltEnvSet are the process-environment seam for
// the ambient port export. Production wires them to the real os functions.
//
// The seam exists so the export's behavior can be asserted without mutating
// this process's environment: these two keys are leak vectors (a stray
// GC_DOLT_PORT can point a test at a live city's Dolt), and cmd/gc holds a
// standing ratchet against growing process-environment mutation in its test
// source — see TESTING.md, "Small debt ratchet". Reassigning these outside
// tests is a bug.
var (
	ambientDoltEnvGet = os.Getenv
	ambientDoltEnvSet = os.Setenv
)

// exportSupervisorAmbientDoltPortEnv projects the live managed Dolt port into
// this process's own environment (GC_DOLT_PORT + BEADS_DOLT_SERVER_PORT).
//
// Children spawned from the supervisor's process env — bd hook subprocesses,
// control-dispatcher ready queries via ambientDoltConnectionQueryPrefix
// (dispatch_runtime.go), order exec — fall back to these ambient coordinates
// when per-call scope resolution transiently comes back without a port
// (gc-74rxa). Before this export existed, a supervisor restart that ADOPTED a
// surviving managed Dolt (systemd restart while the Dolt PID survives) left
// the fresh supervisor process with no ambient port at all: the ambient
// passthrough was disabled, every poll depended on re-resolution succeeding,
// and a transient miss resolved bd at 127.0.0.1:0 and froze order firing.
// Operators pinned a static port via a systemd drop-in as a stopgap, which
// then broke every time the managed Dolt respawned on a different port.
//
// Called from ensureManagedDoltPublishedForRuntime on runtime start and on
// every reconcile tick, so the ambient env follows the live port across
// start, adopt, and respawn-with-new-port. It never unsets: a transiently
// unresolvable state (mid-restart probe window) must not erase the last
// known-good hint, and the next tick with a valid published state rewrites
// it. BEADS_DOLT_SERVER_PORT can be momentarily clobbered by a concurrent
// native-Dolt-open env restore (withNativeDoltOpenEnv holds no lock we take);
// that is benign because GC_DOLT_PORT is not in that scoped key set, takes
// precedence in ambientDoltHostPort, and the per-tick rewrite self-heals.
// Last writer wins when one supervisor manages several dolt-owning cities;
// the ambient values are a fallback hint only, since per-call projections
// strip and re-resolve these keys (mergeRuntimeEnv) whenever scope
// resolution succeeds.
func exportSupervisorAmbientDoltPortEnv(port string) {
	if !supervisorAmbientDoltPortExportEnabled.Load() {
		return
	}
	port = strings.TrimSpace(port)
	if port == "" {
		return
	}
	if ambientDoltEnvGet(envDoltPort) == port && ambientDoltEnvGet(envBeadsDoltServerPort) == port {
		return
	}
	// Errors are dropped deliberately: os.Setenv fails only on a malformed key,
	// and both keys are compile-time literals. There is no runtime condition
	// here that a caller could act on.
	_ = ambientDoltEnvSet(envDoltPort, port)
	_ = ambientDoltEnvSet(envBeadsDoltServerPort, port)
}
