package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"gopkg.in/yaml.v3"
)

func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		_ = listener.Close()
	}()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr = %T, want *net.TCPAddr", listener.Addr())
	}
	return strconv.Itoa(addr.Port)
}

// TestEnsureBeadsProvider_file verifies that file provider is a no-op.
func TestEnsureBeadsProvider_file(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	if err := ensureBeadsProvider(t.TempDir()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestEnsureBeadsProvider_exec calls script with ensure-ready, exit 2 = no-op.
func TestEnsureBeadsProvider_exec(t *testing.T) {
	script := writeTestScript(t, "ensure-ready", 2, "")
	t.Setenv("GC_BEADS", "exec:"+script)
	if err := ensureBeadsProvider(t.TempDir()); err != nil {
		t.Fatalf("expected nil for exit 2, got %v", err)
	}
}

func TestProviderLifecycleProcessEnvProjectsCanonicalDoltPaths(t *testing.T) {
	cityPath := t.TempDir()
	wantCityPath := normalizePathForCompare(cityPath)
	t.Setenv("GC_PACK_STATE_DIR", "/tmp/wrong-pack")
	t.Setenv("GC_DOLT_DATA_DIR", "/tmp/wrong-data")
	t.Setenv("GC_DOLT_LOG_FILE", "/tmp/wrong-log")
	t.Setenv("GC_DOLT_STATE_FILE", "/tmp/wrong-state")
	t.Setenv("GC_DOLT_PID_FILE", "/tmp/wrong-pid")
	t.Setenv("GC_DOLT_LOCK_FILE", "/tmp/wrong-lock")
	t.Setenv("GC_DOLT_CONFIG_FILE", "/tmp/wrong-config")

	envEntries := providerLifecycleProcessEnv(cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}

	packStateDir := citylayout.PackStateDir(wantCityPath, "dolt")
	want := map[string]string{
		"GC_PACK_STATE_DIR":   packStateDir,
		"GC_DOLT_DATA_DIR":    filepath.Join(wantCityPath, ".beads", "dolt"),
		"GC_DOLT_LOG_FILE":    filepath.Join(packStateDir, "dolt.log"),
		"GC_DOLT_STATE_FILE":  filepath.Join(packStateDir, "dolt-provider-state.json"),
		"GC_DOLT_PID_FILE":    filepath.Join(packStateDir, "dolt.pid"),
		"GC_DOLT_LOCK_FILE":   filepath.Join(packStateDir, "dolt.lock"),
		"GC_DOLT_CONFIG_FILE": filepath.Join(packStateDir, "dolt-config.yaml"),
	}
	for key, expected := range want {
		if got := env[key]; got != expected {
			t.Fatalf("providerLifecycleProcessEnv()[%s] = %q, want %q", key, got, expected)
		}
	}
}

func TestProviderLifecycleProcessEnvCanonicalizesSymlinkedCityPath(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(root, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skip("symlinks not supported")
	}
	aliasCity := filepath.Join(aliasParent, "bright-lights")
	realCity := filepath.Join(realParent, "bright-lights")
	if err := os.MkdirAll(realCity, 0o755); err != nil {
		t.Fatal(err)
	}
	wantCityPath := normalizePathForCompare(realCity)

	envEntries := providerLifecycleProcessEnv(aliasCity, "exec:"+gcBeadsBdScriptPath(aliasCity))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}

	packStateDir := citylayout.PackStateDir(wantCityPath, "dolt")
	want := map[string]string{
		"GC_CITY":             wantCityPath,
		"GC_CITY_PATH":        wantCityPath,
		"GC_CITY_RUNTIME_DIR": filepath.Join(wantCityPath, ".gc", "runtime"),
		"GC_PACK_STATE_DIR":   packStateDir,
		"GC_DOLT_DATA_DIR":    filepath.Join(wantCityPath, ".beads", "dolt"),
		"GC_DOLT_STATE_FILE":  filepath.Join(packStateDir, "dolt-provider-state.json"),
		"GC_DOLT_CONFIG_FILE": filepath.Join(packStateDir, "dolt-config.yaml"),
	}
	for key, expected := range want {
		if got := env[key]; got != expected {
			t.Fatalf("providerLifecycleProcessEnv()[%s] = %q, want %q", key, got, expected)
		}
	}
}

func TestProviderLifecycleProcessEnvProjectsResolvedGCBin(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_BIN", "/tmp/wrong-gc")
	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return "/opt/gc/bin/gc" }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })

	envEntries := providerLifecycleProcessEnv(cityPath, "exec:"+gcBeadsBdScriptPath(cityPath))
	env := map[string]string{}
	for _, entry := range envEntries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if got := env["GC_BIN"]; got != "/opt/gc/bin/gc" {
		t.Fatalf("providerLifecycleProcessEnv()[GC_BIN] = %q, want %q", got, "/opt/gc/bin/gc")
	}
}

func TestGcBeadsBdReadOnlyFallbackDoesNotDropProbeDatabase(t *testing.T) {
	cityPath := t.TempDir()
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	scriptData, err := os.ReadFile(gcBeadsBdScriptPath(cityPath))
	if err != nil {
		t.Fatalf("ReadFile(gc-beads-bd): %v", err)
	}
	script := string(scriptData)
	assertNoManagedDoltProbeDrop(t, "gc-beads-bd read-only fallback", script)
	if !strings.Contains(script, "CREATE TABLE IF NOT EXISTS __gc_probe.__probe") {
		t.Fatalf("gc-beads-bd read-only fallback missing qualified persistent probe table")
	}
	assertManagedDoltProbeWrites(t, "gc-beads-bd read-only fallback", script)
}

func TestGcBeadsBdInitRejectsManagedProbeDatabaseName(t *testing.T) {
	for _, dbName := range []string{managedDoltProbeDatabase, strings.ToUpper(managedDoltProbeDatabase), " " + managedDoltProbeDatabase + " "} {
		t.Run(dbName, func(t *testing.T) {
			cityPath := t.TempDir()
			scopePath := filepath.Join(cityPath, "rigs", "frontend")
			if err := os.MkdirAll(filepath.Join(scopePath, ".beads"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := MaterializeBuiltinPacks(cityPath); err != nil {
				t.Fatalf("MaterializeBuiltinPacks: %v", err)
			}
			cmd := exec.Command(gcBeadsBdScriptPath(cityPath), "init", scopePath, "fe", dbName)
			cmd.Env = sanitizedBaseEnv(
				"GC_CITY_PATH="+cityPath,
				"PATH="+os.Getenv("PATH"),
			)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("gc-beads-bd init unexpectedly accepted %s:\n%s", dbName, out)
			}
			if !strings.Contains(string(out), "reserved dolt database name: "+dbName) {
				t.Fatalf("gc-beads-bd init output = %s, want reserved database error", out)
			}
		})
	}
}

func TestEnsureCanonicalScopeMetadataRejectsManagedProbeDatabase(t *testing.T) {
	scopePath := t.TempDir()
	err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scopePath, managedDoltProbeDatabase)
	if err == nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit unexpectedly accepted %s", managedDoltProbeDatabase)
	}
	if !strings.Contains(err.Error(), "reserved pinned dolt_database") || !strings.Contains(err.Error(), "choose a different dolt_database") {
		t.Fatalf("ensureCanonicalScopeMetadataForInit error = %v, want reserved database remediation", err)
	}
}

func TestNormalizeCanonicalBdScopeFilesPreservesExistingManagedProbeDatabase(t *testing.T) {
	cityPath := t.TempDir()
	metadataPath := filepath.Join(cityPath, ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, metadataPath, contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: strings.ToUpper(managedDoltProbeDatabase),
	}); err != nil {
		t.Fatalf("EnsureCanonicalMetadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: hq\nissue-prefix: hq\ndolt.auto-start: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "dogfood-city"}}
	if err := normalizeCanonicalBdScopeFiles(cityPath, cfg); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFiles: %v", err)
	}
	got, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, metadataPath)
	if err != nil {
		t.Fatalf("ReadDoltDatabase: %v", err)
	}
	if !ok || got != strings.ToUpper(managedDoltProbeDatabase) {
		t.Fatalf("dolt_database = %q, ok=%v; want existing reserved name preserved", got, ok)
	}
}

func TestNormalizeCanonicalBdScopeFilesForInitPreservesExistingManagedProbeDatabase(t *testing.T) {
	cityPath := t.TempDir()
	metadataPath := filepath.Join(cityPath, ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, metadataPath, contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: strings.ToUpper(managedDoltProbeDatabase),
	}); err != nil {
		t.Fatalf("EnsureCanonicalMetadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\nissue-prefix: gc\ndolt.auto-start: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := normalizeCanonicalBdScopeFilesForInit(cityPath, cityPath, "gc", strings.ToUpper(managedDoltProbeDatabase)); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFilesForInit: %v", err)
	}

	got, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, metadataPath)
	if err != nil {
		t.Fatalf("ReadDoltDatabase: %v", err)
	}
	if !ok || got != strings.ToUpper(managedDoltProbeDatabase) {
		t.Fatalf("dolt_database = %q, ok=%v; want existing reserved name preserved", got, ok)
	}
}

func TestGcBeadsBdCleanupStaleLocksBoundsLsof(t *testing.T) {
	cityPath := t.TempDir()
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	scriptData, err := os.ReadFile(gcBeadsBdScriptPath(cityPath))
	if err != nil {
		t.Fatalf("ReadFile(gc-beads-bd): %v", err)
	}
	prelude, _, ok := strings.Cut(string(scriptData), "# --- Main ---")
	if !ok {
		t.Fatal("gc-beads-bd script missing main marker")
	}

	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	lockFile := filepath.Join(dataDir, "hq", ".dolt", "noms", "LOCK")
	if err := os.MkdirAll(filepath.Dir(lockFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockFile, []byte("lock\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "lsof"), []byte("#!/bin/sh\nexec sleep 2\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(lsof): %v", err)
	}
	harness := filepath.Join(t.TempDir(), "cleanup-locks.sh")
	body := prelude + fmt.Sprintf(`
DATA_DIR=%q
cleanup_stale_locks
`, dataDir)
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", harness)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_LSOF_TIMEOUT_SECONDS=0.1",
	)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("cleanup_stale_locks timed out after %s\n%s", time.Since(start), out)
	}
	if err != nil {
		t.Fatalf("cleanup_stale_locks failed: %v\n%s", err, out)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cleanup_stale_locks took %s, want bounded lsof timeout", elapsed)
	}
	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("LOCK stat err = %v, want preserved when lsof times out", err)
	}
}

func TestEnsureBeadsProviderPublishesManagedDoltRuntimeStateFromProviderState(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fr"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	providerState := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
  start)
    mkdir -p "$(dirname %q)"
    cat > %q <<'JSON'
{"running":true,"pid":%d,"port":%d,"data_dir":%q,"started_at":"2026-04-14T00:00:00Z"}
JSON
    ;;
  stop)
    ;;
esac
exit 0
`, providerState, providerState, os.Getpid(), port, filepath.Join(cityPath, ".beads", "dolt"))
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)

	if err := ensureBeadsProvider(cityPath); err != nil {
		t.Fatalf("ensureBeadsProvider: %v", err)
	}

	published, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json"))
	if err != nil {
		t.Fatalf("ReadFile(dolt-state.json): %v", err)
	}
	publishedText := string(published)
	if !strings.Contains(publishedText, fmt.Sprintf("\"port\":%d", port)) {
		t.Fatalf("published state missing port %d: %s", port, publishedText)
	}
	if !strings.Contains(publishedText, filepath.Join(cityPath, ".beads", "dolt")) {
		t.Fatalf("published state missing data_dir: %s", publishedText)
	}
	for _, dir := range []string{cityPath, rigPath} {
		data, err := os.ReadFile(filepath.Join(dir, ".beads", "dolt-server.port"))
		if err != nil {
			t.Fatalf("ReadFile(%s/.beads/dolt-server.port): %v", dir, err)
		}
		if got := strings.TrimSpace(string(data)); got != strconv.Itoa(port) {
			t.Fatalf("%s port file = %q, want %d", dir, got, port)
		}
	}
}

func TestPublishManagedDoltRuntimeStateIfOwnedPublishesForInheritedBdRigUnderFileCity(t *testing.T) {
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigPath, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "fe",
	}); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	if err := publishManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
		t.Fatalf("publishManagedDoltRuntimeStateIfOwned: %v", err)
	}

	published, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("read published dolt runtime state: %v", err)
	}
	if published.Port != port {
		t.Fatalf("published port = %d, want %d", published.Port, port)
	}
	portData, err := os.ReadFile(filepath.Join(rigPath, ".beads", "dolt-server.port"))
	if err != nil {
		t.Fatalf("ReadFile(rig dolt-server.port): %v", err)
	}
	if strings.TrimSpace(string(portData)) != strconv.Itoa(port) {
		t.Fatalf("rig dolt-server.port = %q, want %d", strings.TrimSpace(string(portData)), port)
	}
}

func TestManagedDoltLifecycleOwnedIgnoresExplicitBdRigUnderFileCity(t *testing.T) {
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
dolt_host = "db.example.com"
dolt_port = "4406"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigPath, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "fe",
	}); err != nil {
		t.Fatal(err)
	}

	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		t.Fatalf("managedDoltLifecycleOwned: %v", err)
	}
	if owned {
		t.Fatal("managed Dolt lifecycle should not be owned for explicit rig endpoint under file-backed city")
	}
}

func TestManagedDoltLifecycleOwnedReportsInvalidCityConfigForFileCity(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname =\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	owned, err := managedDoltLifecycleOwned(cityPath)
	if err == nil {
		t.Fatalf("managedDoltLifecycleOwned() err = nil, owned = %v; want config load error", owned)
	}
	if !strings.Contains(err.Error(), "load city config for managed dolt ownership") {
		t.Fatalf("managedDoltLifecycleOwned() error = %v, want ownership config context", err)
	}
}

// TestEnsureBeadsProvider_bd_skip verifies bd provider is no-op when GC_DOLT=skip.
func TestEnsureBeadsProvider_bd_skip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	MaterializeBuiltinPacks(dir) //nolint:errcheck
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	if err := ensureBeadsProvider(dir); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestEnsureBeadsProvider_bdAcceptsHealthyServerAfterStartError(t *testing.T) {
	dir := t.TempDir()
	script := gcBeadsBdScriptPath(dir)
	callLog := filepath.Join(dir, "provider.log")
	marker := filepath.Join(dir, "started")
	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcess(t, port)
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(dir), doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      port,
		DataDir:   filepath.Join(dir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("write provider state: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "#!/bin/sh\n" +
		"set -eu\n" +
		"echo \"$1\" >> \"" + callLog + "\"\n" +
		"case \"${1:-}\" in\n" +
		"  start)\n" +
		"    : > \"" + marker + "\"\n" +
		"    echo 'signal: terminated' >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"  health)\n" +
		"    [ -f \"" + marker + "\" ]\n" +
		"    ;;\n" +
		"  *)\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "bd")

	if err := ensureBeadsProvider(dir); err != nil {
		t.Fatalf("ensureBeadsProvider = %v, want nil", err)
	}

	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	got := strings.Fields(string(data))
	want := []string{"start", "health"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
}

func TestEnsureBeadsProvider_execDoesNotMaskStartErrorWithHealth(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "provider.log")
	marker := filepath.Join(dir, "started")
	script := filepath.Join(dir, "provider.sh")
	content := "#!/bin/sh\n" +
		"set -eu\n" +
		"echo \"$1\" >> \"" + callLog + "\"\n" +
		"case \"${1:-}\" in\n" +
		"  start)\n" +
		"    : > \"" + marker + "\"\n" +
		"    echo 'signal: terminated' >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"  health)\n" +
		"    [ -f \"" + marker + "\" ]\n" +
		"    ;;\n" +
		"  *)\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)

	err := ensureBeadsProvider(dir)
	if err == nil {
		t.Fatal("ensureBeadsProvider = nil, want start error")
	}
	if !strings.Contains(err.Error(), "signal: terminated") {
		t.Fatalf("ensureBeadsProvider error = %v, want start error", err)
	}

	data, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read call log: %v", readErr)
	}
	got := strings.Fields(string(data))
	want := []string{"start"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
}

func TestEnsureBeadsProvider_execDoesNotReclassifyProviderAfterStart(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "provider.log")
	marker := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	script := filepath.Join(dir, "provider.sh")
	content := "#!/bin/sh\n" +
		"set -eu\n" +
		"echo \"$1\" >> \"" + callLog + "\"\n" +
		"case \"${1:-}\" in\n" +
		"  start)\n" +
		"    : > \"" + marker + "\"\n" +
		"    i=0\n" +
		"    while [ ! -f \"" + release + "\" ]; do\n" +
		"      i=$((i + 1))\n" +
		"      [ \"$i\" -le 1000 ] || exit 42\n" +
		"      sleep 0.01\n" +
		"    done\n" +
		"    echo 'signal: terminated' >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"  health)\n" +
		"    [ -f \"" + marker + "\" ]\n" +
		"    ;;\n" +
		"  *)\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	originalProvider, hadProvider := os.LookupEnv("GC_BEADS")
	if err := os.Setenv("GC_BEADS", "exec:"+script); err != nil {
		t.Fatalf("set GC_BEADS: %v", err)
	}
	t.Cleanup(func() {
		if hadProvider {
			_ = os.Setenv("GC_BEADS", originalProvider)
			return
		}
		_ = os.Unsetenv("GC_BEADS")
	})

	releaseErr := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				releaseErr <- fmt.Errorf("provider start marker was not written")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := os.Setenv("GC_BEADS", "bd"); err != nil {
			releaseErr <- err
			return
		}
		releaseErr <- os.WriteFile(release, []byte("ok"), 0o644)
	}()

	err := ensureBeadsProvider(dir)
	if releaseErr := <-releaseErr; releaseErr != nil {
		t.Fatalf("release provider script: %v", releaseErr)
	}
	if err == nil {
		t.Fatal("ensureBeadsProvider = nil, want original start error")
	}
	if !strings.Contains(err.Error(), "signal: terminated") {
		t.Fatalf("ensureBeadsProvider error = %v, want start error", err)
	}

	data, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read call log: %v", readErr)
	}
	got := strings.Fields(string(data))
	want := []string{"start"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
}

// TestShutdownBeadsProvider_file verifies that file provider is a no-op.
func TestShutdownBeadsProvider_file(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	if err := shutdownBeadsProvider(t.TempDir()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestShutdownBeadsProvider_exec calls script with shutdown, exit 2 = no-op.
func TestShutdownBeadsProvider_exec(t *testing.T) {
	script := writeTestScript(t, "shutdown", 2, "")
	t.Setenv("GC_BEADS", "exec:"+script)
	if err := shutdownBeadsProvider(t.TempDir()); err != nil {
		t.Fatalf("expected nil for exit 2, got %v", err)
	}
}

// TestShutdownBeadsProvider_bd_skip verifies bd provider is no-op when GC_DOLT=skip.
func TestShutdownBeadsProvider_bd_skip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	MaterializeBuiltinPacks(dir) //nolint:errcheck
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	if err := shutdownBeadsProvider(dir); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestShutdownBeadsProviderBdSkipClearsPublishedRuntimeState(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	MaterializeBuiltinPacks(dir) //nolint:errcheck
	if err := writeDoltState(dir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      33123,
		DataDir:   filepath.Join(dir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltState: %v", err)
	}
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	if err := shutdownBeadsProvider(dir); err != nil {
		t.Fatalf("shutdownBeadsProvider() error = %v", err)
	}
	if _, err := os.Stat(managedDoltStatePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("published dolt runtime state still present, stat err = %v", err)
	}
}

func TestCurrentDoltPortPrefersRuntimeState(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "dolt-server.port"), []byte("38427\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := currentDoltPort(cityDir)
	if got != fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port) {
		t.Fatalf("currentDoltPort() = %q, want %d", got, ln.Addr().(*net.TCPAddr).Port)
	}

	data, err := os.ReadFile(filepath.Join(cityDir, ".beads", "dolt-server.port"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port) {
		t.Fatalf("city port file = %q, want %d", strings.TrimSpace(string(data)), ln.Addr().(*net.TCPAddr).Port)
	}
}

func TestCurrentDoltPortIgnoresReachablePortFileWithoutManagedState(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "dolt-server.port"), []byte(fmt.Sprintf("%d\n", port)), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := currentDoltPort(cityDir); got != "" {
		t.Fatalf("currentDoltPort() = %q, want empty when runtime state is missing", got)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("reachable compatibility port file should be removed, stat err = %v", err)
	}
}

func TestCurrentManagedDoltPortIgnoresNonCanonicalPackState(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })
	payload := fmt.Sprintf(`{"running":true,"pid":%d,"port":%d,"data_dir":%q,"started_at":%q}`,
		os.Getpid(), ln.Addr().(*net.TCPAddr).Port, filepath.Join(cityDir, ".beads", "dolt"), time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "runtime", "packs", "extra", "dolt-state.json"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := currentManagedDoltPort(cityDir); got != "" {
		t.Fatalf("currentManagedDoltPort() = %q, want empty when only non-canonical pack state exists", got)
	}
}

func TestCurrentManagedDoltPortUsesCanonicalPackStateOnly(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := listenOnRandomPort(t)
	t.Cleanup(func() { _ = stale.Close() })
	canonical := listenOnRandomPort(t)
	t.Cleanup(func() { _ = canonical.Close() })
	stalePayload := fmt.Sprintf(`{"running":true,"pid":%d,"port":%d,"data_dir":%q,"started_at":%q}`,
		os.Getpid(), stale.Addr().(*net.TCPAddr).Port, filepath.Join(cityDir, ".beads", "dolt"), time.Now().UTC().Add(time.Minute).Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "runtime", "packs", "extra", "dolt-state.json"), []byte(stalePayload), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      canonical.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if got := currentManagedDoltPort(cityDir); got != fmt.Sprintf("%d", canonical.Addr().(*net.TCPAddr).Port) {
		t.Fatalf("currentManagedDoltPort() = %q, want canonical pack port %d", got, canonical.Addr().(*net.TCPAddr).Port)
	}
}

//nolint:unparam // test helper keeps signature aligned with call sites under comparison
func requireSyncConfiguredDoltPortFiles(t *testing.T, cityPath, provider string, cityDolt config.DoltConfig, cityPrefix string, rigs []config.Rig) {
	t.Helper()
	_ = provider
	if err := syncConfiguredDoltPortFiles(cityPath, cityDolt, cityPrefix, rigs); err != nil {
		t.Fatal(err)
	}
}

func TestSyncConfiguredDoltPortFilesWritesArbitraryRigPaths(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "foobar")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })

	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("dolt.auto-start: true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{}, "gc", []config.Rig{{Name: "foobar", Path: rigDir}})

	for _, tc := range []struct {
		dir    string
		origin string
	}{
		{dir: cityDir, origin: "managed_city"},
		{dir: rigDir, origin: "inherited_city"},
	} {
		data, err := os.ReadFile(filepath.Join(tc.dir, ".beads", "dolt-server.port"))
		if err != nil {
			t.Fatalf("read port file for %s: %v", tc.dir, err)
		}
		if strings.TrimSpace(string(data)) != fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port) {
			t.Fatalf("%s port file = %q, want %d", tc.dir, strings.TrimSpace(string(data)), ln.Addr().(*net.TCPAddr).Port)
		}
		cfgData, err := os.ReadFile(filepath.Join(tc.dir, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read config for %s: %v", tc.dir, err)
		}
		cfg := string(cfgData)
		if strings.Contains(cfg, "dolt.port:") {
			t.Fatalf("%s config still contains dolt.port:\n%s", tc.dir, cfg)
		}
		if !strings.Contains(cfg, "dolt.auto-start: false") {
			t.Fatalf("%s config missing dolt.auto-start normalization:\n%s", tc.dir, cfg)
		}
		if !strings.Contains(cfg, "gc.endpoint_origin: "+tc.origin) {
			t.Fatalf("%s config missing gc.endpoint_origin=%s:\n%s", tc.dir, tc.origin, cfg)
		}
		if !strings.Contains(cfg, "gc.endpoint_status: verified") {
			t.Fatalf("%s config missing gc.endpoint_status:\n%s", tc.dir, cfg)
		}
	}
}

func TestSyncConfiguredDoltPortFilesReconcilesMalformedManagedConfigs(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })

	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("issue_prefix: stale\ndolt.auto-start: true\ndolt_server_port: 3307\n: not yaml\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{}, "gc", []config.Rig{{Name: "frontend", Path: rigDir, Prefix: "fe"}})

	assertManagedScope := func(dir, prefix, origin string) {
		t.Helper()
		cfgData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read config for %s: %v", dir, err)
		}
		cfg := string(cfgData)
		for _, needle := range []string{
			"issue_prefix: " + prefix,
			"issue-prefix: " + prefix,
			"dolt.auto-start: false",
			"gc.endpoint_origin: " + origin,
			"gc.endpoint_status: verified",
			": not yaml",
		} {
			if !strings.Contains(cfg, needle) {
				t.Fatalf("%s config missing %q after malformed reconciliation:\n%s", dir, needle, cfg)
			}
		}
		for _, forbidden := range []string{"dolt_server_port", "dolt.port:", "dolt.host:", "dolt.user:"} {
			if strings.Contains(cfg, forbidden) {
				t.Fatalf("%s config should scrub %q after malformed reconciliation:\n%s", dir, forbidden, cfg)
			}
		}
	}

	assertManagedScope(cityDir, "gc", "managed_city")
	assertManagedScope(rigDir, "fe", "inherited_city")
}

func TestSyncConfiguredDoltPortFilesCreatesMissingManagedConfigs(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{}, "gc", []config.Rig{{Name: "frontend", Path: rigDir, Prefix: "fe"}})

	for _, tc := range []struct {
		dir    string
		prefix string
		origin string
	}{
		{dir: cityDir, prefix: "gc", origin: "managed_city"},
		{dir: rigDir, prefix: "fe", origin: "inherited_city"},
	} {
		cfgPath := filepath.Join(tc.dir, ".beads", "config.yaml")
		cfgData, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatalf("read config for %s: %v", tc.dir, err)
		}
		cfg := string(cfgData)
		for _, needle := range []string{
			"issue_prefix: " + tc.prefix,
			"issue-prefix: " + tc.prefix,
			"gc.endpoint_origin: " + tc.origin,
			"gc.endpoint_status: verified",
			"dolt.auto-start: false",
		} {
			if !strings.Contains(cfg, needle) {
				t.Fatalf("%s config missing %q:%c%s", tc.dir, needle, 10, cfg)
			}
		}
	}
}

func TestSyncConfiguredDoltPortFilesCanonicalizesExternalAndExplicitScopes(t *testing.T) {
	cityDir := t.TempDir()
	inheritedRigDir := filepath.Join(t.TempDir(), "fe")
	explicitRigDir := filepath.Join(t.TempDir(), "ops")

	for _, dir := range []string{cityDir, inheritedRigDir, explicitRigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("issue_prefix: stale\ndolt.auto-start: true\ndolt_server_port: 3307\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "dolt-server.port"), []byte("3307\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cityDoltConfigs.Store(cityDir, config.DoltConfig{Host: "db.example.com", Port: 3307})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityDir) })

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{Host: "db.example.com", Port: 3307}, "gc", []config.Rig{
		{Name: "fe", Path: inheritedRigDir},
		{Name: "ops", Path: explicitRigDir, DoltHost: "rig-db.example.com", DoltPort: "4406"},
	})

	assertNoPortFile := func(dir string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(dir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
			t.Fatalf("expected no port file for %s, stat err = %v", dir, err)
		}
	}

	assertExternalScope := func(dir, origin, host, port string) {
		t.Helper()
		cfgData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read config for %s: %v", dir, err)
		}
		cfg := string(cfgData)
		for _, needle := range []string{
			"gc.endpoint_origin: " + origin,
			"gc.endpoint_status: unverified",
			"dolt.host: " + host,
			"dolt.port: " + port,
			"dolt.auto-start: false",
		} {
			if !strings.Contains(cfg, needle) {
				t.Fatalf("%s config missing %q:\n%s", dir, needle, cfg)
			}
		}
		if strings.Contains(cfg, "dolt_server_port") {
			t.Fatalf("%s config should scrub deprecated port keys:\n%s", dir, cfg)
		}
	}

	assertExternalScope(cityDir, "city_canonical", "db.example.com", "3307")
	assertExternalScope(inheritedRigDir, "inherited_city", "db.example.com", "3307")
	assertExternalScope(explicitRigDir, "explicit", "rig-db.example.com", "4406")
	assertNoPortFile(cityDir)
	assertNoPortFile(inheritedRigDir)
	assertNoPortFile(explicitRigDir)
}

func TestSyncConfiguredDoltPortFilesPreservesLegacyExternalCityConfig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "fe")

	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: stale
dolt.host: legacy-db.example.com
dolt.port: 4406
dolt.user: city-user
dolt.auto-start: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: stale
dolt.auto-start: true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{}, "gc", []config.Rig{{Name: "fe", Path: rigDir, Prefix: "fe"}})

	assertScope := func(dir, origin, prefix, status string) {
		t.Helper()
		cfgData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read config for %s: %v", dir, err)
		}
		cfg := string(cfgData)
		for _, needle := range []string{
			"issue_prefix: " + prefix,
			"issue-prefix: " + prefix,
			"gc.endpoint_origin: " + origin,
			"gc.endpoint_status: " + status,
			"dolt.host: legacy-db.example.com",
			"dolt.port: 4406",
			"dolt.user: city-user",
		} {
			if !strings.Contains(cfg, needle) {
				t.Fatalf("%s config missing %q:\n%s", dir, needle, cfg)
			}
		}
	}

	assertScope(cityDir, "city_canonical", "gc", "unverified")
	assertScope(rigDir, "inherited_city", "fe", "unverified")
}

func TestSyncConfiguredDoltPortFilesPreservesLegacyExplicitRigConfig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "ops")

	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: stale
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: stale
dolt.host: legacy-rig-db.example.com
dolt.port: 5507
dolt.user: rig-user
dolt.auto-start: true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{}, "gc", []config.Rig{{Name: "ops", Path: rigDir, Prefix: "ops", DoltHost: "deprecated-rig-db.example.com", DoltPort: "6607"}})

	cfgData, err := os.ReadFile(filepath.Join(rigDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(cfgData)
	for _, needle := range []string{
		"issue_prefix: ops",
		"issue-prefix: ops",
		"gc.endpoint_origin: explicit",
		"gc.endpoint_status: unverified",
		"dolt.host: legacy-rig-db.example.com",
		"dolt.port: 5507",
		"dolt.user: rig-user",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("rig config missing %q:\n%s", needle, cfg)
		}
	}
	for _, forbidden := range []string{"deprecated-rig-db.example.com", "6607"} {
		if strings.Contains(cfg, forbidden) {
			t.Fatalf("rig config should ignore deprecated rig endpoint authority %q:\n%s", forbidden, cfg)
		}
	}
}

func TestSyncConfiguredDoltPortFilesPrefersCanonicalCityEndpointOverCompatConfig(t *testing.T) {
	cityDir := t.TempDir()
	inheritedRigDir := filepath.Join(t.TempDir(), "fe")

	for _, dir := range []string{cityDir, inheritedRigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: stale
issue-prefix: stale
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: canonical-db.example.com
dolt.port: 4406
dolt.user: city-user
dolt.auto-start: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inheritedRigDir, ".beads", "config.yaml"), []byte("issue_prefix: stale\ndolt.auto-start: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{Host: "deprecated-db.example.com", Port: 3307}, "gc", []config.Rig{{Name: "fe", Path: inheritedRigDir, Prefix: "fe"}})

	assertScope := func(dir, origin, prefix, status string) {
		t.Helper()
		cfgData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read config for %s: %v", dir, err)
		}
		cfg := string(cfgData)
		for _, needle := range []string{
			"issue_prefix: " + prefix,
			"issue-prefix: " + prefix,
			"gc.endpoint_origin: " + origin,
			"gc.endpoint_status: " + status,
			"dolt.host: canonical-db.example.com",
			"dolt.port: 4406",
			"dolt.user: city-user",
		} {
			if !strings.Contains(cfg, needle) {
				t.Fatalf("%s config missing %q:\n%s", dir, needle, cfg)
			}
		}
		for _, forbidden := range []string{"deprecated-db.example.com", "3307", "dolt_server_port"} {
			if strings.Contains(cfg, forbidden) {
				t.Fatalf("%s config should ignore deprecated endpoint authority %q:\n%s", dir, forbidden, cfg)
			}
		}
	}

	assertScope(cityDir, "city_canonical", "gc", "verified")
	assertScope(inheritedRigDir, "inherited_city", "fe", "verified")
}

func TestSyncConfiguredDoltPortFilesPrefersCanonicalExplicitRigEndpointOverCompatConfig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "ops")

	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: stale
issue-prefix: stale
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: stale
issue-prefix: stale
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.host: canonical-rig-db.example.com
dolt.port: 5507
dolt.user: rig-user
dolt.auto-start: true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{}, "gc", []config.Rig{{Name: "ops", Path: rigDir, Prefix: "ops", DoltHost: "deprecated-rig-db.example.com", DoltPort: "6607"}})

	cfgData, err := os.ReadFile(filepath.Join(rigDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(cfgData)
	for _, needle := range []string{
		"issue_prefix: ops",
		"issue-prefix: ops",
		"gc.endpoint_origin: explicit",
		"gc.endpoint_status: verified",
		"dolt.host: canonical-rig-db.example.com",
		"dolt.port: 5507",
		"dolt.user: rig-user",
	} {
		if !strings.Contains(cfg, needle) {
			t.Fatalf("rig config missing %q:\n%s", needle, cfg)
		}
	}
	for _, forbidden := range []string{"deprecated-rig-db.example.com", "6607"} {
		if strings.Contains(cfg, forbidden) {
			t.Fatalf("rig config should ignore deprecated rig endpoint authority %q:\n%s", forbidden, cfg)
		}
	}
}

func TestSyncConfiguredDoltPortFilesPreservesCanonicalCityAndExplicitRigOverCompatInputs(t *testing.T) {
	cityDir := t.TempDir()
	inheritedRigDir := filepath.Join(t.TempDir(), "fe")
	explicitRigDir := filepath.Join(t.TempDir(), "ops")

	files := map[string]string{
		cityDir: `issue_prefix: stale
