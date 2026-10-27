package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	gcruntime "github.com/gastownhall/gascity/internal/runtime"
)

// TestCmdStopBodyRunsPackStopHooksBeforeBeadsShutdown pins the standalone half
// of the pack lifecycle contract (ga-b0o): a city stop must reach pack-owned
// services, and must do so while the bead store is still up so a hook can read
// the ledger on its way down.
func TestCmdStopBodyRunsPackStopHooksBeforeBeadsShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts require a POSIX shell")
	}
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	packRoot := t.TempDir()
	marker := filepath.Join(packRoot, "hub-stopped")
	packDir := writeLifecyclePack(t, packRoot, "hubpack", config.LifecycleEventCityStop,
		"#!/bin/sh\ntouch \""+marker+"\"\nexit 0\n")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "pack-lifecycle-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
		PackDirs:  []string{packDir},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)

	hookRanBeforeBeadsShutdown := false
	overrideShutdownBeadsProviderForStop(t, func(string) error {
		_, err := os.Stat(marker)
		hookRanBeforeBeadsShutdown = err == nil
		return nil
	})

	sp := gcruntime.NewFake()
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (gcruntime.Provider, error) { return sp, nil }

	var stdout, stderr lockedBuffer
	if code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdStopBody() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("gc stop did not run the pack city-stop hook: %v", err)
	}
	if !hookRanBeforeBeadsShutdown {
		t.Error("pack city-stop hook ran after the bead store was shut down")
	}
}

// TestCmdStopBodySurvivesFailingPackHook pins the fail-open contract: a broken
// pack hook is reported, but never turns a successful city stop into a failure.
func TestCmdStopBodySurvivesFailingPackHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook scripts require a POSIX shell")
	}
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	packDir := writeLifecyclePack(t, t.TempDir(), "hubpack", config.LifecycleEventCityStop,
		"#!/bin/sh\necho could not stop hub >&2\nexit 1\n")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "pack-lifecycle-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
		PackDirs:  []string{packDir},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })

	sp := gcruntime.NewFake()
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (gcruntime.Provider, error) { return sp, nil }

	var stdout, stderr lockedBuffer
	if code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdStopBody() = %d, want 0 despite the failing hook; stderr=%q", code, stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "pack hook hubpack:city-stop") || !strings.Contains(got, "could not stop hub") {
		t.Errorf("stderr = %q, want the failing hook reported with its output", got)
	}
}
