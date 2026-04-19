package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestControllerLoopCancel(t *testing.T) {
	sp := runtime.NewFake()
	name := "mayor"
	tp := TemplateParams{
		SessionName:  name,
		TemplateName: name,
		Command:      "echo hello",
	}

	var reconcileCount atomic.Int32
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		reconcileCount.Add(1)
		return DesiredStateResult{State: map[string]TemplateParams{name: tp}}
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer

	// Cancel immediately after initial reconciliation completes.
	go func() {
		for reconcileCount.Load() < 1 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	controllerLoop(ctx, time.Hour, cfg, "test", "", nil, buildFn, sp, nil, nil, nil, nil, nil, events.Discard, nil, nil, nil, nil, &stdout, &stderr)

	if reconcileCount.Load() < 1 {
		t.Error("expected at least one reconciliation")
	}
}

func TestControllerLoopTick(t *testing.T) {
	sp := runtime.NewFake()
	name := "mayor"
	tp := TemplateParams{
		SessionName:  name,
		TemplateName: name,
		Command:      "echo hello",
	}

	var reconcileCount atomic.Int32
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		reconcileCount.Add(1)
		return DesiredStateResult{State: map[string]TemplateParams{name: tp}}
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer

	// Use a very short interval so the tick fires quickly.
	go func() {
		for reconcileCount.Load() < 2 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	controllerLoop(ctx, 10*time.Millisecond, cfg, "test", "", nil, buildFn, sp, nil, nil, nil, nil, nil, events.Discard, nil, nil, nil, nil, &stdout, &stderr)

	if got := reconcileCount.Load(); got < 2 {
		t.Errorf("reconcile count = %d, want >= 2", got)
	}
}

func TestControllerLockExclusion(t *testing.T) {
	dir := shortSocketTempDir(t, "gc-lock-")
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// First lock should succeed.
	lock1, err := acquireControllerLock(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer lock1.Close() //nolint:errcheck // test cleanup

	// Second lock should fail.
	_, err = acquireControllerLock(dir)
	if err == nil {
		t.Fatal("expected error for second lock, got nil")
	}
}

func TestControllerShutdown(t *testing.T) {
	sp := runtime.NewFake()
	// Pre-start an agent to verify shutdown stops it.
	name := "mayor"
	_ = sp.Start(context.Background(), name, runtime.Config{Command: "echo hello"})
	tp := TemplateParams{
		SessionName:  name,
		TemplateName: name,
		Command:      "echo hello",
	}

	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{name: tp}}
	}

	dir := shortSocketTempDir(t, "gc-shutdown-")
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Agents:    []config.Agent{{Name: "mayor", StartCommand: "echo hello"}},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}

	// Write a city.toml so the controller uses the temp dir for bead store
	// operations rather than falling back to cwd (which may contain a slow
	// Dolt-backed .beads/ database).
	tomlPath := writeCityTOML(t, dir, "test", "mayor")

	var stdout, stderr bytes.Buffer

	// Run controller in a goroutine; it will block until canceled.
	// Use a close-able channel so cleanup can detect whether the
	// controller exited without double-draining.
	done := make(chan struct{})
	var exitCode int
	go func() {
		exitCode = runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &stdout, &stderr)
		close(done)
	}()

	// Ensure cleanup: if the test fails, send stop so the goroutine exits.
	t.Cleanup(func() {
		tryStopController(dir, &bytes.Buffer{})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	// Poll for controller socket to become available instead of fixed sleep.
	waitForController(t, dir)

	if !tryStopController(dir, &bytes.Buffer{}) {
		t.Fatal("tryStopController returned false, expected true")
	}

	select {
	case <-done:
		if exitCode != 0 {
			t.Errorf("runController exit code = %d, want 0; stderr: %s", exitCode, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runController did not exit after stop")
	}

	// Agent should have been stopped during shutdown.
	if sp.IsRunning("mayor") {
		t.Error("agent should be stopped after controller shutdown")
	}
}

func TestControllerSocketFallbackUsesShortPathForLongCityPath(t *testing.T) {
	base := shortSocketTempDir(t, "gc-controller-long-")
	cityPath := base
	for len(filepath.Join(normalizePathForCompare(cityPath), ".gc", "controller.sock")) <= controllerSocketPathLimit {
		cityPath = filepath.Join(cityPath, "segment-1234567890")
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	legacy := filepath.Join(cityPath, ".gc", "controller.sock")
	fallback := controllerSocketPath(cityPath)
	if fallback == legacy {
		t.Fatalf("controllerSocketPath(%q) = %q, want fallback path", cityPath, fallback)
	}
	if !strings.HasPrefix(fallback, filepath.Join("/tmp", "gascity-controller")+string(filepath.Separator)) {
		t.Fatalf("controllerSocketPath(%q) = %q, want /tmp/gascity-controller fallback", cityPath, fallback)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	convergenceReqCh := make(chan convergenceRequest, 1)
	pokeCh := make(chan struct{}, 1)
	controlDispatcherCh := make(chan struct{}, 1)
	configDirty := &atomic.Bool{}
	lis, err := startControllerSocket(cityPath, cancel, configDirty, nil, convergenceReqCh, pokeCh, controlDispatcherCh)
	if err != nil {
		t.Fatalf("startControllerSocket: %v", err)
	}
	defer lis.Close()         //nolint:errcheck
	defer os.Remove(fallback) //nolint:errcheck

	if _, err := os.Stat(fallback); err != nil {
		t.Fatalf("stat fallback socket %s: %v", fallback, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy socket %s should not exist, stat err = %v", legacy, err)
	}
	if pid := controllerAlive(cityPath); pid == 0 {
		t.Fatal("controllerAlive = 0, want live controller via fallback socket")
	}
	resp, err := sendControllerCommand(cityPath, "reload")
	if err != nil {
		t.Fatalf("sendControllerCommand(reload): %v", err)
	}
	if strings.TrimSpace(string(resp)) != "ok" {
		t.Fatalf("reload response = %q, want ok", resp)
	}
	if !configDirty.Load() {
		t.Fatal("configDirty = false, want reload to mark dirty")
	}
	select {
	case <-pokeCh:
	default:
		t.Fatal("reload did not enqueue poke")
	}
	if !tryStopController(cityPath, &bytes.Buffer{}) {
		t.Fatal("tryStopController returned false, want true via fallback socket")
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not invoke cancel via fallback socket")
	}
}

// writeCityTOML is a test helper that writes a city.toml with the given agents.
func writeCityTOML(t *testing.T, dir string, cityName string, agentNames ...string) string {
	t.Helper()
	tomlPath := filepath.Join(dir, "city.toml")
	var buf bytes.Buffer
	buf.WriteString("[workspace]\nname = " + `"` + cityName + `"` + "\n\n")
	buf.WriteString("[beads]\nprovider = \"file\"\n\n")
	for _, name := range agentNames {
		buf.WriteString("[[agent]]\nname = " + `"` + name + `"` + "\n")
		buf.WriteString("start_command = \"echo hello\"\n\n")
	}
	if err := os.WriteFile(tomlPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return tomlPath
}

func writeControllerNamedSessionCityTOML(t *testing.T, dir, cityName, mode, idleTimeout string) string {
	t.Helper()
	tomlPath := filepath.Join(dir, "city.toml")
	var buf bytes.Buffer
	buf.WriteString("[workspace]\nname = " + `"` + cityName + `"` + "\n\n")
	buf.WriteString("[beads]\nprovider = \"file\"\n\n")
	buf.WriteString("[[agent]]\nname = \"mayor\"\nstart_command = \"echo hello\"\n")
	if idleTimeout != "" {
		buf.WriteString("idle_timeout = " + `"` + idleTimeout + `"` + "\n")
	}
	buf.WriteString("\n[[named_session]]\ntemplate = \"mayor\"\nmode = " + `"` + mode + `"` + "\n")
	if err := os.WriteFile(tomlPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return tomlPath
}

func TestControllerReloadsConfig(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	dir := shortSocketTempDir(t, "gc-reload-")
	tomlPath := writeCityTOML(t, dir, "test", "mayor")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}

	sp := runtime.NewFake()

	// buildFn creates TemplateParams from the config it receives.
	var lastAgentNames atomic.Value
	var reconcileCount atomic.Int32
	buildFn := func(c *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		reconcileCount.Add(1)
		var names []string
		ds := make(map[string]TemplateParams)
		for _, a := range c.Agents {
			if a.Implicit {
				continue
			}
			names = append(names, a.Name)
			ds[a.Name] = TemplateParams{
				SessionName:  a.Name,
				TemplateName: a.Name,
				Command:      "echo hello",
			}
		}
		lastAgentNames.Store(names)
		return DesiredStateResult{State: ds}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer

	loopDone := make(chan struct{})
	go func() {
		controllerLoop(ctx, 20*time.Millisecond, cfg, "test", tomlPath, nil,
			buildFn, sp, nil, nil, nil, nil, nil, events.Discard, nil, nil, nil, nil, &stdout, &stderr)
		close(loopDone)
	}()

	// Ensure cleanup: cancel and wait for the goroutine to exit.
	t.Cleanup(func() {
		cancel()
		select {
		case <-loopDone:
		case <-time.After(5 * time.Second):
		}
	})

	// Wait for initial reconcile.
	for reconcileCount.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}

	// Overwrite city.toml with a new agent.
	writeCityTOML(t, dir, "test", "mayor", "worker")

	// Wait for the reload to appear in stdout. The fsnotify watcher fires on
	// the directory write, debounce (5ms) sets dirty, and the next tick reloads
	// config and writes "Config reloaded" to stdout. Polling stdout directly
	// avoids depending on reconcile count which varies with tick timing.
	deadline := time.After(5 * time.Second)
	for !strings.Contains(stdout.String(), "Config reloaded") {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for config reload; reconciles=%d stdout=%q stderr=%q",
				reconcileCount.Load(), stdout.String(), stderr.String())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()

	deadline = time.After(1500 * time.Millisecond)
	for {
		names, _ := lastAgentNames.Load().([]string)
		if len(names) == 2 && names[0] == "mayor" && names[1] == "worker" {
			break
		}
		select {
		case <-deadline:
			t.Errorf("expected [mayor worker], got %v", names)
			return
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestControllerReloadsConfigImmediatelyOnWatchEvent(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	dir := shortSocketTempDir(t, "gc-reload-poke-")
	tomlPath := writeCityTOML(t, dir, "test", "mayor")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}

	sp := runtime.NewFake()

	var lastAgentNames atomic.Value
	var reconcileCount atomic.Int32
	buildFn := func(c *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		reconcileCount.Add(1)
		var names []string
		ds := make(map[string]TemplateParams)
		for _, a := range c.Agents {
			if a.Implicit {
				continue
			}
			names = append(names, a.Name)
			ds[a.Name] = TemplateParams{
				SessionName:  a.Name,
				TemplateName: a.Name,
				Command:      "echo hello",
			}
		}
		lastAgentNames.Store(names)
		return DesiredStateResult{State: ds}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer

	loopDone := make(chan struct{})
	go func() {
		controllerLoop(ctx, 30*time.Second, cfg, "test", tomlPath, nil,
			buildFn, sp, nil, nil, nil, nil, nil, events.Discard, nil, nil, nil, nil, &stdout, &stderr)
		close(loopDone)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-loopDone:
		case <-time.After(5 * time.Second):
		}
	})

	for reconcileCount.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}

	writeCityTOML(t, dir, "test", "mayor", "worker")

	deadline := time.After(5 * time.Second)
	for !strings.Contains(stdout.String(), "Config reloaded") {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for immediate config reload; reconciles=%d stdout=%q stderr=%q",
				reconcileCount.Load(), stdout.String(), stderr.String())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	names, _ := lastAgentNames.Load().([]string)
	if len(names) != 2 || names[0] != "mayor" || names[1] != "worker" {
		t.Errorf("expected [mayor worker], got %v", names)
	}
}

func TestBuildIdleTracker_SkipsAlwaysNamedSessionIdleTimeout(t *testing.T) {
	dir := t.TempDir()
	tomlPath := writeControllerNamedSessionCityTOML(t, dir, "test", "always", "5s")

	cfg, _, err := config.LoadWithIncludes(osFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}

	sp := runtime.NewFake()
	sp.SetActivity("mayor", time.Now().Add(-10*time.Minute))

	if tracker := buildIdleTracker(cfg, "test", dir, sp); tracker != nil {
		t.Fatalf("buildIdleTracker(cfg) = %#v, want nil for always-named singleton", tracker)
	}
}

// TestControllerReloadsConventionDiscoveredAgentOnWatchEvent exercises
// tryReloadConfig directly — a fast, deterministic unit test for the
// reload logic. It does NOT cover the watcher/debounce wiring between
// fsnotify and tryReloadConfig; see
// TestControllerLoop_WatcherDrivesConventionAgentReload for that.
func TestControllerReloadsConventionDiscoveredAgentOnWatchEvent(t *testing.T) {
	configureTestDoltIdentityEnv(t)

	dir := shortSocketTempDir(t, "gc-reload-agents-")
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatalf("MkdirAll(agents): %v", err)
	}

	cfg, prov, err := config.LoadWithIncludes(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	initialRev := config.Revision(osFS{}, prov, cfg, dir)

	agentDir := filepath.Join(dir, "agents", "noreen")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(agentDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are noreen.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt.template.md): %v", err)
	}

	result, err := tryReloadConfig(tomlPath, "test", dir)
	if err != nil {
		t.Fatalf("tryReloadConfig() error = %v", err)
	}
	if result.Revision == initialRev {
		t.Fatalf("revision did not change after convention-discovered agent was added: %s", result.Revision)
	}

	var names []string
	for _, a := range result.Cfg.Agents {
		if a.Implicit {
			continue
		}
		names = append(names, a.Name)
	}
	if len(names) != 1 || names[0] != "noreen" {
		t.Fatalf("reloaded agent names = %v, want [noreen]", names)
	}
}

// TestWatchConfigDirs_DetectsFileChangeAndSetsDirty covers the
// fsnotify → debounce → dirty flag wiring. This is the integration
// complement to the reload-logic unit test above: if this test breaks
// but the unit test passes, the watcher glue has regressed
// independently. Focuses on the primitive (watchConfigTargets) rather
// than a full controllerLoop to keep the test fast and free of
// bead-store dependencies.
func TestWatchConfigDirs_DetectsFileChangeAndSetsDirty(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	var dirty atomic.Bool
	pokeCh := make(chan struct{}, 1)
	var stderr bytes.Buffer
	cleanup := watchConfigTargets([]config.WatchTarget{{Path: dir, DiscoverConventions: true}}, &dirty, pokeCh, &stderr)
	defer cleanup()

	// Rewrite city.toml — fsnotify watches the dir, so the write fires
	// a WRITE or CREATE event that flips dirty after the debounce.
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test-v2\"\n"), 0o644); err != nil {
		t.Fatalf("rewrite city.toml: %v", err)
	}

	select {
	case <-pokeCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for watcher poke after city.toml rewrite; stderr=%q", stderr.String())
	}
	if !dirty.Load() {
		t.Fatalf("dirty flag not set after file change; stderr=%q", stderr.String())
	}

	// New directory under a watched dir should be picked up and added to
	// the watch list — verifies the subtree auto-add path (critical for
	// convention-discovered agent dirs created after startup).
	dirty.Store(false)
	// Drain any pending poke so we can observe the next one.
	select {
	case <-pokeCh:
	default:
	}

	agentsDir := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(agents): %v", err)
	}
	// First poke is from the mkdir CREATE event on the watched city dir.
	select {
	case <-pokeCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for poke after agents/ mkdir; stderr=%q", stderr.String())
	}
	if !dirty.Load() {
		t.Fatalf("dirty flag not set after agents/ mkdir; stderr=%q", stderr.String())
	}

	// Now prove the subtree-add path actually registered agents/: create
	// a file INSIDE agents/ and verify the watcher fires again. Without
	// the watcher.Add(event.Name) in watchConfigTargets's event loop, this
	// write would silently miss and a real-world regression (conv-agent
	// file showing up after startup) would be invisible.
	dirty.Store(false)
	select {
	case <-pokeCh:
	default:
	}

	agentFile := filepath.Join(agentsDir, "noreen.template.md")
	if err := os.WriteFile(agentFile, []byte("You are noreen.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(agentFile): %v", err)
	}
	select {
	case <-pokeCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for poke after write inside agents/; subtree watch did not register; stderr=%q", stderr.String())
	}
	if !dirty.Load() {
		t.Fatalf("dirty flag not set after write inside agents/; subtree watch did not register; stderr=%q", stderr.String())
	}
}

func TestWatchConfigDirs_FileSeedStillWatchesFile(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	var dirty atomic.Bool
	pokeCh := make(chan struct{}, 1)
	var stderr bytes.Buffer
	cleanup := watchConfigTargets([]config.WatchTarget{{Path: tomlPath}}, &dirty, pokeCh, &stderr)
	defer cleanup()

	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test-v2\"\n"), 0o644); err != nil {
		t.Fatalf("rewrite city.toml: %v", err)
	}

	select {
	case <-pokeCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for watcher poke after direct file seed changed; stderr=%q", stderr.String())
	}
	if !dirty.Load() {
		t.Fatalf("dirty flag not set after direct file seed changed; stderr=%q", stderr.String())
	}
}

func TestWatchConfigDirs_CityRootDoesNotWatchUnrelatedNestedSubdir(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	dir := t.TempDir()
	nestedDir := filepath.Join(dir, "rigs", "checkout")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll nested unrelated dir: %v", err)
	}
	nestedFile := filepath.Join(nestedDir, "generated.txt")
	if err := os.WriteFile(nestedFile, []byte("first\n"), 0o644); err != nil {
		t.Fatalf("seed nested file: %v", err)
	}

	var dirty atomic.Bool
	pokeCh := make(chan struct{}, 1)
	var stderr bytes.Buffer
	cleanup := watchConfigTargets([]config.WatchTarget{{Path: dir, DiscoverConventions: true}}, &dirty, pokeCh, &stderr)
	defer cleanup()

	select {
	case <-pokeCh:
	default:
	}
	dirty.Store(false)

	if err := os.WriteFile(nestedFile, []byte("second\n"), 0o644); err != nil {
		t.Fatalf("rewrite nested unrelated file: %v", err)
	}

	select {
	case <-pokeCh:
		t.Fatalf("unexpected watcher poke after unrelated nested city-root file changed; stderr=%q", stderr.String())
	case <-time.After(250 * time.Millisecond):
	}
	if dirty.Load() {
		t.Fatalf("dirty flag set after unrelated nested city-root file changed; stderr=%q", stderr.String())
	}
}