issue-prefix: stale
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: city-user
dolt.auto-start: true
`,
		inheritedRigDir: `issue_prefix: stale
issue-prefix: stale
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: city-user
dolt.auto-start: true
`,
		explicitRigDir: `issue_prefix: stale
issue-prefix: stale
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.host: canonical-rig-db.example.com
dolt.port: 4406
dolt.user: rig-user
dolt.auto-start: true
`,
	}
	for dir, cfg := range files {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "dolt-server.port"), []byte("3307\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{Host: "compat-db.example.com", Port: 5507}, "gc", []config.Rig{
		{Name: "fe", Path: inheritedRigDir},
		{Name: "ops", Path: explicitRigDir, DoltHost: "compat-rig-db.example.com", DoltPort: "6608"},
	})

	assertScope := func(dir string, needles []string, forbids []string) {
		t.Helper()
		cfgData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read config for %s: %v", dir, err)
		}
		cfg := string(cfgData)
		for _, needle := range needles {
			if !strings.Contains(cfg, needle) {
				t.Fatalf("%s config missing %q:\n%s", dir, needle, cfg)
			}
		}
		for _, forbid := range forbids {
			if strings.Contains(cfg, forbid) {
				t.Fatalf("%s config should not contain %q:\n%s", dir, forbid, cfg)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
			t.Fatalf("expected no port file for %s, stat err = %v", dir, err)
		}
	}

	assertScope(cityDir,
		[]string{
			"issue_prefix: gc",
			"issue-prefix: gc",
			"gc.endpoint_origin: city_canonical",
			"gc.endpoint_status: verified",
			"dolt.host: canonical-db.example.com",
			"dolt.port: 3307",
			"dolt.user: city-user",
		},
		[]string{"compat-db.example.com", "5507"},
	)
	assertScope(inheritedRigDir,
		[]string{
			"issue_prefix: fe",
			"issue-prefix: fe",
			"gc.endpoint_origin: inherited_city",
			"gc.endpoint_status: verified",
			"dolt.host: canonical-db.example.com",
			"dolt.port: 3307",
			"dolt.user: city-user",
		},
		[]string{"compat-db.example.com", "5507"},
	)
	assertScope(explicitRigDir,
		[]string{
			"issue_prefix: ops",
			"issue-prefix: ops",
			"gc.endpoint_origin: explicit",
			"gc.endpoint_status: verified",
			"dolt.host: canonical-rig-db.example.com",
			"dolt.port: 4406",
			"dolt.user: rig-user",
		},
		[]string{"compat-rig-db.example.com", "6608"},
	)
}

func TestSyncConfiguredDoltPortFilesPreservesCanonicalManagedCityOverCompatExternalInput(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "fe")
	for dir, cfg := range map[string]string{
		cityDir: `issue_prefix: stale
issue-prefix: stale
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: true
`,
		rigDir: `issue_prefix: stale
issue-prefix: stale
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.host: stale-db.example.com
dolt.port: 3307
dolt.auto-start: true
`,
	} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })
	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{Host: "compat-db.example.com", Port: 4406}, "gc", []config.Rig{{Name: "fe", Path: rigDir}})

	assertManagedScope := func(dir, prefix, wantOrigin, wantPort string) {
		t.Helper()
		cfgData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read config for %s: %v", dir, err)
		}
		cfg := string(cfgData)
		for _, needle := range []string{
			"issue_prefix: " + prefix,
			"issue-prefix: " + prefix,
			"gc.endpoint_origin: " + wantOrigin,
			"gc.endpoint_status: verified",
		} {
			if !strings.Contains(cfg, needle) {
				t.Fatalf("%s config missing %q:\n%s", dir, needle, cfg)
			}
		}
		for _, forbid := range []string{"dolt.host:", "dolt.port:", "compat-db.example.com", "4406", "stale-db.example.com", "3307"} {
			if strings.Contains(cfg, forbid) {
				t.Fatalf("%s config should not contain %q:\n%s", dir, forbid, cfg)
			}
		}
		portData, err := os.ReadFile(filepath.Join(dir, ".beads", "dolt-server.port"))
		if err != nil {
			t.Fatalf("read port file for %s: %v", dir, err)
		}
		if got := strings.TrimSpace(string(portData)); got != wantPort {
			t.Fatalf("%s port file = %q, want %q", dir, got, wantPort)
		}
	}

	wantPort := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	assertManagedScope(cityDir, "gc", "managed_city", wantPort)
	assertManagedScope(rigDir, "fe", "inherited_city", wantPort)
}

func TestSyncConfiguredDoltPortFilesRejectsInvalidCanonicalCityState(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: invalid-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	before := mustReadFile(t, filepath.Join(cityDir, ".beads", "config.yaml"))
	err := syncConfiguredDoltPortFiles(cityDir, config.DoltConfig{}, "gc", []config.Rig{{Name: "frontend", Path: rigDir, Prefix: "fe"}})
	if err == nil || !strings.Contains(err.Error(), "invalid canonical city endpoint state") {
		t.Fatalf("syncConfiguredDoltPortFiles() error = %v, want invalid canonical city endpoint state", err)
	}
	after := mustReadFile(t, filepath.Join(cityDir, ".beads", "config.yaml"))
	if string(after) != string(before) {
		t.Fatalf("city config changed despite invalid canonical state:\n%s", after)
	}
}

func TestSyncConfiguredDoltPortFilesRejectsInvalidCanonicalRigState(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: fe
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      3311,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	before := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml"))
	err := syncConfiguredDoltPortFiles(cityDir, config.DoltConfig{}, "gc", []config.Rig{{Name: "frontend", Path: rigDir, Prefix: "fe"}})
	if err == nil || !strings.Contains(err.Error(), "invalid canonical rig endpoint state") {
		t.Fatalf("syncConfiguredDoltPortFiles() error = %v, want invalid canonical rig endpoint state", err)
	}
	after := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml"))
	if string(after) != string(before) {
		t.Fatalf("rig config changed despite invalid canonical state:\n%s", after)
	}
}

func TestSyncConfiguredDoltPortFilesSkipsNonBDProviders(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")

	requireSyncConfiguredDoltPortFiles(t, cityDir, "file", config.DoltConfig{Host: "db.example.com", Port: 3307}, "gc", []config.Rig{{Name: "frontend", Path: rigDir, Prefix: "fe"}})

	for _, path := range []string{
		filepath.Join(cityDir, ".beads", "config.yaml"),
		filepath.Join(cityDir, ".beads", "dolt-server.port"),
		filepath.Join(rigDir, ".beads", "config.yaml"),
		filepath.Join(rigDir, ".beads", "dolt-server.port"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected non-bd sync to leave %s absent, stat err = %v", path, err)
		}
	}
}

func TestSyncConfiguredDoltPortFilesRepairsBdRigUnderFileBackedCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "fe",
	}); err != nil {
		t.Fatal(err)
	}
	wantPort := strconv.Itoa(writeReachableManagedDoltState(t, cityDir))

	requireSyncConfiguredDoltPortFiles(t, cityDir, "file", config.DoltConfig{}, "gc", []config.Rig{{Name: "frontend", Path: rigDir, Prefix: "fe"}})

	data, err := os.ReadFile(filepath.Join(rigDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "gc.endpoint_origin: inherited_city") {
		t.Fatalf("rig config missing inherited city origin:\n%s", text)
	}
	portData, err := os.ReadFile(filepath.Join(rigDir, ".beads", "dolt-server.port"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(portData)) != wantPort {
		t.Fatalf("rig port file = %q, want %s", strings.TrimSpace(string(portData)), wantPort)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("city port file should remain absent for file-backed city, stat err = %v", err)
	}
}

func TestSyncConfiguredDoltPortFilesIgnoresEnvOnlyExternalOverridesForCanonicalFiles(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })

	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte(`issue_prefix: stale
dolt.auto-start: true
`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT_HOST", "env-db.example.com")
	t.Setenv("GC_DOLT_PORT", "3307")
	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{}, "gc", []config.Rig{{Name: "frontend", Path: rigDir, Prefix: "fe"}})

	assertManagedScope := func(dir, prefix, origin string) {
		t.Helper()
		cfgData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read config for %s: %v", dir, err)
		}
		cfg := string(cfgData)
		for _, needle := range []string{
			"issue_prefix: " + prefix,
			"issue-prefix: " + prefix,
			"gc.endpoint_origin: " + origin,
			"gc.endpoint_status: verified",
		} {
			if !strings.Contains(cfg, needle) {
				t.Fatalf("%s config missing %q:%c%s", dir, needle, 10, cfg)
			}
		}
		for _, forbidden := range []string{"dolt.host:", "dolt.port:"} {
			if strings.Contains(cfg, forbidden) {
				t.Fatalf("%s config should ignore env-only external override %q:%c%s", dir, forbidden, 10, cfg)
			}
		}
	}

	assertManagedScope(cityDir, "gc", "managed_city")
	assertManagedScope(rigDir, "fe", "inherited_city")
}

func TestSyncConfiguredDoltPortFilesPreservesVerifiedStatusForUnchangedExternalEndpoints(t *testing.T) {
	cityDir := t.TempDir()
	inheritedRigDir := filepath.Join(t.TempDir(), "fe")
	explicitRigDir := filepath.Join(t.TempDir(), "ops")

	files := map[string]string{
		cityDir: `issue_prefix: stale
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: db.example.com
dolt.port: 3307
dolt.user: city-user
dolt.auto-start: true
`,
		inheritedRigDir: `issue_prefix: stale
gc.endpoint_origin: inherited_city
gc.endpoint_status: unverified
dolt.host: db.example.com
dolt.port: 3307
dolt.user: rig-user
dolt.auto-start: true
`,
		explicitRigDir: `issue_prefix: stale
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.host: rig-db.example.com
dolt.port: 4406
dolt.user: ops-user
dolt.auto-start: true
`,
	}
	for dir, cfg := range files {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cityDoltConfigs.Store(cityDir, config.DoltConfig{Host: "db.example.com", Port: 3307})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityDir) })

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{Host: "db.example.com", Port: 3307}, "gc", []config.Rig{
		{Name: "fe", Path: inheritedRigDir},
		{Name: "ops", Path: explicitRigDir, DoltHost: "rig-db.example.com", DoltPort: "4406"},
	})

	for _, tc := range []struct {
		dir        string
		wantUser   string
		wantStatus string
	}{
		{dir: cityDir, wantUser: "city-user", wantStatus: "verified"},
		{dir: inheritedRigDir, wantUser: "city-user", wantStatus: "verified"},
		{dir: explicitRigDir, wantUser: "ops-user", wantStatus: "verified"},
	} {
		cfgData, err := os.ReadFile(filepath.Join(tc.dir, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read config for %s: %v", tc.dir, err)
		}
		cfg := string(cfgData)
		if !strings.Contains(cfg, "gc.endpoint_status: "+tc.wantStatus) {
			t.Fatalf("%s config should preserve endpoint status %q:%c%s", tc.dir, tc.wantStatus, 10, cfg)
		}
		if !strings.Contains(cfg, "dolt.user: "+tc.wantUser) {
			t.Fatalf("%s config should preserve dolt.user %q:%c%s", tc.dir, tc.wantUser, 10, cfg)
		}
	}
}

func TestSyncConfiguredDoltPortFilesClearsInheritedDoltUserWhenCityClearsIt(t *testing.T) {
	cityDir := t.TempDir()
	inheritedRigDir := filepath.Join(t.TempDir(), "fe")

	files := map[string]string{
		cityDir: `issue_prefix: stale
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: db.example.com
dolt.port: 3307
dolt.auto-start: true
`,
		inheritedRigDir: `issue_prefix: stale
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.host: db.example.com
dolt.port: 3307
dolt.user: stale-user
dolt.auto-start: true
`,
	}
	for dir, cfg := range files {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cityDoltConfigs.Store(cityDir, config.DoltConfig{Host: "db.example.com", Port: 3307})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityDir) })

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{Host: "db.example.com", Port: 3307}, "gc", []config.Rig{{Name: "fe", Path: inheritedRigDir}})

	cfgData, err := os.ReadFile(filepath.Join(inheritedRigDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(cfgData)
	if strings.Contains(cfg, "dolt.user:") {
		t.Fatalf("inherited rig config should clear stale dolt.user when city user is empty:%c%s", 10, cfg)
	}
}

func TestSyncConfiguredDoltPortFilesReconcilesMirroredPrefixesFromCityConfig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")

	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("issue_prefix: stale\nissue-prefix: stale\ndolt.auto-start: true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{}, "gc", []config.Rig{{Name: "frontend", Path: rigDir, Prefix: "fe"}})

	assertPrefix := func(dir, want string) {
		t.Helper()
		cfgData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read config for %s: %v", dir, err)
		}
		cfg := string(cfgData)
		for _, needle := range []string{"issue_prefix: " + want, "issue-prefix: " + want} {
			if !strings.Contains(cfg, needle) {
				t.Fatalf("%s config missing %q:\n%s", dir, needle, cfg)
			}
		}
	}

	assertPrefix(cityDir, "gc")
	assertPrefix(rigDir, "fe")
}

func TestCurrentDoltPortIgnoresDeadRuntimeStateAndPrunesDeadPortFile(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	ln := listenOnRandomPort(t)
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running: true,
		PID:     os.Getpid(),
		Port:    port,
		DataDir: filepath.Join(cityDir, ".beads", "dolt"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "dolt-server.port"), []byte(fmt.Sprintf("%d\n", port)), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := currentDoltPort(cityDir); got != "" {
		t.Fatalf("currentDoltPort() = %q, want empty for dead runtime state", got)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("stale port file should be removed, stat err = %v", err)
	}
}

func TestCurrentDoltPortIgnoresReachablePortFileWhenManagedStateIsStopped(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running: false,
		PID:     0,
		Port:    port,
		DataDir: filepath.Join(cityDir, ".beads", "dolt"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "dolt-server.port"), []byte(fmt.Sprintf("%d\n", port)), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := currentDoltPort(cityDir); got != "" {
		t.Fatalf("currentDoltPort() = %q, want empty when managed state is stopped", got)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("reachable stale port file should be removed, stat err = %v", err)
	}
}

// TestInitBeadsForDir_file verifies that unmarked file cities stay in legacy shared mode.
func TestInitBeadsForDir_file(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	cityDir := t.TempDir()
	if err := initBeadsForDir(cityDir, cityDir, "test", "test"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if fileStoreUsesScopedRoots(cityDir) {
		t.Fatal("unmarked file city should remain legacy-shared")
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gc", "beads.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy file city should not create beads.json on init, stat err = %v", err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	list, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty legacy store, got %#v", list)
	}
}

func TestInitBeadsForDir_fileScopedRigCreatesStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "rig1")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := initBeadsForDir(cityDir, rigDir, "test", "test"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc", "beads.json")); err != nil {
		t.Fatalf("expected scoped rig file store bootstrap, stat err = %v", err)
	}
	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "rig bead", Type: "task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestInitBeadsForDir_fileLegacyRigPreservesSharedCityStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "rig1")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("openScopeLocalFileStore(city): %v", err)
	}
	if err := initBeadsForDir(cityDir, rigDir, "test", "test"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc", "beads.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy shared file city should not create rig store, stat err = %v", err)
	}
	if fileStoreUsesScopedRoots(cityDir) {
		t.Fatal("legacy shared file city should not be marked scoped")
	}
}

func writeMinimalCityToml(t *testing.T, cityDir string) {
	t.Helper()
	content := `[workspace]
