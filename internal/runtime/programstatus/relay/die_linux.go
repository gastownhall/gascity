//go:build linux && (amd64 || arm64)

package relay

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// kernelSigaction is the kernel's struct sigaction on amd64 and arm64.
type kernelSigaction struct {
	handler  uintptr
	flags    uint64
	restorer uintptr
	mask     uint64
}

// dieBySignal ends the process by sig, as if sig had killed it: it resets
// sig's disposition to the default in the kernel (the Go runtime would
// otherwise catch it, and turn a fatal signal such as SIGQUIT or SIGSEGV
// into a traceback and exit status 2) and sends it to the process, with
// core dumps off. It returns when sig's default action does not end the
// process.
func dieBySignal(sig syscall.Signal) {
	dfl := kernelSigaction{} // handler SIG_DFL, no flags, empty mask
	_, _, errno := unix.RawSyscall6(unix.SYS_RT_SIGACTION, uintptr(sig), uintptr(unsafe.Pointer(&dfl)), 0, unsafe.Sizeof(dfl.mask), 0, 0)
	if errno != 0 {
		return
	}
	// The program already dumped its own core if it was going to; the
	// relay's would only duplicate it with nothing of the program's in it.
	// A non-dumpable process dumps nowhere, piped core_pattern included.
	_ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)     // on failure an extra core is the only cost
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}) // likewise
	var set unix.Sigset_t
	set.Val[0] = 1 << (uint(sig) - 1)
	_ = unix.PthreadSigmask(unix.SIG_UNBLOCK, &set, nil) // Go threads run with it unblocked already
	_ = unix.Kill(unix.Getpid(), sig)
	awaitDeath()
}
