package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/bootstrap"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func disableBootstrapForTests(t *testing.T) {
	t.Helper()
	old := bootstrap.BootstrapPacks
	bootstrap.BootstrapPacks = nil
	t.Cleanup(func() { bootstrap.BootstrapPacks = old })
}

func stubInitDependencyChecks(t *testing.T) {
	t.Helper()
	oldLookPath := initLookPath
	initLookPath = func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	t.Cleanup(func() { initLookPath = oldLookPath })

	oldRunVersion := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		switch binary {
		case "bd":
			return "bd version " + bdMinVersion, nil
		case "dolt":
			return "dolt version " + doltMinVersion, nil
		default:
			return binary + " version", nil
		}
	}
	t.Cleanup(func() { initRunVersion = oldRunVersion })
}

func stubInitDoltAuthorIdentity(t *testing.T, values map[string]string) {
	t.Helper()
	old := initRunDoltConfigGet
	initRunDoltConfigGet = func(key string) (string, error) {
		value := strings.TrimSpace(values[key])
		if value == "" {
			return "", errDoltConfigKeyMissing
		}
		return value, nil
	}
	t.Cleanup(func() { initRunDoltConfigGet = old })
}

func writeBootstrappedManagedBdCity(t *testing.T) string {
	t.Helper()
	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "bright-lights"

[beads]
provider = "bd"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func TestMaybePrintWizardProviderGuidanceNeedsAuth(t *testing.T) {
	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, _ []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if fresh {
			t.Fatal("wizard guidance should use cached probe mode")
		}
		return map[string]api.ReadinessItem{
			"claude": {
				Name:        "claude",
				Kind:        api.ProbeKindProvider,
				DisplayName: "Claude Code",
				Status:      api.ProbeStatusNeedsAuth,
			},
		}, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	var stdout bytes.Buffer
	maybePrintWizardProviderGuidance(wizardConfig{
		interactive: true,
		provider:    "claude",
	}, &stdout)

	out := stdout.String()
	if !strings.Contains(out, "Claude Code is not signed in yet") {
		t.Fatalf("stdout = %q, want readiness note", out)
	}
}

func TestProviderStatusFixHintIncludesClaudeOAuthToken(t *testing.T) {
	got := providerStatusFixHint("claude", api.ProbeStatusInvalidConfiguration)
	for _, want := range []string{"`claude.ai`", "`oauth_token`", "`firstParty`"} {
		if !strings.Contains(got, want) {
			t.Fatalf("providerStatusFixHint = %q, want %s", got, want)
		}
	}
}

func TestFinalizeInitBlocksProviderReadinessBeforeSupervisorRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	disableBootstrapForTests(t)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, wizardConfig{
		configName: "minimal",
		provider:   "claude",
	}, "", &initStdout, &initStderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, _ []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("finalizeInit should force a fresh readiness probe")
		}
		return map[string]api.ReadinessItem{
			"claude": {
				Name:        "claude",
				Kind:        api.ProbeKindProvider,
				DisplayName: "Claude Code",
				Status:      api.ProbeStatusNeedsAuth,
			},
		}, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	calledRegister := false
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		calledRegister = true
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	var stdout, stderr bytes.Buffer
	code = finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{
		commandName: "gc init",
	})
	if code != 1 {
		t.Fatalf("finalizeInit = %d, want 1", code)
	}
	if calledRegister {
		t.Fatal("registerCityWithSupervisor should not be called when provider readiness blocks init")
	}
	if !strings.Contains(stderr.String(), "startup is blocked by provider readiness") {
		t.Fatalf("stderr = %q, want provider readiness block message", stderr.String())
	}
	if !strings.Contains(stderr.String(), "run `claude auth login`") {
		t.Fatalf("stderr = %q, want Claude fix hint", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Override: gc init --skip-provider-readiness") {
		t.Fatalf("stderr = %q, want init override hint", stderr.String())
	}
}

func TestFinalizeInitWarnsForUnprobeableCustomProviderAndContinues(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	disableBootstrapForTests(t)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureCityScaffold(cityPath); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCity("bright-lights")
	cfg.Workspace.Provider = "wrapper"
	cfg.Providers = map[string]config.ProviderSpec{
		"wrapper": {
			DisplayName: "Wrapper Agent",
			Command:     "sh",
		},
	}
	content, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, providers []string, _ bool) (map[string]api.ReadinessItem, error) {
		t.Fatalf("unexpected readiness probe for unprobeable provider: %v", providers)
		return nil, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	var stdout, stderr bytes.Buffer
	code := finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{
		commandName: "gc init",
	})
	if code != 0 {
		t.Fatalf("finalizeInit = %d, want 0: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Wrapper Agent is referenced, but Gas City cannot verify its login state automatically yet.") {
		t.Fatalf("stdout = %q, want unprobeable-provider warning", stdout.String())
	}
}