name = "demo"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestInitBeadsForDir_exec calls script with init <dir> <prefix> <dolt_database>.
func TestInitBeadsForDir_exec(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	script := writeTestScript(t, "init", 2, "")
	t.Setenv("GC_BEADS", "exec:"+script)
	if err := initBeadsForDir(cityDir, cityDir, "prefix", "prefix"); err != nil {
		t.Fatalf("expected nil for exit 2, got %v", err)
	}
}

func TestInitBeadsForDir_execPassesCanonicalDoltDatabase(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	logFile := filepath.Join(t.TempDir(), "args.log")
	script := filepath.Join(t.TempDir(), "record-args.sh")
	content := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > %q\nexit 0\n", logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	if err := initBeadsForDir(cityDir, cityDir, "gc", "gascity"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	want := "init " + cityDir + " gc gascity"
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("script args = %q, want %q", got, want)
	}
}

// TestInitBeadsForDirExecSetsBEADSDIR exercises the controller-side exec paths
// that invoke bd init directly and asserts BEADS_DIR=<dir>/.beads is present in
// the subprocess env. The k8s scoped path sets BEADS_DIR inside the provider
// script itself; that behavior is covered by internal/runtime/k8s tests.
// Regression for #399.
func TestInitBeadsForDirExecSetsBEADSDIR(t *testing.T) {
	for _, tc := range []struct {
		name       string
		scriptBase string
		// cityToml uses dolt/rig config appropriate for the exec branch.
		cityToml func(rigRel string) string
	}{
		{
			name:       "gc-beads-bd canonical",
			scriptBase: "gc-beads-bd",
			cityToml: func(rigRel string) string {
				return "[workspace]\nname = \"demo\"\n\n[[rigs]]\nname = \"r\"\npath = \"" + rigRel + "\"\nprefix = \"rg\"\n"
			},
		},
		{
			name:       "generic legacy exec",
			scriptBase: "record-env",
			cityToml: func(rigRel string) string {
				return "[workspace]\nname = \"demo\"\n\n[[rigs]]\nname = \"r\"\npath = \"" + rigRel + "\"\nprefix = \"rg\"\n"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			rigDir := filepath.Join(cityDir, "r")
			if err := os.MkdirAll(rigDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(tc.cityToml("r")), 0o644); err != nil {
				t.Fatal(err)
			}
			logFile := filepath.Join(t.TempDir(), "env.log")
			script := filepath.Join(t.TempDir(), tc.scriptBase)
			content := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = init ]; then printf '%%s\\n' \"${BEADS_DIR:-<unset>}\" > %q; fi\nexit 0\n", logFile)
			if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
				t.Fatal(err)
			}

			t.Setenv("GC_BEADS", "exec:"+script)
			if err := initBeadsForDir(cityDir, rigDir, "rg", "rg-db"); err != nil {
				t.Fatalf("initBeadsForDir: %v", err)
			}

			data, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatalf("read env log: %v", err)
			}
			want := filepath.Join(rigDir, ".beads")
			if got := strings.TrimSpace(string(data)); got != want {
				t.Fatalf("BEADS_DIR = %q, want %q (bd init without BEADS_DIR creates .git as a side effect)", got, want)
			}
		})
	}
}

func TestInitBeadsForDirExecWithoutCityPathPreservesAmbientEnv(t *testing.T) {
	rigDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "env.log")
	script := filepath.Join(t.TempDir(), "record-env")
	content := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = init ]; then printf '%%s|%%s\\n' \"${GC_DOLT_HOST:-}\" \"${BEADS_DIR:-<unset>}\" > %q; fi\nexit 0\n", logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_DOLT_HOST", "ambient-dolt")
	if err := initBeadsForDir("", rigDir, "rg", ""); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(data)), "|")
	if len(parts) != 2 {
		t.Fatalf("env log = %q, want host|beads_dir", string(data))
	}
	if got := parts[0]; got != "ambient-dolt" {
		t.Fatalf("GC_DOLT_HOST = %q, want ambient-dolt", got)
	}
	if got, want := parts[1], filepath.Join(rigDir, ".beads"); got != want {
		t.Fatalf("BEADS_DIR = %q, want %q", got, want)
	}
}

func TestInitBeadsForDirExecPreventsStrayGitInit(t *testing.T) {
	script := filepath.Join(t.TempDir(), "bd-like-provider.sh")
	content := `#!/bin/sh
set -eu
op="$1"
shift
case "$op" in
  init)
    dir="$1"
    mkdir -p "$dir/.beads"
    if [ -z "${BEADS_DIR:-}" ]; then
      mkdir -p "$dir/.git"
    fi
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	rawDir := t.TempDir()
	rawCmd := exec.Command(script, "init", rawDir, "raw")
	rawCmd.Env = sanitizedBaseEnv()
	rawOut, err := rawCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("direct provider init failed: %v\n%s", err, rawOut)
	}
	if _, err := os.Stat(filepath.Join(rawDir, ".beads")); err != nil {
		t.Fatalf("direct provider init did not create .beads: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rawDir, ".git")); err != nil {
		t.Fatalf("direct provider init did not emulate stray .git creation: %v", err)
	}

	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	if err := initBeadsForDir(cityDir, rigDir, "fe", "frontend-db"); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".beads")); err != nil {
		t.Fatalf("initBeadsForDir did not create .beads: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("initBeadsForDir should prevent stray .git creation, stat err = %v", err)
	}
}

func TestRunProviderOpStripsAmbientGCDoltSkip(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	logFile := filepath.Join(t.TempDir(), "env.log")
	script := filepath.Join(t.TempDir(), "record-env.sh")
	content := fmt.Sprintf(`#!/bin/sh
printf '%%s|%%s|%%s|%%s
' "${GC_DOLT:-}" "${GC_DOLT_HOST:-}" "${GC_DOLT_PORT:-}" "${GC_CITY_PATH:-}" > %q
exit 0
`, logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	if err := runProviderOp(script, cityDir, "init", cityDir, "gc", "hq"); err != nil {
		t.Fatalf("runProviderOp: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(data)), "|")
	if len(parts) != 4 {
		t.Fatalf("captured env = %q, want 4 fields", strings.TrimSpace(string(data)))
	}
	if parts[0] != "" {
		t.Fatalf("GC_DOLT leaked into provider env: %q", parts[0])
	}
	if parts[3] != cityDir {
		t.Fatalf("GC_CITY_PATH = %q, want %q", parts[3], cityDir)
	}
}

func TestInitBeadsForDirExecGcBeadsBdPreservesCityRuntimeEnv(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	logFile := filepath.Join(t.TempDir(), "env.log")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	content := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
  init)
    printf '%%s|%%s|%%s|%%s
' "${GC_CITY_PATH:-}" "${GC_CITY_RUNTIME_DIR:-}" "${GC_PACK_STATE_DIR:-}" "${GC_DOLT_DATA_DIR:-}" > %q
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`, logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_CITY_PATH", "/wrong-city")
	t.Setenv("GC_CITY_RUNTIME_DIR", "/wrong-runtime")
	t.Setenv("GC_PACK_STATE_DIR", "/wrong-pack")
	t.Setenv("GC_DOLT_DATA_DIR", "/wrong-data")

	if err := initBeadsForDir(cityDir, cityDir, "gc", "hq"); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(data)), "|")
	if len(parts) != 4 {
		t.Fatalf("captured env = %q, want 4 fields", strings.TrimSpace(string(data)))
	}
	if parts[0] != cityDir {
		t.Fatalf("GC_CITY_PATH = %q, want %q", parts[0], cityDir)
	}
	if parts[1] != filepath.Join(cityDir, ".gc", "runtime") {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want %q", parts[1], filepath.Join(cityDir, ".gc", "runtime"))
	}
	if parts[2] != citylayout.PackStateDir(cityDir, "dolt") {
		t.Fatalf("GC_PACK_STATE_DIR = %q, want %q", parts[2], citylayout.PackStateDir(cityDir, "dolt"))
	}
	if parts[3] != filepath.Join(cityDir, ".beads", "dolt") {
		t.Fatalf("GC_DOLT_DATA_DIR = %q, want %q", parts[3], filepath.Join(cityDir, ".beads", "dolt"))
	}
}

func TestInitBeadsForDirExecGcBeadsBdNormalizesCanonicalFilesAfterProviderInit(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	content := `#!/bin/sh
set -eu
op="$1"
shift || true
case "$op" in
  init)
    dir="$1"
    mkdir -p "$dir/.beads"
    cat > "$dir/.beads/metadata.json" <<'EOF'
{
  "database": "sqlite",
  "backend": "sqlite",
  "dolt_mode": "local",
  "dolt_database": "wrong"
}
EOF
    cat > "$dir/.beads/config.yaml" <<'EOF'
issue_prefix: wrong
issue-prefix: wrong
gc.endpoint_origin: explicit
gc.endpoint_status: unverified
dolt.host: stale.example
dolt.port: 3307
dolt.user: stale
EOF
    exit 0
    ;;
  list)
    printf '[]\n'
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)

	if err := initBeadsForDir(cityDir, cityDir, "gc", "hq"); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	configState, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(cityDir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("ReadConfigState: %v", err)
	}
	if !ok {
		t.Fatal("ReadConfigState() = !ok, want canonical config")
	}
	if configState.IssuePrefix != "gc" {
		t.Fatalf("IssuePrefix = %q, want gc", configState.IssuePrefix)
	}
	if configState.EndpointOrigin != contract.EndpointOriginManagedCity {
		t.Fatalf("EndpointOrigin = %q, want %q", configState.EndpointOrigin, contract.EndpointOriginManagedCity)
	}
	if configState.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("EndpointStatus = %q, want %q", configState.EndpointStatus, contract.EndpointStatusVerified)
	}
	if configState.DoltHost != "" || configState.DoltPort != "" || configState.DoltUser != "" {
		t.Fatalf("managed city config should scrub tracked endpoint defaults, got host=%q port=%q user=%q", configState.DoltHost, configState.DoltPort, configState.DoltUser)
	}

	metaData, err := os.ReadFile(filepath.Join(cityDir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("ReadFile(metadata): %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("Unmarshal(metadata): %v", err)
	}
	if got := strings.TrimSpace(fmt.Sprint(meta["database"])); got != "dolt" {
		t.Fatalf("metadata database = %q, want dolt", got)
	}
	if got := strings.TrimSpace(fmt.Sprint(meta["backend"])); got != "dolt" {
		t.Fatalf("metadata backend = %q, want dolt", got)
	}
	if got := strings.TrimSpace(fmt.Sprint(meta["dolt_mode"])); got != "server" {
		t.Fatalf("metadata dolt_mode = %q, want server", got)
	}
	if got := strings.TrimSpace(fmt.Sprint(meta["dolt_database"])); got != "hq" {
		t.Fatalf("metadata dolt_database = %q, want hq", got)
	}
}

func TestInitBeadsForDir_execGcBeadsK8sUsesScopedLifecycleEnv(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[dolt]
host = "city-db.example.com"
port = 3307

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

	logFile := filepath.Join(t.TempDir(), "env.log")
	script := filepath.Join(t.TempDir(), "gc-beads-k8s")
	content := fmt.Sprintf(`#!/bin/sh
printf '%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s\n' "${GC_STORE_ROOT:-}" "${GC_STORE_SCOPE:-}" "${GC_BEADS_PREFIX:-}" "${GC_DOLT_HOST:-}" "${GC_DOLT_PORT:-}" "${GC_RIG:-}" "${GC_RIG_ROOT:-}" "${GC_PROVIDER:-}" > %q
exit 0
`, logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	if err := initBeadsForDir(cityDir, rigDir, "fe", "frontend-db"); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	want := strings.Join([]string{
		rigDir,
		"rig",
		"fe",
		"rig-db.example.com",
		"4407",
		"frontend",
		rigDir,
		"exec:" + script,
	}, "|")
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("scoped init env = %q, want %q", got, want)
	}
}

func TestInitBeadsForDir_execOmitsCanonicalDoltDatabaseWhenUnknown(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	logFile := filepath.Join(t.TempDir(), "args.log")
	script := filepath.Join(t.TempDir(), "record-args.sh")
	content := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > %q\nexit 0\n", logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	if err := initBeadsForDir(cityDir, cityDir, "gc", ""); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	want := "init " + cityDir + " gc"
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("script args = %q, want %q", got, want)
	}
}

// TestInitBeadsForDir_bd_skip verifies bd provider is no-op when GC_DOLT=skip.
func TestInitBeadsForDirExecGcBeadsBdPassesComputedCanonicalDoltDatabase(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	logFile := filepath.Join(t.TempDir(), "args.log")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	content := fmt.Sprintf(`#!/bin/sh
set -eu
op="$1"
shift || true
case "$op" in
  init)
    printf '%%s\n' "$*" > %q
    exit 0
    ;;
  list)
    printf '[]\n'
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`, logFile)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	if err := initBeadsForDir(cityDir, cityDir, "gc", ""); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	want := cityDir + " gc hq"
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("script args = %q, want %q", got, want)
	}
}

func TestInitBeadsForDir_bd_skip(t *testing.T) {
	dir := t.TempDir()
	writeMinimalCityToml(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	MaterializeBuiltinPacks(dir) //nolint:errcheck
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	if err := initBeadsForDir(dir, dir, "test", "test"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestInitBeadsForDirBdMaterializedScriptPreservesCityPath(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeBuiltinPacks(cityDir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := `#!/bin/sh
set -eu
case "${1:-}" in
  init)
    mkdir -p "$PWD/.beads"
    printf '{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}\n' > "$PWD/.beads/metadata.json"
    exit 0
    ;;
  config|migrate|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	configureTestDoltIdentityEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	if err := initBeadsForDir(cityDir, cityDir, "gc", "hq"); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}
}

func TestInitBeadsForDirBdMaterializedScriptIgnoresAmbientCityRuntimeEnv(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeBuiltinPacks(cityDir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	captureFile := filepath.Join(t.TempDir(), "bd-init-env.txt")
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-}" in
  init)
    mkdir -p "$PWD/.beads"
    printf '{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}\n' > "$PWD/.beads/metadata.json"
    printf '%%s|%%s|%%s|%%s\n' \
      "${GC_CITY_PATH:-}" \
      "${GC_CITY_RUNTIME_DIR:-}" \
      "${GC_PACK_STATE_DIR:-}" \
      "${BEADS_DIR:-}" > %q
    exit 0
    ;;
  config|migrate|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, captureFile)
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	configureTestDoltIdentityEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	t.Setenv("GC_CITY_PATH", "/wrong-city")
	t.Setenv("GC_CITY_RUNTIME_DIR", "/wrong-runtime")
	t.Setenv("GC_PACK_STATE_DIR", "/wrong-pack")
	t.Setenv("BEADS_DIR", "/wrong/.beads")

	if err := initBeadsForDir(cityDir, cityDir, "gc", "hq"); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(data)), "|")
	if len(parts) != 4 {
		t.Fatalf("captured env = %q, want 4 fields", strings.TrimSpace(string(data)))
	}
	if parts[0] != cityDir {
		t.Fatalf("GC_CITY_PATH = %q, want %q", parts[0], cityDir)
	}
	if parts[1] != filepath.Join(cityDir, ".gc", "runtime") {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want %q", parts[1], filepath.Join(cityDir, ".gc", "runtime"))
	}
	if parts[2] != citylayout.PackStateDir(cityDir, "dolt") {
		t.Fatalf("GC_PACK_STATE_DIR = %q, want %q", parts[2], citylayout.PackStateDir(cityDir, "dolt"))
	}
	if parts[3] != filepath.Join(cityDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", parts[3], filepath.Join(cityDir, ".beads"))
	}
}

// TestRunProviderOp_exit2 verifies exit 2 is treated as success (not needed).
func TestRunProviderOp_exit2(t *testing.T) {
	script := writeTestScript(t, "", 2, "")
	if err := runProviderOp(script, "", "ensure-ready"); err != nil {
		t.Fatalf("expected nil for exit 2, got %v", err)
	}
}

// TestRunProviderOp_exit0 verifies exit 0 is success.
func TestRunProviderOp_exit0(t *testing.T) {
	script := writeTestScript(t, "", 0, "")
	if err := runProviderOp(script, "", "ensure-ready"); err != nil {
		t.Fatalf("expected nil for exit 0, got %v", err)
	}
}

// TestRunProviderOp_error verifies exit 1 propagates the error with stderr.
func TestRunProviderOp_error(t *testing.T) {
	script := writeTestScript(t, "", 1, "server crashed")
	err := runProviderOp(script, "", "ensure-ready")
	if err == nil {
		t.Fatal("expected error for exit 1")
	}
	if got := err.Error(); got != "exec beads ensure-ready: server crashed" {
		t.Fatalf("unexpected error message: %s", got)
	}
}

// TestRunProviderOp_errorNoStderr verifies exit 1 with no stderr uses exec error.
func TestRunProviderOp_errorNoStderr(t *testing.T) {
	script := writeTestScript(t, "", 1, "")
	err := runProviderOp(script, "", "shutdown")
	if err == nil {
		t.Fatal("expected error for exit 1")
	}
	if got := err.Error(); got == "" {
		t.Fatal("expected non-empty error")
	}
}

// TestRunProviderOp_setsCityRuntimeEnv verifies city runtime env vars are set in the script env.
func TestRunProviderOp_setsCityRuntimeEnv(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "check-env.sh")
	content := "#!/bin/sh\nif [ \"$GC_CITY\" = \"" + dir + "\" ] && [ \"$GC_CITY_PATH\" = \"" + dir + "\" ] && [ \"$GC_CITY_RUNTIME_DIR\" = \"" + filepath.Join(dir, ".gc", "runtime") + "\" ]; then exit 0; else exit 1; fi\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runProviderOp(script, dir, "health"); err != nil {
		t.Fatalf("expected city runtime env to be set, got %v", err)
	}
}

func TestRunProviderOpSanitizesInheritedRuntimeEnv(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sanitize-env.sh")
	content := "#!/bin/sh\n" +
		"test \"$GC_CITY\" = \"" + dir + "\" || exit 1\n" +
		"test \"$GC_CITY_PATH\" = \"" + dir + "\" || exit 1\n" +
		"test \"$GC_CITY_RUNTIME_DIR\" = \"" + filepath.Join(dir, ".gc", "runtime") + "\" || exit 1\n" +
		"test -z \"$GC_PACK_STATE_DIR\" || exit 1\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", "/wrong")
	t.Setenv("GC_CITY_PATH", "/wrong")
	t.Setenv("GC_CITY_ROOT", "/wrong")
	t.Setenv("GC_CITY_RUNTIME_DIR", "/wrong/.gc/runtime")
	t.Setenv("GC_PACK_STATE_DIR", "/wrong/.gc/runtime/packs/dolt")
	if err := runProviderOp(script, dir, "health"); err != nil {
		t.Fatalf("expected sanitized runtime env, got %v", err)
	}
}

func TestStartBeadsLifecycleDoesNotMutateProcessDoltEnv(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("BEADS_DOLT_SERVER_PORT")
	_ = os.Unsetenv("BEADS_DOLT_SERVER_HOST")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      4406,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	if got := os.Getenv("GC_DOLT_PORT"); got != "" {
		t.Fatalf("GC_DOLT_PORT = %q, want empty", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want empty", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want empty", got)
	}
}

func TestGcBeadsBdStartUsesRootBeadsDataDir(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(homeDir, ".gitconfig")
	if err := os.WriteFile(gitConfig, []byte("[user]\n\tname = Test User\n\temail = test@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	poisonRuntimeDir := filepath.Join(t.TempDir(), "poison-runtime")
	poisonPackStateDir := filepath.Join(poisonRuntimeDir, "packs", "dolt")
	poisonStateFile := filepath.Join(poisonPackStateDir, "dolt-provider-state.json")
	t.Setenv("GC_CITY_RUNTIME_DIR", poisonRuntimeDir)
	t.Setenv("GC_PACK_STATE_DIR", poisonPackStateDir)
	t.Setenv("GC_DOLT_STATE_FILE", poisonStateFile)

	scriptEnv := sanitizedBaseEnv(
		"HOME="+homeDir,
		"GIT_CONFIG_GLOBAL="+gitConfig,
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{
			filepath.Dir(doltPath),
			os.Getenv("PATH"),
		}, string(os.PathListSeparator)),
	)

	runScript := func(args ...string) {
		t.Helper()
		cmd := exec.Command(script, args...)
		cmd.Env = scriptEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	t.Cleanup(func() {
		cmd := exec.Command(script, "stop")
		cmd.Env = scriptEnv
		_ = cmd.Run()
	})

	runScript("start")

	stateFile := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json")
	state, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read provider state file: %v", err)
	}
	if !strings.Contains(string(state), filepath.Join(cityPath, ".beads", "dolt")) {
		t.Fatalf("provider state file should point at .beads/dolt, got:\n%s", state)
	}

	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json")); !os.IsNotExist(err) {
		t.Fatalf("canonical dolt-state.json should not be shell-owned, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("dolt-server.port should not be written by shell start, stat err = %v", err)
	}
	if _, err := os.Stat(poisonStateFile); !os.IsNotExist(err) {
		t.Fatalf("start leaked ambient GC_* state to %q, stat err = %v", poisonStateFile, err)
	}
}

func TestGcBeadsBdStartRetriesAutoPortBindConflict(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	attemptsFile := filepath.Join(stateDir, "attempts")
	portsFile := filepath.Join(stateDir, "ports")

	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := fmt.Sprintf(`#!/bin/sh
set -eu
attempts_file=%q
ports_file=%q
cmd="${1:-}"
case "$cmd" in
  config)
    exit 0
    ;;
  --host)
    exit 0
    ;;
  sql-server)
    config_file=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --config)
          shift
          config_file="$1"
          ;;
      esac
      shift || true
    done
    port=$(awk '/^[[:space:]]*port:/{print $2; exit}' "$config_file")
    printf '%%s\n' "$port" >> "$ports_file"
    count=0
    if [ -f "$attempts_file" ]; then
      count=$(cat "$attempts_file")
    fi
    count=$((count + 1))
    printf '%%s\n' "$count" > "$attempts_file"
    if [ "$count" -eq 1 ]; then
      echo "Starting server with Config HP=\"0.0.0.0:${port}\"|T=\"300000\"|R=\"false\"|L=\"warning\""
      echo "listen tcp 0.0.0.0:${port}: bind: address already in use"
      exit 1
    fi
    sleep 60
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, attemptsFile, portsFile)
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeNC := filepath.Join(binDir, "nc")
	if err := os.WriteFile(fakeNC, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	scriptEnv := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	t.Cleanup(func() {
		cmd := exec.Command(script, "stop")
		cmd.Env = scriptEnv
		_ = cmd.Run()
	})

	cmd := exec.Command(script, "start")
	cmd.Env = scriptEnv
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}

	data, err := os.ReadFile(portsFile)
	if err != nil {
		t.Fatalf("read attempted ports: %v", err)
	}
	ports := strings.Fields(string(data))
	if len(ports) != 2 {
		t.Fatalf("attempted ports = %v, want two startup attempts", ports)
	}
	if ports[0] == ports[1] {
		t.Fatalf("retry reused busy port %s", ports[0])
	}

	state, err := readDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("read provider state: %v", err)
	}
	if got := strconv.Itoa(state.Port); got != ports[1] {
		t.Fatalf("provider state port = %q, want retry port %q", got, ports[1])
	}
}

func TestGcBeadsBdInitRetriesRootStoreVerification(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"mc"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	listCountFile := filepath.Join(t.TempDir(), "bd-list-count")
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := `#!/bin/sh
set -eu
count_file="` + listCountFile + `"
cmd="${1:-}"
case "$cmd" in
  list)
    count=0
    if [ -f "$count_file" ]; then
      count=$(cat "$count_file")
    fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$count_file"
    if [ "$count" -lt 3 ]; then
      exit 1
    fi
    exit 0
    ;;
  config)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	configureTestDoltIdentityEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	if err := initBeadsForDir(cityPath, cityPath, "mc", "mc"); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	data, err := os.ReadFile(listCountFile)
	if err != nil {
		t.Fatalf("read list retry count: %v", err)
	}
	if strings.TrimSpace(string(data)) != "3" {
		t.Fatalf("expected bd list to retry until third attempt, got %q", strings.TrimSpace(string(data)))
	}
}

