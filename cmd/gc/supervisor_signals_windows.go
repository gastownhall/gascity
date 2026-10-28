//go:build windows

package main

import (
	"os"
	"syscall"
)

// supervisorControlSignals are the signals the supervisor asks the OS to
// deliver on Windows.
//
// SIGHUP is deliberately absent. Windows has no hangup: Go's console handler
// raises SIGINT for Ctrl+C and Ctrl+Break and SIGTERM for close, logoff and
// shutdown, and nothing raises SIGHUP. Registering it would still compile and
// still return nil — signal.Notify has no way to report that a platform cannot
// deliver a signal — so the supervisor would advertise a reload path that
// silently never fires.
//
// Reload on Windows is the "reload" command on the supervisor control
// endpoint, which `gc supervisor reload` speaks. That path is not a
// compatibility shim: it is the same command the Unix SIGHUP handler funnels
// into, it reports back busy/ok/timeout rather than being fire-and-forget, and
// it is already what the supervisor's own callers use.
//
// Shutdown needs no such substitute. SIGTERM does arrive here, and the
// authoritative graceful stop is the control endpoint's "stop" command in
// either case — `gc supervisor stop` drains managed cities and waits for
// done:ok, where a signal only starts the same drain without reporting its
// outcome.
func supervisorControlSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}

// supervisorReloadSignalHint names the reload path that replaces the signal
// this platform cannot deliver, so an operator reading the supervisor log is
// not left to discover it.
func supervisorReloadSignalHint() string {
	return "gc supervisor: reload signal is unavailable on windows; use 'gc supervisor reload'"
}