func TestFinalizeInitFetchesRemotePacksBeforeProviderReadiness(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	disableBootstrapForTests(t)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureCityScaffold(cityPath); err != nil {
		t.Fatal(err)
	}

	remote := initBareProviderPackRepo(t, "remote-pack", "claude")
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

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, providers []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("finalizeInit should force a fresh readiness probe")
		}
		if len(providers) != 1 || providers[0] != "claude" {
			t.Fatalf("providers = %v, want [claude]", providers)
		}
		return map[string]api.ReadinessItem{
			"claude": {
				Name:        "claude",
				Kind:        api.ProbeKindProvider,
				DisplayName: "Claude Code",
				Status:      api.ProbeStatusConfigured,
			},
		}, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	var stdout, stderr bytes.Buffer
	code := finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{
		commandName: "gc init",
	})
	if code != 0 {
		t.Fatalf("finalizeInit = %d, want 0: %s", code, stderr.String())
	}

	cacheDir := config.PackCachePath(cityPath, "remote-pack", config.PackSource{Source: remote})
	if _, err := os.Stat(filepath.Join(cacheDir, "pack.toml")); err != nil {
		t.Fatalf("expected fetched pack cache at %s: %v", cacheDir, err)
	}
}

func TestFinalizeInitDoesNotWriteImplicitImportState(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, defaultWizardConfig(), "", &initStdout, &initStderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}

	oldLookPath := initLookPath
	initLookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	t.Cleanup(func() { initLookPath = oldLookPath })

	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	var stdout, stderr bytes.Buffer
	code = finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{
		commandName:           "gc init",
		skipProviderReadiness: true,
	})
	if code != 0 {
		t.Fatalf("finalizeInit = %d, want 0: %s", code, stderr.String())
	}

	implicitPath := filepath.Join(os.Getenv("GC_HOME"), "implicit-import.toml")
	if _, err := os.Stat(implicitPath); !os.IsNotExist(err) {
		t.Fatalf("implicit-import.toml should not be created during finalizeInit, stat err = %v", err)
	}
}