func writeGcBeadsBdInitEnvCaptureScript(t *testing.T, captureFile string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	body := fmt.Sprintf(`#!/bin/sh
set -eu
op="$1"
shift
case "$op" in
  init)
    printf '%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s
' "${GC_DOLT_HOST:-}" "${GC_DOLT_PORT:-}" "${GC_DOLT_USER:-}" "${GC_DOLT_PASSWORD:-}" "${BEADS_DOLT_SERVER_HOST:-}" "${BEADS_DOLT_SERVER_PORT:-}" "${BEADS_DOLT_SERVER_USER:-}" "${BEADS_DOLT_PASSWORD:-}" "${GC_PACK_STATE_DIR:-}" > %q
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`, captureFile)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestInitAndHookDirExecGcBeadsBdProjectsCanonicalExternalCityEnv(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := `[workspace]
name = "demo"

[dolt]
host = "city-db.example.com"
port = 3307
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureFile := filepath.Join(t.TempDir(), "init-env-city")
	script := writeGcBeadsBdInitEnvCaptureScript(t, captureFile)
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_DOLT_HOST", "ambient.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("GC_PACK_STATE_DIR", "/wrong/.gc/runtime/packs/dolt")
	if err := initAndHookDir(cityPath, cityPath, "gc"); err != nil {
		t.Fatalf("initAndHookDir(city external): %v", err)
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := strings.Join([]string{"city-db.example.com", "3307", "city-user", "city-pass", "city-db.example.com", "3307", "city-user", "city-pass", citylayout.PackStateDir(cityPath, "dolt")}, "|")
	if got != want {
		t.Fatalf("captured external city init env = %q, want %q", got, want)
	}
}

func TestInitAndHookDirExecGcBeadsBdProjectsCanonicalExplicitRigEnv(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
dolt_host = "rig-db.example.com"
dolt_port = "4407"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	rigCfg := `issue_prefix: fe
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: rig-db.example.com
dolt.port: 4407
dolt.user: rig-user
`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(rigCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureFile := filepath.Join(t.TempDir(), "init-env-rig")
	script := writeGcBeadsBdInitEnvCaptureScript(t, captureFile)
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_DOLT_HOST", "ambient.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("GC_PACK_STATE_DIR", "/wrong/.gc/runtime/packs/dolt")
	if err := initAndHookDir(cityPath, rigPath, "fe"); err != nil {
		t.Fatalf("initAndHookDir(explicit rig): %v", err)
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := strings.Join([]string{"rig-db.example.com", "4407", "rig-user", "rig-pass", "rig-db.example.com", "4407", "rig-user", "rig-pass", citylayout.PackStateDir(cityPath, "dolt")}, "|")
	if got != want {
		t.Fatalf("captured explicit rig init env = %q, want %q", got, want)
	}
}

func TestInitAndHookDirExecGcBeadsBdProjectsInheritedExternalRigEnv(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "demo"

[dolt]
host = "city-db.example.com"
port = 3307

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	cityCfg := `issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cityCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	rigCfg := `issue_prefix: fe
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(rigCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureFile := filepath.Join(t.TempDir(), "init-env-inherited-rig")
	script := writeGcBeadsBdInitEnvCaptureScript(t, captureFile)
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_DOLT_HOST", "ambient.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")
	if err := initAndHookDir(cityPath, rigPath, "fe"); err != nil {
		t.Fatalf("initAndHookDir(inherited rig): %v", err)
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := strings.Join([]string{"city-db.example.com", "3307", "city-user", "city-pass", "city-db.example.com", "3307", "city-user", "city-pass", citylayout.PackStateDir(cityPath, "dolt")}, "|")
	if got != want {
		t.Fatalf("captured inherited rig init env = %q, want %q", got, want)
	}
}

func TestHealthBeadsProviderExecGcBeadsBdProjectsCanonicalExternalCityEnv(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureFile := filepath.Join(t.TempDir(), "health-env-city")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
op="$1"
shift
case "$op" in
  health)
    printf '%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s|%%s
' "${GC_DOLT_HOST:-}" "${GC_DOLT_PORT:-}" "${GC_DOLT_USER:-}" "${GC_DOLT_PASSWORD:-}" "${BEADS_DOLT_SERVER_HOST:-}" "${BEADS_DOLT_SERVER_PORT:-}" "${BEADS_DOLT_SERVER_USER:-}" "${BEADS_DOLT_PASSWORD:-}" "${GC_PACK_STATE_DIR:-}" > %q
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`, captureFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_DOLT_HOST", "ambient.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("GC_PACK_STATE_DIR", "/wrong/.gc/runtime/packs/dolt")
	if err := healthBeadsProvider(cityPath); err != nil {
		t.Fatalf("healthBeadsProvider: %v", err)
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := strings.Join([]string{"city-db.example.com", "3307", "city-user", "city-pass", "city-db.example.com", "3307", "city-user", "city-pass", citylayout.PackStateDir(cityPath, "dolt")}, "|")
	if got != want {
		t.Fatalf("captured health env = %q, want %q", got, want)
	}
}

func TestHealthBeadsProviderWaitsForStorePingAfterRecovery(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\n"
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	port := reserveRandomTCPPort(t)
	listener := startTCPListenerProcess(t, port)
	defer func() {
		_ = listener.Process.Kill()
		_ = listener.Wait()
	}()
	state := doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), state); err != nil {
		t.Fatalf("write provider state: %v", err)
	}

	opsFile := filepath.Join(t.TempDir(), "provider-ops.log")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$1" >> %q
case "$1" in
  health)
    echo unhealthy >&2
    exit 1
    ;;
  recover)
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`, opsFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bdCalls := filepath.Join(t.TempDir(), "bd-calls.log")
	countFile := bdCalls + ".count"
	fakeBD := filepath.Join(binDir, "bd")
	fakeBody := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" >> %q
count=0
if [ -f %q ]; then
  count=$(cat %q)
fi
count=$((count + 1))
printf '%%s\n' "$count" > %q
if [ "$1" = "list" ]; then
  if [ "$count" -eq 1 ]; then
    echo '{"error":"failed to open database: dolt circuit breaker is open: server appears down, failing fast (cooldown 5s)"}' >&2
    exit 1
  fi
  printf '[]\n'
  exit 0
fi
echo "unexpected bd command: $*" >&2
exit 2
`, bdCalls, countFile, countFile, countFile)
	if err := os.WriteFile(fakeBD, []byte(fakeBody), 0o755); err != nil {
		t.Fatal(err)
	}

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", strings.Join([]string{binDir, oldPath}, string(os.PathListSeparator)))

	if err := healthBeadsProvider(cityPath); err != nil {
		t.Fatalf("healthBeadsProvider() error = %v", err)
	}

	if _, err := os.Stat(managedDoltStatePath(cityPath)); err != nil {
		t.Fatalf("published dolt runtime state missing after recovery: %v", err)
	}
	calls, err := os.ReadFile(bdCalls)
	if err != nil {
		t.Fatalf("read bd calls: %v", err)
	}
	if got := strings.Count(string(calls), "list --json --limit 0"); got < 2 {
		t.Fatalf("bd ping call count = %d, want at least 2; calls:\n%s", got, string(calls))
	}
	ops, err := os.ReadFile(opsFile)
	if err != nil {
		t.Fatalf("read provider ops: %v", err)
	}
	opLines := strings.Fields(strings.TrimSpace(string(ops)))
	if len(opLines) < 2 || opLines[0] != "health" || opLines[1] != "recover" {
		t.Fatalf("provider ops = %q, want first health then recover", string(ops))
	}
}

func TestHealthBeadsProviderPublishesManagedRuntimeStateWhenHealthyButUnpublished(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath, _ := setupManagedBdWaitTestCity(t)

	if err := os.Remove(managedDoltStatePath(cityPath)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove published dolt runtime state: %v", err)
	}
	if got := currentManagedDoltPort(cityPath); got != "" {
		t.Fatalf("currentManagedDoltPort() = %q, want empty after removing published state", got)
	}

	if err := healthBeadsProvider(cityPath); err != nil {
		t.Fatalf("healthBeadsProvider() error = %v", err)
	}

	state, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("read published dolt runtime state: %v", err)
	}
	if !state.Running {
		t.Fatalf("published.Running = false, want true")
	}
	if got := currentManagedDoltPort(cityPath); got == "" {
		t.Fatal("currentManagedDoltPort() = empty, want published managed port")
	}
}

func TestEnsureBeadsProviderExecGcBeadsBdProjectsCanonicalPackStateDir(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	captureFile := filepath.Join(t.TempDir(), "start-env")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
op="$1"
shift
case "$op" in
  start)
    printf '%%s
' "${GC_PACK_STATE_DIR:-}" > %q
    exit 2
    ;;
  *)
    exit 2
    ;;
esac
`, captureFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_PACK_STATE_DIR", "/wrong/.gc/runtime/packs/dolt")
	if err := ensureBeadsProvider(cityPath); err != nil {
		t.Fatalf("ensureBeadsProvider: %v", err)
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), citylayout.PackStateDir(cityPath, "dolt"); got != want {
		t.Fatalf("captured start GC_PACK_STATE_DIR = %q, want %q", got, want)
	}
}

func TestInitAndHookDirRejectsInvalidCanonicalCityEndpointState(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	invalidCfg := `issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: invalid-db.example.com
dolt.port: 3307
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(invalidCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+writeGcBeadsBdInitEnvCaptureScript(t, filepath.Join(t.TempDir(), "should-not-run")))
	if err := initAndHookDir(cityPath, cityPath, "gc"); err == nil || !strings.Contains(err.Error(), "invalid canonical city endpoint state") {
		t.Fatalf("initAndHookDir() error = %v, want invalid canonical city endpoint state", err)
	}
}

func TestInitAndHookDirExecGcBeadsBdCanonicalizesScopeFilesInGo(t *testing.T) {
	cityPath := t.TempDir()
	writeExecStoreCityConfig(t, cityPath, "demo", "gc", nil)

	captureFile := filepath.Join(t.TempDir(), "canonical-files-owned")
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "gc-beads-bd")
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
op="$1"
shift
case "$op" in
  init)
    printf '%%s
' "${GC_CANONICAL_FILES_OWNED:-}" > %q
    dir="$1"
    mkdir -p "$dir/.beads"
    cat > "$dir/.beads/config.yaml" <<'YAML'
issue-prefix: stale
dolt.auto-start: true
YAML
    cat > "$dir/.beads/metadata.json" <<'JSON'
{"database":"legacy","backend":"legacy","dolt_mode":"embedded","dolt_database":"wrong-db","dolt_host":"127.0.0.1","dolt_server_port":"3307"}
JSON
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`, captureFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	if err := initAndHookDir(cityPath, cityPath, "gc"); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}

	if data, err := os.ReadFile(captureFile); err != nil {
		t.Fatalf("read capture file: %v", err)
	} else if got := strings.TrimSpace(string(data)); got != "" {
		t.Fatalf("GC_CANONICAL_FILES_OWNED = %q, want empty", got)
	}

	cfgState, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("ReadConfigState: %v", err)
	}
	if !ok {
		t.Fatal("expected canonical config.yaml to exist")
	}
	if cfgState.IssuePrefix != "gc" {
		t.Fatalf("IssuePrefix = %q, want gc", cfgState.IssuePrefix)
	}
	if cfgState.EndpointOrigin != contract.EndpointOriginManagedCity {
		t.Fatalf("EndpointOrigin = %q, want %q", cfgState.EndpointOrigin, contract.EndpointOriginManagedCity)
	}
	if cfgState.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("EndpointStatus = %q, want %q", cfgState.EndpointStatus, contract.EndpointStatusVerified)
	}

	metaPath := filepath.Join(cityPath, ".beads", "metadata.json")
	if db, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, metaPath); err != nil {
		t.Fatalf("ReadDoltDatabase: %v", err)
	} else if !ok || db != "hq" {
		t.Fatalf("dolt_database = %q, ok=%v, want hq/true", db, ok)
	}
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	metaText := string(metaData)
	for _, forbidden := range []string{"dolt_host", "dolt_server_port"} {
		if strings.Contains(metaText, forbidden) {
			t.Fatalf("metadata should scrub %s: %s", forbidden, metaText)
		}
	}
}

func TestGcBeadsBdInitTightensBeadsDirPermissions(t *testing.T) {
	tests := []struct {
		name             string
		preexistingDir   bool
		existingMetadata bool
		wantInitPerm     string
	}{
		{name: "fresh_init", preexistingDir: false, existingMetadata: false, wantInitPerm: "700"},
		{name: "preexisting_dir_without_metadata", preexistingDir: true, existingMetadata: false, wantInitPerm: "700"},
		{name: "existing_metadata", preexistingDir: true, existingMetadata: true, wantInitPerm: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}

			if tc.preexistingDir {
				if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o775); err != nil {
					t.Fatal(err)
				}
			}
			if tc.existingMetadata {
				if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gascity"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			if err := MaterializeBuiltinPacks(cityPath); err != nil {
				t.Fatalf("MaterializeBuiltinPacks: %v", err)
			}
			script := gcBeadsBdScriptPath(cityPath)

			binDir := filepath.Join(t.TempDir(), "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}

			initPermFile := filepath.Join(t.TempDir(), "bd-init-perm")
			fakeBd := filepath.Join(binDir, "bd")
			fakeBdScript := `#!/bin/sh
set -eu
perm_file="` + initPermFile + `"
case "${1:-}" in
  init)
    last=""
    for arg in "$@"; do
      last="$arg"
    done
    if [ -d "$last/.beads" ]; then
      if stat -c %a "$last/.beads" >/dev/null 2>&1; then
        stat -c %a "$last/.beads" > "$perm_file"
      else
        stat -f %Lp "$last/.beads" > "$perm_file"
      fi
    else
      printf 'missing
' > "$perm_file"
    fi
    mkdir -p "$last/.beads"
    chmod 775 "$last/.beads"
    cat > "$last/.beads/metadata.json" <<'JSON'
{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gascity"}
JSON
    exit 0
    ;;
  config|migrate|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
			if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
				t.Fatal(err)
			}

			fakeDolt := filepath.Join(binDir, "dolt")
			if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(script, "init", cityPath, "gc", "gascity")
			cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
				"GC_CITY_PATH="+cityPath,
				"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
			)...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
			}

			if tc.wantInitPerm == "" {
				if _, err := os.Stat(initPermFile); !os.IsNotExist(err) {
					t.Fatalf("bd init should not run for existing metadata, stat err=%v", err)
				}
			} else {
				data, err := os.ReadFile(initPermFile)
				if err != nil {
					t.Fatalf("read init perm: %v", err)
				}
				got := strings.TrimSpace(string(data))
				if len(got) > 3 {
					got = got[len(got)-3:]
				}
				if got != tc.wantInitPerm {
					t.Fatalf("bd init saw .beads perm %q, want effective bits %q", strings.TrimSpace(string(data)), tc.wantInitPerm)
				}
			}

			info, err := os.Stat(filepath.Join(cityPath, ".beads"))
			if err != nil {
				t.Fatalf("stat .beads: %v", err)
			}
			if got := info.Mode().Perm(); got != beadsDirPerm {
				t.Fatalf(".beads perm = %o, want %o", got, beadsDirPerm)
			}
		})
	}
}

func TestGcBeadsBdInitFailsWhenBeadsDirPermissionsCannotBeTightened(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o775); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	realChmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatalf("LookPath(chmod): %v", err)
	}
	fakeChmod := filepath.Join(binDir, "chmod")
	fakeChmodScript := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "$#" -ge 2 ] && [ "$1" = "700" ] && [ "$2" = %q ]; then
  echo "chmod blocked" >&2
  exit 1
fi
exec %q "$@"
`, filepath.Join(cityPath, ".beads"), realChmod)
	if err := os.WriteFile(fakeChmod, []byte(fakeChmodScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeBd := filepath.Join(binDir, "bd")
	if err := os.WriteFile(fakeBd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "gascity")
	cmd.Env = sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc-beads-bd init unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(string(out), "failed to set "+filepath.Join(cityPath, ".beads")+" permissions to 700") {
		t.Fatalf("init error = %q, want chmod failure", string(out))
	}
}

func TestGcBeadsBdInitPinsManagedDoltEnvForBdSubcommands(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := `#!/bin/sh
set -eu
capture_dir="` + captureDir + `"
cmd="${1:-}"
record() {
  name="$1"
  printf '%s|%s|%s|%s|%s\n' "${GC_DOLT_HOST:-}" "${GC_DOLT_PORT:-}" "${BEADS_DOLT_SERVER_HOST:-}" "${BEADS_DOLT_SERVER_PORT:-}" "${BEADS_DIR:-}" > "$capture_dir/$name"
}
case "$cmd" in
  init)
    last=""
    for arg in "$@"; do
      last="$arg"
    done
    mkdir -p "$last/.beads"
    record init.env
    exit 0
    ;;
  config)
    record config.env
    exit 0
    ;;
  migrate)
    record migrate.env
    exit 0
    ;;
  list)
    record list.env
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configureTestDoltIdentityEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	t.Setenv("GC_DOLT_HOST", "rig-db.example.com")
	t.Setenv("GC_DOLT_PORT", "3307")

	if err := initBeadsForDir(cityPath, rigDir, "re", "re"); err != nil {
		t.Fatalf("initBeadsForDir: %v", err)
	}

	wantPinned := strings.Join([]string{
		"rig-db.example.com",
		"3307",
		"rig-db.example.com",
		"3307",
		filepath.Join(rigDir, ".beads"),
	}, "|")
	for _, name := range []string{"config.env", "migrate.env"} {
		data, err := os.ReadFile(filepath.Join(captureDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got := strings.TrimSpace(string(data)); got != wantPinned {
			t.Fatalf("%s = %q, want %q", name, got, wantPinned)
		}
	}
	listData, err := os.ReadFile(filepath.Join(captureDir, "list.env"))
	if err != nil {
		t.Fatalf("read list.env: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(listData)), "|")
	if len(parts) != 5 {
		t.Fatalf("list.env = %q, want 5 fields", strings.TrimSpace(string(listData)))
	}
	if parts[0] != "rig-db.example.com" || parts[1] != "3307" {
		t.Fatalf("list.env host/port = %q|%q, want rig-db.example.com|3307", parts[0], parts[1])
	}
	if parts[4] != filepath.Join(rigDir, ".beads") {
		t.Fatalf("list.env BEADS_DIR = %q, want %q", parts[4], filepath.Join(rigDir, ".beads"))
	}
}

func TestGcBeadsBdInitBackfillsRepoIDMigrationWhenMetadataExistsWithoutProjectID(t *testing.T) {
	skipSlowCmdGCTest(t, "runs the materialized gc-beads-bd init script with GC_BIN helper; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gascity"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := fmt.Sprintf(`#!/bin/sh
set -eu
capture_dir=%q
cmd="${1:-}"
case "$cmd" in
  init)
    : > "$capture_dir/init.called"
    exit 0
    ;;
  migrate)
    : > "$capture_dir/migrate.called"
    python3 - <<'PY' "$PWD/.beads/metadata.json"
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
data["project_id"] = "backfilled-project-id"
path.write_text(json.dumps(data, indent=2) + "\n")
PY
    exit 0
    ;;
  config|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, captureDir)
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "gascity")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+currentGCBinaryForTests(t),
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(captureDir, "migrate.called")); err != nil {
		t.Fatalf("expected migrate to run on metadata fast path, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(captureDir, "init.called")); !os.IsNotExist(err) {
		t.Fatalf("bd init should be skipped on metadata fast path, stat err = %v", err)
	}
	metaData, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("ReadFile(metadata.json): %v", err)
	}
	if !strings.Contains(string(metaData), `"project_id": "backfilled-project-id"`) {
		t.Fatalf("metadata.json missing backfilled project_id:\n%s", metaData)
	}
}

func TestGcBeadsBdInitUsesProjectIDHelperWhenRepoIDMigrationFails(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gascity"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := fmt.Sprintf(`#!/bin/sh
set -eu
capture_dir=%q
cmd="${1:-}"
case "$cmd" in
  init)
    : > "$capture_dir/init.called"
    exit 0
    ;;
  migrate)
    : > "$capture_dir/migrate.called"
    echo 'failed to compute repository ID: not a git repository' >&2
    exit 1
    ;;
  config|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, captureDir)
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeGC := filepath.Join(binDir, "gc-helper")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
capture_dir=%q
cmd="$1 $2"
shift 2
case "$cmd" in
  'dolt-state ensure-project-id')
    metadata=''
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --metadata)
          metadata="$2"
          shift 2
          ;;
        --host|--port|--user|--database)
          shift 2
          ;;
        *)
          exit 64
          ;;
      esac
    done
    : > "$capture_dir/helper.called"
    python3 - <<'PY' "$metadata"
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
data['project_id'] = 'helper-project-id'
path.write_text(json.dumps(data, indent=2) + '\n')
PY
    printf 'project_id\thelper-project-id\nmetadata_updated\ttrue\ndatabase_updated\ttrue\nsource\tgenerated\n'
    ;;
  'dolt-config normalize-scope')
    : > "$capture_dir/normalize.called"
    exit 0
    ;;
  *)
    echo "unexpected gc helper args: $cmd $*" >&2
    exit 64
    ;;
