package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	gcruntime "github.com/gastownhall/gascity/internal/runtime"
)

// packStopHookCityRuntime builds a managed city runtime whose config carries a
// single pack shipping a city-stop hook. The hook touches marker, so a test can
// assert whether the managed teardown reached it.
func packStopHookCityRuntime(t *testing.T) (cr *CityRuntime, marker string) {
	t.Helper()
	sp := &lifecycleOrderProvider{Fake: gcruntime.NewFake()}
	cr = serverLifecycleCityRuntime(t, sp)

	root := t.TempDir()
	marker = filepath.Join(root, "stopped")
	packDir := writeLifecyclePack(t, root, "hubpack", "city-stop",
		"#!/bin/sh\ntouch \""+marker+"\"\nexit 0\n")
	cr.cfg.PackDirs = []string{packDir}
	return cr, marker
}

// TestCityRuntimeShutdownRunsPackStopHooks pins the managed half of the pack
// lifecycle contract: a supervisor- or controller-hosted city stop must reach
// pack-owned services. `gc stop` delegates to the controller whenever one is
// running, so without this the hooks would only ever fire for standalone
// cities and a pack-owned daemon would survive every managed shutdown.
func TestCityRuntimeShutdownRunsPackStopHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts require a POSIX shell")
	}
	cr, marker := packStopHookCityRuntime(t)
	markOwnedForTest(cr)

	cr.shutdown()

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("managed shutdown did not run the pack city-stop hook: %v", err)
	}
}

// TestCityRuntimeShutdownWithoutOwnershipSkipsPackStopHooks mirrors the
// server-teardown ownership guard: a discarded runtime (failed adoption,
// controller-lock failure) calls shutdown() too, and must not stop the live
// owner's pack services out from under it.
func TestCityRuntimeShutdownWithoutOwnershipSkipsPackStopHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts require a POSIX shell")
	}
	cr, marker := packStopHookCityRuntime(t)
	// Deliberately NOT markOwnedForTest: this models the discarded runtime.

	cr.shutdown()

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an un-owned shutdown ran the pack city-stop hook")
	}
}

// TestCityRuntimeShutdownPreservingSessionsSkipsPackStopHooks pins the
// preserve-sessions case: those sessions keep running for the next
// supervisor to adopt, so the pack services they depend on must stay up.
func TestCityRuntimeShutdownPreservingSessionsSkipsPackStopHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts require a POSIX shell")
	}
	cr, marker := packStopHookCityRuntime(t)
	markOwnedForTest(cr)
	cr.preserveSessionsOnShutdown()

	cr.shutdown()

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a preserve-sessions shutdown ran the pack city-stop hook")
	}
}
