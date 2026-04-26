package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

//nolint:unparam // tests override hook behavior but keep fixed timeout/poll values for determinism
func withSupervisorTestHooks(t *testing.T, ensure func(stdout, stderr io.Writer) int, reload func(stdout, stderr io.Writer) int, alive func() int, running func(string) (bool, string, bool), timeout, poll time.Duration) {
	t.Helper()

	oldEnsure := ensureSupervisorRunningHook
	oldReload := reloadSupervisorHook
	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	oldError := supervisorCityErrorHook
	oldWaitForStop := waitForSupervisorControllerStopHook
	oldRegister := registerCityWithSupervisorTestHook
	oldTimeout := supervisorCityReadyTimeout
	oldPoll := supervisorCityPollInterval

	ensureSupervisorRunningHook = ensure
	reloadSupervisorHook = reload
	supervisorAliveHook = alive
	supervisorCityRunningHook = running
	supervisorCityErrorHook = supervisorCityError
	waitForSupervisorControllerStopHook = waitForStandaloneControllerStop
	registerCityWithSupervisorTestHook = nil
	supervisorCityReadyTimeout = timeout
	supervisorCityPollInterval = poll

	t.Cleanup(func() {
		ensureSupervisorRunningHook = oldEnsure
		reloadSupervisorHook = oldReload
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
		supervisorCityErrorHook = oldError
		waitForSupervisorControllerStopHook = oldWaitForStop
		registerCityWithSupervisorTestHook = oldRegister
		supervisorCityReadyTimeout = oldTimeout
		supervisorCityPollInterval = oldPoll
	})
}