esac
`, captureDir)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "gascity")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+fakeGC,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(captureDir, "migrate.called")); err != nil {
		t.Fatalf("expected migrate attempt before helper fallback, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(captureDir, "helper.called")); err != nil {
		t.Fatalf("expected project-id helper fallback, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(captureDir, "normalize.called")); err != nil {
		t.Fatalf("expected normalize helper call, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(captureDir, "init.called")); !os.IsNotExist(err) {
		t.Fatalf("bd init should be skipped on metadata fast path, stat err = %v", err)
	}
	metaData, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("ReadFile(metadata.json): %v", err)
	}
	if !strings.Contains(string(metaData), `"project_id": "helper-project-id"`) {
		t.Fatalf("metadata.json missing helper project_id:\n%s", metaData)
	}
}

func TestGcBeadsBdInitSkipsRepoIDMigrationWhenProjectIDAlreadyPresent(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := `#!/bin/sh
set -eu
capture_dir="` + captureDir + `"
cmd="${1:-}"
case "$cmd" in
  init)
    last=""
    for arg in "$@"; do
      last="$arg"
    done
    mkdir -p "$last/.beads"
    cat > "$last/.beads/metadata.json" <<'JSON'
{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gascity","project_id":"already-present"}
JSON
    exit 0
    ;;
  migrate)
    : > "$capture_dir/migrate.called"
    exit 0
    ;;
  config|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "gascity")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+currentGCBinaryForTests(t),
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(captureDir, "migrate.called")); !os.IsNotExist(err) {
		t.Fatalf("migrate should be skipped when project_id already exists, stat err = %v", err)
	}
}

func TestGcBeadsBdInitUsesExplicitDoltDatabaseForRegistration(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"wrong-db","dolt_host":"127.0.0.1","dolt_user":"legacy","dolt_password":"secret","dolt_server_host":"legacy.example.com","dolt_server_port":"3307","dolt_server_user":"legacy-user","dolt_port":"4406"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sqlLog := filepath.Join(t.TempDir(), "dolt-sql.log")
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := `#!/bin/sh
set -eu
case "${1:-}" in
  config|migrate|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "` + sqlLog + `"
exit 0
`
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "gascity")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}

	sqlData, err := os.ReadFile(sqlLog)
	if err != nil {
		t.Fatalf("read sql log: %v", err)
	}
	sqlText := string(sqlData)
	if !strings.Contains(sqlText, "USE `gascity`") {
		t.Fatalf("expected registration probe for explicit database, got:\n%s", sqlText)
	}
	if strings.Contains(sqlText, "USE `wrong-db`") {
		t.Fatalf("should not register against stale metadata identity:\n%s", sqlText)
	}
}

func TestGcBeadsBdInitFastPathNormalizesBeforeBdConfigAndProjectIDBackfill(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"wrong-db","dolt_host":"127.0.0.1","dolt_user":"legacy","dolt_password":"secret","dolt_server_host":"legacy.example.com","dolt_server_port":"3307","dolt_server_user":"legacy-user","dolt_port":"4406"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := fmt.Sprintf(`#!/bin/sh
set -eu
capture_dir=%q
record_db() {
  python3 -c 'import json, pathlib, sys; meta = json.loads(pathlib.Path(sys.argv[1]).read_text()); log = pathlib.Path(sys.argv[2]); db = meta.get("dolt_database", ""); prefix = log.read_text() if log.exists() else ""; log.write_text(prefix + db + "\n")' "$1" "$2"
}
case "${1:-}" in
  config)
    record_db "$PWD/.beads/metadata.json" "$capture_dir/config-db.log"
    exit 0
    ;;
  migrate)
    record_db "$PWD/.beads/metadata.json" "$capture_dir/migrate-db.log"
    python3 -c 'import json, pathlib, sys; path = pathlib.Path(sys.argv[1]); meta = json.loads(path.read_text()); meta["project_id"] = "backfilled-project-id"; path.write_text(json.dumps(meta, indent=2) + "\n")' "$PWD/.beads/metadata.json"
    exit 0
    ;;
  list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, captureDir)
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeGC := filepath.Join(binDir, "gc-helper")
	fakeGCScript := `#!/bin/sh
set -eu
subcmd="$1 $2"
shift 2
case "$subcmd" in
  "dolt-config normalize-scope")
    dir=""
    database=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          shift 2
          ;;
        --dir)
          dir="$2"
          shift 2
          ;;
        --prefix)
          shift 2
          ;;
        --dolt-database)
          database="$2"
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    python3 -c 'import json, pathlib, sys; meta_path = pathlib.Path(sys.argv[1]); database = sys.argv[2]; meta = json.loads(meta_path.read_text()); meta["database"] = "dolt"; meta["backend"] = "dolt"; meta["dolt_mode"] = "server"; meta["dolt_database"] = database; [meta.pop(key, None) for key in ["dolt_host", "dolt_user", "dolt_password", "dolt_server_host", "dolt_server_port", "dolt_server_user", "dolt_port"]]; meta_path.write_text(json.dumps(meta, indent=2) + "\n")' "$dir/.beads/metadata.json" "$database"
    exit 0
    ;;
  "dolt-state ensure-project-id")
    exit 0
    ;;
  *)
    echo "unexpected gc helper args: $subcmd $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "gascity")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+fakeGC,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}

	for _, name := range []string{"config-db.log", "migrate-db.log"} {
		data, err := os.ReadFile(filepath.Join(captureDir, name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		lines := strings.Fields(string(data))
		if len(lines) == 0 {
			t.Fatalf("%s empty", name)
		}
		for _, line := range lines {
			if line != "gascity" {
				t.Fatalf("%s line = %q, want gascity", name, line)
			}
		}
	}
	metaData, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	metaText := string(metaData)
	if !strings.Contains(metaText, `"dolt_database": "gascity"`) || !strings.Contains(metaText, `"project_id": "backfilled-project-id"`) {
		t.Fatalf("metadata = %s", metaText)
	}
}

func TestGcBeadsBdInitFastPathPreservesExistingManagedProbeDatabase(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: strings.ToUpper(managedDoltProbeDatabase),
	}); err != nil {
		t.Fatalf("EnsureCanonicalMetadata: %v", err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	captureDir := t.TempDir()
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := fmt.Sprintf(`#!/bin/sh
set -eu
capture_dir=%q
record_db() {
  python3 -c 'import json, pathlib, sys; meta = json.loads(pathlib.Path(sys.argv[1]).read_text()); log = pathlib.Path(sys.argv[2]); db = meta.get("dolt_database", ""); prefix = log.read_text() if log.exists() else ""; log.write_text(prefix + db + "\n")' "$1" "$2"
}
case "${1:-}" in
  config)
    record_db "$PWD/.beads/metadata.json" "$capture_dir/config-db.log"
    exit 0
    ;;
  migrate)
    record_db "$PWD/.beads/metadata.json" "$capture_dir/migrate-db.log"
    python3 -c 'import json, pathlib, sys; path = pathlib.Path(sys.argv[1]); meta = json.loads(path.read_text()); meta["project_id"] = "backfilled-project-id"; path.write_text(json.dumps(meta, indent=2) + "\n")' "$PWD/.beads/metadata.json"
    exit 0
    ;;
  list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, captureDir)
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeGC := filepath.Join(binDir, "gc-helper")
	fakeGCScript := `#!/bin/sh
set -eu
subcmd="$1 $2"
shift 2
case "$subcmd" in
  "dolt-config normalize-scope")
    dir=""
    database=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          shift 2
          ;;
        --dir)
          dir="$2"
          shift 2
          ;;
        --prefix)
          shift 2
          ;;
        --dolt-database)
          database="$2"
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    python3 -c 'import json, pathlib, sys; meta_path = pathlib.Path(sys.argv[1]); database = sys.argv[2]; meta = json.loads(meta_path.read_text()); meta["database"] = "dolt"; meta["backend"] = "dolt"; meta["dolt_mode"] = "server"; meta["dolt_database"] = database; [meta.pop(key, None) for key in ["dolt_host", "dolt_user", "dolt_password", "dolt_server_host", "dolt_server_port", "dolt_server_user", "dolt_port"]]; meta_path.write_text(json.dumps(meta, indent=2) + "\n")' "$dir/.beads/metadata.json" "$database"
    exit 0
    ;;
  "dolt-state ensure-project-id")
    exit 0
    ;;
  *)
    echo "unexpected gc helper args: $subcmd $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", strings.ToUpper(managedDoltProbeDatabase))
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+fakeGC,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}

	for _, name := range []string{"config-db.log", "migrate-db.log"} {
		data, err := os.ReadFile(filepath.Join(captureDir, name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		lines := strings.Fields(string(data))
		if len(lines) == 0 {
			t.Fatalf("%s empty", name)
		}
		for _, line := range lines {
			if line != strings.ToUpper(managedDoltProbeDatabase) {
				t.Fatalf("%s line = %q, want %s", name, line, strings.ToUpper(managedDoltProbeDatabase))
			}
		}
	}

	metaData, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	metaText := string(metaData)
	if !strings.Contains(metaText, `"dolt_database": "`+strings.ToUpper(managedDoltProbeDatabase)+`"`) || !strings.Contains(metaText, `"project_id": "backfilled-project-id"`) {
		t.Fatalf("metadata = %s", metaText)
	}
}

func TestEnforceCanonicalScopeMetadataForInitRepairsWrongDoltDatabaseFromExplicitCanonicalIdentity(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"wrong-db","dolt_host":"127.0.0.1","dolt_user":"legacy","dolt_password":"secret","dolt_server_host":"legacy.example.com","dolt_server_port":"3307","dolt_server_user":"legacy-user","dolt_port":"4406"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := enforceCanonicalScopeMetadataForInit(fsys.OSFS{}, cityPath, "gascity"); err != nil {
		t.Fatalf("enforceCanonicalScopeMetadataForInit: %v", err)
	}

	metaData, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	metaText := string(metaData)
	if !strings.Contains(metaText, `"dolt_database": "gascity"`) {
		t.Fatalf("metadata should be repaired to canonical database:\n%s", metaText)
	}
	for _, forbidden := range []string{"dolt_host", "dolt_user", "dolt_password", "dolt_server_host", "dolt_server_port", "dolt_server_user", "dolt_port"} {
		if strings.Contains(metaText, forbidden) {
			t.Fatalf("metadata should scrub deprecated field %s:\n%s", forbidden, metaText)
		}
	}
}

func TestEnforceCanonicalScopeMetadataForInitScrubsDeprecatedMetadataEndpointAuthFields(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"legacy","backend":"legacy","dolt_mode":"embedded","dolt_database":"wrong-db","custom":"keep","dolt_host":"127.0.0.1","dolt_user":"legacy-user","dolt_password":"legacy-pass","dolt_server_host":"legacy.example.com","dolt_server_port":"3307","dolt_server_user":"legacy-server-user","dolt_port":"4406"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := enforceCanonicalScopeMetadataForInit(fsys.OSFS{}, cityPath, "gascity"); err != nil {
		t.Fatalf("enforceCanonicalScopeMetadataForInit: %v", err)
	}
	if err := enforceCanonicalScopeMetadataForInit(fsys.OSFS{}, cityPath, "gascity"); err != nil {
		t.Fatalf("second enforceCanonicalScopeMetadataForInit: %v", err)
	}

	metaData, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	for _, forbidden := range []string{"dolt_host", "dolt_user", "dolt_password", "dolt_server_host", "dolt_server_port", "dolt_server_user", "dolt_port"} {
		if _, ok := meta[forbidden]; ok {
			t.Fatalf("metadata should scrub %s: %s", forbidden, string(metaData))
		}
	}
	for key, want := range map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": "gascity",
		"custom":        "keep",
	} {
		if got := strings.TrimSpace(fmt.Sprint(meta[key])); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestGcBeadsBdInitPreservesMetadataIdentityWhenCanonicalUnknownAndDatabaseMustBeCreated(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gascity"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sqlLog := filepath.Join(t.TempDir(), "dolt-sql.log")
	createdFile := filepath.Join(t.TempDir(), "created-gascity")

	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := `#!/bin/sh
set -eu
case "${1:-}" in
  list|config|migrate)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := `#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
printf '%s\n' "$query" >> "` + sqlLog + `"
case "$query" in
  'USE ` + "`gascity`" + `')
    if [ -f "` + createdFile + `" ]; then
      exit 0
    fi
    exit 1
    ;;
  'CREATE DATABASE IF NOT EXISTS ` + "`gascity`" + `')
    : > "` + createdFile + `"
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}

	metaData, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if got := string(metaData); !strings.Contains(got, `"dolt_database":"gascity"`) && !strings.Contains(got, `"dolt_database": "gascity"`) {
		t.Fatalf("metadata should preserve existing database identity:\n%s", got)
	}

	sqlData, err := os.ReadFile(sqlLog)
	if err != nil {
		t.Fatalf("read sql log: %v", err)
	}
	sqlText := string(sqlData)
	if !strings.Contains(sqlText, "CREATE DATABASE IF NOT EXISTS `gascity`") {
		t.Fatalf("expected canonical database creation, got:\n%s", sqlText)
	}
	if strings.Contains(sqlText, "CREATE DATABASE IF NOT EXISTS `gc`") {
		t.Fatalf("should not create prefix database when preserving metadata identity:\n%s", sqlText)
	}
}

// TestGcBeadsBdInitFastPathRepairsRuntimeConfigDirectly guards the fix for
// bd v1.0.3 rejecting DB-backed config writes during the managed fast path
// after the schema already exists. In that state, the script should repair
// issue_prefix and types.custom directly without falling back to bd init.
func TestGcBeadsBdInitFastPathRepairsRuntimeConfigDirectly(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed metadata.json, simulating seedDeferredManagedBeadsBeforeProviderReadiness
	// writing it before Dolt starts (the trigger for the fast path on a fresh city).
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	initArgsFile := filepath.Join(t.TempDir(), "unexpected-bd-init-args")
	sqlLogFile := filepath.Join(t.TempDir(), "dolt-sql-args")
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := fmt.Sprintf(`#!/bin/sh
set -eu
cmd="${1:-}"
case "$cmd" in
  config)
    sub="${2:-}"
    key="${3:-}"
    if [ "$sub" = "set" ] && { [ "$key" = "issue_prefix" ] || [ "$key" = "types.custom" ]; }; then
      echo "$key must not be set through bd config set" >&2
      exit 2
    fi
    exit 0
    ;;
  init)
    printf '%%s\n' "$@" > %q
    echo "bd init fallback should not run" >&2
    exit 2
    ;;
  migrate|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, initArgsFile)
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %q\nexit 0\n", sqlLogFile)
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "hq")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}

	if data, err := os.ReadFile(initArgsFile); err == nil {
		t.Fatalf("bd init fallback unexpectedly ran with argv:\n%s", data)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat bd init argv: %v", err)
	}

	sqlData, err := os.ReadFile(sqlLogFile)
	if err != nil {
		t.Fatalf("read dolt SQL log: %v", err)
	}
	sqlText := string(sqlData)
	for _, want := range []string{
		"SELECT 1 FROM config LIMIT 1",
		"USE `hq`",
		"VALUES ('issue_prefix', 'gc') ON DUPLICATE KEY UPDATE",
		"VALUES ('types.custom', 'molecule,convoy,message,event,gate,merge-request,agent,role,rig,session,spec,convergence') ON DUPLICATE KEY UPDATE",
	} {
		if !strings.Contains(sqlText, want) {
			t.Fatalf("dolt SQL log missing %q:\n%s", want, sqlText)
		}
	}
}

func TestGcBeadsBdInitMetadataOnlyFallsThroughToForcedBdInitWithPinnedDatabaseWhenSchemaMissing(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	initArgsFile := filepath.Join(t.TempDir(), "bd-init-args")
	initCountFile := filepath.Join(t.TempDir(), "bd-init-count")
	sqlLogFile := filepath.Join(t.TempDir(), "dolt-sql-args")
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := fmt.Sprintf(`#!/bin/sh
set -eu
cmd="${1:-}"
case "$cmd" in
  init)
    has_force=false
    for arg in "$@"; do
      if [ "$arg" = "--force" ]; then
        has_force=true
      fi
    done
    if [ "$has_force" != "true" ]; then
      echo "bd init fallback must force reinitialize existing workspace" >&2
      exit 2
    fi
    printf '1\n' > %q
    printf '%%s\n' "$@" > %q
    exit 0
    ;;
  config|migrate|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, initCountFile, initArgsFile)
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := fmt.Sprintf(`#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
printf '%%s\n' "$query" >> %q
case "$query" in
  'USE `+"`hq`"+`; SELECT 1 FROM config LIMIT 1')
    if [ ! -f %q ]; then
      echo "table not found: config" >&2
      exit 1
    fi
    exit 0
    ;;
  'USE `+"`hq`"+`; INSERT INTO config (`+"`key`"+`, value) VALUES ('\''types.custom'\'', '\''molecule,convoy,message,event,gate,merge-request,agent,role,rig,session,spec,convergence'\'') ON DUPLICATE KEY UPDATE value = VALUES(value)')
    exit 0
    ;;
  'USE `+"`hq`"+`; INSERT INTO config (`+"`key`"+`, value) VALUES ('\''issue_prefix'\'', '\''gc'\'') ON DUPLICATE KEY UPDATE value = VALUES(value)')
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, sqlLogFile, initCountFile)
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "hq")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(initArgsFile)
	if err != nil {
		t.Fatalf("expected bd init fallback to run: %v", err)
	}
	got := string(data)
	for _, want := range []string{"--force", "--server", "-p", "gc", "--database", "hq", cityPath} {
		if !strings.Contains(got, want) {
			t.Fatalf("bd init argv missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "-p hq") {
		t.Fatalf("bd init should keep visible prefix gc while pinning database hq:\n%s", got)
	}
}

func TestGcBeadsBdInitWaitsForSchemaVisibilityBeforeRuntimeRepair(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	probeCountFile := filepath.Join(t.TempDir(), "schema-probe-count")
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := `#!/bin/sh
set -eu
case "${1:-}" in
  init|config|migrate|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := fmt.Sprintf(`#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
case "$query" in
  'USE `+"`hq`"+`; SELECT 1 FROM config LIMIT 1')
    count=0
    if [ -f %q ]; then
      count=$(cat %q)
    fi
    count=$((count + 1))
    printf '%%s\n' "$count" > %q
    if [ "$count" -lt 3 ]; then
      echo "table not found: config" >&2
      exit 1
    fi
    exit 0
    ;;
  'USE `+"`hq`"+`; INSERT INTO config (`+"`key`"+`, value) VALUES ('\''types.custom'\'', '\''molecule,convoy,message,event,gate,merge-request,agent,role,rig,session,spec,convergence'\'') ON DUPLICATE KEY UPDATE value = VALUES(value)')
    if [ ! -f %q ] || [ "$(cat %q)" -lt 3 ]; then
      echo "table not found: config" >&2
      exit 1
    fi
    exit 0
    ;;
  'USE `+"`hq`"+`; INSERT INTO config (`+"`key`"+`, value) VALUES ('\''issue_prefix'\'', '\''gc'\'') ON DUPLICATE KEY UPDATE value = VALUES(value)')
    if [ ! -f %q ] || [ "$(cat %q)" -lt 3 ]; then
      echo "table not found: config" >&2
      exit 1
    fi
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, probeCountFile, probeCountFile, probeCountFile, probeCountFile, probeCountFile, probeCountFile, probeCountFile)
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "hq")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(probeCountFile)
	if err != nil {
		t.Fatalf("read schema probe count: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "3" {
		t.Fatalf("schema probe count = %q, want 3", got)
	}
}

func TestGcBeadsBdInitRetriesPlainInitWhenSchemaStillMissingAfterSuccess(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	initCountFile := filepath.Join(t.TempDir(), "bd-init-count")
	initArgsFile := filepath.Join(t.TempDir(), "bd-init-args")
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-}" in
  init)
    count=0
    if [ -f %q ]; then
      count=$(cat %q)
    fi
    count=$((count + 1))
    printf '%%s\n' "$count" > %q
    printf '%%s\n' "$*" >> %q
    exit 0
    ;;
  config|migrate|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, initCountFile, initCountFile, initCountFile, initArgsFile)
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := fmt.Sprintf(`#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
case "$query" in
  'USE `+"`hq`"+`')
    exit 0
    ;;
  'CREATE DATABASE IF NOT EXISTS `+"`hq`"+`')
    exit 0
    ;;
  'DROP DATABASE IF EXISTS `+"`beads_gc`"+`')
    exit 0
    ;;
  'USE `+"`hq`"+`; SELECT 1 FROM config LIMIT 1')
    count=0
    if [ -f %q ]; then
      count=$(cat %q)
    fi
    if [ "$count" -lt 2 ]; then
      echo "table not found: config" >&2
      exit 1
    fi
    exit 0
    ;;
  'USE `+"`hq`"+`; INSERT INTO config (`+"`key`"+`, value) VALUES ('\''types.custom'\'', '\''molecule,convoy,message,event,gate,merge-request,agent,role,rig,session,spec,convergence'\'') ON DUPLICATE KEY UPDATE value = VALUES(value)')
    count=0
    if [ -f %q ]; then
      count=$(cat %q)
    fi
    if [ "$count" -lt 2 ]; then
      echo "table not found: config" >&2
      exit 1
    fi
    exit 0
    ;;
  'USE `+"`hq`"+`; INSERT INTO config (`+"`key`"+`, value) VALUES ('\''issue_prefix'\'', '\''gc'\'') ON DUPLICATE KEY UPDATE value = VALUES(value)')
    count=0
    if [ -f %q ]; then
      count=$(cat %q)
    fi
    if [ "$count" -lt 2 ]; then
      echo "table not found: config" >&2
      exit 1
    fi
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, initCountFile, initCountFile, initCountFile, initCountFile, initCountFile, initCountFile)
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "hq")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}

	countData, err := os.ReadFile(initCountFile)
	if err != nil {
		t.Fatalf("read init count: %v", err)
	}
	if got := strings.TrimSpace(string(countData)); got != "2" {
		t.Fatalf("bd init count = %q, want 2", got)
	}

	argsData, err := os.ReadFile(initArgsFile)
	if err != nil {
		t.Fatalf("read init args: %v", err)
	}
	gotArgs := string(argsData)
	for _, want := range []string{"init --quiet --server -p gc --database hq"} {
		if !strings.Contains(gotArgs, want) {
			t.Fatalf("bd init retry args missing %q:\n%s", want, gotArgs)
		}
	}
	if strings.Contains(gotArgs, "--force") {
		t.Fatalf("post-init schema retry should rerun plain init, got:\n%s", gotArgs)
	}
}

