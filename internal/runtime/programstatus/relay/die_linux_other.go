//go:build linux && !amd64 && !arm64

package relay

import "syscall"

// dieBySignal ends the process by sig, as if sig had killed it, through
// [dieByExec]: off amd64 and arm64 the relay does not carry the kernel's
// sigaction layout to reset the runtime's handler with. It returns when sig
// would not end the process.
func dieBySignal(sig syscall.Signal) {
	dieByExec(sig)
}