func TestFinalizeInitReportsConfigLoadErrorDuringProviderPreflight(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	disableBootstrapForTests(t)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureCityScaffold(cityPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n[broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{
		commandName: "gc init",
	})
	if code != 1 {
		t.Fatalf("finalizeInit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "startup is blocked by configuration loading") {
		t.Fatalf("stderr = %q, want configuration loading message", stderr.String())
	}
	if !strings.Contains(stderr.String(), "loading config for provider readiness") {
		t.Fatalf("stderr = %q, want config load detail", stderr.String())
	}
}

func TestFinalizeInitWithoutProgressSkipsStepCounter(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	disableBootstrapForTests(t)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, wizardConfig{
		configName: "minimal",
		provider:   "claude",
	}, "", &initStdout, &initStderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, _ []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("finalizeInit should force a fresh readiness probe")
		}
		return map[string]api.ReadinessItem{
			"claude": {
				Name:        "claude",
				Kind:        api.ProbeKindProvider,
				DisplayName: "Claude Code",
				Status:      api.ProbeStatusConfigured,
			},
		}, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) { return true, "", true },
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code = finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{
		commandName:  "gc init",
		showProgress: false,
	})
	if code != 0 {
		t.Fatalf("finalizeInit = %d, want 0: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "[9/9]") {
		t.Fatalf("stdout = %q, want no progress counter", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Waiting for supervisor to start city...") {
		t.Fatalf("stdout = %q, want plain wait message", stdout.String())
	}
}

func TestCmdInitResumesFinalizeForExistingCity(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	disableBootstrapForTests(t)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, wizardConfig{
		configName: "gastown",
		provider:   "claude",
	}, "", &initStdout, &initStderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, providers []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("cmdInit resume should force a fresh readiness probe")
		}
		if len(providers) != 1 || providers[0] != "claude" {
			t.Fatalf("providers = %v, want [claude]", providers)
		}
		return map[string]api.ReadinessItem{
			"claude": {
				Name:        "claude",
				Kind:        api.ProbeKindProvider,
				DisplayName: "Claude Code",
				Status:      api.ProbeStatusNeedsAuth,
			},
		}, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	calledRegister := false
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		calledRegister = true
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	var stdout, stderr bytes.Buffer
	code = cmdInit([]string{cityPath}, "", "", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("cmdInit = %d, want 1", code)
	}
	if calledRegister {
		t.Fatal("registerCityWithSupervisor should not run when provider readiness blocks resumed init")
	}
	if strings.Contains(stderr.String(), "already initialized") {
		t.Fatalf("stderr = %q, want resumed readiness guidance instead of already initialized", stderr.String())
	}
	if !strings.Contains(stdout.String(), "resuming startup checks") {
		t.Fatalf("stdout = %q, want resume notice", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Referenced providers not ready:") {
		t.Fatalf("stderr = %q, want provider readiness guidance", stderr.String())
	}
}

func TestCmdInitSkipProviderReadinessBypassesBlockedProvider(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	disableBootstrapForTests(t)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, wizardConfig{
		configName: "minimal",
		provider:   "claude",
	}, "", &initStdout, &initStderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}

	probeCalled := false
	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, _ []string, _ bool) (map[string]api.ReadinessItem, error) {
		probeCalled = true
		return map[string]api.ReadinessItem{
			"claude": {
				Name:        "claude",
				Kind:        api.ProbeKindProvider,
				DisplayName: "Claude Code",
				Status:      api.ProbeStatusNeedsAuth,
			},
		}, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	calledRegister := false
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		calledRegister = true
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	var stdout, stderr bytes.Buffer
	code = cmdInitWithOptions([]string{cityPath}, "", "", "", &stdout, &stderr, true, false)
	if code != 0 {
		t.Fatalf("cmdInitWithOptions = %d, want 0: %s", code, stderr.String())
	}
	if probeCalled {
		t.Fatal("provider readiness probe should be skipped")
	}
	if !calledRegister {
		t.Fatal("registerCityWithSupervisor should run when readiness is skipped")
	}
	if !strings.Contains(stdout.String(), "Skipping provider readiness checks") {
		t.Fatalf("stdout = %q, want skip readiness progress", stdout.String())
	}
}

func TestShellQuotePathQuotesMetacharacters(t *testing.T) {
	got := shellQuotePathForOS("/tmp/test&dir", "linux")
	want := "'/tmp/test&dir'"
	if got != want {
		t.Fatalf("shellQuotePathForOS = %q, want %q", got, want)
	}
}

func TestShellQuotePathForOSEmptyString(t *testing.T) {
	got := shellQuotePathForOS("", "linux")
	if got != "''" {
		t.Fatalf("shellQuotePathForOS empty = %q, want %q", got, "''")
	}
}

func TestShellQuotePathForOSWindows(t *testing.T) {
	got := shellQuotePathForOS(`C:\my city`, "windows")
	want := `"C:\my city"`
	if got != want {
		t.Fatalf("shellQuotePathForOS windows = %q, want %q", got, want)
	}
}

func TestInitRunVersionTimesOutHungVersionCommand(t *testing.T) {
	oldCommandContext := initRunVersionCommandContext
	oldTimeout := initRunVersionTimeout
	initRunVersionTimeout = 50 * time.Millisecond
	initRunVersionCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcessInitRunVersionHang", "--")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}
	t.Cleanup(func() {
		initRunVersionCommandContext = oldCommandContext
		initRunVersionTimeout = oldTimeout
	})

	start := time.Now()
	line, err := initRunVersion("hung-binary")
	if err == nil {
		t.Fatal("initRunVersion error = nil, want timeout")
	}
	if line != "" {
		t.Fatalf("initRunVersion line = %q, want empty on timeout", line)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("initRunVersion elapsed = %v, want timeout-bound execution", elapsed)
	}
}