func TestGcBeadsBdInitDropsMetadataBeforeRetryingInitAfterForcedFallback(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	initCountFile := filepath.Join(t.TempDir(), "bd-init-count")
	initArgsFile := filepath.Join(t.TempDir(), "bd-init-args")
	initStateFile := filepath.Join(t.TempDir(), "bd-init-state")
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-}" in
  init)
    count=0
    if [ -f %q ]; then
      count=$(cat %q)
    fi
    count=$((count + 1))
    printf '%%s\n' "$count" > %q
    if [ -f "$BEADS_DIR/metadata.json" ]; then
      printf 'metadata=yes args=%%s\n' "$*" >> %q
    else
      printf 'metadata=no args=%%s\n' "$*" >> %q
    fi
    printf '%%s\n' "$*" >> %q
    exit 0
    ;;
  config|migrate|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, initCountFile, initCountFile, initCountFile, initStateFile, initStateFile, initArgsFile)
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := fmt.Sprintf(`#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
case "$query" in
  'USE `+"`hq`"+`')
    exit 0
    ;;
  'CREATE DATABASE IF NOT EXISTS `+"`hq`"+`')
    exit 0
    ;;
  'DROP DATABASE IF EXISTS `+"`beads_gc`"+`')
    exit 0
    ;;
  'USE `+"`hq`"+`; SELECT 1 FROM config LIMIT 1')
    count=0
    if [ -f %q ]; then
      count=$(cat %q)
    fi
    if [ "$count" -lt 2 ]; then
      echo "table not found: config" >&2
      exit 1
    fi
    exit 0
    ;;
  'USE `+"`hq`"+`; INSERT INTO config (`+"`key`"+`, value) VALUES ('\''types.custom'\'', '\''molecule,convoy,message,event,gate,merge-request,agent,role,rig,session,spec,convergence'\'') ON DUPLICATE KEY UPDATE value = VALUES(value)')
    exit 0
    ;;
  'USE `+"`hq`"+`; INSERT INTO config (`+"`key`"+`, value) VALUES ('\''issue_prefix'\'', '\''gc'\'') ON DUPLICATE KEY UPDATE value = VALUES(value)')
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, initCountFile, initCountFile)
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", cityPath, "gc", "hq")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}

	stateData, err := os.ReadFile(initStateFile)
	if err != nil {
		t.Fatalf("read init state: %v", err)
	}
	gotState := string(stateData)
	for _, want := range []string{
		"metadata=yes args=init --force --quiet --server -p gc --database hq",
		"metadata=no args=init --quiet --server -p gc --database hq",
	} {
		if !strings.Contains(gotState, want) {
			t.Fatalf("init state missing %q:\n%s", want, gotState)
		}
	}
}

// ── isExternalDolt tests ──────────────────────────────────────────────

func TestIsExternalDoltEnvFallback(t *testing.T) {
	// Without per-city config registered, isExternalDolt falls back to
	// env vars with localhost exclusion (backwards compat).
	tests := []struct {
		host string
		want bool
	}{
		{"", false},
		{"localhost", false},
		{"127.0.0.1", false},
		{"0.0.0.0", false},
		{"mini2.hippo-tilapia.ts.net", true},
		{"10.0.0.5", true},
		{"dolt.example.com", true},
	}
	cityPath := t.TempDir()
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if tt.host == "" {
				t.Setenv("GC_DOLT_HOST", "")
				_ = os.Unsetenv("GC_DOLT_HOST")
			} else {
				t.Setenv("GC_DOLT_HOST", tt.host)
			}
			if got := isExternalDolt(cityPath); got != tt.want {
				t.Errorf("isExternalDolt(%q) with env host=%q = %v, want %v", cityPath, tt.host, got, tt.want)
			}
		})
	}
}

func TestIsExternalDoltWithConfig(t *testing.T) {
	// With per-city config registered, any explicit host or port means
	// "user-managed" regardless of whether host is localhost.
	tests := []struct {
		name string
		cfg  config.DoltConfig
		want bool
	}{
		{"empty config", config.DoltConfig{}, false},
		{"remote host", config.DoltConfig{Host: "mini2.hippo-tilapia.ts.net"}, true},
		{"localhost host", config.DoltConfig{Host: "localhost"}, true},
		{"127.0.0.1 host", config.DoltConfig{Host: "127.0.0.1"}, true},
		{"port only", config.DoltConfig{Port: 3307}, true},
		{"host and port", config.DoltConfig{Host: "mini2", Port: 3307}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GC_DOLT_HOST", "")
			_ = os.Unsetenv("GC_DOLT_HOST")
			cityPath := t.TempDir()
			if tt.cfg.Host != "" || tt.cfg.Port != 0 {
				cityDoltConfigs.Store(cityPath, tt.cfg)
				t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })
			}
			if got := isExternalDolt(cityPath); got != tt.want {
				t.Errorf("isExternalDolt(%q) with cfg=%+v = %v, want %v", cityPath, tt.cfg, got, tt.want)
			}
		})
	}
}

// ── per-city dolt config registration tests ─────────────────────────

func TestDoltHostForCityPrefersConfiguredTargetOverEnv(t *testing.T) {
	cityPath := t.TempDir()
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "config-host.example.com", Port: 3307})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	t.Setenv("GC_DOLT_HOST", "user-override.example.com")

	if got := doltHostForCity(cityPath); got != "config-host.example.com" {
		t.Errorf("doltHostForCity = %q, want configured target %q", got, "config-host.example.com")
	}
}

func TestDoltHostForCityFallsBackToConfig(t *testing.T) {
	cityPath := t.TempDir()
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "mini2.hippo-tilapia.ts.net", Port: 3307})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")

	if got := doltHostForCity(cityPath); got != "mini2.hippo-tilapia.ts.net" {
		t.Errorf("doltHostForCity = %q, want config value %q", got, "mini2.hippo-tilapia.ts.net")
	}
}

func TestDoltPortForCityPrefersConfiguredTargetOverEnv(t *testing.T) {
	cityPath := t.TempDir()
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Port: 3307})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	t.Setenv("GC_DOLT_PORT", "9999")

	if got := doltPortForCity(cityPath); got != "3307" {
		t.Errorf("doltPortForCity = %q, want configured target %q", got, "3307")
	}
}

func TestDoltPortForCityFallsBackToConfig(t *testing.T) {
	cityPath := t.TempDir()
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Port: 3307})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")

	if got := doltPortForCity(cityPath); got != "3307" {
		t.Errorf("doltPortForCity = %q, want config value %q", got, "3307")
	}
}

func TestConfiguredCityDoltTargetPrefersCanonicalConfigOverCompatRegistration(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	host, port, ok := configuredCityDoltTarget(cityPath)
	if !ok {
		t.Fatal("configuredCityDoltTarget() = not configured, want canonical target")
	}
	if host != "canonical-db.example.com" || port != "3307" {
		t.Fatalf("configuredCityDoltTarget() = (%q, %q), want canonical target", host, port)
	}
	if got := doltHostForCity(cityPath); got != "canonical-db.example.com" {
		t.Fatalf("doltHostForCity() = %q, want canonical host", got)
	}
	if got := doltPortForCity(cityPath); got != "3307" {
		t.Fatalf("doltPortForCity() = %q, want canonical port", got)
	}
}

func TestConfiguredCityDoltTargetDoesNotFallbackToCompatRegistrationWhenCanonicalManaged(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	if host, port, ok := configuredCityDoltTarget(cityPath); ok {
		t.Fatalf("configuredCityDoltTarget() = (%q, %q, %v), want no external target", host, port, ok)
	}
	if got := doltHostForCity(cityPath); got != "" {
		t.Fatalf("doltHostForCity() = %q, want empty for managed canonical city", got)
	}
	if got := doltPortForCity(cityPath); got != "" {
		t.Fatalf("doltPortForCity() = %q, want empty for managed canonical city", got)
	}
	if isExternalDolt(cityPath) {
		t.Fatal("isExternalDolt() = true, want false for managed canonical city")
	}
}

func TestConfiguredCityDoltTargetTreatsLegacyExternalConfigAsAuthoritativeWhenPresent(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
dolt.auto-start: false
dolt.host: legacy-db.example.com
dolt.port: 3311
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	host, port, ok := configuredCityDoltTarget(cityPath)
	if !ok {
		t.Fatal("configuredCityDoltTarget() = not configured, want legacy canonical target")
	}
	if host != "legacy-db.example.com" || port != "3311" {
		t.Fatalf("configuredCityDoltTarget() = (%q, %q), want legacy file target", host, port)
	}
}

func TestConfiguredCityDoltTargetFallsBackToCompatRegistrationWhenLegacyFileHasNoEndpointAuthority(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	host, port, ok := configuredCityDoltTarget(cityPath)
	if !ok {
		t.Fatal("configuredCityDoltTarget() = not configured, want compat fallback")
	}
	if host != "compat-db.example.com" || port != "4406" {
		t.Fatalf("configuredCityDoltTarget() = (%q, %q), want compat target", host, port)
	}
	if !isExternalDolt(cityPath) {
		t.Fatal("isExternalDolt() = false, want true for compat fallback before canonicalization")
	}
}

func TestValidateCanonicalCompatDoltDriftRejectsCityMismatch(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS", "bd")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Dolt:      config.DoltConfig{Host: "compat-db.example.com", Port: 4406},
	}

	err := validateCanonicalCompatDoltDrift(cityPath, cfg)
	if err == nil || !strings.Contains(err.Error(), "canonical city endpoint") {
		t.Fatalf("validateCanonicalCompatDoltDrift() error = %v, want canonical city drift error", err)
	}
}

func TestGcBeadsBdStartIgnoresReachableCompatPortFileInput(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	chosenPortFile := filepath.Join(t.TempDir(), "chosen-port")
	fakeDolt := filepath.Join(binDir, "dolt")
	fakeScript := fmt.Sprintf(`#!/bin/sh
set -eu
chosen_file=%q
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    printf '%%s\n' "$port" > "$chosen_file"
    exec python3 - "$port" <<'INNERPY'
import signal
import socket
import sys
import time
port = int(sys.argv[1])
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
`, chosenPortFile)
	if err := os.WriteFile(fakeDolt, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	compatPort := reserveRandomTCPPort(t)
	compatListener := startTCPListenerProcess(t, compatPort)
	providerPort := reserveRandomTCPPort(t)
	providerListener := startTCPListenerProcess(t, providerPort)

	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), doltRuntimeState{
		Running: true,
		PID:     providerListener.Process.Pid,
		Port:    providerPort,
		DataDir: filepath.Join(cityPath, ".beads", "dolt"),
	}); err != nil {
		t.Fatalf("write provider state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt-server.port"), []byte(fmt.Sprintf("%d\n", compatPort)), 0o644); err != nil {
		t.Fatalf("write compat port file: %v", err)
	}

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	runStart := func() {
		t.Helper()
		cmd := exec.Command(script, "start")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
		}
	}
	runStart()
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
		if compatListener != nil && compatListener.Process != nil {
			_ = compatListener.Process.Kill()
		}
		if compatListener != nil {
			_ = compatListener.Wait()
		}
		if providerListener != nil && providerListener.Process != nil {
			_ = providerListener.Process.Kill()
		}
		if providerListener != nil {
			_ = providerListener.Wait()
		}
	})

	if _, err := os.Stat(chosenPortFile); !os.IsNotExist(err) {
		t.Fatalf("expected existing provider-state server to skip launching fake dolt, stat err = %v", err)
	}
	state, err := readDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("read provider state: %v", err)
	}
	if state.Port != providerPort || state.PID != providerListener.Process.Pid {
		t.Fatalf("provider state = %+v, want existing provider listener port=%d pid=%d", state, providerPort, providerListener.Process.Pid)
	}
}

func writeFakeManagedConfigWriterGC(t *testing.T, binDir, invocationFile string) string {
	t.Helper()
	fakeGC := filepath.Join(binDir, "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
invocation_file=%q
subcmd="$1 $2"
shift 2
case "$subcmd" in
  "dolt-config write-managed")
    config_file=""
    host=""
    port=""
    data_dir=""
    log_level="warning"
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --file)
          config_file="$2"
          shift 2
          ;;
        --host)
          host="$2"
          shift 2
          ;;
        --port)
          port="$2"
          shift 2
          ;;
        --data-dir)
          data_dir="$2"
          shift 2
          ;;
        --log-level)
          log_level="$2"
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 65
          ;;
      esac
    done
    printf 'gc dolt-config write-managed\n' >> "$invocation_file"
    mkdir -p "$(dirname "$config_file")"
    cat > "$config_file" <<EOF
# rendered by fake gc
log_level: $log_level
listener:
  port: $port
  host: $host
  max_connections: 1000
  read_timeout_millis: 300000
  write_timeout_millis: 300000

data_dir: "$data_dir"

behavior:
  auto_gc_behavior:
    enable: true
    archive_level: 1
EOF
    ;;
  "dolt-state allocate-port")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--state-file)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state allocate-port\n' >> "$invocation_file"
    printf '3311\n'
    ;;
  "dolt-state inspect-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--port)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state inspect-managed\n' >> "$invocation_file"
    printf 'managed_pid\t0\n'
    printf 'managed_source\t\n'
    printf 'managed_owned\tfalse\n'
    printf 'managed_deleted_inodes\tfalse\n'
    printf 'port_holder_pid\t0\n'
    printf 'port_holder_owned\tfalse\n'
    printf 'port_holder_deleted_inodes\tfalse\n'
    ;;
  "dolt-state existing-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port|--user|--timeout-ms)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state existing-managed\n' >> "$invocation_file"
    printf 'managed_pid\t0\n'
    printf 'managed_owned\tfalse\n'
    printf 'deleted_inodes\tfalse\n'
    printf 'state_port\t0\n'
    printf 'ready\tfalse\n'
    printf 'reusable\tfalse\n'
    ;;
  "dolt-state probe-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state probe-managed\n' >> "$invocation_file"
    printf 'running\tfalse\n'
    printf 'port_holder_pid\t0\n'
    printf 'port_holder_owned\tfalse\n'
    printf 'port_holder_deleted_inodes\tfalse\n'
    printf 'tcp_reachable\tfalse\n'
    ;;
  "dolt-state wait-ready")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port|--user|--pid|--timeout-ms)
          shift 2
          ;;
        --check-deleted)
          shift 1
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state wait-ready\n' >> "$invocation_file"
    printf 'ready\ttrue\n'
    printf 'pid_alive\ttrue\n'
    printf 'deleted_inodes\tfalse\n'
    ;;
  "dolt-state stop-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--port)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state stop-managed\n' >> "$invocation_file"
    printf 'had_pid\ttrue\n'
    printf 'pid\t123\n'
    printf 'forced\tfalse\n'
    ;;
  "dolt-state recover-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port|--user|--log-level|--timeout-ms)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state recover-managed\n' >> "$invocation_file"
    printf 'diagnosed_read_only\t%%s\n' "${GC_FAKE_RECOVER_DIAGNOSED_READ_ONLY:-false}"
    printf 'had_pid\ttrue\n'
    printf 'forced\tfalse\n'
    printf 'ready\ttrue\n'
    printf 'pid\t12345\n'
    printf 'port\t3311\n'
    printf 'healthy\ttrue\n'
    ;;
  "dolt-state query-probe")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --host|--port|--user)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state query-probe\n' >> "$invocation_file"
    ;;
  "dolt-state read-only-check")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --host|--port|--user)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state read-only-check\n' >> "$invocation_file"
    exit 1
    ;;
  "dolt-state preflight-clean")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state preflight-clean\n' >> "$invocation_file"
    ;;
  "dolt-state runtime-layout")
    city=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          city="$2"
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    pack_dir="$city/.gc/runtime/packs/dolt-from-gc"
    printf 'gc dolt-state runtime-layout\n' >> "$invocation_file"
    printf 'GC_PACK_STATE_DIR\t%%s\n' "$pack_dir"
    printf 'GC_DOLT_DATA_DIR\t%%s\n' "$city/.beads/dolt"
    printf 'GC_DOLT_LOG_FILE\t%%s\n' "$pack_dir/dolt.log"
    printf 'GC_DOLT_STATE_FILE\t%%s\n' "$pack_dir/dolt-provider-state.json"
    printf 'GC_DOLT_PID_FILE\t%%s\n' "$pack_dir/dolt.pid"
    printf 'GC_DOLT_LOCK_FILE\t%%s\n' "$pack_dir/dolt.lock"
    printf 'GC_DOLT_CONFIG_FILE\t%%s\n' "$pack_dir/dolt-config.yaml"
    ;;
  "dolt-state start-managed")
    city=""
    host=""
    port=""
    log_level="warning"
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          city="$2"
          shift 2
          ;;
        --host|--port|--user|--log-level|--timeout-ms)
          if [ "$1" = "--host" ]; then host="$2"; fi
          if [ "$1" = "--port" ]; then port="$2"; fi
          if [ "$1" = "--log-level" ]; then log_level="$2"; fi
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 66
          ;;
      esac
    done
    pack_dir="$city/.gc/runtime/packs/dolt-from-gc"
    data_dir="$city/.beads/dolt"
    config_file="$pack_dir/dolt-config.yaml"
    state_file="$pack_dir/dolt-provider-state.json"
    pid_file="$pack_dir/dolt.pid"
    printf 'gc dolt-state start-managed\n' >> "$invocation_file"
    if [ -n "${GC_FAKE_FD9_STATUS_FILE:-}" ]; then
      if (: >&9) 2>/dev/null; then
        printf 'open\n' > "$GC_FAKE_FD9_STATUS_FILE"
      else
        printf 'closed\n' > "$GC_FAKE_FD9_STATUS_FILE"
      fi
    fi
    mkdir -p "$pack_dir" "$data_dir"
    cat > "$config_file" <<EOF
# rendered by fake gc
log_level: $log_level
listener:
  port: $port
  host: $host
  max_connections: 1000
  read_timeout_millis: 300000
  write_timeout_millis: 300000

data_dir: "$data_dir"

behavior:
  auto_gc_behavior:
    enable: true
    archive_level: 1
EOF
    printf '12345\n' > "$pid_file"
    printf '{"running":true,"pid":12345,"port":%%s,"data_dir":"%%s","started_at":"2026-04-14T00:00:00Z"}\n' "$port" "$data_dir" > "$state_file"
    printf 'ready\ttrue\n'
    printf 'pid\t12345\n'
    printf 'port\t%%s\n' "$port"
    printf 'address_in_use\tfalse\n'
    printf 'attempts\t1\n'
    ;;
  "dolt-state write-provider")
    state_file=""
    pid=""
    running=""
    port=""
    data_dir=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --file)
          state_file="$2"
          shift 2
          ;;
        --pid)
          pid="$2"
          shift 2
          ;;
        --running)
          running="$2"
          shift 2
          ;;
        --port)
          port="$2"
          shift 2
          ;;
        --data-dir)
          data_dir="$2"
          shift 2
          ;;
        --started-at)
          shift 2
          ;;
        *)
          echo "unexpected arg: $1" >&2
          exit 67
          ;;
      esac
    done
    printf 'gc dolt-state write-provider\n' >> "$invocation_file"
    mkdir -p "$(dirname "$state_file")"
    printf '{"running":%%s,"pid":%%s,"port":%%s,"data_dir":"%%s","started_at":"2026-04-14T00:00:00Z"}\n' "$running" "$pid" "$port" "$data_dir" > "$state_file"
    ;;
  dolt-state\ *cleanup*|dolt-state\ *preflight*|dolt-state\ *quarantine*|dolt-state\ *stale*)
    printf 'gc %%s\n' "$subcmd" >> "$invocation_file"
    ;;
  *)
    echo "unexpected gc args: $subcmd $*" >&2
    exit 64
    ;;
esac
`, invocationFile)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return fakeGC
}

func writeFakeManagedConfigWriterDolt(t *testing.T, binDir string) {
	t.Helper()
	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := `#!/bin/sh
set -eu
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    data_dir=$(awk '/data_dir:/ {print $2; exit}' "$config_file" | tr -d '"')
    exec python3 - "$port" "$data_dir" <<'INNERPY'
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
data_dir = sys.argv[2]
if data_dir:
    os.makedirs(data_dir, exist_ok=True)
    os.chdir(data_dir)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestGcBeadsBdStartDoesNotReplaceLiveLockFileInode(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not installed")
	}

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "dolt-invocation")
	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := `#!/bin/sh
set -eu
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    printf 'sql-server\n' >> "$GC_FAKE_DOLT_INVOCATION_FILE"
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    data_dir=$(awk '/data_dir:/ {print $2; exit}' "$config_file" | tr -d '"')
    exec python3 - "$port" "$data_dir" <<'INNERPY'
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
data_dir = sys.argv[2]
if data_dir:
    os.makedirs(data_dir, exist_ok=True)
    os.chdir(data_dir)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}

	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		t.Fatal(err)
	}

	readyFile := filepath.Join(t.TempDir(), "holder-ready")
	releaseFile := filepath.Join(t.TempDir(), "holder-release")
	holder := exec.Command("sh", "-c", `
set -eu
lock_file="$1"
ready_file="$2"
release_file="$3"
: > "$lock_file"
exec 9>"$lock_file"
flock 9
printf 'ready\n' > "$ready_file"
while [ ! -f "$release_file" ]; do
  sleep 0.1
 done
`, "sh", layout.LockFile, readyFile, releaseFile)
	holder.Env = sanitizedBaseEnv("PATH=" + os.Getenv("PATH"))
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- holder.Wait()
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(releaseFile, []byte("release\n"), 0o644)
		select {
		case err := <-holderDone:
			if err != nil {
				t.Errorf("lock holder exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = holder.Process.Kill()
			<-holderDone
			t.Errorf("timed out waiting for lock holder to exit")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lock holder to acquire flock")
		}
		time.Sleep(25 * time.Millisecond)
	}

	inodeFor := func(path string) uint64 {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("Stat(%s) did not expose syscall.Stat_t", path)
		}
		return stat.Ino
	}
	beforeInode := inodeFor(layout.LockFile)

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_DOLT_PORT=3311",
		"GC_DOLT_CONCURRENT_START_READY_TIMEOUT_MS=1000",
		"GC_FAKE_DOLT_INVOCATION_FILE="+invocationFile,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("gc-beads-bd start unexpected error: %v", err)
		}
	}
	if exitCode == 0 {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
		t.Fatalf("gc-beads-bd start unexpectedly succeeded while another process held the start lock\n%s", out)
	}
	if exitCode != 1 {
		t.Fatalf("gc-beads-bd start exit %d, want 1\n%s", exitCode, out)
	}
	if !strings.Contains(string(out), "could not acquire dolt start lock") {
		t.Fatalf("gc-beads-bd start output = %q, want lock acquisition failure", out)
	}
	afterInode := inodeFor(layout.LockFile)
	if afterInode != beforeInode {
		t.Fatalf("lock inode changed from %d to %d while original holder was still live", beforeInode, afterInode)
	}
	if invocation, err := os.ReadFile(invocationFile); err == nil && strings.TrimSpace(string(invocation)) != "" {
		t.Fatalf("dolt should not have been invoked while the start lock was held:\n%s", string(invocation))
	}
}

func TestGcBeadsBdStartWaitsForConcurrentStarterSuccess(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	startedFile := filepath.Join(t.TempDir(), "starter-ready")
	nowFile := filepath.Join(t.TempDir(), "gc-now-ms")
	fakeGC := filepath.Join(binDir, "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
invocation_file=%q
started_file=%q
now_file=%q
subcmd="$1 $2"
shift 2
case "$subcmd" in
  "dolt-state now-ms")
    if [ -f "$now_file" ]; then
      now=$(cat "$now_file")
    else
      now=1000000
    fi
    printf '%%s\n' "$now"
    printf '%%s\n' $((now + 250)) > "$now_file"
    ;;
  "dolt-state runtime-layout")
    city=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          city="$2"
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state runtime-layout\n' >> "$invocation_file"
    printf 'GC_PACK_STATE_DIR\t%%s\n' %q
    printf 'GC_DOLT_DATA_DIR\t%%s\n' %q
    printf 'GC_DOLT_LOG_FILE\t%%s\n' %q
    printf 'GC_DOLT_STATE_FILE\t%%s\n' %q
    printf 'GC_DOLT_PID_FILE\t%%s\n' %q
    printf 'GC_DOLT_LOCK_FILE\t%%s\n' %q
    printf 'GC_DOLT_CONFIG_FILE\t%%s\n' %q
    ;;
  "dolt-state existing-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port|--user|--timeout-ms)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state existing-managed\n' >> "$invocation_file"
    if [ -f "$started_file" ]; then
      printf 'managed_pid\t4242\n'
      printf 'managed_owned\ttrue\n'
      printf 'deleted_inodes\tfalse\n'
      printf 'state_port\t3311\n'
      printf 'ready\ttrue\n'
      printf 'reusable\ttrue\n'
    else
      printf 'managed_pid\t0\n'
      printf 'managed_owned\tfalse\n'
      printf 'deleted_inodes\tfalse\n'
      printf 'state_port\t0\n'
      printf 'ready\tfalse\n'
      printf 'reusable\tfalse\n'
    fi
    ;;
  "dolt-state probe-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state probe-managed
' >> "$invocation_file"
    printf 'running	true
'
    printf 'port_holder_pid	4242
'
    printf 'port_holder_owned	true
'
    printf 'port_holder_deleted_inodes	false
'
    printf 'tcp_reachable	true
'
    ;;
  "dolt-state query-probe")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --host|--port|--user)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state query-probe
