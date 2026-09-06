//go:build !windows

package main

import (
	"os/signal"
	"syscall"
)

// ignoreSIGPIPE makes a write to a closed stdout or stderr return EPIPE to Go
// code instead of killing the process.
//
// Go's default disposition for SIGPIPE is deliberately asymmetric: a failed
// write to any other descriptor returns EPIPE, but one to file descriptor 1 or
// 2 raises SIGPIPE and terminates the program, on the theory that a CLI whose
// output pipe closed (`gc ... | head`) should die quietly like a C program.
//
// That default is wrong for gc, and specifically it made the claim-delivery
// unwind unreachable. `gc hook --claim` writes its result to a tool pipe the
// provider can close at any moment; when it did, the process was killed INSIDE
// the write, so the compensating release never ran and the claim it had already
// won stayed parked on a session that would never execute it. The fence can
// only unwind a claim if the process survives long enough to notice it could
// not deliver it — see writeHookClaimWorkResultForBead.
//
// signal.Ignore is what flips it: with SIGPIPE ignored, the runtime lets the
// write fail normally and Go code sees the EPIPE. Every gc write path already
// either checks its write error or deliberately discards it, so nothing depends
// on the process dying here; the paths that piped output to a closed reader now
// exit through their own error handling instead of by signal.
func ignoreSIGPIPE() {
	signal.Ignore(syscall.SIGPIPE)
}
