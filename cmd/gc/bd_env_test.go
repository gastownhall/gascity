package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// ── Dolt config wiring tests (issue 011) ──────────────────────────────

func TestBdRuntimeEnvIncludesDoltHost(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "mini2.hippo-tilapia.ts.net")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env := bdRuntimeEnv(cityPath)

	if got := env["GC_DOLT_HOST"]; got != "mini2.hippo-tilapia.ts.net" {
		t.Errorf("GC_DOLT_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Errorf("GC_DOLT_PORT = %q, want %q", got, "3307")
	}
}

func TestBdRuntimeEnvExternalHostSkipsLocalState(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "remote.example.com")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env := bdRuntimeEnv(cityPath)

	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Errorf("GC_DOLT_PORT = %q, want %q (should use env, not local state)", got, "3307")
	}
}

func TestCityRuntimeProcessEnvIncludesDoltHost(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "mini2.hippo-tilapia.ts.net")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env := cityRuntimeProcessEnv(cityPath)

	var foundHost, foundPort bool
	for _, entry := range env {
		if strings.HasPrefix(entry, "GC_DOLT_HOST=") {
			foundHost = true
			if got := strings.TrimPrefix(entry, "GC_DOLT_HOST="); got != "mini2.hippo-tilapia.ts.net" {
				t.Errorf("GC_DOLT_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
			}
		}
		if strings.HasPrefix(entry, "GC_DOLT_PORT=") {
			foundPort = true
			if got := strings.TrimPrefix(entry, "GC_DOLT_PORT="); got != "3307" {
				t.Errorf("GC_DOLT_PORT = %q, want %q", got, "3307")
			}
		}
	}
	if !foundHost {
		t.Error("GC_DOLT_HOST not found in cityRuntimeProcessEnv output")
	}
	if !foundPort {
		t.Error("GC_DOLT_PORT not found in cityRuntimeProcessEnv output")
	}
}

func TestMergeRuntimeEnvIncludesDoltHost(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"GC_DOLT_HOST=old-host",
	}
	overrides := map[string]string{
		"GC_DOLT_HOST": "new-host.example.com",
	}
	result := mergeRuntimeEnv(parent, overrides)

	var count int
	for _, entry := range result {
		if strings.HasPrefix(entry, "GC_DOLT_HOST=") {
			count++
			if got := strings.TrimPrefix(entry, "GC_DOLT_HOST="); got != "new-host.example.com" {
				t.Errorf("GC_DOLT_HOST = %q, want %q", got, "new-host.example.com")
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 GC_DOLT_HOST entry, got %d", count)
	}
}

func TestBdRuntimeEnvLocalHostNoHostKey(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")

	cityPath := t.TempDir()
	env := bdRuntimeEnv(cityPath)

	if _, ok := env["GC_DOLT_HOST"]; ok {
		t.Error("GC_DOLT_HOST should not be present when not configured")
	}
}

func TestOpenStoreAtForCityUsesExplicitCityForExternalRig(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	externalRig := filepath.Join(t.TempDir(), "test-external")
	if err := os.MkdirAll(externalRig, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "file")

	store, err := openStoreAtForCity(externalRig, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "external rig bead", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cityStore, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if _, err := cityStore.Get(created.ID); err != nil {
		t.Fatalf("city store should see created bead %s: %v", created.ID, err)
	}
}

func TestMergeRuntimeEnvReplacesInheritedRuntimeKeys(t *testing.T) {
	env := mergeRuntimeEnv([]string{
		"BEADS_DIR=/rig/.beads",
		"PATH=/bin",
		"GC_CITY_PATH=/wrong",
		"GC_DOLT_PORT=9999",
		"GC_PACK_STATE_DIR=/wrong/.gc/runtime/packs/dolt",
		"GC_RIG=demo",
		"GC_RIG_ROOT=/rig",
	}, map[string]string{
		"GC_CITY_PATH": "/city",
		"GC_DOLT_PORT": "31364",
	})

	got := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}

	if got["GC_CITY_PATH"] != "/city" {
		t.Fatalf("GC_CITY_PATH = %q, want %q", got["GC_CITY_PATH"], "/city")
	}
	if got["GC_DOLT_PORT"] != "31364" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got["GC_DOLT_PORT"], "31364")
	}
	if _, ok := got["BEADS_DIR"]; ok {
		t.Fatalf("BEADS_DIR should be removed, env = %#v", got)
	}
	if _, ok := got["GC_PACK_STATE_DIR"]; ok {
		t.Fatalf("GC_PACK_STATE_DIR should be removed, env = %#v", got)
	}
	if _, ok := got["GC_RIG"]; ok {
		t.Fatalf("GC_RIG should be removed, env = %#v", got)
	}
	if _, ok := got["GC_RIG_ROOT"]; ok {
		t.Fatalf("GC_RIG_ROOT should be removed, env = %#v", got)
	}
}