' >> "$invocation_file"
    if [ -f "$started_file" ]; then
      exit 0
    fi
    exit 1
    ;;
  "dolt-state write-provider")
    state_file=""
    pid=""
    running=""
    port=""
    data_dir=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --file)
          state_file="$2"
          shift 2
          ;;
        --pid)
          pid="$2"
          shift 2
          ;;
        --running)
          running="$2"
          shift 2
          ;;
        --port)
          port="$2"
          shift 2
          ;;
        --data-dir)
          data_dir="$2"
          shift 2
          ;;
        --started-at)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state write-provider
' >> "$invocation_file"
    mkdir -p "$(dirname "$state_file")"
    printf '{"running":%%s,"pid":%%s,"port":%%s,"data_dir":"%%s","started_at":"2026-04-14T00:00:00Z"}
' "$running" "$pid" "$port" "$data_dir" > "$state_file"
    ;;
  *)
    echo "unexpected gc args: $subcmd $*" >&2
    exit 64
    ;;
esac
`, invocationFile, startedFile, nowFile, layout.PackStateDir, layout.DataDir, layout.LogFile, layout.StateFile, layout.PIDFile, layout.LockFile, layout.ConfigFile)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nset -eu\ncase \"${1:-}\" in\n  config)\n    exit 0\n    ;;\n  *)\n    printf 'dolt %s\\n' \"$*\" >> \"$GC_FAKE_DOLT_INVOCATION_FILE\"\n    exit 1\n    ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	invokedDolt := filepath.Join(t.TempDir(), "dolt-invocation")

	readyFile := filepath.Join(t.TempDir(), "holder-ready")
	holder := exec.Command("sh", "-c", `
set -eu
lock_file="$1"
ready_file="$2"
started_file="$3"
: > "$lock_file"
exec 9>"$lock_file"
flock 9
printf 'ready\n' > "$ready_file"
sleep 4
printf 'ready\n' > "$started_file"
sleep 1
`, "sh", layout.LockFile, readyFile, startedFile)
	holder.Env = sanitizedBaseEnv("PATH=" + os.Getenv("PATH"))
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lock holder to acquire flock")
		}
		time.Sleep(25 * time.Millisecond)
	}

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_DOLT_PORT=3311",
		"GC_BIN="+fakeGC,
		"GC_FAKE_DOLT_INVOCATION_FILE="+invokedDolt,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd start failed while concurrent starter was making progress: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(mustReadFile(t, layout.PIDFile))); got != "4242" {
		t.Fatalf("pid file = %q, want 4242", got)
	}
	if _, err := os.Stat(startedFile); err != nil {
		t.Fatalf("concurrent starter success marker missing after start returned: %v", err)
	}
	if invocation, err := os.ReadFile(invokedDolt); err == nil && strings.TrimSpace(string(invocation)) != "" {
		t.Fatalf("dolt should not have been invoked while concurrent starter won:\n%s", string(invocation))
	}
}

func TestGcBeadsBdStartWaitsForSlowConcurrentStarterSuccess(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	startedFile := filepath.Join(t.TempDir(), "starter-ready")
	nowFile := filepath.Join(t.TempDir(), "gc-now-ms")
	fakeGC := filepath.Join(binDir, "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
invocation_file=%q
started_file=%q
now_file=%q
subcmd="$1 $2"
shift 2
case "$subcmd" in
  "dolt-state now-ms")
    if [ -f "$now_file" ]; then
      now=$(cat "$now_file")
    else
      now=1000000
    fi
    printf '%%s\n' "$now"
    printf '%%s\n' $((now + 250)) > "$now_file"
    ;;
  "dolt-state runtime-layout")
    city=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          city="$2"
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state runtime-layout\n' >> "$invocation_file"
    printf 'GC_PACK_STATE_DIR\t%%s\n' %q
    printf 'GC_DOLT_DATA_DIR\t%%s\n' %q
    printf 'GC_DOLT_LOG_FILE\t%%s\n' %q
    printf 'GC_DOLT_STATE_FILE\t%%s\n' %q
    printf 'GC_DOLT_PID_FILE\t%%s\n' %q
    printf 'GC_DOLT_LOCK_FILE\t%%s\n' %q
    printf 'GC_DOLT_CONFIG_FILE\t%%s\n' %q
    ;;
  "dolt-state existing-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port|--user|--timeout-ms)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state existing-managed\n' >> "$invocation_file"
    if [ -f "$started_file" ]; then
      printf 'managed_pid\t4242\n'
      printf 'managed_owned\ttrue\n'
      printf 'deleted_inodes\tfalse\n'
      printf 'state_port\t3311\n'
      printf 'ready\ttrue\n'
      printf 'reusable\ttrue\n'
    else
      printf 'managed_pid\t0\n'
      printf 'managed_owned\tfalse\n'
      printf 'deleted_inodes\tfalse\n'
      printf 'state_port\t0\n'
      printf 'ready\tfalse\n'
      printf 'reusable\tfalse\n'
    fi
    ;;
  "dolt-state probe-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state probe-managed
' >> "$invocation_file"
    printf 'running	true
'
    printf 'port_holder_pid	4242
'
    printf 'port_holder_owned	true
'
    printf 'port_holder_deleted_inodes	false
'
    printf 'tcp_reachable	true
'
    ;;
  "dolt-state query-probe")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --host|--port|--user)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state query-probe
' >> "$invocation_file"
    if [ -f "$started_file" ]; then
      exit 0
    fi
    exit 1
    ;;
  "dolt-state write-provider")
    state_file=""
    pid=""
    running=""
    port=""
    data_dir=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --file)
          state_file="$2"
          shift 2
          ;;
        --pid)
          pid="$2"
          shift 2
          ;;
        --running)
          running="$2"
          shift 2
          ;;
        --port)
          port="$2"
          shift 2
          ;;
        --data-dir)
          data_dir="$2"
          shift 2
          ;;
        --started-at)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state write-provider
' >> "$invocation_file"
    mkdir -p "$(dirname "$state_file")"
    printf '{"running":%%s,"pid":%%s,"port":%%s,"data_dir":"%%s","started_at":"2026-04-14T00:00:00Z"}
' "$running" "$pid" "$port" "$data_dir" > "$state_file"
    ;;
  *)
    echo "unexpected gc args: $subcmd $*" >&2
    exit 64
    ;;
esac
`, invocationFile, startedFile, nowFile, layout.PackStateDir, layout.DataDir, layout.LogFile, layout.StateFile, layout.PIDFile, layout.LockFile, layout.ConfigFile)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nset -eu\ncase \"${1:-}\" in\n  config)\n    exit 0\n    ;;\n  *)\n    printf 'dolt %s\\n' \"$*\" >> \"$GC_FAKE_DOLT_INVOCATION_FILE\"\n    exit 1\n    ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	invokedDolt := filepath.Join(t.TempDir(), "dolt-invocation")

	readyFile := filepath.Join(t.TempDir(), "holder-ready")
	holder := exec.Command("sh", "-c", `
set -eu
lock_file="$1"
ready_file="$2"
started_file="$3"
: > "$lock_file"
exec 9>"$lock_file"
flock 9
printf 'ready\n' > "$ready_file"
sleep 11
printf 'ready\n' > "$started_file"
sleep 1
`, "sh", layout.LockFile, readyFile, startedFile)
	holder.Env = sanitizedBaseEnv("PATH=" + os.Getenv("PATH"))
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lock holder to acquire flock")
		}
		time.Sleep(25 * time.Millisecond)
	}

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_DOLT_PORT=3311",
		"GC_DOLT_CONCURRENT_START_READY_TIMEOUT_MS=12000",
		"GC_BIN="+fakeGC,
		"GC_FAKE_DOLT_INVOCATION_FILE="+invokedDolt,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd start failed while slow concurrent starter was making progress: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(mustReadFile(t, layout.PIDFile))); got != "4242" {
		t.Fatalf("pid file = %q, want 4242", got)
	}
	if _, err := os.Stat(startedFile); err != nil {
		t.Fatalf("concurrent starter success marker missing after start returned: %v", err)
	}
	if invocation, err := os.ReadFile(invokedDolt); err == nil && strings.TrimSpace(string(invocation)) != "" {
		t.Fatalf("dolt should not have been invoked while concurrent starter won:\n%s", string(invocation))
	}
}

func TestGcBeadsBdStartConcurrentWaitPassesRemainingExistingManagedBudget(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	nowFile := filepath.Join(t.TempDir(), "gc-now-ms")
	fakeGC := filepath.Join(binDir, "gc")
	fakeGCScript := fmt.Sprintf(`#!/bin/sh
set -eu
invocation_file=%q
now_file=%q
subcmd="$1 $2"
shift 2
case "$subcmd" in
  "dolt-state now-ms")
    if [ -f "$now_file" ]; then
      step=$(cat "$now_file")
    else
      step=0
    fi
    case "$step" in
      0)
        printf '1000000\n'
        printf '1\n' > "$now_file"
        ;;
      1)
        printf '1000000\n'
        printf '2\n' > "$now_file"
        ;;
      *)
        printf '1001000\n'
        printf '3\n' > "$now_file"
        ;;
    esac
    ;;
  "dolt-state runtime-layout")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state runtime-layout\n' >> "$invocation_file"
    printf 'GC_PACK_STATE_DIR\t%%s\n' %q
    printf 'GC_DOLT_DATA_DIR\t%%s\n' %q
    printf 'GC_DOLT_LOG_FILE\t%%s\n' %q
    printf 'GC_DOLT_STATE_FILE\t%%s\n' %q
    printf 'GC_DOLT_PID_FILE\t%%s\n' %q
    printf 'GC_DOLT_LOCK_FILE\t%%s\n' %q
    printf 'GC_DOLT_CONFIG_FILE\t%%s\n' %q
    ;;
  "dolt-state existing-managed")
    timeout_ms=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port|--user)
          shift 2
          ;;
        --timeout-ms)
          timeout_ms="$2"
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state existing-managed timeout=%%s\n' "$timeout_ms" >> "$invocation_file"
    if [ "${timeout_ms:-0}" -gt 1500 ]; then
      sleep 2
    fi
    printf 'managed_pid\t4242\n'
    printf 'managed_owned\ttrue\n'
    printf 'deleted_inodes\tfalse\n'
    printf 'state_port\t3311\n'
    printf 'ready\tfalse\n'
    printf 'reusable\tfalse\n'
    ;;
  "dolt-state probe-managed")
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --city|--host|--port)
          shift 2
          ;;
        *)
          exit 66
          ;;
      esac
    done
    printf 'gc dolt-state probe-managed\n' >> "$invocation_file"
    printf 'running\tfalse\n'
    printf 'port_holder_pid\t0\n'
    printf 'port_holder_owned\tfalse\n'
    printf 'port_holder_deleted_inodes\tfalse\n'
    printf 'tcp_reachable\tfalse\n'
    ;;
  *)
    echo "unexpected gc args: $subcmd $*" >&2
    exit 64
    ;;
esac
`, invocationFile, nowFile, layout.PackStateDir, layout.DataDir, layout.LogFile, layout.StateFile, layout.PIDFile, layout.LockFile, layout.ConfigFile)
	if err := os.WriteFile(fakeGC, []byte(fakeGCScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nset -eu\ncase \"${1:-}\" in\n  config)\n    exit 0\n    ;;\n  *)\n    exit 1\n    ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	readyFile := filepath.Join(t.TempDir(), "holder-ready")
	holder := exec.Command("sh", "-c", `
set -eu
lock_file="$1"
ready_file="$2"
: > "$lock_file"
exec 9>"$lock_file"
flock 9
printf 'ready\n' > "$ready_file"
sleep 5
`, "sh", layout.LockFile, readyFile)
	holder.Env = sanitizedBaseEnv("PATH=" + os.Getenv("PATH"))
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lock holder to acquire flock")
		}
		time.Sleep(25 * time.Millisecond)
	}

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_DOLT_PORT=3311",
		"GC_DOLT_CONCURRENT_START_READY_TIMEOUT_MS=1000",
		"GC_BIN="+fakeGC,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("gc-beads-bd start unexpected error: %v", err)
		}
	}
	if exitCode != 1 {
		t.Fatalf("gc-beads-bd start exit %d, want 1\n%s", exitCode, out)
	}
	if !strings.Contains(string(out), "could not acquire dolt start lock") {
		t.Fatalf("gc-beads-bd start output = %q, want lock acquisition failure", out)
	}
	invocation := string(mustReadFile(t, invocationFile))
	if strings.Contains(invocation, "timeout=30000") {
		t.Fatalf("existing-managed should not receive the default 30s timeout inside concurrent wait:\n%s", invocation)
	}
	if !strings.Contains(invocation, "timeout=1000") {
		t.Fatalf("existing-managed should receive the remaining wait budget on the first attempt:\n%s", invocation)
	}
}

func TestGcBeadsBdStartUsesGCBinManagedConfigWriter(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	fakeGC := writeFakeManagedConfigWriterGC(t, binDir, invocationFile)
	writeFakeManagedConfigWriterDolt(t, binDir)

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+fakeGC,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
	})

	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	invocationText := strings.TrimSpace(string(invocation))
	for _, want := range []string{"gc dolt-state runtime-layout", "gc dolt-state allocate-port", "gc dolt-state existing-managed", "gc dolt-state probe-managed", "gc dolt-state start-managed"} {
		if !strings.Contains(invocationText, want) {
			t.Fatalf("gc invocation missing %q: %s", want, invocationText)
		}
	}
	configData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt-from-gc", "dolt-config.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(dolt-config.yaml): %v", err)
	}
	if !strings.Contains(string(configData), "# rendered by fake gc") {
		t.Fatalf("dolt-config.yaml was not rendered by GC_BIN:\n%s", string(configData))
	}
	state, err := readDoltRuntimeStateFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt-from-gc", "dolt-provider-state.json"))
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile: %v", err)
	}
	if state.Port == 0 {
		t.Fatalf("provider state port = %d, want non-zero", state.Port)
	}
	if !strings.Contains(string(configData), fmt.Sprintf("port: %d", state.Port)) {
		t.Fatalf("dolt-config.yaml missing helper-selected port %d:\n%s", state.Port, string(configData))
	}
}

func TestGcBeadsBdStartManagedHelperDoesNotInheritStartLockFD(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	fd9StatusFile := filepath.Join(t.TempDir(), "fd9-status")
	fakeGC := writeFakeManagedConfigWriterGC(t, binDir, invocationFile)
	writeFakeManagedConfigWriterDolt(t, binDir)

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+fakeGC,
		"GC_FAKE_FD9_STATUS_FILE="+fd9StatusFile,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
	})

	status := strings.TrimSpace(string(mustReadFile(t, fd9StatusFile)))
	if status != "closed" {
		t.Fatalf("dolt-state start-managed inherited fd 9 = %q, want closed", status)
	}
}

func TestGcBeadsBdStopUsesGCBinStopManagedHelperWhenAvailable(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	fakeGC := writeFakeManagedConfigWriterGC(t, binDir, invocationFile)

	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.PIDFile, []byte("123\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+fakeGC,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "stop")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd stop failed: %v\n%s", err, out)
	}

	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	if !strings.Contains(strings.TrimSpace(string(invocation)), "gc dolt-state stop-managed") {
		t.Fatalf("gc invocation missing stop-managed helper: %s", string(invocation))
	}
}

func TestGcBeadsBdRecoverUsesGCBinRecoverManagedHelperWhenAvailable(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	fakeGC := writeFakeManagedConfigWriterGC(t, binDir, invocationFile)

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+fakeGC,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "recover")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd recover failed: %v\n%s", err, out)
	}

	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("ReadFile(invocation): %v", err)
	}
	if !strings.Contains(strings.TrimSpace(string(invocation)), "gc dolt-state recover-managed") {
		t.Fatalf("gc invocation missing recover-managed helper: %s", string(invocation))
	}
}

func TestGcBeadsBdRecoverHelperPreservesReadOnlyWarning(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	fakeGC := writeFakeManagedConfigWriterGC(t, binDir, invocationFile)

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+fakeGC,
		"GC_FAKE_RECOVER_DIAGNOSED_READ_ONLY=true",
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "recover")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd recover failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "detected read-only dolt server") {
		t.Fatalf("recover output missing read-only warning: %s", string(out))
	}
}

func TestManagedDoltConfigGoWriterMatchesShellFallbackSemantics(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the materialized gc-beads-bd shell fallback; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	goConfigPath := filepath.Join(t.TempDir(), "go", "dolt-config.yaml")
	if err := writeManagedDoltConfigFile(goConfigPath, "0.0.0.0", "3311", filepath.Join(cityPath, ".beads", "dolt"), "info"); err != nil {
		t.Fatalf("writeManagedDoltConfigFile: %v", err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDolt := filepath.Join(binDir, "dolt")
	fakeDoltScript := `#!/bin/sh
set -eu
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    data_dir=$(awk '/data_dir:/ {print $2; exit}' "$config_file" | tr -d '"')
    exec python3 - "$port" "$data_dir" <<'INNERPY'
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
data_dir = sys.argv[2]
if data_dir:
    os.makedirs(data_dir, exist_ok=True)
    os.chdir(data_dir)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeDolt, []byte(fakeDoltScript), 0o755); err != nil {
		t.Fatal(err)
	}
	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_DOLT_PORT=3311",
		"GC_DOLT_LOGLEVEL=info",
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
	})
	shellConfigPath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	goConfig := readManagedDoltConfigForTest(t, goConfigPath)
	shellConfig := readManagedDoltConfigForTest(t, shellConfigPath)
	if goConfig != shellConfig {
		t.Fatalf("managed config mismatch\nGo: %+v\nShell: %+v", goConfig, shellConfig)
	}
}

type managedDoltConfigForTest struct {
	LogLevel string `yaml:"log_level"`
	Listener struct {
		Port               int    `yaml:"port"`
		Host               string `yaml:"host"`
		MaxConnections     int    `yaml:"max_connections"`
		BackLog            int    `yaml:"back_log"`
		MaxConnTimeoutMS   int    `yaml:"max_connections_timeout_millis"`
		ReadTimeoutMillis  int    `yaml:"read_timeout_millis"`
		WriteTimeoutMillis int    `yaml:"write_timeout_millis"`
	} `yaml:"listener"`
	DataDir  string `yaml:"data_dir"`
	Behavior struct {
		AutoGCBehavior struct {
			Enable       bool `yaml:"enable"`
			ArchiveLevel int  `yaml:"archive_level"`
		} `yaml:"auto_gc_behavior"`
	} `yaml:"behavior"`
}

func readManagedDoltConfigForTest(t *testing.T, path string) managedDoltConfigForTest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var cfg managedDoltConfigForTest
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal(%s): %v\n%s", path, err, data)
	}
	return cfg
}

func readDoltStartCountForTest(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read start count: %v", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse start count %q: %v", strings.TrimSpace(string(data)), err)
	}
	return count
}

func TestGcBeadsBdStartIsIdempotentWhenAlreadyRunning(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	countFile := filepath.Join(t.TempDir(), "dolt-start-count")
	fakeDolt := filepath.Join(binDir, "dolt")
	port := freeLoopbackPort(t)
	fakeScript := `#!/bin/sh
set -eu
count_file="` + countFile + `"
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    count=0
    if [ -f "$count_file" ]; then
      count=$(cat "$count_file")
    fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$count_file"
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    data_dir=$(awk '/data_dir:/ {print $2; exit}' "$config_file" | tr -d '"')
    exec python3 - "$port" "$data_dir" <<'INNERPY'
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
data_dir = sys.argv[2]
if data_dir:
    os.makedirs(data_dir, exist_ok=True)
    os.chdir(data_dir)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeDolt, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_DOLT_PORT="+port,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)

	runStart := func() {
		t.Helper()
		cmd := exec.Command(script, "start")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
		}
	}

	runStart()
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
	})

	firstPIDData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.pid"))
	if err != nil {
		t.Fatalf("read first pid file: %v", err)
	}
	firstPID := strings.TrimSpace(string(firstPIDData))
	if firstPID == "" {
		t.Fatal("first pid file is empty")
	}
	initialStartCount := readDoltStartCountForTest(t, countFile)

	runStart()

	secondPIDData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.pid"))
	if err != nil {
		t.Fatalf("read second pid file: %v", err)
	}
	secondPID := strings.TrimSpace(string(secondPIDData))
	if secondPID != firstPID {
		t.Fatalf("repeated start changed pid from %q to %q", firstPID, secondPID)
	}

	if got := readDoltStartCountForTest(t, countFile); got != initialStartCount {
		t.Fatalf("dolt sql-server launch count = %d, want unchanged from initial %d", got, initialStartCount)
	}

	state, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if !strings.Contains(string(state), "\"pid\":"+firstPID) {
		t.Fatalf("provider state file should preserve original pid %s, got: %s", firstPID, state)
	}
}

func TestGcBeadsBdStartRestartsServerHoldingDeletedDataInodes(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	countFile := filepath.Join(t.TempDir(), "dolt-start-count")
	deletedMarkerFile := filepath.Join(t.TempDir(), "deleted-inode-held")
	fakeDolt := filepath.Join(binDir, "dolt")
	port := freeLoopbackPort(t)
	fakeScript := fmt.Sprintf(`#!/bin/sh
set -eu
count_file=%q
data_dir=%q
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    count=0
    if [ -f "$count_file" ]; then
      count=$(cat "$count_file")
    fi
    count=$((count + 1))
    printf '%%s\n' "$count" > "$count_file"
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    exec python3 - "$port" "$data_dir" %q <<'INNERPY'
import os
import signal
import socket
import sys
import time
port = int(sys.argv[1])
data_dir = sys.argv[2]
marker_path = sys.argv[3]
os.makedirs(data_dir, exist_ok=True)
os.chdir(data_dir)
open_file = None
if not os.path.exists(marker_path):
    with open(marker_path, "w") as marker:
        marker.write("held")
    stale = os.path.join(data_dir, "stale-open.txt")
    open_file = open(stale, "w+")
    open_file.write("stale")
    open_file.flush()
    os.unlink(stale)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
`, countFile, filepath.Join(cityPath, ".beads", "dolt"), deletedMarkerFile)
	if err := os.WriteFile(fakeDolt, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_DOLT_PORT="+port,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)

	runStart := func() {
		t.Helper()
		cmd := exec.Command(script, "start")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
		}
	}

	runStart()
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
	})

	firstPIDData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.pid"))
	if err != nil {
		t.Fatalf("read first pid file: %v", err)
	}
	firstPID := strings.TrimSpace(string(firstPIDData))
	if firstPID == "" {
		t.Fatal("first pid file is empty")
	}
	initialStartCount := readDoltStartCountForTest(t, countFile)

	runStart()

	secondPIDData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.pid"))
	if err != nil {
		t.Fatalf("read second pid file: %v", err)
	}
	secondPID := strings.TrimSpace(string(secondPIDData))
	if secondPID == "" {
		t.Fatal("second pid file is empty")
	}

	if got := readDoltStartCountForTest(t, countFile); got <= initialStartCount {
		t.Fatalf("dolt sql-server launch count = %d, want greater than initial %d", got, initialStartCount)
	}
}