func TestHelperProcessInitRunVersionHang(_ *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

func initBareProviderPackRepo(t *testing.T, name, provider string) string {
	t.Helper()

	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	bareDir := filepath.Join(root, name+".git")

	mustGit(t, "", "init", workDir)
	packToml := strings.Join([]string{
		"[pack]",
		`name = "` + name + `"`,
		`version = "1.0.0"`,
		"schema = 1",
		"",
		"[[agent]]",
		`name = "worker"`,
		`provider = "` + provider + `"`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(workDir, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "initial")
	mustGit(t, workDir, "clone", "--bare", workDir, bareDir)
	return bareDir
}

func TestCheckHardDependenciesTreatsExecGcBeadsBdAsBdContract(t *testing.T) {
	t.Setenv("GC_BEADS", "exec:/tmp/gc-beads-bd")

	oldLookPath := initLookPath
	initLookPath = func(name string) (string, error) {
		if name == "dolt" {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}
	t.Cleanup(func() { initLookPath = oldLookPath })

	oldRunVersion := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		switch binary {
		case "bd":
			return "bd version " + bdMinVersion, nil
		case "flock", "tmux", "jq", "git", "pgrep", "lsof":
			return binary + " version", nil
		default:
			return binary + " version " + doltMinVersion, nil
		}
	}
	t.Cleanup(func() { initRunVersion = oldRunVersion })

	missing := checkHardDependencies(t.TempDir())
	if len(missing) != 1 {
		t.Fatalf("missing deps = %#v, want only dolt", missing)
	}
	if missing[0].name != "dolt" {
		t.Fatalf("missing dep = %#v, want dolt", missing[0])
	}
}

func TestCheckHardDependenciesRequiresBoundedRunnerForBdContract(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	oldLookPath := initLookPath
	initLookPath = func(name string) (string, error) {
		switch name {
		case "timeout", "gtimeout", "python3":
			return "", os.ErrNotExist
		default:
			return "/usr/bin/" + name, nil
		}
	}
	t.Cleanup(func() { initLookPath = oldLookPath })

	oldRunVersion := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		switch binary {
		case "bd":
			return "bd version " + bdMinVersion, nil
		case "flock", "tmux", "jq", "git", "pgrep", "lsof":
			return binary + " version", nil
		default:
			return binary + " version " + doltMinVersion, nil
		}
	}
	t.Cleanup(func() { initRunVersion = oldRunVersion })

	missing := checkHardDependencies(t.TempDir())
	if len(missing) != 1 {
		t.Fatalf("missing deps = %#v, want only bounded runner", missing)
	}
	if missing[0].name != "timeout/gtimeout/python3" {
		t.Fatalf("missing dep = %#v, want timeout/gtimeout/python3", missing[0])
	}
}

func TestCheckHardDependenciesAcceptsPythonFallbackForBdContract(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	oldLookPath := initLookPath
	initLookPath = func(name string) (string, error) {
		switch name {
		case "timeout", "gtimeout":
			return "", os.ErrNotExist
		default:
			return "/usr/bin/" + name, nil
		}
	}
	t.Cleanup(func() { initLookPath = oldLookPath })

	oldRunVersion := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		switch binary {
		case "bd":
			return "bd version " + bdMinVersion, nil
		case "flock", "tmux", "jq", "git", "pgrep", "lsof":
			return binary + " version", nil
		default:
			return binary + " version " + doltMinVersion, nil
		}
	}
	t.Cleanup(func() { initRunVersion = oldRunVersion })

	if missing := checkHardDependencies(t.TempDir()); len(missing) != 0 {
		t.Fatalf("missing deps = %#v, want python3 fallback to satisfy bounded runner", missing)
	}
}

func TestCheckHardDependenciesRejectsDoltPreReleaseAtFloor(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	oldLookPath := initLookPath
	initLookPath = func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	t.Cleanup(func() { initLookPath = oldLookPath })

	oldRunVersion := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		switch binary {
		case "dolt":
			return "dolt version 1.86.2-rc1", nil
		case "bd":
			return "bd version " + bdMinVersion, nil
		case "flock", "tmux", "jq", "git", "pgrep", "lsof":
			return binary + " version", nil
		default:
			return binary + " version " + doltMinVersion, nil
		}
	}
	t.Cleanup(func() { initRunVersion = oldRunVersion })

	missing := checkHardDependencies(t.TempDir())
	if len(missing) != 1 {
		t.Fatalf("missing deps = %#v, want only dolt prerelease rejection", missing)
	}
	if !strings.Contains(missing[0].name, "dolt") || !strings.Contains(missing[0].name, "1.86.2-rc1") {
		t.Fatalf("missing dep = %#v, want dolt prerelease version in dependency name", missing[0])
	}
}

