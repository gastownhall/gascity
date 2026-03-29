package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

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

// TestEnsureBeadsProvider_bd_skip verifies bd provider is no-op when GC_DOLT=skip.
func TestEnsureBeadsProvider_bd_skip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	MaterializeBeadsBdScript(dir) //nolint:errcheck
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	if err := ensureBeadsProvider(dir); err != nil {
		t.Fatalf("expected nil, got %v", err)
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
	MaterializeBeadsBdScript(dir) //nolint:errcheck
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	if err := shutdownBeadsProvider(dir); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCurrentDoltPortPrefersRuntimeState(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
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

func TestSyncConfiguredDoltPortFilesWritesArbitraryRigPaths(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "foobar")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })

	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("dolt.port: 1234\ndolt.auto-start: true\n"), 0o644); err != nil {
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

	syncConfiguredDoltPortFiles(cityDir, []config.Rig{{Name: "foobar", Path: rigDir}})

	for _, dir := range []string{cityDir, rigDir} {
		data, err := os.ReadFile(filepath.Join(dir, ".beads", "dolt-server.port"))
		if err != nil {
			t.Fatalf("read port file for %s: %v", dir, err)
		}
		if strings.TrimSpace(string(data)) != fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port) {
			t.Fatalf("%s port file = %q, want %d", dir, strings.TrimSpace(string(data)), ln.Addr().(*net.TCPAddr).Port)
		}
		cfgData, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read config for %s: %v", dir, err)
		}
		cfg := string(cfgData)
		if strings.Contains(cfg, "dolt.port:") {
			t.Fatalf("%s config still contains dolt.port:\n%s", dir, cfg)
		}
		if !strings.Contains(cfg, "dolt.auto-start: false") {
			t.Fatalf("%s config missing dolt.auto-start normalization:\n%s", dir, cfg)
		}
	}
}

func TestCurrentDoltPortIgnoresDeadRuntimeStateAndPrunesDeadPortFile(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
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

func TestReadDoltPortOverwritesInheritedValue(t *testing.T) {
	cityDir := t.TempDir()
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

	t.Setenv("GC_DOLT_PORT", "9999")
	readDoltPort(cityDir)
	if got := os.Getenv("GC_DOLT_PORT"); got != fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port) {
		t.Fatalf("GC_DOLT_PORT = %q, want %d", got, ln.Addr().(*net.TCPAddr).Port)
	}
}

// TestInitBeadsForDir_file verifies that file provider is a no-op.
func TestInitBeadsForDir_file(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	if err := initBeadsForDir(t.TempDir(), t.TempDir(), "test"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestInitBeadsForDir_exec calls script with init <dir> <prefix>.
func TestInitBeadsForDir_exec(t *testing.T) {
	script := writeTestScript(t, "init", 2, "")
	t.Setenv("GC_BEADS", "exec:"+script)
	if err := initBeadsForDir(t.TempDir(), "/some/dir", "prefix"); err != nil {
		t.Fatalf("expected nil for exit 2, got %v", err)
	}
}

// TestInitBeadsForDir_bd_skip verifies bd provider is no-op when GC_DOLT=skip.
func TestInitBeadsForDir_bd_skip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	MaterializeBeadsBdScript(dir) //nolint:errcheck
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	if err := initBeadsForDir(dir, t.TempDir(), "test"); err != nil {
		t.Fatalf("expected nil, got %v", err)
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
	content := "#!/bin/sh\nif [ \"$GC_CITY_PATH\" = \"" + dir + "\" ] && [ \"$GC_CITY_ROOT\" = \"" + dir + "\" ] && [ \"$GC_CITY_RUNTIME_DIR\" = \"" + filepath.Join(dir, ".gc", "runtime") + "\" ]; then exit 0; else exit 1; fi\n"
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
		"test \"$GC_CITY_PATH\" = \"" + dir + "\" || exit 1\n" +
		"test \"$GC_CITY_ROOT\" = \"" + dir + "\" || exit 1\n" +
		"test \"$GC_CITY_RUNTIME_DIR\" = \"" + filepath.Join(dir, ".gc", "runtime") + "\" || exit 1\n" +
		"test -z \"$GC_PACK_STATE_DIR\" || exit 1\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", "/wrong")
	t.Setenv("GC_CITY_ROOT", "/wrong")
	t.Setenv("GC_CITY_RUNTIME_DIR", "/wrong/.gc/runtime")
	t.Setenv("GC_PACK_STATE_DIR", "/wrong/.gc/runtime/packs/dolt")
	if err := runProviderOp(script, dir, "health"); err != nil {
		t.Fatalf("expected sanitized runtime env, got %v", err)
	}
}

// TestStartBeadsLifecycle_InstallsAgentHooks verifies that startBeadsLifecycle
// installs agent hooks for both the city and all rigs.
func TestStartBeadsLifecycle_InstallsAgentHooks(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{
			Name:              "test-city",
			InstallAgentHooks: []string{"gemini"},
		},
		Rigs: []config.Rig{
			{Name: "my-rig", Path: rigPath},
		},
	}

	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}

	// Verify gemini hooks installed in city dir.
	cityHook := filepath.Join(cityPath, ".gemini", "settings.json")
	if _, err := os.Stat(cityHook); err != nil {
		t.Errorf("city gemini hook not created: %v", err)
	}

	// Verify gemini hooks installed in rig dir.
	rigHook := filepath.Join(rigPath, ".gemini", "settings.json")
	if _, err := os.Stat(rigHook); err != nil {
		t.Errorf("rig gemini hook not created: %v", err)
	}
}