func TestGcBeadsBdEnsureReadyDoesNotRestartAfterTransientTCPProbeFailure(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	countFile := filepath.Join(t.TempDir(), "dolt-start-count")
	fakeDolt := filepath.Join(binDir, "dolt")
	port := freeLoopbackPort(t)
	fakeScript := fmt.Sprintf(`#!/bin/sh
set -eu
count_file=%q
case "${1:-}" in
  config)
    exit 0
    ;;
  sql-server)
    count=0
    if [ -f "$count_file" ]; then
      count=$(cat "$count_file")
    fi
    count=$((count + 1))
    printf '%%s\n' "$count" > "$count_file"
    config_file=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--config" ]; then
        config_file="$arg"
        break
      fi
      prev="$arg"
    done
    port=$(awk '/port:/ {print $2; exit}' "$config_file")
    exec python3 - "$port" <<'INNERPY'
import signal
import socket
import sys
import time
port = int(sys.argv[1])
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("0.0.0.0", port))
sock.listen(128)
sock.settimeout(1.0)
def _stop(*_args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    try:
        conn, _ = sock.accept()
        conn.close()
    except socket.timeout:
        continue
INNERPY
    ;;
  *)
    exit 0
    ;;
esac
`, countFile)
	if err := os.WriteFile(fakeDolt, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Must use sanitizedBaseEnv, not append(os.Environ(), ...). Raw
	// inheritance leaks GC_CITY_RUNTIME_DIR / GC_PACK_STATE_DIR /
	// GC_DOLT_STATE_FILE from the user's shell into this script, aiming
	// dolt-provider-state.json at the user's real registered city
	// instead of this test's t.TempDir() — confirmed in the wild on a
	// dev workstation where a previous run of this test clobbered a
	// live city. Regression guard for gastownhall/gascity#938.
	poisonRuntimeDir := filepath.Join(t.TempDir(), "poison-runtime")
	poisonPackStateDir := filepath.Join(poisonRuntimeDir, "packs", "dolt")
	poisonStateFile := filepath.Join(poisonPackStateDir, "dolt-provider-state.json")
	t.Setenv("GC_CITY_RUNTIME_DIR", poisonRuntimeDir)
	t.Setenv("GC_PACK_STATE_DIR", poisonPackStateDir)
	t.Setenv("GC_DOLT_STATE_FILE", poisonStateFile)
	baseEnv := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BIN=",
		"GC_DOLT_PORT="+port,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)

	runScript := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command(script, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gc-beads-bd %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	runScript(baseEnv, "start")
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = baseEnv
		_ = stop.Run()
	})

	firstPIDData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.pid"))
	if err != nil {
		t.Fatalf("read first pid file: %v", err)
	}
	firstPID := strings.TrimSpace(string(firstPIDData))
	if firstPID == "" {
		t.Fatal("first pid file is empty")
	}
	initialStartCount := readDoltStartCountForTest(t, countFile)

	realNC, err := exec.LookPath("nc")
	if err != nil {
		t.Skip("nc not installed")
	}
	shimDir := filepath.Join(t.TempDir(), "shim")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	probeFile := filepath.Join(shimDir, "nc-once")
	shimPath := filepath.Join(shimDir, "nc")
	shim := fmt.Sprintf(`#!/bin/sh
set -eu
probe_file=%q
real_nc=%q
if [ ! -f "$probe_file" ]; then
  : > "$probe_file"
  exit 1
fi
exec "$real_nc" "$@"
`, probeFile, realNC)
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	envWithShim := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"GC_BIN=",
		"GC_DOLT_PORT="+port,
		"PATH="+strings.Join([]string{shimDir, binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)

	runScript(envWithShim, "ensure-ready")

	secondPIDData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.pid"))
	if err != nil {
		t.Fatalf("read second pid file: %v", err)
	}
	secondPID := strings.TrimSpace(string(secondPIDData))
	if secondPID != firstPID {
		t.Fatalf("ensure-ready changed pid from %q to %q after transient tcp probe failure", firstPID, secondPID)
	}

	if got := readDoltStartCountForTest(t, countFile); got != initialStartCount {
		t.Fatalf("dolt sql-server launch count = %d, want unchanged from initial %d", got, initialStartCount)
	}
	if _, err := os.Stat(poisonStateFile); !os.IsNotExist(err) {
		t.Fatalf("ensure-ready leaked ambient GC_* state to %q, stat err = %v", poisonStateFile, err)
	}
}

func TestValidateCanonicalCompatDoltDriftRejectsInheritedRigCompatOverrideWithRelativePath(t *testing.T) {
	cityPath := t.TempDir()
	rigRel := "frontend"
	rigPath := filepath.Join(cityPath, rigRel)
	t.Setenv("GC_BEADS", "bd")
	for _, dir := range []string{cityPath, rigPath} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(`issue_prefix: fe
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{{
			Name:     "frontend",
			Path:     rigRel,
			Prefix:   "fe",
			DoltHost: "rig-db.example.com",
			DoltPort: "4406",
		}},
	}
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if chdirErr := os.Chdir(origWD); chdirErr != nil {
			t.Fatalf("restore cwd: %v", chdirErr)
		}
	}()

	err = validateCanonicalCompatDoltDrift(cityPath, cfg)
	if err == nil || !strings.Contains(err.Error(), `rig "frontend"`) {
		t.Fatalf("validateCanonicalCompatDoltDrift() error = %v, want inherited rig compat error", err)
	}
}

func TestValidateCanonicalCompatDoltDriftRejectsInheritedRigCompatOverride(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	t.Setenv("GC_BEADS", "bd")
	for _, dir := range []string{cityPath, rigPath} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(`issue_prefix: fe
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{{
			Name:     "frontend",
			Path:     rigPath,
			Prefix:   "fe",
			DoltHost: "rig-db.example.com",
			DoltPort: "4406",
		}},
	}

	err := validateCanonicalCompatDoltDrift(cityPath, cfg)
	if err == nil || !strings.Contains(err.Error(), `rig "frontend"`) {
		t.Fatalf("validateCanonicalCompatDoltDrift() error = %v, want inherited rig compat error", err)
	}
}

func TestStartBeadsLifecycleFailsOnCanonicalCompatDoltDrift(t *testing.T) {
	cityPath := t.TempDir()
	callLog := filepath.Join(cityPath, "op-calls.log")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$1\" >> "+callLog+"\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "bd")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Dolt:      config.DoltConfig{Host: "compat-db.example.com", Port: 4406},
	}
	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err == nil || !strings.Contains(err.Error(), "canonical city endpoint") {
		t.Fatalf("startBeadsLifecycle() error = %v, want canonical city drift error", err)
	}
	if _, err := os.Stat(callLog); !os.IsNotExist(err) {
		t.Fatalf("provider should not start before drift validation, stat err = %v", err)
	}
}

func TestConfiguredCityDoltTargetDoesNotFallbackToCompatRegistrationWhenCanonicalCityOriginInvalid(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "env-db.example.com")
	t.Setenv("GC_DOLT_PORT", "5510")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: invalid-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	if host, port, ok := configuredCityDoltTarget(cityPath); ok {
		t.Fatalf("configuredCityDoltTarget() = (%q, %q, %v), want no target for invalid canonical city origin", host, port, ok)
	}
	if got := doltHostForCity(cityPath); got != "" {
		t.Fatalf("doltHostForCity() = %q, want empty for invalid canonical city origin", got)
	}
	if got := doltPortForCity(cityPath); got != "" {
		t.Fatalf("doltPortForCity() = %q, want empty for invalid canonical city origin", got)
	}
	if isExternalDolt(cityPath) {
		t.Fatal("isExternalDolt() = true, want false for invalid canonical city origin")
	}
}

func TestDoltHostAndPortForCityDoNotFallbackToEnvWhenCanonicalCityOriginInvalid(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: invalid-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT_HOST", "env-db.example.com")
	t.Setenv("GC_DOLT_PORT", "4406")

	if got := doltHostForCity(cityPath); got != "" {
		t.Fatalf("doltHostForCity() = %q, want empty for invalid canonical city origin", got)
	}
	if got := doltPortForCity(cityPath); got != "" {
		t.Fatalf("doltPortForCity() = %q, want empty for invalid canonical city origin", got)
	}
	if isExternalDolt(cityPath) {
		t.Fatal("isExternalDolt() = true, want false for invalid canonical city origin even with env override")
	}
}

func TestDoltHostAndPortForCityDoNotFallbackToEnvWhenManagedCanonicalTracksEndpoint(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: stale-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT_HOST", "env-db.example.com")
	t.Setenv("GC_DOLT_PORT", "4406")

	if got := doltHostForCity(cityPath); got != "" {
		t.Fatalf("doltHostForCity() = %q, want empty for invalid managed canonical city", got)
	}
	if got := doltPortForCity(cityPath); got != "" {
		t.Fatalf("doltPortForCity() = %q, want empty for invalid managed canonical city", got)
	}
	if isExternalDolt(cityPath) {
		t.Fatal("isExternalDolt() = true, want false for invalid managed canonical city even with env override")
	}
}

func TestConfiguredCityDoltTargetDoesNotFallbackToCompatRegistrationWhenManagedCanonicalTracksEndpoint(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "env-db.example.com")
	t.Setenv("GC_DOLT_PORT", "5510")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: stale-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	if host, port, ok := configuredCityDoltTarget(cityPath); ok {
		t.Fatalf("configuredCityDoltTarget() = (%q, %q, %v), want no target for invalid managed canonical city", host, port, ok)
	}
	if got := doltHostForCity(cityPath); got != "" {
		t.Fatalf("doltHostForCity() = %q, want empty for invalid managed canonical city", got)
	}
	if got := doltPortForCity(cityPath); got != "" {
		t.Fatalf("doltPortForCity() = %q, want empty for invalid managed canonical city", got)
	}
	if isExternalDolt(cityPath) {
		t.Fatal("isExternalDolt() = true, want false for invalid managed canonical city")
	}
}

func TestStartBeadsLifecycleRejectsInvalidCanonicalCityState(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "bd")
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err == nil || !strings.Contains(err.Error(), "invalid canonical city endpoint state") {
		t.Fatalf("startBeadsLifecycle() error = %v, want invalid canonical city endpoint state", err)
	}
}

func TestStartBeadsLifecycleRejectsInvalidCanonicalRigState(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(`issue_prefix: fe
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "bd")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "frontend", Path: rigPath, Prefix: "fe"}},
	}
	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err == nil || !strings.Contains(err.Error(), "invalid canonical rig endpoint state") {
		t.Fatalf("startBeadsLifecycle() error = %v, want invalid canonical rig endpoint state", err)
	}
}

func TestStartBeadsLifecycleRegistersDoltConfig(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Dolt:      config.DoltConfig{Host: "mini2.hippo-tilapia.ts.net", Port: 3307},
	}
	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	// Config should be registered for this city.
	if got := doltHostForCity(cityPath); got != "mini2.hippo-tilapia.ts.net" {
		t.Errorf("doltHostForCity after lifecycle = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
	}
	if got := doltPortForCity(cityPath); got != "3307" {
		t.Errorf("doltPortForCity after lifecycle = %q, want %q", got, "3307")
	}
}

func TestStartBeadsLifecycleManagedDeferredDoesNotRequireRuntimeState(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	callLog := filepath.Join(cityPath, "op-calls.log")
	providerState := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json")
	ln := listenOnRandomPort(t)
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptBody := fmt.Sprintf(`#!/bin/sh
	echo "$@" >> %q
	if [ "$1" = "start" ]; then
	  mkdir -p "$(dirname %q)"
	  cat > %q <<'JSON'
	{"running":true,"pid":%d,"port":%d,"data_dir":%q,"started_at":"2026-04-14T00:00:00Z"}
	JSON
	fi
	if [ "$1" = "init" ]; then
	  mkdir -p "$2/.beads"
	fi
	exit 0
	`, callLog, providerState, providerState, os.Getpid(), port, filepath.Join(cityPath, ".beads", "dolt"))
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	seedDeferredManagedBeads(cityPath, cityPath, "tc", "hq")
	seedDeferredManagedBeads(cityPath, rigPath, "rg", "rg")
	if err := writeDoltRuntimeStateFile(providerState, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "bd")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig", Path: rigPath, Prefix: "rg"}},
	}

	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}

	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("reading call log: %v", err)
	}
	ops := string(data)
	for _, needle := range []string{
		"start",
		"init " + cityPath + " tc hq",
		"init " + rigPath + " rg rg",
	} {
		if !strings.Contains(ops, needle) {
			t.Fatalf("call log missing %q:\n%s", needle, ops)
		}
	}
}

func TestHealthBeadsProviderDoesNotRecoverExternalLoopbackTarget(t *testing.T) {
	cityPath := t.TempDir()
	callLog := filepath.Join(cityPath, "op-calls.log")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptText := "#!/bin/sh\necho \"$1\" >> " + callLog + "\nif [ \"$1\" = \"health\" ]; then\n  echo \"health failed\" >&2\n  exit 1\nfi\nexit 0\n"
	if err := os.WriteFile(script, []byte(scriptText), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: 127.0.0.1
dolt.port: "4406"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)

	// Defensively ensure the call log does not pre-exist. t.TempDir()
	// provides a fresh directory, but other test-global resolution paths
	// (e.g., beadsProvider → gcBeadsBdScriptPath) may resolve to the same
	// script location and invoke it before this test reaches the SUT call.
	_ = os.Remove(callLog)

	err := healthBeadsProvider(cityPath)
	if err == nil || !strings.Contains(err.Error(), "exec beads health: health failed") {
		t.Fatalf("healthBeadsProvider() error = %v, want direct external health failure", err)
	}

	data, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("reading call log: %v", readErr)
	}
	ops := strings.TrimSpace(string(data))
	if ops != "health" {
		t.Fatalf("call log = %q, want only health", ops)
	}
}

func TestShutdownBeadsProviderSkipsExternalLoopbackTarget(t *testing.T) {
	cityPath := t.TempDir()
	callLog := filepath.Join(cityPath, "op-calls.log")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$1\" >> "+callLog+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: 127.0.0.1
dolt.port: "4406"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+script)
	if err := shutdownBeadsProvider(cityPath); err != nil {
		t.Fatalf("shutdownBeadsProvider() error = %v", err)
	}
	if _, err := os.Stat(callLog); !os.IsNotExist(err) {
		t.Fatalf("shutdownBeadsProvider() should not invoke stop for external loopback target, stat err = %v", err)
	}
}

// ── startBeadsLifecycle skips provider for external ───────────────────

func TestStartBeadsLifecycleSkipsProviderForExternalHost(t *testing.T) {
	cityPath := t.TempDir()
	// Install a test script that tracks which operations are called.
	// "start" should NOT be called (skipped by external host guard).
	// "init" will be called but exits 2 (not needed).
	callLog := filepath.Join(cityPath, "op-calls.log")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$1\" >> "+callLog+"\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "operator-override.example.com")
	t.Setenv("GC_DOLT_PORT", "5511")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "operator-override.example.com")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "5511")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Dolt:      config.DoltConfig{Host: "mini2.hippo-tilapia.ts.net", Port: 3307},
	}
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle with external host: %v", err)
	}

	// Verify "start" was NOT called (skipped by guard).
	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("reading call log: %v", err)
	}
	ops := strings.TrimSpace(string(data))
	for _, line := range strings.Split(ops, "\n") {
		if strings.TrimSpace(line) == "start" {
			t.Errorf("ensureBeadsProvider('start') was called — should be skipped for external host with bd provider")
		}
	}

	// Startup should not rewrite process-global Dolt env. Later callers resolve
	// explicit per-scope env instead of depending on controller ambient state.
	if got := os.Getenv("GC_DOLT_HOST"); got != "operator-override.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want operator override preserved", got)
	}
	if got := os.Getenv("GC_DOLT_PORT"); got != "5511" {
		t.Fatalf("GC_DOLT_PORT = %q, want operator override preserved", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "operator-override.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want inherited process env preserved", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "5511" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want inherited process env preserved", got)
	}
}

func TestNormalizeCanonicalBdScopeFilesRepairsCityAndRigScopeFiles(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq","dolt_host":"127.0.0.1","dolt_user":"root"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"fr","dolt_server_host":"127.0.0.1","dolt_user":"root"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: dgdb\nissue-prefix: dgdb\ndolt.auto-start: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("issue_prefix: fr\nissue-prefix: fr\ndolt.auto-start: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "dogfood-city"},
		Rigs:      []config.Rig{{Name: "frontend", Path: rigPath, Prefix: "fr"}},
	}
	if err := normalizeCanonicalBdScopeFiles(cityPath, cfg); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFiles: %v", err)
	}

	cityCfg, err := os.ReadFile(filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cityText := string(cityCfg)
	for _, want := range []string{"gc.endpoint_origin: managed_city", "gc.endpoint_status: verified", "dolt.auto-start: false"} {
		if !strings.Contains(cityText, want) {
			t.Fatalf("city config missing %q:\n%s", want, cityText)
		}
	}
	if strings.Contains(cityText, "dolt.host:") || strings.Contains(cityText, "dolt.port:") {
		t.Fatalf("city config should not track endpoint defaults for managed city:\n%s", cityText)
	}

	rigCfg, err := os.ReadFile(filepath.Join(rigPath, ".beads", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rigText := string(rigCfg)
	for _, want := range []string{"gc.endpoint_origin: inherited_city", "gc.endpoint_status: verified", "dolt.auto-start: false"} {
		if !strings.Contains(rigText, want) {
			t.Fatalf("rig config missing %q:\n%s", want, rigText)
		}
	}
	if strings.Contains(rigText, "dolt.host:") || strings.Contains(rigText, "dolt.port:") {
		t.Fatalf("inherited managed rig should not track endpoint defaults:\n%s", rigText)
	}

	for _, metaPath := range []string{
		filepath.Join(cityPath, ".beads", "metadata.json"),
		filepath.Join(rigPath, ".beads", "metadata.json"),
	} {
		data, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatal(err)
		}
		metaText := string(data)
		for _, forbidden := range []string{"dolt_host", "dolt_user", "dolt_password", "dolt_server_host", "dolt_server_port", "dolt_server_user", "dolt_port"} {
			if strings.Contains(metaText, forbidden) {
				t.Fatalf("metadata %s should scrub %s:\n%s", metaPath, forbidden, metaText)
			}
		}
	}
}

func TestGcBeadsBdInitNormalizesScopeAndRemovesLocalServerArtifacts(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "gascity"
prefix = "gc"

[beads]
provider = "bd"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	probeLog := filepath.Join(t.TempDir(), "dolt-probe.log")
	fakeBd := filepath.Join(binDir, "bd")
	fakeBdScript := `#!/bin/sh
set -eu
case "${1:-}" in
  init)
    last=""
    for arg in "$@"; do
      last="$arg"
    done
    mkdir -p "$last/.beads"
    cat > "$last/.beads/metadata.json" <<'JSON'
{"database":"legacy","backend":"legacy","dolt_mode":"embedded","dolt_database":"wrong-db","dolt_server_host":"127.0.0.1","dolt_server_port":"3307"}
JSON
    cat > "$last/.beads/config.yaml" <<'YAML'
issue-prefix: stale
dolt.auto-start: true
dolt_server_port: 3307
YAML
    : > "$last/.beads/dolt-server.pid"
    : > "$last/.beads/dolt-server.lock"
    : > "$last/.beads/dolt-server.log"
    printf '3307\n' > "$last/.beads/dolt-server.port"
    exit 0
    ;;
  list)
    db=$(python3 -c 'import json, pathlib, sys; meta = json.loads(pathlib.Path(sys.argv[1]).read_text()); print(meta.get("dolt_database", ""), end="")' "$PWD/.beads/metadata.json")
    printf '%s\t%s\n' "${GC_FAKE_BD_CALLER:-unknown}" "$db" >> "` + probeLog + `"
    exit 0
    ;;
  migrate)
    python3 -c 'import json, pathlib, sys; path = pathlib.Path(sys.argv[1]); data = json.loads(path.read_text()); data["project_id"] = "normalized-project-id"; path.write_text(json.dumps(data, indent=2) + "\n")' "$PWD/.beads/metadata.json"
    exit 0
    ;;
  config|list)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(fakeBd, []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script, "init", rigPath, "fe", "fe")
	cmd.Env = sanitizedBaseEnv(append(gcBeadsBdTestHomeEnv(t),
		"GC_CITY_PATH="+cityPath,
		"GC_BIN="+currentGCBinaryForTests(t),
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}

	metaData, err := os.ReadFile(filepath.Join(rigPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("ReadFile(rig metadata): %v", err)
	}
	metaText := string(metaData)
	for _, forbidden := range []string{"dolt_host", "dolt_user", "dolt_password", "dolt_server_host", "dolt_server_port", "dolt_server_user", "dolt_port", "wrong-db"} {
		if strings.Contains(metaText, forbidden) {
			t.Fatalf("rig metadata still contains %q:\n%s", forbidden, metaText)
		}
	}
	for _, want := range []string{`"database": "dolt"`, `"backend": "dolt"`, `"dolt_mode": "server"`, `"dolt_database": "fe"`} {
		if !strings.Contains(metaText, want) {
			t.Fatalf("rig metadata missing %q:\n%s", want, metaText)
		}
	}

	rigCfg, err := os.ReadFile(filepath.Join(rigPath, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(rig config): %v", err)
	}
	cfgText := string(rigCfg)
	for _, want := range []string{"issue_prefix: fe", "gc.endpoint_origin: inherited_city", "gc.endpoint_status: verified"} {
		if !strings.Contains(cfgText, want) {
			t.Fatalf("rig config missing %q:\n%s", want, cfgText)
		}
	}
	for _, forbidden := range []string{"dolt.host:", "dolt.port:", "dolt_server_port"} {
		if strings.Contains(cfgText, forbidden) {
			t.Fatalf("rig config still contains %q:\n%s", forbidden, cfgText)
		}
	}

	for _, name := range []string{"dolt-server.pid", "dolt-server.lock", "dolt-server.log", "dolt-server.port"} {
		if _, err := os.Stat(filepath.Join(rigPath, ".beads", name)); !os.IsNotExist(err) {
			t.Fatalf("rig %s should be removed after init, stat err = %v", name, err)
		}
	}

	t.Setenv("GC_FAKE_BD_CALLER", "raw")
	_ = runRawBDFromDir(t, fakeBd, rigPath, "list")

	t.Setenv("GC_FAKE_BD_CALLER", "gc")
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"--city", cityPath, "--rig", "frontend", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd list = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	probeData, err := os.ReadFile(probeLog)
	if err != nil {
		t.Fatalf("read probe log: %v", err)
	}
	if got := strings.TrimSpace(string(probeData)); got != "raw\tfe\ngc\tfe" {
		t.Fatalf("probe log = %q, want repaired rig database for both raw bd and gc bd", got)
	}
}

func TestNormalizeCanonicalBdScopeFilesMaterializesMissingMetadata(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: ci\nissue-prefix: ci\ndolt.auto-start: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("issue_prefix: fr\nissue-prefix: fr\ndolt.auto-start: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "dogfood-city"},
		Rigs:      []config.Rig{{Name: "frontend", Path: rigPath, Prefix: "fr"}},
	}
	if err := normalizeCanonicalBdScopeFiles(cityPath, cfg); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFiles: %v", err)
	}

	cityMeta, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cityMeta), `"dolt_database": "hq"`) {
		t.Fatalf("city metadata = %s, want hq dolt_database", string(cityMeta))
	}

	rigMeta, err := os.ReadFile(filepath.Join(rigPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rigMeta), `"dolt_database": "fr"`) {
		t.Fatalf("rig metadata = %s, want fr dolt_database", string(rigMeta))
	}
}

func TestGcBeadsBdStartFallsBackToShellManagedConfigWriterWhenGCBinUnset(t *testing.T) {
	skipSlowCmdGCTest(t, "starts the materialized gc-beads-bd shell fallback; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityPath)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	invocationFile := filepath.Join(t.TempDir(), "gc-invocation")
	_ = writeFakeManagedConfigWriterGC(t, binDir, invocationFile)
	writeFakeManagedConfigWriterDolt(t, binDir)

	env := sanitizedBaseEnv(
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	cmd := exec.Command(script, "start")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd start failed: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		_ = stop.Run()
	})

	if _, err := os.Stat(invocationFile); !os.IsNotExist(err) {
		t.Fatalf("PATH gc should not be used for hidden helpers when GC_BIN is unset, stat err = %v", err)
	}
	configData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(dolt-config.yaml): %v", err)
	}
	if strings.Contains(string(configData), "# rendered by fake gc") {
		t.Fatalf("dolt-config.yaml should be rendered by shell fallback, not PATH gc:\n%s", string(configData))
	}
	state, err := readDoltRuntimeStateFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json"))
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile: %v", err)
	}
	if state.Port == 0 {
		t.Fatalf("provider state port = %d, want non-zero", state.Port)
	}
}