func TestCheckHardDependenciesRequiresBdToolsForBdRigUnderFileCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
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
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	oldLookPath := initLookPath
	initLookPath = func(name string) (string, error) {
		if name == "dolt" {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}
	t.Cleanup(func() { initLookPath = oldLookPath })

	oldRunVersion := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		switch binary {
		case "bd":
			return "bd version " + bdMinVersion, nil
		case "flock", "tmux", "jq", "git", "pgrep", "lsof":
			return binary + " version", nil
		default:
			return binary + " version " + doltMinVersion, nil
		}
	}
	t.Cleanup(func() { initRunVersion = oldRunVersion })

	missing := checkHardDependencies(cityDir)
	if len(missing) != 1 || missing[0].name != "dolt" {
		t.Fatalf("missing deps = %#v, want only dolt for bd-backed rig", missing)
	}
}

func TestCheckHardDependenciesRequiresBdToolsForSiteBoundBdRigUnderFileCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
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
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte(`[[rig]]
name = "frontend"
path = "frontend"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	oldLookPath := initLookPath
	initLookPath = func(name string) (string, error) {
		if name == "dolt" {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}
	t.Cleanup(func() { initLookPath = oldLookPath })

	oldRunVersion := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		switch binary {
		case "bd":
			return "bd version " + bdMinVersion, nil
		case "flock", "tmux", "jq", "git", "pgrep", "lsof":
			return binary + " version", nil
		default:
			return binary + " version " + doltMinVersion, nil
		}
	}
	t.Cleanup(func() { initRunVersion = oldRunVersion })

	missing := checkHardDependencies(cityDir)
	if len(missing) != 1 || missing[0].name != "dolt" {
		t.Fatalf("missing deps = %#v, want only dolt for site-bound bd-backed rig", missing)
	}
}

func TestFinalizeInitCanonicalizesBdStoreBeforeProviderReadinessBlock(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, wizardConfig{
		configName: "minimal",
		provider:   "claude",
	}, "", &initStdout, &initStderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, _ []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("finalizeInit should force a fresh readiness probe")
		}
		if _, err := os.Stat(filepath.Join(cityPath, ".beads", "metadata.json")); err != nil {
			t.Fatalf("metadata.json missing before readiness block: %v", err)
		}
		if _, err := os.Stat(filepath.Join(cityPath, ".beads", "config.yaml")); err != nil {
			t.Fatalf("config.yaml missing before readiness block: %v", err)
		}
		return map[string]api.ReadinessItem{
			"claude": {
				Name:        "claude",
				Kind:        api.ProbeKindProvider,
				DisplayName: "Claude Code",
				Status:      api.ProbeStatusNeedsAuth,
			},
		}, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	calledRegister := false
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		calledRegister = true
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	var stdout, stderr bytes.Buffer
	code = finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{commandName: "gc init"})
	if code != 1 {
		t.Fatalf("finalizeInit = %d, want 1", code)
	}
	if calledRegister {
		t.Fatal("registerCityWithSupervisor should not run when provider readiness blocks init")
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "metadata.json")); err != nil {
		t.Fatalf("metadata.json missing after readiness block: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "config.yaml")); err != nil {
		t.Fatalf("config.yaml missing after readiness block: %v", err)
	}
}

func TestFinalizeInitBlocksManagedBdWhenDoltIdentityMissing(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	disableBootstrapForTests(t)
	stubInitDependencyChecks(t)
	stubInitDoltAuthorIdentity(t, map[string]string{})

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, wizardConfig{
		configName: "minimal",
		provider:   "claude",
	}, "", &initStdout, &initStderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(context.Context, []string, bool) (map[string]api.ReadinessItem, error) {
		t.Fatal("provider readiness should not run before Dolt identity is configured")
		return nil, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	var stdout, stderr bytes.Buffer
	code = finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{commandName: "gc init"})
	if code != 1 {
		t.Fatalf("finalizeInit = %d, want 1", code)
	}
	text := stderr.String()
	for _, want := range []string{
		"startup is blocked by Dolt author identity",
		"user.name",
		"user.email",
		`dolt config --global --add user.name "Your Name"`,
		`dolt config --global --add user.email "you@example.com"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stderr missing %q:\n%s", want, text)
		}
	}
}