func TestGcBeadsBdStartUsesRootBeadsDataDir(t *testing.T) {
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
	script, err := MaterializeBeadsBdScript(cityPath)
	if err != nil {
		t.Fatalf("MaterializeBeadsBdScript: %v", err)
	}

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(homeDir, ".gitconfig")
	if err := os.WriteFile(gitConfig, []byte("[user]\n\tname = Test User\n\temail = test@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	scriptEnv := append(os.Environ(),
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

	stateFile := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	state, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if !strings.Contains(string(state), filepath.Join(cityPath, ".beads", "dolt")) {
		t.Fatalf("state file should point at .beads/dolt, got:\n%s", state)
	}

	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "dolt-server.port")); err != nil {
		t.Fatalf("dolt-server.port missing: %v", err)
	}
}

func TestGcBeadsBdInitRetriesRootStoreVerification(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"mc"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	script, err := MaterializeBeadsBdScript(cityPath)
	if err != nil {
		t.Fatalf("MaterializeBeadsBdScript: %v", err)
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

	cmd := exec.Command(script, "init", cityPath, "mc")
	cmd.Env = append(os.Environ(),
		"GC_CITY_PATH="+cityPath,
		"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(listCountFile)
	if err != nil {
		t.Fatalf("read list retry count: %v", err)
	}
	if strings.TrimSpace(string(data)) != "3" {
		t.Fatalf("expected bd list to retry until third attempt, got %q", strings.TrimSpace(string(data)))
	}
}

func listenOnRandomPort(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func writeDoltState(cityPath string, state doltRuntimeState) error {
	stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	data := fmt.Sprintf(`{"running":%t,"pid":%d,"port":%d,"data_dir":%q,"started_at":%q}`,
		state.Running, state.PID, state.Port, state.DataDir, state.StartedAt)
	return os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(data), 0o644)
}

// writeTestScript creates a shell script that exits with the given code.
// If stderrMsg is non-empty, the script writes it to stderr before exiting.
func writeTestScript(t *testing.T, _ string, exitCode int, stderrMsg string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "test-beads.sh")

	content := "#!/bin/sh\n"
	if stderrMsg != "" {
		content += "echo '" + stderrMsg + "' >&2\n"
	}
	content += "exit " + itoa(exitCode) + "\n"

	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// itoa is a simple int to string converter for test scripts.
func itoa(n int) string {
	return []string{"0", "1", "2"}[n]
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

func TestDoltHostForCityPrefersEnvOverConfig(t *testing.T) {
	cityPath := t.TempDir()
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "config-host.example.com", Port: 3307})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	t.Setenv("GC_DOLT_HOST", "user-override.example.com")

	if got := doltHostForCity(cityPath); got != "user-override.example.com" {
		t.Errorf("doltHostForCity = %q, want user env override %q", got, "user-override.example.com")
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

func TestDoltPortForCityPrefersEnvOverConfig(t *testing.T) {
	cityPath := t.TempDir()
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Port: 3307})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	t.Setenv("GC_DOLT_PORT", "9999")

	if got := doltPortForCity(cityPath); got != "9999" {
		t.Errorf("doltPortForCity = %q, want user env override %q", got, "9999")
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

// ── readDoltPort with external host ───────────────────────────────────

func TestReadDoltPortPreservesExternalHostPort(t *testing.T) {
	cityDir := t.TempDir()
	// Register per-city config (simulates startBeadsLifecycle having run).
	cityDoltConfigs.Store(cityDir, config.DoltConfig{Host: "mini2.hippo-tilapia.ts.net", Port: 3307})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityDir) })

	t.Setenv("GC_DOLT_PORT", "3307")

	// Write a local state file that would normally override — it should NOT.
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

	readDoltPort(cityDir)

	if got := os.Getenv("GC_DOLT_PORT"); got != "3307" {
		t.Errorf("GC_DOLT_PORT = %q, want %q (external config preserved)", got, "3307")
	}
}

// ── startBeadsLifecycle skips provider for external ───────────────────

func TestStartBeadsLifecycleSkipsProviderForExternalHost(t *testing.T) {
	cityPath := t.TempDir()
	// Install a test script that tracks which operations are called.
	// "start" should NOT be called (skipped by external host guard).
	// "init" will be called but exits 2 (not needed).
	callLog := filepath.Join(cityPath, "op-calls.log")
	script := filepath.Join(cityPath, ".gc", "system", "bin", "gc-beads-bd")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$1\" >> "+callLog+"\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "bd")

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
}