func TestBdCommandRunnerForCityPinsCityStoreEnv(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("BEADS_DIR", "/rig/.beads")
	t.Setenv("GC_RIG", "demo-rig")
	t.Setenv("GC_RIG_ROOT", "/rig")

	runner := bdCommandRunnerForCity(cityDir)
	out, err := runner(cityDir, "sh", "-c", `printf '%s\n%s\n%s\n%s\n' "$GC_CITY_PATH" "$BEADS_DIR" "$GC_RIG" "$GC_RIG_ROOT"`)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(string(out), "\n")
	if len(lines) != 5 {
		t.Fatalf("lines = %q, want 5 lines including trailing newline", string(out))
	}
	lines = lines[:4]
	if len(lines) != 4 {
		t.Fatalf("lines = %q, want 4 lines", string(out))
	}
	if lines[0] != cityDir {
		t.Fatalf("GC_CITY_PATH = %q, want %q", lines[0], cityDir)
	}
	if lines[1] != filepath.Join(cityDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", lines[1], filepath.Join(cityDir, ".beads"))
	}
	if lines[2] != "" {
		t.Fatalf("GC_RIG = %q, want empty", lines[2])
	}
	if lines[3] != "" {
		t.Fatalf("GC_RIG_ROOT = %q, want empty", lines[3])
	}
}

// BUG: PR #201 — bdStoreForRig() does not exist. All bd operations use
// bdStoreForCity() which returns a store rooted at the city level, not the
// rig level. For rig-scoped bead IDs, the city-level store cannot resolve
// them because it looks in the city's .beads directory, not the rig's.
//
// This test demonstrates that:
// 1. bdStoreForRig is needed but does not exist (only bdStoreForCity exists)
// 2. bdRuntimeEnv sets BEADS_DIR to the city's .beads, not a rig's
// 3. bdCommandRunnerForCity always pins BEADS_DIR to cityDir/.beads
func TestBdStoreForRig_DoesNotExist(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a rig directory — a separate repository outside the city.
	rigDir := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// bdRuntimeEnv always sets BEADS_DIR to cityDir/.beads.
	// A rig-scoped agent needs BEADS_DIR=rigDir/.beads, but no
	// bdStoreForRig() exists to produce that.
	env := bdRuntimeEnv(cityDir)
	beadsDir := env["BEADS_DIR"]
	wantCityBeads := filepath.Join(cityDir, ".beads")
	if beadsDir != wantCityBeads {
		t.Errorf("BEADS_DIR = %q, want %q (city-level)", beadsDir, wantCityBeads)
	}
	rigBeadsDir := filepath.Join(rigDir, ".beads")
	if beadsDir == rigBeadsDir {
		t.Error("BEADS_DIR unexpectedly points to rig — bdStoreForRig may have been added")
	}

	// bdCommandRunnerForCity pins BEADS_DIR to the RUNNER's dir arg (not the
	// rig). This is the command runner used by bdStoreForCity. It always
	// constructs env with cityDir context, never rig-specific context.
	runner := bdCommandRunnerForCity(cityDir)

	// Run a command in the rig directory to see what BEADS_DIR is set to.
	out, err := runner(rigDir, "sh", "-c", `printf '%s' "$BEADS_DIR"`)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}

	// BEADS_DIR is pinned to rigDir/.beads (the runner overrides per-call dir),
	// but GC_RIG and GC_RIG_ROOT are always empty — no rig context is injected.
	gotBeadsDir := string(out)
	wantRunnerBeads := filepath.Join(rigDir, ".beads")
	if gotBeadsDir != wantRunnerBeads {
		t.Errorf("runner BEADS_DIR = %q, want %q", gotBeadsDir, wantRunnerBeads)
	}

	// Verify GC_RIG is empty — the runner does not know which rig it serves.
	rigOut, err := runner(rigDir, "sh", "-c", `printf '%s' "$GC_RIG"`)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if string(rigOut) != "" {
		t.Errorf("GC_RIG = %q, want empty (no rig context in bdCommandRunnerForCity)", string(rigOut))
	}

	// PR #201 adds bdStoreForRig which opens a store at the rig directory
	// with rig-level Dolt config. Verify it returns a store pointed at the
	// rig path, not the city path. Also verify bdRuntimeEnvForRig injects
	// rig-level Dolt host/port when configured.
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:     "myrig",
			Path:     rigDir,
			DoltHost: "rig-host",
			DoltPort: "3307",
		}},
	}

	// bdRuntimeEnvForRig should inject rig-level Dolt config.
	rigEnv := bdRuntimeEnvForRig(cityDir, cfg, rigDir)
	if rigEnv["BEADS_DOLT_HOST"] != "rig-host" {
		t.Errorf("BEADS_DOLT_HOST = %q, want %q", rigEnv["BEADS_DOLT_HOST"], "rig-host")
	}
	if rigEnv["BEADS_DOLT_PORT"] != "3307" {
		t.Errorf("BEADS_DOLT_PORT = %q, want %q", rigEnv["BEADS_DOLT_PORT"], "3307")
	}
	// BEADS_DIR should be cleared so bd discovers .beads from rig cwd.
	if _, hasBeadsDir := rigEnv["BEADS_DIR"]; hasBeadsDir {
		t.Error("BEADS_DIR should be cleared for rig-level routing")
	}
}
