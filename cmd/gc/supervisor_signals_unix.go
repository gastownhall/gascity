//go:build !windows

package main

import (
	"os"
	"syscall"
)

// supervisorControlSignals are the signals the supervisor asks the OS to
// deliver: SIGINT and SIGTERM request shutdown, SIGHUP requests an immediate
// reconcile.
func supervisorControlSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}
}

// supervisorReloadSignalHint is empty where the reload signal exists, so the
// operator-facing text stays exactly as it was.
func supervisorReloadSignalHint() string { return "" }