func TestDoStartBlocksManagedBdWhenDoltIdentityMissingBeforeSupervisorRegistration(t *testing.T) {
	clearInheritedBeadsEnv(t)
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_DOLT", "")
	stubInitDependencyChecks(t)
	stubInitDoltAuthorIdentity(t, map[string]string{})

	cityPath := writeBootstrappedManagedBdCity(t)

	calledRegister := false
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		calledRegister = true
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	var stdout, stderr bytes.Buffer
	code := doStart([]string{cityPath}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if calledRegister {
		t.Fatal("registerCityWithSupervisor should not run before Dolt identity is configured")
	}
	text := stderr.String()
	for _, want := range []string{
		"gc start: city created, but startup is blocked by Dolt author identity",
		"user.name",
		"user.email",
		`dolt config --global --add user.name "Your Name"`,
		`dolt config --global --add user.email "you@example.com"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stderr missing %q:\n%s", want, text)
		}
	}
}

func TestDoStartForegroundBlocksManagedBdWhenDoltIdentityMissingBeforeLifecycle(t *testing.T) {
	clearInheritedBeadsEnv(t)
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_DOLT", "")
	stubInitDependencyChecks(t)
	stubInitDoltAuthorIdentity(t, map[string]string{})

	cityPath := writeBootstrappedManagedBdCity(t)

	var stdout, stderr bytes.Buffer
	code := doStart([]string{cityPath}, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart --foreground code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	text := stderr.String()
	for _, want := range []string{
		"gc start: city created, but startup is blocked by Dolt author identity",
		"user.name",
		"user.email",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stderr missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `hint: run "gc doctor"`) {
		t.Fatalf("stderr shows lifecycle failure instead of identity preflight:\n%s", text)
	}
}

func TestDoStartForegroundReportsHardDependenciesBeforeDoltIdentity(t *testing.T) {
	clearInheritedBeadsEnv(t)
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_DOLT", "")

	oldLookPath := initLookPath
	initLookPath = func(name string) (string, error) {
		if name == "bd" {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}
	t.Cleanup(func() { initLookPath = oldLookPath })

	oldRunVersion := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		switch binary {
		case "bd":
			return "bd version " + bdMinVersion, nil
		case "dolt":
			return "dolt version " + doltMinVersion, nil
		default:
			return binary + " version", nil
		}
	}
	t.Cleanup(func() { initRunVersion = oldRunVersion })

	oldDoltConfigGet := initRunDoltConfigGet
	initRunDoltConfigGet = func(string) (string, error) {
		t.Fatal("Dolt identity should not be probed before hard dependency failures are reported")
		return "", nil
	}
	t.Cleanup(func() { initRunDoltConfigGet = oldDoltConfigGet })

	cityPath := writeBootstrappedManagedBdCity(t)

	var stdout, stderr bytes.Buffer
	code := doStart([]string{cityPath}, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart --foreground code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	text := stderr.String()
	for _, want := range []string{
		"gc start: missing required dependencies:",
		"bd",
		"gc start: install the missing dependencies, then try again",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stderr missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "startup is blocked by Dolt author identity") {
		t.Fatalf("stderr reports identity before hard dependencies:\n%s", text)
	}
}

func TestCheckDoltAuthorIdentitySkipsWhenGCDoltSkip(t *testing.T) {
	clearInheritedBeadsEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", " skip ")
	stubInitDependencyChecks(t)

	old := initRunDoltConfigGet
	initRunDoltConfigGet = func(string) (string, error) {
		t.Fatal("Dolt identity should not be probed when GC_DOLT=skip")
		return "", nil
	}
	t.Cleanup(func() { initRunDoltConfigGet = old })

	if status := checkDoltAuthorIdentity(t.TempDir()); status.blocked() {
		t.Fatalf("checkDoltAuthorIdentity blocked with GC_DOLT=skip: %#v", status)
	}
}

func TestGCDoltSkipTrimsWhitespace(t *testing.T) {
	t.Setenv("GC_DOLT", " skip ")

	if !gcDoltSkip() {
		t.Fatal("gcDoltSkip() = false, want true for whitespace-padded skip")
	}
}

func TestCheckDoltAuthorIdentityReportsPartialMissingKey(t *testing.T) {
	clearInheritedBeadsEnv(t)
	t.Setenv("GC_BEADS", "bd")
	stubInitDependencyChecks(t)
	stubInitDoltAuthorIdentity(t, map[string]string{"user.name": "Test User"})

	status := checkDoltAuthorIdentity(t.TempDir())
	if len(status.probeErrors) != 0 {
		t.Fatalf("probe errors = %#v, want none", status.probeErrors)
	}
	if got, want := strings.Join(status.missingKeys, ","), "user.email"; got != want {
		t.Fatalf("missing keys = %q, want %q", got, want)
	}
}

func TestCheckDoltAuthorIdentitySkipsWhenDoltMissing(t *testing.T) {
	clearInheritedBeadsEnv(t)
	t.Setenv("GC_BEADS", "bd")

	oldLookPath := initLookPath
	initLookPath = func(name string) (string, error) {
		if name == "dolt" {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}
	t.Cleanup(func() { initLookPath = oldLookPath })

	old := initRunDoltConfigGet
	initRunDoltConfigGet = func(string) (string, error) {
		t.Fatal("Dolt identity should not be probed when dolt is not on PATH")
		return "", nil
	}
	t.Cleanup(func() { initRunDoltConfigGet = old })

	if status := checkDoltAuthorIdentity(t.TempDir()); status.blocked() {
		t.Fatalf("checkDoltAuthorIdentity blocked without dolt on PATH: %#v", status)
	}
}

func TestCheckDoltAuthorIdentitySkipsRigExternalDoltUnderFileCity(t *testing.T) {
	clearInheritedBeadsEnv(t)
	t.Setenv("GC_BEADS", "file")
	stubInitDependencyChecks(t)

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
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
dolt_host = "rig-db.example.com"
dolt_port = "3307"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	old := initRunDoltConfigGet
	initRunDoltConfigGet = func(string) (string, error) {
		t.Fatal("rig-only external Dolt should not require local identity")
		return "", nil
	}
	t.Cleanup(func() { initRunDoltConfigGet = old })

	if status := checkDoltAuthorIdentity(cityDir); status.blocked() {
		t.Fatalf("checkDoltAuthorIdentity blocked for rig external Dolt: %#v", status)
	}
}

func TestCheckDoltAuthorIdentitySkipsCityExternalDoltFromConfig(t *testing.T) {
	clearInheritedBeadsEnv(t)
	stubInitDependencyChecks(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[dolt]
host = "city-db.example.com"
port = 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}

	old := initRunDoltConfigGet
	initRunDoltConfigGet = func(string) (string, error) {
		t.Fatal("city external Dolt should not require local identity")
		return "", nil
	}
	t.Cleanup(func() { initRunDoltConfigGet = old })

	if status := checkDoltAuthorIdentity(cityDir); status.blocked() {
		t.Fatalf("checkDoltAuthorIdentity blocked for city external Dolt: %#v", status)
	}
}

func TestCheckDoltAuthorIdentityUsesCanonicalManagedCityOverStaleExternalConfig(t *testing.T) {
	clearInheritedBeadsEnv(t)
	stubInitDependencyChecks(t)

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[dolt]
host = "stale-db.example.com"
port = 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	stubInitDoltAuthorIdentity(t, map[string]string{})

	status := checkDoltAuthorIdentity(cityDir)
	if got, want := strings.Join(status.missingKeys, ","), "user.name,user.email"; got != want {
		t.Fatalf("missing keys = %q, want %q", got, want)
	}
	if len(status.probeErrors) != 0 {
		t.Fatalf("probe errors = %#v, want none", status.probeErrors)
	}
}

func TestCheckDoltAuthorIdentityReportsProbeErrorsSeparately(t *testing.T) {
	clearInheritedBeadsEnv(t)
	t.Setenv("GC_BEADS", "bd")
	stubInitDependencyChecks(t)

	old := initRunDoltConfigGet
	initRunDoltConfigGet = func(key string) (string, error) {
		if key == "user.name" {
			return "", fmt.Errorf("dolt config probe timed out after 2s")
		}
		return "test@example.com", nil
	}
	t.Cleanup(func() { initRunDoltConfigGet = old })

	status := checkDoltAuthorIdentity(t.TempDir())
	if len(status.missingKeys) != 0 {
		t.Fatalf("missing keys = %#v, want none", status.missingKeys)
	}
	if len(status.probeErrors) != 1 || status.probeErrors[0].key != "user.name" {
		t.Fatalf("probe errors = %#v, want user.name probe error", status.probeErrors)
	}

	var stderr bytes.Buffer
	printDoltAuthorIdentityBlock(&stderr, "gc init", status)
	text := stderr.String()
	for _, want := range []string{
		"Could not verify Dolt identity:",
		"user.name: dolt config probe timed out after 2s",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stderr missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Missing Dolt config:") {
		t.Fatalf("stderr misreported probe error as missing config:\n%s", text)
	}
}

func TestInitRunDoltConfigGetReportsExitStderrAsProbeError(t *testing.T) {
	binDir := t.TempDir()
	doltPath := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(doltPath, []byte("#!/bin/sh\necho 'unreadable global config' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	value, err := initRunDoltConfigGet("user.name")
	if value != "" {
		t.Fatalf("value = %q, want empty", value)
	}
	if err == nil {
		t.Fatal("initRunDoltConfigGet error = nil, want probe error")
	}
	if errors.Is(err, errDoltConfigKeyMissing) {
		t.Fatalf("initRunDoltConfigGet error = %v, want probe error not missing-key sentinel", err)
	}
	if !strings.Contains(err.Error(), "unreadable global config") {
		t.Fatalf("initRunDoltConfigGet error = %v, want stderr detail", err)
	}
}

func TestInitRunDoltConfigGetTreatsSilentEmptyExitAsMissingKey(t *testing.T) {
	binDir := t.TempDir()
	doltPath := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(doltPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	value, err := initRunDoltConfigGet("user.name")
	if value != "" {
		t.Fatalf("value = %q, want empty", value)
	}
	if !errors.Is(err, errDoltConfigKeyMissing) {
		t.Fatalf("initRunDoltConfigGet error = %v, want missing-key sentinel", err)
	}
}

func TestFinalizeInitCanonicalizesBdStoreBeforeProviderReadinessBlockWithoutSkip(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	configureIsolatedRuntimeEnv(t)
	stubInitDoltAuthorIdentity(t, map[string]string{
		"user.name":  "gc-test",
		"user.email": "gc-test@test.local",
	})

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, wizardConfig{
		configName: "minimal",
		provider:   "claude",
	}, "", &initStdout, &initStderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, _ []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("finalizeInit should force a fresh readiness probe")
		}
		if _, err := os.Stat(filepath.Join(cityPath, ".beads", "metadata.json")); err != nil {
			t.Fatalf("metadata.json missing before readiness block: %v", err)
		}
		if _, err := os.Stat(filepath.Join(cityPath, ".beads", "config.yaml")); err != nil {
			t.Fatalf("config.yaml missing before readiness block: %v", err)
		}
		return map[string]api.ReadinessItem{
			"claude": {
				Name:        "claude",
				Kind:        api.ProbeKindProvider,
				DisplayName: "Claude Code",
				Status:      api.ProbeStatusNeedsAuth,
			},
		}, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	var stdout, stderr bytes.Buffer
	code = finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{commandName: "gc init"})
	if code != 1 {
		t.Fatalf("finalizeInit = %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "metadata.json")); err != nil {
		t.Fatalf("metadata.json missing after readiness block: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "config.yaml")); err != nil {
		t.Fatalf("config.yaml missing after readiness block: %v", err)
	}
}

func TestFinalizeInitDoesNotRunBdProviderBeforeProviderReadinessBlock(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_DOLT", "")
	stubInitDoltAuthorIdentity(t, map[string]string{
		"user.name":  "gc-test",
		"user.email": "gc-test@test.local",
	})

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, wizardConfig{
		configName: "minimal",
		provider:   "claude",
	}, "", &initStdout, &initStderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}

	spyDir := t.TempDir()
	callLog := filepath.Join(spyDir, "gc-beads-bd.calls")
	spy := filepath.Join(spyDir, "gc-beads-bd")
	scriptBody := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nexit 0\n", callLog)
	if err := os.WriteFile(spy, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "")
	cityConfigPath := filepath.Join(cityPath, "city.toml")
	cityConfig, err := os.ReadFile(cityConfigPath)
	if err != nil {
		t.Fatalf("ReadFile(city.toml): %v", err)
	}
	cityConfig = append(cityConfig, []byte(fmt.Sprintf("\n[beads]\nprovider = %q\n", "exec:"+spy))...)
	if err := os.WriteFile(cityConfigPath, cityConfig, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, _ []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("finalizeInit should force a fresh readiness probe")
		}
		return map[string]api.ReadinessItem{
			"claude": {
				Name:        "claude",
				Kind:        api.ProbeKindProvider,
				DisplayName: "Claude Code",
				Status:      api.ProbeStatusNeedsAuth,
			},
		}, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	var stdout, stderr bytes.Buffer
	code = finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{commandName: "gc init"})
	if code != 1 {
		t.Fatalf("finalizeInit = %d, want 1", code)
	}
	if data, err := os.ReadFile(callLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("gc-beads-bd should not run before provider readiness passes, got:\n%s", data)
	}
}