func TestRegisterCityWithSupervisorKeepsRegistrationWhenCityNeverBecomesReady(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n[session]\nstartup_timeout = \"20ms\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reloads := 0
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int {
			reloads++
			return 0
		},
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", true },
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisor(cityPath, &stdout, &stderr, "gc register", true)
	// The command reports failure (exit code 1) when the city doesn't start,
	// but keeps the registration so the supervisor can retry automatically.
	if code != 1 {
		t.Fatalf("registerCityWithSupervisor code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "keeping registration") {
		t.Fatalf("stderr = %q, want keep-registration message", stderr.String())
	}
	if reloads != 1 {
		t.Fatalf("reloadSupervisorHook called %d times, want 1", reloads)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || canonicalTestPath(entries[0].Path) != canonicalTestPath(cityPath) {
		t.Fatalf("expected registry to retain %s, got %v", cityPath, entries)
	}
}

func TestRegisterCityForAPIRegistersWithoutWaitingForReadiness(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := registerCityForAPI(cityPath, "api-name"); err != nil {
		t.Fatalf("registerCityForAPI: %v", err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("registry entries = %+v, want one", entries)
	}
	assertSameTestPath(t, entries[0].Path, cityPath)
	if entries[0].EffectiveName() != "api-name" {
		t.Fatalf("effective name = %q, want api-name", entries[0].EffectiveName())
	}
}

func TestRegisterCityWithSupervisorRetriesControllerLockInitFailure(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(cityPath, "city.toml")
	initialInfo, err := os.Stat(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	initialMod := initialInfo.ModTime()

	reloads := 0
	waited := 0
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int {
			reloads++
			return 0
		},
		func() int { return 4242 },
		func(string) (bool, string, bool) {
			info, err := os.Stat(tomlPath)
			if err != nil {
				t.Fatalf("stat city.toml: %v", err)
			}
			if reloads >= 2 && info.ModTime().After(initialMod) {
				return true, "", true
			}
			return false, "init_failed", true
		},
		20*time.Millisecond,
		time.Millisecond,
	)
	supervisorCityErrorHook = func(string) string {
		return "controller lock: controller already running"
	}
	waitForSupervisorControllerStopHook = func(path string, timeout time.Duration) error {
		waited++
		if canonicalTestPath(path) != canonicalTestPath(cityPath) {
			t.Fatalf("wait path = %q, want %q", path, cityPath)
		}
		if timeout != supervisorCityStopTimeout(cityPath) {
			t.Fatalf("wait timeout = %s, want %s", timeout, supervisorCityStopTimeout(cityPath))
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisor(cityPath, &stdout, &stderr, "gc register", true)
	if code != 0 {
		t.Fatalf("registerCityWithSupervisor code = %d, want 0\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	if reloads != 2 {
		t.Fatalf("reloadSupervisorHook called %d times, want 2", reloads)
	}
	if waited != 1 {
		t.Fatalf("waitForSupervisorControllerStopHook called %d times, want 1", waited)
	}
	finalInfo, err := os.Stat(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !finalInfo.ModTime().After(initialMod) {
		t.Fatalf("city.toml modtime = %s, want after %s", finalInfo.ModTime(), initialMod)
	}
	if strings.Contains(stderr.String(), "keeping registration") {
		t.Fatalf("stderr = %q, did not expect keep-registration message", stderr.String())
	}
}

func TestRegisterCityWithSupervisorKeepsRegistrationWhenReloadFails(t *testing.T) {
	skipSlowCmdGCTest(t, "exercises supervisor registration retry behavior; run make test-cmd-gc-process for scenario coverage")
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reloads := 0
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int {
			reloads++
			return 1
		},
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", true },
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisor(cityPath, &stdout, &stderr, "gc register", true)
	if code != 1 {
		t.Fatalf("registerCityWithSupervisor code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "keeping registration") {
		t.Fatalf("stderr = %q, want keep-registration message", stderr.String())
	}
	if reloads != 2 {
		t.Fatalf("reloadSupervisorHook called %d times, want 2", reloads)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || canonicalTestPath(entries[0].Path) != canonicalTestPath(cityPath) {
		t.Fatalf("expected registry to retain %s, got %v", cityPath, entries)
	}
}

func TestRegisterCityWithSupervisorFailsFastWhenSupervisorStopsDuringWait(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	configText := strings.Join([]string{
		"[workspace]",
		`name = "bright-lights"`,
		"[session]",
		`startup_timeout = "5s"`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(configText), 0o644); err != nil {
		t.Fatal(err)
	}

	aliveChecks := 0
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int {
			aliveChecks++
			if aliveChecks <= 1 {
				return 4242
			}
			return 0
		},
		func(string) (bool, string, bool) { return false, "", false },
		5*time.Second,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	started := time.Now()
	code := registerCityWithSupervisor(cityPath, &stdout, &stderr, "gc register", true)
	if code != 1 {
		t.Fatalf("registerCityWithSupervisor code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "supervisor stopped before city became ready") {
		t.Fatalf("stderr = %q, want supervisor-stopped message", stderr.String())
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("registerCityWithSupervisor took %v, want fast failure when supervisor stops", elapsed)
	}
	if !strings.Contains(stderr.String(), "keeping registration") {
		t.Fatalf("stderr = %q, want keep-registration message", stderr.String())
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || canonicalTestPath(entries[0].Path) != canonicalTestPath(cityPath) {
		t.Fatalf("expected registry to retain %s, got %v", cityPath, entries)
	}
}

func TestRegisterCityWithSupervisorWaitsForConfiguredStartupTimeout(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n[session]\nstartup_timeout = \"200ms\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(75 * time.Millisecond)
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) {
			return time.Now().After(startedAt), "", true
		},
		20*time.Millisecond,
		5*time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisor(cityPath, &stdout, &stderr, "gc register", true)
	if code != 0 {
		t.Fatalf("registerCityWithSupervisor code = %d, want 0: %s", code, stderr.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	// Registry.Register stores the same canonical comparison form used by
	// runtime path comparisons.
	resolvedCityPath := canonicalTestPath(cityPath)
	if len(entries) != 1 || entries[0].Path != resolvedCityPath {
		t.Fatalf("expected retained registry entry for %s, got %v", resolvedCityPath, entries)
	}
}

func TestRegisterCityWithSupervisorFetchesRemotePacksBeforeLoadingIncludes(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	remote := initBarePackRepo(t, "remote-pack")
	configText := strings.Join([]string{
		"[workspace]",
		`name = "bright-lights"`,
		`includes = ["remote-pack"]`,
		"",
		"[packs.remote-pack]",
		`source = "` + remote + `"`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(configText), 0o644); err != nil {
		t.Fatal(err)
	}

	// Before packs are fetched, the pack is not found and silently skipped.
	// effectiveCityName still succeeds (missing packs are non-fatal) but
	// the pack's agents/config won't be loaded until after fetch.
	name, err := effectiveCityName(cityPath)
	if err != nil {
		t.Fatalf("effectiveCityName should succeed even with missing pack: %v", err)
	}
	if name != "bright-lights" {
		t.Fatalf("expected name %q, got %q", "bright-lights", name)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 0 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisor(cityPath, &stdout, &stderr, "gc register", true)
	if code != 0 {
		t.Fatalf("registerCityWithSupervisor code = %d, want 0: %s", code, stderr.String())
	}

	cacheDir := config.PackCachePath(cityPath, "remote-pack", config.PackSource{Source: remote})
	if _, err := os.Stat(filepath.Join(cacheDir, "pack.toml")); err != nil {
		t.Fatalf("expected fetched pack cache at %s: %v", cacheDir, err)
	}
}

func TestEffectiveCityNameUsesWorkspaceSiteBinding(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "target-basename")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "site.toml"), []byte("workspace_name = \"site-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	name, err := effectiveCityName(cityPath)
	if err != nil {
		t.Fatalf("effectiveCityName returned error: %v", err)
	}
	if name != "site-city" {
		t.Fatalf("effectiveCityName = %q, want %q", name, "site-city")
	}
}

func writeCityWithUnmaterializedGastownImport(t *testing.T) string {
	t.Helper()

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	packToml := `[pack]
name = "bright-lights"
schema = 2

[imports.gastown]
source = ".gc/system/packs/gastown"
`
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func TestEffectiveCityNameMaterializesBuiltinPackImportsBeforeLoad(t *testing.T) {
	cityPath := writeCityWithUnmaterializedGastownImport(t)

	name, err := effectiveCityName(cityPath)
	if err != nil {
		t.Fatalf("effectiveCityName returned error: %v", err)
	}
	if name != "bright-lights" {
		t.Fatalf("effectiveCityName = %q, want %q", name, "bright-lights")
	}
	if _, err := os.Stat(filepath.Join(cityPath, citylayout.SystemPacksRoot, "gastown", "pack.toml")); err != nil {
		t.Fatalf("expected gastown builtin pack to be materialized before config load: %v", err)
	}
}

func TestLoadSupervisorCityConfigMaterializesBuiltinPackImportsBeforeLoad(t *testing.T) {
	cityPath := writeCityWithUnmaterializedGastownImport(t)

	cfg, _, err := loadSupervisorCityConfig(cityPath)
	if err != nil {
		t.Fatalf("loadSupervisorCityConfig returned error: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Fatalf("workspace name = %q, want %q", cfg.Workspace.Name, "bright-lights")
	}
	if _, err := os.Stat(filepath.Join(cityPath, citylayout.SystemPacksRoot, "gastown", "pack.toml")); err != nil {
		t.Fatalf("expected gastown builtin pack to be materialized before supervisor config load: %v", err)
	}
}

func TestLoadStartCityConfigMaterializesBuiltinPackImportsBeforeLoad(t *testing.T) {
	cityPath := writeCityWithUnmaterializedGastownImport(t)

	cfg, _, err := loadStartCityConfig(cityPath)
	if err != nil {
		t.Fatalf("loadStartCityConfig returned error: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Fatalf("workspace name = %q, want %q", cfg.Workspace.Name, "bright-lights")
	}
	if _, err := os.Stat(filepath.Join(cityPath, citylayout.SystemPacksRoot, "gastown", "pack.toml")); err != nil {
		t.Fatalf("expected gastown builtin pack to be materialized before start config load: %v", err)
	}
}

func TestLoadSlingCityConfigMaterializesBuiltinPackImportsBeforeLoad(t *testing.T) {
	cityPath := writeCityWithUnmaterializedGastownImport(t)

	cfg, _, err := loadSlingCityConfig(cityPath)
	if err != nil {
		t.Fatalf("loadSlingCityConfig returned error: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Fatalf("workspace name = %q, want %q", cfg.Workspace.Name, "bright-lights")
	}
	if _, err := os.Stat(filepath.Join(cityPath, citylayout.SystemPacksRoot, "gastown", "pack.toml")); err != nil {
		t.Fatalf("expected gastown builtin pack to be materialized before sling config load: %v", err)
	}
}

func TestLoadConfigCommandCityConfigMaterializesBuiltinPackImportsBeforeLoad(t *testing.T) {
	cityPath := writeCityWithUnmaterializedGastownImport(t)

	cfg, _, err := loadConfigCommandCityConfig(cityPath)
	if err != nil {
		t.Fatalf("loadConfigCommandCityConfig returned error: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Fatalf("workspace name = %q, want %q", cfg.Workspace.Name, "bright-lights")
	}
	if _, err := os.Stat(filepath.Join(cityPath, citylayout.SystemPacksRoot, "gastown", "pack.toml")); err != nil {
		t.Fatalf("expected gastown builtin pack to be materialized before config command load: %v", err)
	}
}

func TestRegisterCityWithSupervisorNameOverrideMaterializesBuiltinPackImports(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	cityPath := writeCityWithUnmaterializedGastownImport(t)

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 0 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisorNamed(cityPath, "machine-alias", &stdout, &stderr, "gc register", false)
	if code != 0 {
		t.Fatalf("registerCityWithSupervisorNamed code = %d, want 0: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(cityPath, citylayout.SystemPacksRoot, "gastown", "pack.toml")); err != nil {
		t.Fatalf("expected gastown builtin pack to be materialized before alias registration: %v", err)
	}
}

func TestRegisterCityWithSupervisorRejectsStandaloneController(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	root := shortSocketTempDir(t, "gc-ctl-")

	cityPath := filepath.Join(root, "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck // test cleanup

	go func() {
		conn, acceptErr := lis.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close() //nolint:errcheck // test cleanup
		buf := make([]byte, 32)
		n, _ := conn.Read(buf)
		if strings.Contains(string(buf[:n]), "ping") {
			conn.Write([]byte("4242\n")) //nolint:errcheck // best-effort reply
		}
	}()

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisor(cityPath, &stdout, &stderr, "gc start", true)
	if code != 1 {
		t.Fatalf("registerCityWithSupervisor code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "standalone controller already running") {
		t.Fatalf("stderr = %q, want standalone-controller error", stderr.String())
	}
	if !strings.Contains(stderr.String(), "for "+shellQuotePath(cityPath)) {
		t.Fatalf("stderr = %q, want shell-quoted city path", stderr.String())
	}
	if !strings.Contains(stderr.String(), "PID 4242") {
		t.Fatalf("stderr = %q, want controller PID", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Authority: standalone controller PID 4242") {
		t.Fatalf("stderr = %q, want standalone-controller authority", stderr.String())
	}
	wantNext := "Next: gc stop " + shellQuotePath(cityPath) + " && gc start " + shellQuotePath(cityPath)
	if !strings.Contains(stderr.String(), wantNext) {
		t.Fatalf("stderr = %q, want next command %q", stderr.String(), wantNext)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty registry after standalone-controller rejection, got %v", entries)
	}
}

func TestSupervisorRetryCommand(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city with spaces")

	tests := []struct {
		name        string
		commandName string
		want        string
	}{
		{
			name:        "start retries start",
			commandName: "gc start",
			want:        "gc start " + shellQuotePath(cityPath),
		},
		{
			name:        "register retries register",
			commandName: "gc register",
			want:        "gc register " + shellQuotePath(cityPath),
		},
		{
			name:        "init retries start",
			commandName: "gc init",
			want:        "gc start " + shellQuotePath(cityPath),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := supervisorRetryCommand(tt.commandName, cityPath); got != tt.want {
				t.Fatalf("supervisorRetryCommand(%q, %q) = %q, want %q", tt.commandName, cityPath, got, tt.want)
			}
		})
	}
}

func TestRegisterCityWithSupervisorAllowsAlreadyManagedCity(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	reloads := 0
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int {
			reloads++
			return 0
		},
		func() int { return 4242 },
		func(path string) (bool, string, bool) {
			return canonicalTestPath(path) == canonicalTestPath(cityPath), "", true
		},
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisorNamed(cityPath, "new-name", &stdout, &stderr, "gc register", true)
	if code != 0 {
		t.Fatalf("registerCityWithSupervisorNamed code = %d, want 0: %s", code, stderr.String())
	}
	if reloads != 1 {
		t.Fatalf("reloadSupervisorHook called %d times, want 1", reloads)
	}
	if !strings.Contains(stdout.String(), "Registered city 'new-name'") {
		t.Fatalf("stdout = %q, want updated registration message", stdout.String())
	}

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 registered city, got %v", entries)
	}
	if entries[0].Name != "new-name" {
		t.Fatalf("registry name = %q, want %q", entries[0].Name, "new-name")
	}
}

func TestRegisterCityWithSupervisorRejectsStandaloneControllerForStoppedManagedCity(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	root := shortSocketTempDir(t, "gc-ctl-")

	cityPath := filepath.Join(root, "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck // test cleanup

	go func() {
		conn, acceptErr := lis.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close() //nolint:errcheck // test cleanup
		buf := make([]byte, 32)
		n, _ := conn.Read(buf)
		if strings.Contains(string(buf[:n]), "ping") {
			conn.Write([]byte("4242\n")) //nolint:errcheck // best-effort reply
		}
	}()

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 9999 },
		func(path string) (bool, string, bool) {
			return false, "", canonicalTestPath(path) == canonicalTestPath(cityPath)
		},
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisorNamed(cityPath, "new-name", &stdout, &stderr, "gc register", true)
	if code != 1 {
		t.Fatalf("registerCityWithSupervisorNamed code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "standalone controller already running") {
		t.Fatalf("stderr = %q, want standalone-controller error", stderr.String())
	}
	if !strings.Contains(stderr.String(), "PID 4242") {
		t.Fatalf("stderr = %q, want controller PID", stderr.String())
	}
}

func TestRegisterCityWithSupervisorAllowsCityStartingUnderSupervisor(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	reloads := 0
	runningChecks := 0
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int {
			reloads++
			return 0
		},
		func() int { return 4242 },
		func(path string) (bool, string, bool) {
			if canonicalTestPath(path) != canonicalTestPath(cityPath) {
				return false, "", false
			}
			runningChecks++
			if runningChecks == 1 {
				return false, "starting_bead_store", true
			}
			return true, "", true
		},
		100*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisorNamed(cityPath, "new-name", &stdout, &stderr, "gc register", true)
	if code != 0 {
		t.Fatalf("registerCityWithSupervisorNamed code = %d, want 0: %s", code, stderr.String())
	}
	if reloads != 1 {
		t.Fatalf("reloadSupervisorHook called %d times, want 1", reloads)
	}
	if !strings.Contains(stdout.String(), "Registered city 'new-name'") {
		t.Fatalf("stdout = %q, want updated registration message", stdout.String())
	}
}

func TestRegisterCityWithSupervisorRejectsStandaloneControllerDuringSupervisorStartupPhase(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	root := shortSocketTempDir(t, "gc-ctl-")

	cityPath := filepath.Join(root, "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck // test cleanup

	go func() {
		conn, acceptErr := lis.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close() //nolint:errcheck // test cleanup
		buf := make([]byte, 32)
		n, _ := conn.Read(buf)
		if strings.Contains(string(buf[:n]), "ping") {
			conn.Write([]byte("4242\n")) //nolint:errcheck // best-effort reply
		}
	}()

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 9999 },
		func(path string) (bool, string, bool) {
			return false, "starting_bead_store", canonicalTestPath(path) == canonicalTestPath(cityPath)
		},
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisorNamed(cityPath, "new-name", &stdout, &stderr, "gc register", true)
	if code != 1 {
		t.Fatalf("registerCityWithSupervisorNamed code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "standalone controller already running") {
		t.Fatalf("stderr = %q, want standalone-controller error", stderr.String())
	}
	if !strings.Contains(stderr.String(), "PID 4242") {
		t.Fatalf("stderr = %q, want controller PID", stderr.String())
	}
}

func TestUnregisterCityFromSupervisorRestoresRegistrationOnReloadFailure(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 1 },
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	handled, code := unregisterCityFromSupervisor(cityPath, &stdout, &stderr, "gc unregister")
	if !handled || code != 1 {
		t.Fatalf("unregisterCityFromSupervisor = (%t, %d), want (true, 1)", handled, code)
	}
	if !strings.Contains(stderr.String(), "restored registration") {
		t.Fatalf("stderr = %q, want restore message", stderr.String())
	}

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	// Registry.Register stores the same canonical comparison form used by
	// runtime path comparisons.
	resolvedCityPath := canonicalTestPath(cityPath)
	if len(entries) != 1 || entries[0].Path != resolvedCityPath {
		t.Fatalf("expected restored registry entry for %s, got %v", resolvedCityPath, entries)
	}
}

func TestUnregisterCityFromSupervisorWaitsForControllerStop(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	var waitedPath string
	var waitedTimeout time.Duration
	waitForSupervisorControllerStopHook = func(path string, timeout time.Duration) error {
		waitedPath = path
		waitedTimeout = timeout
		return nil
	}

	var stdout, stderr bytes.Buffer
	handled, code := unregisterCityFromSupervisor(cityPath, &stdout, &stderr, "gc unregister")
	if !handled || code != 0 {
		t.Fatalf("unregisterCityFromSupervisor = (%t, %d), want (true, 0)", handled, code)
	}
	if canonicalTestPath(waitedPath) != canonicalTestPath(cityPath) {
		t.Fatalf("waited for %q, want %q", waitedPath, cityPath)
	}
	if waitedTimeout != supervisorCityStopTimeout(cityPath) {
		t.Fatalf("wait timeout = %s, want %s", waitedTimeout, supervisorCityStopTimeout(cityPath))
	}
}

func TestUnregisterCityFromSupervisorSkipsProbesWhenCityDirMissing(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	reloads := 0
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int {
			reloads++
			return 0
		},
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	// If the guard regresses, the stale waitForSupervisorControllerStopHook
	// default would call acquireControllerLock on the missing .gc dir and
	// surface the cascading "probing standalone controller" spew — fail
	// the test loudly if the probe path is entered.
	waitForSupervisorControllerStopHook = func(string, time.Duration) error {
		t.Fatalf("waitForSupervisorControllerStopHook called when city dir is gone")
		return nil
	}

	if err := os.RemoveAll(cityPath); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	handled, code := unregisterCityFromSupervisor(cityPath, &stdout, &stderr, "gc unregister")
	if !handled || code != 0 {
		t.Fatalf("unregisterCityFromSupervisor = (%t, %d), want (true, 0)", handled, code)
	}
	if !strings.Contains(stdout.String(), "Unregistered city 'bright-lights'") {
		t.Fatalf("stdout = %q, want success line for 'bright-lights'", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty (no cascading probe/restore spew)", stderr.String())
	}
	if reloads != 1 {
		t.Fatalf("reloadSupervisorHook called %d times, want 1 (nudge supervisor reconcile)", reloads)
	}

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty registry after unregister, got %v", entries)
	}
}

func TestUnregisterCityFromSupervisorReturnsReloadFailureWhenCityDirMissing(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	reloads := 0
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_ io.Writer, stderr io.Writer) int {
			reloads++
			_, _ = io.WriteString(stderr, "gc supervisor reload: reconcile queue is busy; try again shortly\n")
			return 1
		},
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	waitForSupervisorControllerStopHook = func(string, time.Duration) error {
		t.Fatalf("waitForSupervisorControllerStopHook called when city dir is gone")
		return nil
	}

	if err := os.RemoveAll(cityPath); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	handled, code := unregisterCityFromSupervisor(cityPath, &stdout, &stderr, "gc unregister")
	if !handled || code != 1 {
		t.Fatalf("unregisterCityFromSupervisor = (%t, %d), want (true, 1)", handled, code)
	}
	if !strings.Contains(stdout.String(), "Unregistered city 'bright-lights'") {
		t.Fatalf("stdout = %q, want success line for 'bright-lights'", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc supervisor reload: reconcile queue is busy; try again shortly") {
		t.Fatalf("stderr = %q, want reload failure", stderr.String())
	}
	if strings.Contains(stderr.String(), "restored registration") || strings.Contains(stderr.String(), "restore failed") {
		t.Fatalf("stderr = %q, want reload failure only", stderr.String())
	}
	if reloads != 1 {
		t.Fatalf("reloadSupervisorHook called %d times, want 1", reloads)
	}

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty registry after failed reload with missing city dir, got %v", entries)
	}
}

func TestReconcileCitiesUnregisterEventUsesManagedCityName(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	cityPath := filepath.Join(t.TempDir(), "basename-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	close(done)
	registry := newCityRegistry()
	registry.Add(cityPath, &managedCity{
		name:    "effective-city",
		started: true,
		cancel:  func() {},
		done:    done,
	})

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	var stdout, stderr bytes.Buffer
	reconcileCities(reg, registry, supervisor.PublicationConfig{}, &stdout, &stderr)

	recorded, err := events.ReadAll(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("ReadAll(events): %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("recorded %d events, want 1", len(recorded))
	}
	got := recorded[0]
	if got.Type != events.CityUnregistered {
		t.Fatalf("event.Type = %q, want %q", got.Type, events.CityUnregistered)
	}
	if got.Subject != "effective-city" {
		t.Fatalf("event.Subject = %q, want effective-city", got.Subject)
	}
	var payload struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload): %v", err)
	}
	if payload.Name != "effective-city" {
		t.Fatalf("payload.Name = %q, want effective-city", payload.Name)
	}
	if payload.Path != cityPath {
		t.Fatalf("payload.Path = %q, want %q", payload.Path, cityPath)
	}
}

func TestUnregisterCityFromSupervisorRestoresRegistrationWhenControllerStopWaitFails(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	waitForSupervisorControllerStopHook = func(string, time.Duration) error {
		return io.EOF
	}

	var stdout, stderr bytes.Buffer
	handled, code := unregisterCityFromSupervisor(cityPath, &stdout, &stderr, "gc unregister")
	if !handled || code != 1 {
		t.Fatalf("unregisterCityFromSupervisor = (%t, %d), want (true, 1)", handled, code)
	}
	if !strings.Contains(stderr.String(), "restored registration") {
		t.Fatalf("stderr = %q, want restore message", stderr.String())
	}

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	resolvedCityPath := canonicalTestPath(cityPath)
	if len(entries) != 1 || entries[0].Path != resolvedCityPath {
		t.Fatalf("expected restored registry entry for %s, got %v", resolvedCityPath, entries)
	}
}

func TestControllerStatusForSupervisorManagedCityStopped(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4242 }
	supervisorCityRunningHook = func(string) (bool, string, bool) { return false, "", true }
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})

	ctrl := controllerStatusForCity(cityPath)
	if ctrl.Running || ctrl.PID != 4242 || ctrl.Mode != "supervisor" {
		t.Fatalf("controller status = %+v, want stopped supervisor PID", ctrl)
	}
}

func TestControllerStatusForSupervisorManagedCityPreservesInitStatus(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4242 }
	supervisorCityRunningHook = func(string) (bool, string, bool) { return false, "starting_bead_store", true }
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})

	ctrl := controllerStatusForCity(cityPath)
	if ctrl.Running || ctrl.PID != 4242 || ctrl.Mode != "supervisor" || ctrl.Status != "starting_bead_store" {
		t.Fatalf("controller status = %+v, want init-progress supervisor PID", ctrl)
	}
}

func TestCmdStopSupervisorManagedCityReliesOnSupervisorCleanup(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	root := shortSocketTempDir(t, "gcstop-")

	cityPath := filepath.Join(root, "city")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int {
			if err := shutdownBeadsProvider(cityPath); err != nil {
				t.Fatalf("shutdownBeadsProvider: %v", err)
			}
			return 0
		},
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck

	stopped := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := lis.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		buf := make([]byte, 32)
		n, _ := conn.Read(buf)
		if strings.Contains(string(buf[:n]), "stop") {
			stopped <- struct{}{}
		}
		conn.Write([]byte("ok\n")) //nolint:errcheck
	}()

	var stdout, stderr bytes.Buffer
	code := cmdStop([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdStop code = %d, want 0: %s", code, stderr.String())
	}
	select {
	case <-stopped:
		t.Fatal("did not expect a legacy controller stop request for a supervisor-managed city")
	case <-time.After(100 * time.Millisecond):
	}

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected city to be unregistered after stop, got %v", entries)
	}

	ops := readOpLog(t, logFile)
	assertSingleStopWithBenignNoise(t, ops)
}

func TestReconcileCitiesNameDriftStopsBeadsProvider(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	root, err := os.MkdirTemp("", "gc-drift-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) }) //nolint:errcheck

	cityPath := filepath.Join(root, "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "new-name"); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultCity("old-name")
	sp := runtime.NewFake()
	var cityOut, cityErr bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "old-name",
		Cfg:      &cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{}
		},
		Rec:    events.Discard,
		Stdout: &cityOut,
		Stderr: &cityErr,
	})

	done := make(chan struct{})
	close(done)
	registry := newCityRegistry()
	registry.Add(cityPath, &managedCity{
		cr:      cr,
		name:    "old-name",
		started: true,
		cancel:  func() {},
		done:    done,
	})
	var stdout, stderr bytes.Buffer

	reconcileCities(reg, registry, supervisor.PublicationConfig{}, &stdout, &stderr)

	ops := readOpLog(t, logFile)
	assertSingleStopWithBenignNoise(t, ops)
}

func TestSupervisorCreatesControllerSocketForManagedCity(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := shortSocketTempDir(t, "gc-supervisor-city-")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "test-city"); err != nil {
		t.Fatal(err)
	}

	cr := newCityRegistry()
	var stdout, stderr bytes.Buffer
	reconcileCities(reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)

	sockPath := filepath.Join(canonicalTestPath(cityPath), ".gc", "controller.sock")
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("controller.sock not created at %s after reconcileCities: %v", sockPath, err)
	}

	if pid := controllerAlive(canonicalTestPath(cityPath)); pid == 0 {
		t.Fatal("controller socket exists but does not respond to ping")
	}

	// Verify convergence commands are routed through the event loop.
	// An unknown command returns a domain error rather than the "no bead store"
	// sentinel, proving the full socket → event-loop → handler path is wired.
	reply, err := sendConvergenceRequest(canonicalTestPath(cityPath), convergenceRequest{
		Command: "list", // not a valid command; exercises the handler dispatch path
	})
	if err != nil {
		t.Fatalf("sendConvergenceRequest: %v", err)
	}
	if strings.Contains(reply.Error, "convergence not available") {
		t.Fatalf("convergence event loop wired but convHandler is nil; got: %q", reply.Error)
	}
	if !strings.Contains(reply.Error, "unknown convergence command") {
		t.Fatalf("expected 'unknown convergence command' error, got: %q", reply.Error)
	}

	// Cleanup: cancel the city goroutine and wait for it to exit.
	if done := cr.CancelCity(canonicalTestPath(cityPath)); done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("city goroutine did not exit in time")
		}
	}
}

var testGitEnvBlacklist = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
}

func initBarePackRepo(t *testing.T, name string) string {
	t.Helper()

	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	bareDir := filepath.Join(root, name+".git")

	mustGit(t, "", "init", workDir)
	if err := os.MkdirAll(filepath.Join(workDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	packToml := strings.Join([]string{
		"[pack]",
		`name = "` + name + `"`,
		`version = "1.0.0"`,
		"schema = 1",
		"",
		"[[agent]]",
		`name = "worker"`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(workDir, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "prompts", "worker.md"), []byte("you are a worker"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "initial")
	mustGit(t, workDir, "clone", "--bare", workDir, bareDir)
	return bareDir
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	fullArgs := append([]string{"-c", "core.hooksPath="}, args...)
	cmd := exec.Command("git", fullArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	for _, env := range os.Environ() {
		if key, _, ok := strings.Cut(env, "="); ok && testGitEnvBlacklist[key] {
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), string(out), err)
	}
}

func TestWaitForSupervisorCityPrintsStatusChanges(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	statuses := []string{"loading_config", "starting_bead_store", "resolving_formulas", "starting_agents"}
	callIdx := 0
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) {
			if callIdx >= len(statuses) {
				return true, "", true
			}
			s := statuses[callIdx]
			callIdx++
			return false, s, true
		},
		2*time.Second,
		time.Millisecond,
	)

	var stdout bytes.Buffer
	err := waitForSupervisorCity("/fake/city", true, 2*time.Second, &stdout)
	if err != nil {
		t.Fatalf("waitForSupervisorCity error = %v", err)
	}
	out := stdout.String()
	for _, expected := range []string{
		"Loading configuration...",
		"Starting bead store...",
		"Resolving formulas...",
		"Starting agents...",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("stdout = %q, want %q", out, expected)
		}
	}
}

func TestListCitiesIncludesInitStatus(t *testing.T) {
	reg := newCityRegistry()
	reg.Add("/running", &managedCity{
		name:    "running-city",
		started: true,
		cr:      &CityRuntime{cityName: "running-city"},
	})
	// Add init status via BatchUpdate (city not in main map yet).
	reg.BatchUpdate(func(
		_ map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		initStatus["/loading"] = cityInitProgress{name: "loading-city", status: "starting_bead_store"}
	})

	list := reg.ListCities()
	if len(list) != 2 {
		t.Fatalf("ListCities() returned %d cities, want 2", len(list))
	}

	found := map[string]bool{}
	for _, ci := range list {
		found[ci.Name] = true
		switch ci.Name {
		case "running-city":
			if !ci.Running {
				t.Error("running-city should be Running=true")
			}
			if ci.Status != "" {
				t.Errorf("running-city Status = %q, want empty", ci.Status)
			}
		case "loading-city":
			if ci.Running {
				t.Error("loading-city should be Running=false")
			}
			if ci.Status != "starting_bead_store" {
				t.Errorf("loading-city Status = %q, want starting_bead_store", ci.Status)
			}
		}
	}
	if !found["running-city"] || !found["loading-city"] {
		t.Fatalf("missing expected cities in %v", list)
	}
}

func TestReconcileCitiesSkipsCityAlreadyInitializing(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	registry := newCityRegistry()
	registry.BatchUpdate(func(
		_ map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		initStatus[cityPath] = cityInitProgress{name: "bright-lights", status: "starting_bead_store"}
	})

	var stdout, stderr bytes.Buffer
	reconcileCities(reg, registry, supervisor.PublicationConfig{}, &stdout, &stderr)

	registry.ReadCallback(func(
		_ map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		if _, ok := initStatus[cityPath]; !ok {
			t.Fatalf("initStatus missing for %s after reconcile", cityPath)
		}
		if rec := initFailures[cityPath]; rec != nil {
			t.Fatalf("unexpected init failure while city was already initializing: %+v", rec)
		}
	})
}

func TestPublishManagedCityMarksRunningBeforeInitialReconcile(t *testing.T) {
	registry := newCityRegistry()
	cityPath := "/tmp/bright-lights"
	cs := &controllerState{}
	mc := &managedCity{
		cr:     &CityRuntime{cityName: "bright-lights", cs: cs},
		name:   "bright-lights",
		status: "adopting_sessions",
	}

	registry.BatchUpdate(func(
		_ map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		initStatus[cityPath] = cityInitProgress{name: "bright-lights", status: "checking_agent_images"}
		initFailures[cityPath] = &initFailRecord{lastError: "old failure"}
	})

	if alreadyRunning := publishManagedCity(registry, cityPath, mc); alreadyRunning {
		t.Fatal("publishManagedCity reported already running for a new city")
	}

	cities := registry.ListCities()
	if len(cities) != 1 {
		t.Fatalf("ListCities() returned %d cities, want 1", len(cities))
	}
	if !cities[0].Running {
		t.Fatalf("city Running = false, want true: %+v", cities[0])
	}
	if cities[0].Status != "" {
		t.Fatalf("city Status = %q, want empty once published", cities[0].Status)
	}
	if got := registry.CityState("bright-lights"); got != cs {
		t.Fatalf("CityState() = %#v, want controller state", got)
	}

	registry.ReadCallback(func(
		_ map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		initFailures map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		if _, ok := initStatus[cityPath]; ok {
			t.Fatalf("initStatus[%s] still present after publish", cityPath)
		}
		if _, ok := initFailures[cityPath]; ok {
			t.Fatalf("initFailures[%s] still present after publish", cityPath)
		}
	})
}

func TestStartupSessionComputationsDoNotQueryBeadStore(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	cfg := config.DefaultCity("bright-lights")
	cfg.Agents = []config.Agent{
		{
			Name:              "worker",
			Dir:               "gascity",
			Suspended:         true,
			IdleTimeout:       "5m",
			MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2),
		},
		{
			Name:        "solo",
			IdleTimeout: "5m",
		},
	}

	sp := runtime.NewFake()
	suspended := computeSuspendedNames(&cfg, "bright-lights", "/fake/city")
	poolSessions := computePoolSessions(&cfg, "bright-lights", "/fake/city", sp)
	poolDeathHandlers := computePoolDeathHandlers(&cfg, "bright-lights", "/fake/city", sp, nil)
	idleTracker := buildIdleTracker(&cfg, "bright-lights", "/fake/city", sp)

	if len(suspended) == 0 {
		t.Fatal("computeSuspendedNames() returned no entries")
	}
	if len(poolSessions) != 2 {
		t.Fatalf("computePoolSessions() returned %d entries, want 2", len(poolSessions))
	}
	if len(poolDeathHandlers) != 0 && len(poolDeathHandlers) != 2 {
		t.Fatalf("computePoolDeathHandlers() returned %d handlers, want 0 or 2", len(poolDeathHandlers))
	}
	if idleTracker == nil {
		t.Fatal("buildIdleTracker() returned nil, want tracker")
	}

	if ops := readOpLog(t, logFile); len(ops) != 0 {
		t.Fatalf("startup session computations should not touch bead store, got ops %v", ops)
	}
}