func TestWatchConfigDirs_SymlinkSeedDirWatchesNestedPreExistingDir(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	dir := t.TempDir()
	targetDir := filepath.Join(dir, "agents-target")
	nestedAgentDir := filepath.Join(targetDir, "sample-agent")
	if err := os.MkdirAll(nestedAgentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll nested symlink target dir: %v", err)
	}
	linkDir := filepath.Join(dir, "agents")
	if err := os.Symlink(targetDir, linkDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	promptPath := filepath.Join(nestedAgentDir, "prompt.template.md")
	if err := os.WriteFile(promptPath, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("seed nested symlink target file: %v", err)
	}

	var dirty atomic.Bool
	pokeCh := make(chan struct{}, 1)
	var stderr bytes.Buffer
	cleanup := watchConfigTargets([]config.WatchTarget{{Path: linkDir, Recursive: true}}, &dirty, pokeCh, &stderr)
	defer cleanup()

	if err := os.WriteFile(promptPath, []byte("edited\n"), 0o644); err != nil {
		t.Fatalf("rewrite symlink target file: %v", err)
	}

	select {
	case <-pokeCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for watcher poke after nested symlink seed dir changed; stderr=%q", stderr.String())
	}
	if !dirty.Load() {
		t.Fatalf("dirty flag not set after nested symlink seed dir changed; stderr=%q", stderr.String())
	}
}

func TestWatchConfigDirs_RecreatedRecursiveSubdirStillWatched(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	agentDir := filepath.Join(agentsDir, "sample-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll agent dir: %v", err)
	}
	promptPath := filepath.Join(agentDir, "prompt.template.md")
	if err := os.WriteFile(promptPath, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}

	var dirty atomic.Bool
	pokeCh := make(chan struct{}, 1)
	var stderr bytes.Buffer
	cleanup := watchConfigTargets([]config.WatchTarget{{Path: agentsDir, Recursive: true}}, &dirty, pokeCh, &stderr)
	defer cleanup()

	if err := os.RemoveAll(agentDir); err != nil {
		t.Fatalf("RemoveAll agent dir: %v", err)
	}
	select {
	case <-pokeCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for watcher poke after recursive subdir removal; stderr=%q", stderr.String())
	}

	dirty.Store(false)
	select {
	case <-pokeCh:
	default:
	}
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("recreate agent dir: %v", err)
	}
	if err := os.WriteFile(promptPath, []byte("recreated\n"), 0o644); err != nil {
		t.Fatalf("seed recreated prompt: %v", err)
	}
	select {
	case <-pokeCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for watcher poke after recursive subdir recreation; stderr=%q", stderr.String())
	}

	dirty.Store(false)
	select {
	case <-pokeCh:
	default:
	}
	if err := os.WriteFile(promptPath, []byte("edited\n"), 0o644); err != nil {
		t.Fatalf("edit recreated prompt: %v", err)
	}
	select {
	case <-pokeCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for watcher poke after edit in recreated recursive subdir; stderr=%q", stderr.String())
	}
	if !dirty.Load() {
		t.Fatalf("dirty flag not set after edit in recreated recursive subdir; stderr=%q", stderr.String())
	}
}

