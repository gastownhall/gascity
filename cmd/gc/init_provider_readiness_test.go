package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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

func TestFinalizeInitBlocksProviderReadinessBeforeSupervisorRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	disableBootstrapForTests(t)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, wizardConfig{
		configName: "tutorial",
		provider:   "claude",
	}, "", &initStdout, &initStderr)
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

func TestFinalizeInitBootstrapsImplicitImports(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BOOTSTRAP", "on")

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, defaultWizardConfig(), "", &initStdout, &initStderr)
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
	data, err := os.ReadFile(implicitPath)
	if err != nil {
		t.Fatalf("reading implicit-import.toml: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `[imports."import"]`) {
		t.Fatalf("implicit-import.toml missing import entry:\n%s", text)
	}
	if !strings.Contains(text, `source = "github.com/gastownhall/gc-import"`) {
		t.Fatalf("implicit-import.toml missing import source:\n%s", text)
	}
}

func TestFinalizeInitReportsBootstrapFailure(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BOOTSTRAP", "on")

	oldBootstrap := bootstrap.BootstrapPacks
	bootstrap.BootstrapPacks = []bootstrap.Entry{{
		Name:     "import",
		Source:   "github.com/gastownhall/gc-import",
		Version:  "0.2.0",
		AssetDir: "packs/missing",
	}}
	t.Cleanup(func() { bootstrap.BootstrapPacks = oldBootstrap })

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, defaultWizardConfig(), "", &initStdout, &initStderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}

	var stdout, stderr bytes.Buffer
	code = finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{
		commandName:           "gc init",
		skipProviderReadiness: true,
	})
	if code != 1 {
		t.Fatalf("finalizeInit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "bootstrapping implicit imports") {
		t.Fatalf("stderr = %q, want bootstrap failure message", stderr.String())
	}
}

func TestInitRunVersionTimesOut(t *testing.T) {
	script := filepath.Join(t.TempDir(), "hang-version.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldTimeout := initVersionTimeout
	oldWaitDelay := initVersionWaitDelay
	initVersionTimeout = 200 * time.Millisecond
	initVersionWaitDelay = 50 * time.Millisecond
	t.Cleanup(func() { initVersionTimeout = oldTimeout })
	t.Cleanup(func() { initVersionWaitDelay = oldWaitDelay })

	started := time.Now()
	_, err := initRunVersion(script)
	if err == nil {
		t.Fatal("initRunVersion error = nil, want timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("initRunVersion error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("initRunVersion elapsed = %s, want fast timeout", elapsed)
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
		configName: "tutorial",
		provider:   "claude",
	}, "", &initStdout, &initStderr)
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
	if strings.Contains(stdout.String(), "[8/8]") {
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
	}, "", &initStdout, &initStderr)
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
		configName: "tutorial",
		provider:   "claude",
	}, "", &initStdout, &initStderr)
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
	code = cmdInitWithOptions([]string{cityPath}, "", "", "", &stdout, &stderr, true)
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
