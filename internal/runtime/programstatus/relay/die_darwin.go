//go:build darwin

package relay

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// dieBySignal ends the process by sig, as if sig had killed it, through
// [dieByExec]. It returns when sig would not end the process; on Darwin
// SIGIO and SIGINFO are ignored by default as well.
func dieBySignal(sig syscall.Signal) {
	dieByExec(sig, unix.SIGIO, unix.SIGINFO)
}
