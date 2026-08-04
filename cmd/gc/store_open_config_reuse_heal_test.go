package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// newHealTestCity writes a minimal bd-provider city.
func newHealTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	toml := "name = \"heal\"\nprefix = \"hl\"\n\n[beads]\nprovider = \"bd\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
	return cityPath
}

// Loading the city config is not a pure read: it runs the builtin-cache
// readiness pass that rehydrates an evicted or corrupted cache. Opening a
// store with a caller-supplied config skips that load, and must not thereby
// skip the readiness pass for a city this process has never readied.
//
// Every caller on the bd path supplies a config that was loaded — and so
// healed — earlier in the same process. This pins the general shape instead:
// a config that arrived without a readiness pass still gets one.
func TestSuppliedConfigStillHealsACityThisProcessNeverReadied(t *testing.T) {
	clearGCEnv(t) // isolated GC_HOME so the heal never touches the shared test cache
	cityPath := newHealTestCity(t)

	// A config loaded deliberately without the readiness pass, standing in for
	// any future caller that hands one to a store open.
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loading config without the readiness pass: %v", err)
	}
	if builtinRuntimeReadied(cityPath) {
		t.Fatal("city reports a completed readiness pass before any heal ran")
	}

	// The open itself may fail in this bare city; the readiness pass is what
	// this pins, and it runs before the store is touched.
	_, _ = openStoreResultAtForCityWithConfig(
		filepath.Join(cityPath, ".beads"), cityPath, cfg, gate.ModeUnset, false, false)

	if !builtinRuntimeReadied(cityPath) {
		t.Fatal("a store open handed a config skipped the builtin readiness pass; " +
			"an evicted or corrupted cache would go unrepaired")
	}
}

// A city already readied in this process needs no second pass: the readiness
// pass ran at the config load the caller is reusing, and re-running it is the
// entire cost the reuse exists to avoid (the pass, not the parse, is ~99% of
// a config load). This pins that tradeoff so it is visible rather than
// implied.
func TestSuppliedConfigSkipsTheReadinessPassForAnAlreadyReadiedCity(t *testing.T) {
	clearGCEnv(t)
	cityPath := newHealTestCity(t)

	materializeBuiltinPacksForTest(t, cityPath)
	if !builtinRuntimeReadied(cityPath) {
		t.Fatal("city does not report a completed readiness pass after a full readiness pass")
	}

	// Corrupt exactly what TestEnsureBuiltinRuntimeAssetsRehydratesCorruptedCache
	// corrupts. A re-run of the pass would restore it.
	target := bundledGcBeadsBdScriptForTest(t)
	const corrupted = "#!/bin/sh\necho corrupted\n"
	if err := os.WriteFile(target, []byte(corrupted), 0o755); err != nil {
		t.Fatalf("corrupting cached script: %v", err)
	}

	if err := ensureBuiltinRuntimeAssetsForSuppliedConfig(cityPath, io.Discard); err != nil {
		t.Fatalf("ensureBuiltinRuntimeAssetsForSuppliedConfig: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(script): %v", err)
	}
	if string(got) != corrupted {
		t.Fatal("the readiness pass re-ran for a city already readied in this process; " +
			"that is the cost the config reuse exists to avoid")
	}
}

// The guard must not report readiness for a city whose pass ended degraded —
// only a fully successful pass licenses skipping the next one.
func TestBuiltinRuntimeReadiedIsFalseBeforeAnyPass(t *testing.T) {
	clearGCEnv(t)
	cityPath := newHealTestCity(t)

	if builtinRuntimeReadied(cityPath) {
		t.Fatal("a city with no readiness pass reports ready")
	}
	if builtinRuntimeReadied(filepath.Join(cityPath, "nonexistent")) {
		t.Fatal("an unknown city path reports ready")
	}
}
