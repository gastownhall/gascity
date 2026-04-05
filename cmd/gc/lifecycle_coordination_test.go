package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// writeSpyScript creates a shell script that logs operations to a file and
// recreates .beads/ on init (simulating bd init wiping hooks). Returns the
// script path.
func writeSpyScript(t *testing.T, logFile string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "spy-beads.sh")

	// The spy logs "op arg1 arg2 ..." to logFile, one line per call.
	// For "init" operations, it also creates .beads/ in the target dir
	// (simulating bd init creating the directory, which wipes hooks).
	content := `#!/bin/sh
echo "$@" >> "` + logFile + `"
case "$1" in
  init)
    # Simulate bd init: create .beads/ (may wipe existing hooks)
    mkdir -p "$2/.beads"
    ;;
esac
exit 0
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// readOpLog reads the spy script's operation log and returns the lines.
func readOpLog(t *testing.T, logFile string) []string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// assertHooksExist checks that all bead hooks exist at the given directory.
func assertHooksExist(t *testing.T, dir, context string) {
	t.Helper()
	for _, hook := range []string{"on_create", "on_close", "on_update"} {
		path := filepath.Join(dir, ".beads", "hooks", hook)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("hook %s missing at %s (%s): %v", hook, dir, context, err)
		}
	}
}

// testCityConfig creates a minimal config.City with the given rigs.
func testCityConfig(cityName string, rigs []config.Rig) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: cityName},
		Rigs:      rigs,
	}
}

// TestLifecycleCoordination_InitRigAddStart exercises the consolidated
// lifecycle functions using GC_BEADS=exec:<spy> to verify ordering and
// hook survival across gc init → gc rig add → gc start.
func TestLifecycleCoordination_InitRigAddStart(t *testing.T) {
	cityPath := t.TempDir()
	cityName := "testcity"
	rigPath := filepath.Join(cityPath, "rigs", "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \""+cityName+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	// Phase 1: gc init — initDirIfReady for city root.
	prefix := "tc"
	deferred, err := initDirIfReady(cityPath, cityPath, prefix)
	if err != nil {
		t.Fatalf("initDirIfReady (city): %v", err)
	}
	if deferred {
		t.Fatal("expected exec: provider not to defer")
	}

	ops := readOpLog(t, logFile)
	// probe + start + init
	if len(ops) != 3 {
		t.Fatalf("expected 3 ops after city init, got %d: %v", len(ops), ops)
	}
	if !strings.HasPrefix(ops[0], "probe") {
		t.Fatalf("expected probe first, got: %s", ops[0])
	}
	if !strings.HasPrefix(ops[1], "start") {
		t.Fatalf("expected start second, got: %s", ops[1])
	}
	if !strings.HasPrefix(ops[2], "init "+cityPath) {
		t.Fatalf("expected init op for city, got: %s", ops[2])
	}
	assertHooksExist(t, cityPath, "after city init")

	// Phase 2: gc rig add — initDirIfReady for rig.
	rigPrefix := "mr"
	deferred, err = initDirIfReady(cityPath, rigPath, rigPrefix)
	if err != nil {
		t.Fatalf("initDirIfReady (rig): %v", err)
	}
	if deferred {
		t.Fatal("expected exec: provider not to defer")
	}

	ops = readOpLog(t, logFile)
	// +probe + start + init (6 total)
	if len(ops) != 6 {
		t.Fatalf("expected 6 ops after rig add, got %d: %v", len(ops), ops)
	}
	if !strings.HasPrefix(ops[5], "init "+rigPath) {
		t.Fatalf("expected init op for rig, got: %s", ops[5])
	}
	assertHooksExist(t, rigPath, "after rig add")

	// Phase 3: Simulate hook wipe (bd init recreates .beads/).
	if err := os.RemoveAll(filepath.Join(cityPath, ".beads", "hooks")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(rigPath, ".beads", "hooks")); err != nil {
		t.Fatal(err)
	}

	// Phase 4: gc start — startBeadsLifecycle reinstalls everything.
	cfg := testCityConfig(cityName, []config.Rig{
		{Name: "myrig", Path: rigPath, Prefix: rigPrefix},
	})
	if err := startBeadsLifecycle(cityPath, cityName, cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}

	ops = readOpLog(t, logFile)
	// +start + init(city) + init(rig) = 9 total
	if len(ops) != 9 {
		t.Fatalf("expected 9 ops total, got %d: %v", len(ops), ops)
	}

	// Verify hooks reinstalled at both paths after start.
	assertHooksExist(t, cityPath, "after start")
	assertHooksExist(t, rigPath, "after start")
}

// TestLifecycleCoordination_StartOrder verifies that start precedes any
// init call when using startBeadsLifecycle. This catches bugs where init
// runs before the backing service is ready.
func TestLifecycleCoordination_StartOrder(t *testing.T) {
	cityPath := t.TempDir()
	cityName := "ordertest"
	rigPath := filepath.Join(cityPath, "rigs", "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \""+cityName+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	cfg := testCityConfig(cityName, []config.Rig{
		{Name: "myrig", Path: rigPath, Prefix: "mr"},
	})
	if err := startBeadsLifecycle(cityPath, cityName, cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}

	ops := readOpLog(t, logFile)
	if len(ops) < 2 {
		t.Fatalf("expected at least 2 ops, got %d: %v", len(ops), ops)
	}

	// First op must be start.
	if !strings.HasPrefix(ops[0], "start") {
		t.Fatalf("first op should be start, got: %s", ops[0])
	}

	// All subsequent ops must be init.
	for i := 1; i < len(ops); i++ {
		if !strings.HasPrefix(ops[i], "init ") {
			t.Fatalf("op[%d] should be init, got: %s", i, ops[i])
		}
	}
}

// TestLifecycleCoordination_StopOrder verifies that stop is called
// during gc stop via shutdownBeadsProvider.
func TestLifecycleCoordination_StopOrder(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"stoptest\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	if err := shutdownBeadsProvider(cityPath); err != nil {
		t.Fatalf("shutdownBeadsProvider: %v", err)
	}

	ops := readOpLog(t, logFile)
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %d: %v", len(ops), ops)
	}
	if !strings.HasPrefix(ops[0], "stop") {
		t.Fatalf("expected stop op, got: %s", ops[0])
	}
}

// TestLifecycleCoordination_InitDirIfReady_BdDeferred verifies that the bd
// provider returns deferred=true (Dolt isn't running during gc init).
// With the exec: mapping, bd → gc-beads-bd script → probe exits 2 (GC_DOLT=skip)
// → deferred=true.
func TestLifecycleCoordination_InitDirIfReady_BdDeferred(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	MaterializeBeadsBdScript(dir) //nolint:errcheck
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	deferred, err := initDirIfReady(dir, dir, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !deferred {
		t.Fatal("expected bd provider to defer init")
	}

	configData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read deferred config: %v", err)
	}
	configText := string(configData)
	if !strings.Contains(configText, "issue_prefix: test") {
		t.Fatalf("deferred config missing issue_prefix:\n%s", configText)
	}
	if !strings.Contains(configText, "dolt.auto-start: false") {
		t.Fatalf("deferred config missing dolt.auto-start guard:\n%s", configText)
	}

	metaData, err := os.ReadFile(filepath.Join(dir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read deferred metadata: %v", err)
	}
	metaText := string(metaData)
	for _, needle := range []string{`"backend": "dolt"`, `"database": "dolt"`, `"dolt_mode": "server"`, `"dolt_database": "test"`} {
		if !strings.Contains(metaText, needle) {
			t.Fatalf("deferred metadata missing %s:\n%s", needle, metaText)
		}
	}
}
