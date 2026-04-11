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

	var stdout bytes.Buffer
	var stderr syncBuffer

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
	waitForController(t, dir, 5*time.Second, done, &stderr)

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

	deadline = time.After(5 * time.Second)
	for {
		names, _ := lastAgentNames.Load().([]string)
		if len(names) == 2 && names[0] == "mayor" && names[1] == "worker" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for reconciled agents [mayor worker]; got %v stdout=%q stderr=%q",
				names, stdout.String(), stderr.String())
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

	deadline := time.After(1500 * time.Millisecond)
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
	var reconcileCount atomic.Int32

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
		reconcileCount.Add(1)
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
		controllerLoop(ctx, 20*time.Millisecond, cfg, "test", tomlPath, config.WatchDirs(prov, cfg, dir),
			buildFn, sp, nil, nil, nil, nil, nil, events.Discard, nil, nil, nil, nil, &stdout, &stderr)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

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
		if reconcileCount.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := reconcileCount.Load(); got < 2 {
		t.Fatalf("controller did not reach steady-state loop; reconcileCount=%d stdout=%q stderr=%q", got, stdout.String(), stderr.String())
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
		handleControllerConn(server, cityPath, func() {}, convergenceReqCh, pokeCh, controlDispatcherCh)
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

	var stdout bytes.Buffer
	var stderr syncBuffer

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
	waitForController(t, dir, 5*time.Second, done, &stderr)

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

// syncBuffer is a concurrency-safe bytes.Buffer for use as an io.Writer
// (e.g. capturing stderr from a goroutine) that can be read safely from
// another goroutine. It implements io.Writer plus String/Len accessors.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sb *syncBuffer) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *syncBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.String()
}

func (sb *syncBuffer) Len() int {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Len()
}

// waitForController polls until the controller socket at dir is responsive,
// or fails the test after the given timeout. If done is non-nil it is checked
// on each poll iteration; a closed channel means runController exited early
// and the real error is in stderr rather than a socket timeout.
func waitForController(t *testing.T, dir string, timeout time.Duration, done <-chan struct{}, stderr *syncBuffer) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if controllerAlive(dir) != 0 {
			return
		}
		// Detect early exit: runController returned before the socket
		// became responsive. Report stderr so the real error surfaces
		// instead of a misleading "timed out" message.
		if done != nil {
			select {
			case <-done:
				var diag string
				if stderr != nil {
					diag = stderr.String()
				}
				t.Fatalf("controller exited before socket became ready; stderr: %s", diag)
			default:
			}
		}
		select {
		case <-deadline:
			msg := "timed out waiting for controller socket to become available"
			if stderr != nil {
				if s := stderr.String(); s != "" {
					msg += "; stderr: " + s
				}
			}
			t.Fatal(msg)
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
func (osFS) ReadDir(name string) ([]os.DirEntry, error)           { return os.ReadDir(name) }
func (osFS) Rename(oldpath, newpath string) error                 { return os.Rename(oldpath, newpath) }
func (osFS) Remove(name string) error                             { return os.Remove(name) }
func (osFS) Chmod(name string, mode os.FileMode) error            { return os.Chmod(name, mode) }
