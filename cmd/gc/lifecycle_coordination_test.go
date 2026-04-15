package main

import (
	"encoding/json"
	"fmt"
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

func assertBeadsDirPerm(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, ".beads"))
	if err != nil {
		t.Fatalf("stat .beads at %s: %v", dir, err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf(".beads perm at %s = %o, want 0700", dir, perm)
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
	assertBeadsDirPerm(t, cityPath)

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
	assertBeadsDirPerm(t, rigPath)

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
	assertBeadsDirPerm(t, cityPath)
	assertBeadsDirPerm(t, rigPath)
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
	MaterializeBuiltinPacks(dir) //nolint:errcheck
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
	if !strings.Contains(configText, "gc.endpoint_origin: managed_city") {
		t.Fatalf("deferred config missing managed origin:\n%s", configText)
	}
	if !strings.Contains(configText, "gc.endpoint_status: verified") {
		t.Fatalf("deferred config missing endpoint status:\n%s", configText)
	}
	if !strings.Contains(configText, "dolt.auto-start: false") {
		t.Fatalf("deferred config missing dolt.auto-start guard:\n%s", configText)
	}

	metaData, err := os.ReadFile(filepath.Join(dir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read deferred metadata: %v", err)
	}
	metaText := string(metaData)
	for _, needle := range []string{`"backend": "dolt"`, `"database": "dolt"`, `"dolt_mode": "server"`, `"dolt_database": "hq"`} {
		if !strings.Contains(metaText, needle) {
			t.Fatalf("deferred metadata missing %s:\n%s", needle, metaText)
		}
	}
	for _, forbidden := range []string{"dolt_host", "dolt_user", "dolt_password", "dolt_server_host", "dolt_server_port", "dolt_server_user", "dolt_port"} {
		if strings.Contains(metaText, forbidden) {
			t.Fatalf("deferred metadata should scrub deprecated key %s:\n%s", forbidden, metaText)
		}
	}
}

func TestLifecycleCoordination_InitDirIfReady_BdDeferredPreservesExistingDoltDatabaseWhenCanonicalUnknown(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(`{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"gascity"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	MaterializeBuiltinPacks(dir) //nolint:errcheck
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	deferred, err := initDirIfReady(dir, dir, "gc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !deferred {
		t.Fatal("expected bd provider to defer init")
	}

	metaData, err := os.ReadFile(filepath.Join(dir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if got := strings.TrimSpace(fmt.Sprint(meta["dolt_database"])); got != "gascity" {
		t.Fatalf("dolt_database = %q, want %q", got, "gascity")
	}
}

func TestSeedDeferredManagedBeadsUsesExplicitDoltDatabase(t *testing.T) {
	dir := t.TempDir()

	seedDeferredManagedBeads(dir, dir, "gc", "gascity")

	configData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if got := string(configData); !strings.Contains(got, "issue_prefix: gc") {
		t.Fatalf("config should keep the bead prefix, got:\n%s", got)
	}
	if got := string(configData); !strings.Contains(got, "gc.endpoint_origin: managed_city") {
		t.Fatalf("config should set managed origin, got:\n%s", got)
	}

	metaData, err := os.ReadFile(filepath.Join(dir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	metaText := string(metaData)
	for _, needle := range []string{`"backend": "dolt"`, `"database": "dolt"`, `"dolt_mode": "server"`, `"dolt_database": "gascity"`} {
		if !strings.Contains(metaText, needle) {
			t.Fatalf("metadata missing %s:\n%s", needle, metaText)
		}
	}
	for _, forbidden := range []string{"dolt_host", "dolt_user", "dolt_password", "dolt_server_host", "dolt_server_port", "dolt_server_user", "dolt_port"} {
		if strings.Contains(metaText, forbidden) {
			t.Fatalf("metadata should scrub deprecated key %s:\n%s", forbidden, metaText)
		}
	}
}

func TestSeedDeferredManagedBeadsNormalizesMalformedExistingConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("issue-prefix: stale\ndolt.auto-start: true\ndolt_server_port: 3307\n: not yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(dir, dir, "gc", "hq")

	configData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg := string(configData)
	for _, needle := range []string{
		"issue_prefix: gc",
		"issue-prefix: gc",
		"dolt.auto-start: false",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		": not yaml",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("config missing %q after malformed deferred normalization:\n%s", needle, cfg)
		}
	}
	if strings.Contains(cfg, "dolt_server_port") {
		t.Fatalf("config should scrub deprecated port key after malformed deferred normalization:\n%s", cfg)
	}
}

func TestSeedDeferredManagedBeadsTreatsSymlinkedCityRootAsManaged(t *testing.T) {
	root := t.TempDir()
	cityDir := filepath.Join(root, "city")
	cityLink := filepath.Join(root, "city-link")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cityDir, cityLink); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	seedDeferredManagedBeads(cityLink, cityDir, "gc", "hq")

	configData, err := os.ReadFile(filepath.Join(cityDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if got := string(configData); !strings.Contains(got, "gc.endpoint_origin: managed_city") {
		t.Fatalf("config should keep managed origin for symlinked city root, got:\n%s", got)
	}
}

func TestSeedDeferredManagedBeadsIgnoresEnvOnlyExternalOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_DOLT_HOST", "env-db.example.com")
	t.Setenv("GC_DOLT_PORT", "3307")

	seedDeferredManagedBeads(dir, dir, "gc", "hq")

	configData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg := string(configData)
	for _, needle := range []string{
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("config missing %q:\n%s", needle, cfg)
		}
	}
	for _, forbidden := range []string{"env-db.example.com", "dolt.host:", "dolt.port:"} {
		if strings.Contains(cfg, forbidden) {
			t.Fatalf("config should not persist env-only endpoint %q:\n%s", forbidden, cfg)
		}
	}
}

func TestSeedDeferredManagedBeadsPreservesLegacyExternalCityUser(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: stale
dolt.host: legacy-db.example.com
dolt.port: 3307
dolt.user: city-user
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(cityDir, cityDir, "gc", "hq")

	configData, err := os.ReadFile(filepath.Join(cityDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg := string(configData)
	for _, needle := range []string{
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: unverified",
		"dolt.host: legacy-db.example.com",
		"dolt.port: 3307",
		"dolt.user: city-user",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("config missing %q:\n%s", needle, cfg)
		}
	}
}

func TestSeedDeferredManagedBeadsInheritsVerifiedExternalCityStatusForRig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: db.example.com
dolt.port: 3307
dolt.user: city-user
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(cityDir, rigDir, "fe", "fe")

	configData, err := os.ReadFile(filepath.Join(rigDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read rig config: %v", err)
	}
	cfg := string(configData)
	for _, needle := range []string{
		"gc.endpoint_origin: inherited_city",
		"gc.endpoint_status: verified",
		"dolt.host: db.example.com",
		"dolt.port: 3307",
		"dolt.user: city-user",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("config missing %q:\n%s", needle, cfg)
		}
	}
}

func TestSeedDeferredManagedBeadsUsesRegisteredExternalCityTarget(t *testing.T) {
	cityDir := t.TempDir()
	cityDoltConfigs.Store(cityDir, config.DoltConfig{Host: "db.example.com", Port: 3307})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityDir) })

	seedDeferredManagedBeads(cityDir, cityDir, "gc", "hq")

	configData, err := os.ReadFile(filepath.Join(cityDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg := string(configData)
	for _, needle := range []string{
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: unverified",
		"dolt.host: db.example.com",
		"dolt.port: 3307",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("config missing %q:\n%s", needle, cfg)
		}
	}
}

func TestSeedDeferredManagedBeadsUsesCompatCityExternalBeforeStartup(t *testing.T) {
	cityDir := t.TempDir()
	cityToml := `[workspace]
name = "test-city"
prefix = "gc"

[dolt]
host = "compat-db.example.com"
port = 3307
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(cityDir, cityDir, "gc", "hq")

	configData, err := os.ReadFile(filepath.Join(cityDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg := string(configData)
	for _, needle := range []string{
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: unverified",
		"dolt.host: compat-db.example.com",
		"dolt.port: 3307",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("config missing %q:\n%s", needle, cfg)
		}
	}
}

func TestSeedDeferredManagedBeadsUsesCompatInheritedRigEndpointBeforeStartup(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"
prefix = "gc"

[dolt]
host = "compat-db.example.com"
port = 3307

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(cityDir, rigDir, "fe", "fe")

	configData, err := os.ReadFile(filepath.Join(rigDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read rig config: %v", err)
	}
	cfg := string(configData)
	for _, needle := range []string{
		"gc.endpoint_origin: inherited_city",
		"gc.endpoint_status: unverified",
		"dolt.host: compat-db.example.com",
		"dolt.port: 3307",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("config missing %q:\n%s", needle, cfg)
		}
	}
}

func TestSeedDeferredManagedBeadsUsesCompatExplicitRigEndpointBeforeStartup(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"
prefix = "gc"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
dolt_host = "rig-db.example.com"
dolt_port = "4407"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(cityDir, rigDir, "fe", "fe")

	configData, err := os.ReadFile(filepath.Join(rigDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read rig config: %v", err)
	}
	cfg := string(configData)
	for _, needle := range []string{
		"gc.endpoint_origin: explicit",
		"gc.endpoint_status: unverified",
		"dolt.host: rig-db.example.com",
		"dolt.port: 4407",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("config missing %q:\n%s", needle, cfg)
		}
	}
}

func TestSeedDeferredManagedBeadsPreservesExplicitRigConfig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: fe
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.host: rig-db.example.com
dolt.port: 4406
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(cityDir, rigDir, "fe", "fe")

	configData, err := os.ReadFile(filepath.Join(rigDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg := string(configData)
	for _, needle := range []string{
		"gc.endpoint_origin: explicit",
		"gc.endpoint_status: verified",
		"dolt.host: rig-db.example.com",
		"dolt.port: 4406",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("config missing %q:\n%s", needle, cfg)
		}
	}
}

func TestSeedDeferredManagedBeadsPreservesExistingDoltDatabaseWhenCanonicalUnknown(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(`{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"gascity"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(dir, dir, "gc", "")

	metaData, err := os.ReadFile(filepath.Join(dir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if got := strings.TrimSpace(fmt.Sprint(meta["dolt_database"])); got != "gascity" {
		t.Fatalf("dolt_database = %q, want %q", got, "gascity")
	}
}

// TestSeedDeferredManagedBeadsCreatesDirWith0700 asserts that fresh .beads
// directories created during deferred init satisfy bd's recommended 0700
// permission. Wider perms cause bd to emit a warning on every call, which
// spams agent pod output and is treated as a hard failure by the
// controller's collectAssignedWorkBeads stderr-as-error path (hl-39km).
func TestSeedDeferredManagedBeadsCreatesDirWith0700(t *testing.T) {
	dir := t.TempDir()

	seedDeferredManagedBeads(dir, dir, "gc", "test")

	info, err := os.Stat(filepath.Join(dir, ".beads"))
	if err != nil {
		t.Fatalf("stat .beads: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf(".beads perm = %o, want 0700", perm)
	}
}

// TestSeedDeferredManagedBeadsTightensExistingDir asserts that pre-existing
// .beads directories with looser permissions are tightened on next call.
// Required because persistent volumes carry directories created by older
// gascity versions that used 0o755.
func TestSeedDeferredManagedBeadsTightensExistingDir(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Force 0755 explicitly — the test process umask may have reduced it.
	if err := os.Chmod(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	seedDeferredManagedBeads(dir, dir, "gc", "test")

	info, err := os.Stat(beadsDir)
	if err != nil {
		t.Fatalf("stat .beads: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf(".beads perm = %o, want 0700", perm)
	}
}
