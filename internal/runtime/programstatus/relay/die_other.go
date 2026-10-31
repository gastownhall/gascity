//go:build darwin || (linux && !amd64 && !arm64)

package relay

import (
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// dieBySignal ends the process by sig, as if sig had killed it, for the
// signals the Go runtime itself dies by once nothing is subscribed to them
// (SIGHUP, SIGINT, SIGTERM, SIGUSR1, SIGUSR2, SIGALRM, SIGKILL and the
// like). It returns for a signal the runtime would turn into a traceback
// (SIGQUIT, SIGABRT, SIGSEGV and the like), or whose default action does not
// end the process.
func dieBySignal(sig syscall.Signal) {
	switch sig {
	case unix.SIGQUIT, unix.SIGILL, unix.SIGTRAP, unix.SIGABRT, unix.SIGBUS, unix.SIGFPE, unix.SIGSEGV, unix.SIGSYS:
		return
	}
	signal.Reset(sig)
	_ = unix.Kill(unix.Getpid(), sig)
	awaitDeath()
}
