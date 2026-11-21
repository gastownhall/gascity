package main

import (
	"fmt"
	"io"
	"os"
	"runtime/pprof"
)

// gcCPUProfileEnv names the debug environment variable that makes a gc
// invocation write a CPU profile of itself. Its value is a path prefix; the
// profile lands in "<value>.<pid>" so that every gc process inheriting the
// variable (each hook and patrol read in a session) gets its own file.
const gcCPUProfileEnv = "GC_CPUPROFILE"

// startCPUProfileFromEnv starts a runtime/pprof CPU profile for this process
// when GC_CPUPROFILE is set, and returns the function that finishes it. The
// profile is complete only when stop runs, so a process that is killed first
// leaves a partial file.
//
// Profiling is a diagnostic: a profile that cannot be started is reported on
// stderr and the command runs unprofiled, and the returned stop never touches
// a profile this call did not start.
func startCPUProfileFromEnv(stderr io.Writer) (stop func()) {
	prefix := os.Getenv(gcCPUProfileEnv)
	if prefix == "" {
		return func() {}
	}
	path := fmt.Sprintf("%s.%d", prefix, os.Getpid())
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "gc: %s: creating profile: %v\n", gcCPUProfileEnv, err) //nolint:errcheck // best-effort stderr
		return func() {}
	}
	if err := pprof.StartCPUProfile(file); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		fmt.Fprintf(stderr, "gc: %s: starting profile: %v\n", gcCPUProfileEnv, err) //nolint:errcheck // best-effort stderr
		return func() {}
	}
	return func() {
		pprof.StopCPUProfile()
		if err := file.Close(); err != nil {
			fmt.Fprintf(stderr, "gc: %s: closing %s: %v\n", gcCPUProfileEnv, path, err) //nolint:errcheck // best-effort stderr
		}
	}
}
