package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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

// TestCityRuntimeRunRunsPackStartHooks pins the start half of the pack
// lifecycle contract at its call site: the supervisor's own startup path, not
// just the hook runner in isolation. `gc start` hands the city to a
// CityRuntime whenever a controller is running, so without this a pack-owned
// daemon would only ever come up for standalone cities.
func TestCityRuntimeRunRunsPackStartHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts require a POSIX shell")
	}
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	packRoot := t.TempDir()
	marker := filepath.Join(packRoot, "started")
	cfg.PackDirs = []string{writeLifecyclePack(t, packRoot, "hubpack", config.LifecycleEventCityStart,
		"#!/bin/sh\ntouch \""+marker+"\"\nexit 0\n")}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sp := gcruntime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, gcruntime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops: newDrainOps(sp),
		Rec:  events.Discard,
		// Startup is complete by the time onStarted fires, so canceling
		// here stops the run loop at its first context check — after the
		// city-start hooks, which run immediately below it.
		OnStarted: func() { cancel() },
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})

	cr.run(ctx)

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("city runtime startup did not run the pack city-start hook: %v", err)
	}
}