// Regression for gastownhall/gascity#780:
// fsnotify watches are non-recursive — watcher.Add(dir) covers only the
// immediate directory. Pack v2's convention layout pushes agent prompts,
// commands, and formulas into subdirectories that exist at startup. Edits
// to those nested files used to fire no event, silently breaking hot
// reload. This test proves nested edits to pre-existing subtrees now fire.
func TestWatchConfigDirs_Regression780_DetectsEditInPreExistingNestedSubdir(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	dir := t.TempDir()
	// Pre-existing nested layout (mirrors pack v2 convention discovery):
	// agents/<name>/prompt.template.md and agents/<name>/overlay/settings.json.
	agentsDir := filepath.Join(dir, "agents")
	nestedAgentDir := filepath.Join(agentsDir, "sample-agent")
	if err := os.MkdirAll(filepath.Join(nestedAgentDir, "overlay"), 0o755); err != nil {
		t.Fatalf("MkdirAll nested: %v", err)
	}
	promptPath := filepath.Join(nestedAgentDir, "prompt.template.md")
	if err := os.WriteFile(promptPath, []byte("original prompt\n"), 0o644); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	overlayPath := filepath.Join(nestedAgentDir, "overlay", "settings.json")
	if err := os.WriteFile(overlayPath, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	var dirty atomic.Bool
	pokeCh := make(chan struct{}, 1)
	var stderr bytes.Buffer
	cleanup := watchConfigTargets([]config.WatchTarget{
		{Path: dir, DiscoverConventions: true},
		{Path: agentsDir, Recursive: true},
	}, &dirty, pokeCh, &stderr)
	defer cleanup()

	// Drain any startup poke.
	select {
	case <-pokeCh:
	default:
	}
	dirty.Store(false)

	// Edit a two-levels-deep file. Without recursive watching, no event fires.
	if err := os.WriteFile(promptPath, []byte("edited prompt\n"), 0o644); err != nil {
		t.Fatalf("edit prompt: %v", err)
	}
	select {
	case <-pokeCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for poke after edit to %s; pre-existing nested subdir was not watched; stderr=%q", promptPath, stderr.String())
	}
	if !dirty.Load() {
		t.Fatalf("dirty flag not set after edit to nested file %s; stderr=%q", promptPath, stderr.String())
	}

	// And a three-levels-deep edit, for overlay/ subtrees.
	dirty.Store(false)
	select {
	case <-pokeCh:
	default:
	}
	if err := os.WriteFile(overlayPath, []byte(`{"a":2}`), 0o644); err != nil {
		t.Fatalf("edit overlay: %v", err)
	}
	select {
	case <-pokeCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for poke after edit to %s; overlay subtree was not watched; stderr=%q", overlayPath, stderr.String())
	}
	if !dirty.Load() {
		t.Fatalf("dirty flag not set after edit to %s; stderr=%q", overlayPath, stderr.String())
	}
}

func TestControllerReloadsNamedSessionModeAndAppliesIdleTimeout(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	dir := t.TempDir()
	tomlPath := writeControllerNamedSessionCityTOML(t, dir, "test", "always", "")

	cfg, prov, err := config.LoadWithIncludes(osFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo hello"}); err != nil {
		t.Fatalf("fake start mayor: %v", err)
	}
	sp.SetActivity("mayor", time.Now().Add(-10*time.Minute))
	var lastIdleTimeout atomic.Value

	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", dir, err)
	}

	makeTemplateParams := func(c *config.City, a config.Agent) TemplateParams {
		tp := TemplateParams{
			SessionName:  startupSessionName(c.Workspace.Name, a.QualifiedName(), c.Workspace.SessionTemplate),
			TemplateName: a.QualifiedName(),
			Command:      "echo hello",
		}
		if named := config.FindNamedSession(c, a.QualifiedName()); named != nil {
			tp.SessionName = config.NamedSessionRuntimeName(c.Workspace.Name, c.Workspace, a.QualifiedName())
			tp.Alias = a.QualifiedName()
			tp.ConfiguredNamedIdentity = a.QualifiedName()
			tp.ConfiguredNamedMode = named.ModeOrDefault()
		}
		return tp
	}

	seedTP := makeTemplateParams(cfg, cfg.Agents[0])
	seedCfg := templateParamsToConfig(seedTP)
	if _, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:mayor"},
		Metadata: map[string]string{
			"session_name":               seedTP.SessionName,
			"template":                   seedTP.TemplateName,
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "always",
			"config_hash":                runtime.CoreFingerprint(seedCfg),
			"live_hash":                  runtime.LiveFingerprint(seedCfg),
			"generation":                 "1",
			"continuation_epoch":         "1",
			"instance_token":             "seed",
		},
	}); err != nil {
		t.Fatalf("seed canonical mayor bead: %v", err)
	}

	buildFn := func(c *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		if len(c.Agents) > 0 {
			lastIdleTimeout.Store(c.Agents[0].IdleTimeout)
		}
		ds := make(map[string]TemplateParams)
		for _, a := range c.Agents {
			tp := makeTemplateParams(c, a)
			ds[tp.SessionName] = tp
		}
		return DesiredStateResult{State: ds}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer

	done := make(chan struct{})
	go func() {
		controllerLoop(ctx, 20*time.Millisecond, cfg, "test", tomlPath, config.WatchTargets(prov, cfg, dir),
			buildFn, sp, nil, nil, nil, nil, nil, events.Discard, nil, nil, nil, nil, &stdout, &stderr)
		close(done)
	}()
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("controller did not exit during cleanup; stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				_ = os.RemoveAll(dir)
				if _, err := os.Stat(dir); os.IsNotExist(err) {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			entries, _ := os.ReadDir(filepath.Join(dir, ".gc"))
			t.Fatalf("controller temp dir persisted after shutdown; .gc entries=%v stdout=%q stderr=%q", entries, stdout.String(), stderr.String())
		})
	}
	t.Cleanup(shutdown)

	waitForNamedMode := func(want string, timeout time.Duration) beads.Bead {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			loopStore, openErr := openCityStoreAt(dir)
			if openErr != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			all, listErr := loopStore.ListByLabel(sessionBeadLabel, 0, beads.IncludeClosed)
			if listErr == nil {
				for _, b := range all {
					if b.Status == "closed" {
						continue
					}
					if b.Metadata["session_name"] == "mayor" && b.Metadata[namedSessionModeMetadata] == want {
						return b
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		all, _ := store.ListByLabel(sessionBeadLabel, 0, beads.IncludeClosed)
		t.Fatalf("timed out waiting for configured_named_mode=%q; beads=%v stdout=%q stderr=%q", want, all, stdout.String(), stderr.String())
		return beads.Bead{}
	}

	waitForNamedMode("always", 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String(), "City started.") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "City started.") {
		t.Fatalf("controller never reached started state; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	writeControllerNamedSessionCityTOML(t, dir, "test", "on_demand", "5s")
	parsedCfg, _, err := config.LoadWithIncludes(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("reload parse: %v", err)
	}
	if got := parsedCfg.Agents[0].IdleTimeout; got != "5s" {
		t.Fatalf("parsed idle_timeout = %q, want %q", got, "5s")
	}
	if got := parsedCfg.Agents[0].IdleTimeoutDuration(); got != 5*time.Second {
		t.Fatalf("parsed idle_timeout duration = %v, want %v", got, 5*time.Second)
	}
	tracker, ok := buildIdleTracker(parsedCfg, "test", dir, sp).(*memoryIdleTracker)
	if !ok || tracker == nil {
		t.Fatal("buildIdleTracker(parsedCfg) = nil, want tracker")
	}
	if !tracker.checkIdle("mayor", sp, time.Now()) {
		t.Fatalf("fresh idle tracker did not consider mayor idle; activity=%v timeouts=%v", sp.Activity["mayor"], tracker.timeouts)
	}

	bead := waitForNamedMode("on_demand", 5*time.Second)
	if got := bead.Metadata["session_name"]; got != "mayor" {
		t.Fatalf("session_name after reload = %q, want mayor", got)
	}
	if got, _ := lastIdleTimeout.Load().(string); got != "5s" {
		t.Fatalf("controller buildFn idle_timeout = %q, want %q", got, "5s")
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !sp.IsRunning("mayor") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sp.IsRunning("mayor") {
		t.Fatalf("mayor still running after idle_timeout reload; stdout=%q stderr=%q calls=%v", stdout.String(), stderr.String(), sp.Calls)
	}
	if !strings.Contains(stdout.String(), "Config reloaded") {
		t.Fatalf("stdout missing config reload marker: %q", stdout.String())
	}
	shutdown()
}

func TestHandleControllerConnControlDispatcher(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	convergenceReqCh := make(chan convergenceRequest, 1)
	pokeCh := make(chan struct{}, 1)
	controlDispatcherCh := make(chan struct{}, 1)
	cityPath := t.TempDir()

	done := make(chan struct{})
	go func() {
		handleControllerConn(server, cityPath, func() {}, nil, nil, convergenceReqCh, pokeCh, controlDispatcherCh)
		close(done)
	}()

	if _, err := client.Write([]byte("control-dispatcher\n")); err != nil {
		t.Fatalf("write command: %v", err)
	}
	buf := make([]byte, 16)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if got := string(buf[:n]); got != "ok\n" {
		t.Fatalf("ack = %q, want %q", got, "ok\n")
	}

	select {
	case <-controlDispatcherCh:
	default:
		t.Fatal("control-dispatcher channel was not signaled")
	}

	select {
	case <-pokeCh:
		t.Fatal("generic poke channel should remain untouched")
	default:
	}

	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleControllerConn did not exit")
	}
}

func TestControllerReloadInvalidConfig(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	dir := shortSocketTempDir(t, "gc-invalid-")
	tomlPath := writeCityTOML(t, dir, "test", "mayor")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}

	sp := runtime.NewFake()
	var reconcileCount atomic.Int32
	buildFn := func(c *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		reconcileCount.Add(1)
		ds := make(map[string]TemplateParams)
		for _, a := range c.Agents {
			ds[a.Name] = TemplateParams{
				SessionName:  a.Name,
				TemplateName: a.Name,
				Command:      "echo hello",
			}
		}
		return DesiredStateResult{State: ds}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer

	go controllerLoop(ctx, 20*time.Millisecond, cfg, "test", tomlPath, nil,
		buildFn, sp, nil, nil, nil, nil, nil, events.Discard, nil, nil, nil, nil, &stdout, &stderr)

	// Wait for initial reconcile.
	for reconcileCount.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}

	// Write invalid TOML.
	if err := os.WriteFile(tomlPath, []byte("[[[ bad toml"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wait for a tick to process the bad config.
	target := reconcileCount.Load() + 2
	deadline := time.After(3 * time.Second)
	for reconcileCount.Load() < target {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for tick after invalid config")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	time.Sleep(50 * time.Millisecond) // let controllerLoop goroutine exit before TempDir cleanup

	if !strings.Contains(stderr.String(), "config reload") {
		t.Errorf("expected config reload error in stderr, got: %s", stderr.String())
	}
	if strings.Contains(stdout.String(), "Config reloaded.") {
		t.Error("should not have reloaded invalid config")
	}
}

func TestControllerReloadCityNameChange(t *testing.T) {
	old := debounceDelay
	debounceDelay = 5 * time.Millisecond
	t.Cleanup(func() { debounceDelay = old })

	dir := shortSocketTempDir(t, "gc-rename-")
	tomlPath := writeCityTOML(t, dir, "test", "mayor")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}

	sp := runtime.NewFake()
	var reconcileCount atomic.Int32
	buildFn := func(c *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		reconcileCount.Add(1)
		ds := make(map[string]TemplateParams)
		for _, a := range c.Agents {
			ds[a.Name] = TemplateParams{
				SessionName:  a.Name,
				TemplateName: a.Name,
				Command:      "echo hello",
			}
		}
		return DesiredStateResult{State: ds}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer

	go controllerLoop(ctx, 20*time.Millisecond, cfg, "test", tomlPath, nil,
		buildFn, sp, nil, nil, nil, nil, nil, events.Discard, nil, nil, nil, nil, &stdout, &stderr)

	// Wait for initial reconcile.
	for reconcileCount.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}

	// Change the city name.
	writeCityTOML(t, dir, "different-city", "mayor")

	// Wait for tick.
	target := reconcileCount.Load() + 2
	deadline := time.After(3 * time.Second)
	for reconcileCount.Load() < target {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for tick after name change")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	time.Sleep(50 * time.Millisecond) // let controllerLoop goroutine exit before TempDir cleanup

	if !strings.Contains(stderr.String(), "workspace.name changed") {
		t.Errorf("expected workspace.name change rejection in stderr, got: %s", stderr.String())
	}
	if strings.Contains(stdout.String(), "Config reloaded.") {
		t.Error("should not have reloaded config with changed city name")
	}
}

func TestConfigReloadSummary(t *testing.T) {
	tests := []struct {
		name                           string
		oldAgents, oldRigs, newA, newR int
		wantAgents, wantRigs           string
	}{
		{"no change", 3, 2, 3, 2, "3 agents", "2 rigs"},
		{"agents added", 2, 1, 5, 1, "5 agents (+3)", "1 rigs"},
		{"agents removed", 5, 1, 3, 1, "3 agents (-2)", "1 rigs"},
		{"rigs added", 1, 0, 1, 2, "1 agents", "2 rigs (+2)"},
		{"rigs removed", 1, 3, 1, 1, "1 agents", "1 rigs (-2)"},
		{"both changed", 2, 3, 4, 1, "4 agents (+2)", "1 rigs (-2)"},
		{"zero to zero", 0, 0, 0, 0, "0 agents", "0 rigs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := configReloadSummary(tt.oldAgents, tt.oldRigs, tt.newA, tt.newR)
			if !strings.Contains(got, tt.wantAgents) {
				t.Errorf("agents: got %q, want substring %q", got, tt.wantAgents)
			}
			if !strings.Contains(got, tt.wantRigs) {
				t.Errorf("rigs: got %q, want substring %q", got, tt.wantRigs)
			}
		})
	}
}

func TestControllerReloadCommandReloadsConfigImmediately(t *testing.T) {
	old := debounceDelay
	debounceDelay = 10 * time.Second
	t.Cleanup(func() { debounceDelay = old })

	dir := shortSocketTempDir(t, "gc-reload-cmd-")
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tomlPath := writeCityTOML(t, dir, "test", "mayor")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()

	var lastAgentNames atomic.Value
	var reconcileCount atomic.Int32
	buildFn := func(c *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		reconcileCount.Add(1)
		var names []string
		ds := make(map[string]TemplateParams)
		for _, a := range c.Agents {
			if a.Implicit {
				continue
			}
			names = append(names, a.Name)
			ds[a.Name] = TemplateParams{SessionName: a.Name, TemplateName: a.Name, Command: "echo hello"}
		}
		lastAgentNames.Store(names)
		return DesiredStateResult{State: ds}
	}

	var stdout, stderr bytes.Buffer
	done := make(chan struct{})
	go func() {
		runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &stdout, &stderr)
		close(done)
	}()
	t.Cleanup(func() {
		tryStopController(dir, &bytes.Buffer{})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	waitForController(t, dir)
	deadline := time.After(5 * time.Second)
	for reconcileCount.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for initial reconcile")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	writeCityTOML(t, dir, "test", "mayor", "worker")

	before := reconcileCount.Load()
	resp, err := sendControllerCommand(dir, "reload")
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if string(resp) != "ok" {
		t.Fatalf("reload response = %q, want %q", string(resp), "ok")
	}

	deadline = time.After(1500 * time.Millisecond)
	for reconcileCount.Load() <= before || !strings.Contains(stdout.String(), "Config reloaded") {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for reload command to apply config; reconciles=%d stdout=%q stderr=%q", reconcileCount.Load(), stdout.String(), stderr.String())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	names, _ := lastAgentNames.Load().([]string)
	if len(names) != 2 || names[0] != "mayor" || names[1] != "worker" {
		t.Fatalf("expected [mayor worker], got %v", names)
	}
}

func TestControllerPokeTriggersImmediate(t *testing.T) {
	sp := runtime.NewFake()

	var reconcileCount atomic.Int32
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		reconcileCount.Add(1)
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	dir := shortSocketTempDir(t, "gc-poke-")
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Beads:     config.BeadsConfig{Provider: "file"},
	}

	// Write a city.toml so the controller uses the temp dir for bead store
	// operations rather than falling back to cwd.
	tomlPath := writeCityTOML(t, dir, "test")

	var stdout, stderr bytes.Buffer

	done := make(chan struct{})
	go func() {
		runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &stdout, &stderr)
		close(done)
	}()

	// Ensure cleanup: if the test fails, send stop so the goroutine exits.
	t.Cleanup(func() {
		tryStopController(dir, &bytes.Buffer{})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	// Poll for controller socket to become available.
	waitForController(t, dir)

	// Wait for initial tick.
	deadline := time.After(5 * time.Second)
	for reconcileCount.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for initial reconcile")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Record count, then poke.
	before := reconcileCount.Load()
	resp, err := sendControllerCommand(dir, "poke")
	if err != nil {
		t.Fatalf("poke failed: %v", err)
	}
	if string(resp) != "ok" {
		t.Errorf("poke response = %q, want %q", string(resp), "ok")
	}

	// Wait for an additional reconcile triggered by poke.
	deadline = time.After(3 * time.Second)
	for reconcileCount.Load() <= before {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for poke-triggered reconcile")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Stop controller.
	tryStopController(dir, &bytes.Buffer{})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not exit")
	}
}

// waitForController polls until the controller socket at dir is responsive,
// or fails the test after the given timeout. This replaces fixed sleeps that
// are unreliable under load.
func waitForController(t *testing.T, dir string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if controllerAlive(dir) != 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for controller socket to become available")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// osFS is a minimal fsys.FS for test helpers that delegates to the os package.
type osFS struct{}

func (osFS) ReadFile(name string) ([]byte, error)                 { return os.ReadFile(name) }
func (osFS) WriteFile(name string, d []byte, p os.FileMode) error { return os.WriteFile(name, d, p) }
func (osFS) MkdirAll(path string, perm os.FileMode) error         { return os.MkdirAll(path, perm) }
func (osFS) Stat(name string) (os.FileInfo, error)                { return os.Stat(name) }
func (osFS) Lstat(name string) (os.FileInfo, error)               { return os.Lstat(name) }
func (osFS) ReadDir(name string) ([]os.DirEntry, error)           { return os.ReadDir(name) }
func (osFS) Rename(oldpath, newpath string) error                 { return os.Rename(oldpath, newpath) }
func (osFS) Remove(name string) error                             { return os.Remove(name) }
func (osFS) Chmod(name string, mode os.FileMode) error            { return os.Chmod(name, mode) }
