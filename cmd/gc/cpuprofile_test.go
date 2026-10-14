package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// burnSink keeps burnCPU's result live so the loop is not optimized away.
var burnSink int

// burnCPU keeps the process on-CPU long enough for the profiler to sample.
func burnCPU() int {
	sum := 0
	for i := 0; i < 50_000_000; i++ {
		sum += i % 7
	}
	return sum
}

// requireGzipProfile fails unless path holds a gzip stream, which is what
// runtime/pprof writes for every CPU profile (even one with no samples).
func requireGzipProfile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading profile: %v", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s is not a gzip pprof profile: %v", path, err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("%s gzip stream is truncated: %v", path, err)
	}
}

// requireProfilerFree fails if some CPU profile is still running, then
// releases the one it just started. runtime/pprof allows a single profile per
// process, so this is how the tests observe that stop() really stopped it.
func requireProfilerFree(t *testing.T) {
	t.Helper()
	if err := pprof.StartCPUProfile(io.Discard); err != nil {
		t.Fatalf("a CPU profile is still running: %v", err)
	}
	pprof.StopCPUProfile()
}

func TestCPUProfileFromEnvWritesPidSuffixedProfile(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "gc.prof")
	t.Setenv(gcCPUProfileEnv, prefix)
	var stderr bytes.Buffer

	stop := startCPUProfileFromEnv(&stderr)
	burnSink = burnCPU()
	stop()

	requireGzipProfile(t, prefix+"."+strconv.Itoa(os.Getpid()))
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	requireProfilerFree(t)
}

func TestCPUProfileFromEnvUnsetDoesNothing(t *testing.T) {
	t.Setenv(gcCPUProfileEnv, "")
	var stderr bytes.Buffer

	stop := startCPUProfileFromEnv(&stderr)
	requireProfilerFree(t)
	stop()

	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestCPUProfileFromEnvReportsUncreatablePath(t *testing.T) {
	t.Setenv(gcCPUProfileEnv, filepath.Join(t.TempDir(), "no-such-dir", "gc.prof"))
	var stderr bytes.Buffer

	stop := startCPUProfileFromEnv(&stderr)
	stop()

	if got := stderr.String(); !strings.Contains(got, gcCPUProfileEnv) {
		t.Errorf("stderr = %q, want a warning naming %s", got, gcCPUProfileEnv)
	}
	requireProfilerFree(t)
}

func TestCPUProfileFromEnvLeavesARunningProfilerAlone(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "gc.prof")
	t.Setenv(gcCPUProfileEnv, prefix)
	if err := pprof.StartCPUProfile(io.Discard); err != nil {
		t.Fatalf("starting the competing profile: %v", err)
	}
	t.Cleanup(pprof.StopCPUProfile)
	var stderr bytes.Buffer

	stop := startCPUProfileFromEnv(&stderr)
	stop()

	if got := stderr.String(); !strings.Contains(got, gcCPUProfileEnv) {
		t.Errorf("stderr = %q, want a warning naming %s", got, gcCPUProfileEnv)
	}
	if _, err := os.Stat(prefix + "." + strconv.Itoa(os.Getpid())); !os.IsNotExist(err) {
		t.Errorf("a profile file was left behind for the profile that never started (stat err = %v)", err)
	}
	// The competing profile must survive: stop() may only end its own.
	if err := pprof.StartCPUProfile(io.Discard); err == nil {
		t.Error("stop() ended a profile it did not start")
	}
}

// TestCPUProfileEnvScript runs the real gc entry point (mainExitCode, in a
// subprocess) with GC_CPUPROFILE set, so it proves the wiring and not just the
// helper: a command that returns normally leaves a profile at <prefix>.<pid>.
func TestCPUProfileEnvScript(t *testing.T) {
	testscript.Run(t, newTestscriptParams(t, filepath.Join("testdata", "cpuprofile-env.txtar")))
}

// gzipProfilesCmd implements the gzip-profiles testscript command, registered
// by newTestscriptParams so every harness that runs testdata/*.txtar (the
// directory-wide TestTutorial01 included) can run cpuprofile-env.txtar.
//
// Usage: gzip-profiles <dir> <prefix> <count>. Exactly <count> files in <dir>
// start with "<prefix>." and hold a complete gzip stream.
func gzipProfilesCmd(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 3 {
		ts.Fatalf("usage: gzip-profiles <dir> <prefix> <count>")
	}
	want, err := strconv.Atoi(args[2])
	if err != nil {
		ts.Fatalf("count %q: %v", args[2], err)
	}
	entries, err := os.ReadDir(ts.MkAbs(args[0]))
	if err != nil {
		ts.Fatalf("reading %s: %v", args[0], err)
	}
	got := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), args[1]+".") {
			continue
		}
		got++
		data, err := os.ReadFile(filepath.Join(ts.MkAbs(args[0]), entry.Name()))
		if err != nil {
			ts.Fatalf("reading %s: %v", entry.Name(), err)
		}
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			ts.Fatalf("%s is not a gzip profile: %v", entry.Name(), err)
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			ts.Fatalf("%s gzip stream is truncated: %v", entry.Name(), err)
		}
	}
	if got != want {
		ts.Fatalf("%d profile(s) named %s.<pid> in %s, want %d", got, args[1], args[0], want)
	}
}
