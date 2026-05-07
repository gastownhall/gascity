package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

type recordingStopProvider struct {
	*runtime.Fake
	stops      chan string
	interrupts chan string
}

func newRecordingStopProvider() *recordingStopProvider {
	return &recordingStopProvider{
		Fake:       runtime.NewFake(),
		stops:      make(chan string, 8),
		interrupts: make(chan string, 8),
	}
}

func (p *recordingStopProvider) Stop(name string) error {
	p.stops <- name
	return p.Fake.Stop(name)
}

func (p *recordingStopProvider) Interrupt(name string) error {
	p.interrupts <- name
	return p.Fake.Interrupt(name)
}

func TestCmdStopWaitsForStandaloneControllerExit(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-stop-")
	for legacyLen := len(filepath.Join(dir, ".gc", "controller.sock")); legacyLen <= 120; legacyLen = len(filepath.Join(dir, ".gc", "controller.sock")) {
		dir = filepath.Join(dir, "very-long-controller-path-segment")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := controllerSocketPath(dir); got == filepath.Join(dir, ".gc", "controller.sock") {
		t.Fatalf("controllerSocketPath(%q) = legacy path %q, want short fallback", dir, got)
	}
	if got, want := controllerSocketPath(dir), controllerSocketPath(canonicalTestPath(dir)); got != want {
		t.Fatalf("controllerSocketPath fallback mismatch across equivalent paths: %q vs %q", got, want)
	}

	sp := newGatedStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}
	const seededSession = "seeded-session"

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		running, _ := sp.ListRunning("")
		for _, name := range running {
			sp.release(name)
		}
		tryStopController(dir, &bytes.Buffer{})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	waitForControllerAvailable(t, dir)
	if err := sp.Start(context.Background(), seededSession, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{dir}, &stdout, &stderr, 0, false)
	}()

	stopped := sp.waitForStops(t, 1)
	if len(stopped) != 1 || stopped[0] != seededSession {
		t.Fatalf("stop targets = %v, want [%s]", stopped, seededSession)
	}

	select {
	case code := <-stopDone:
		t.Fatalf("cmdStop returned early with code %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	case <-time.After(200 * time.Millisecond):
	}

	sp.release(stopped[0])

	select {
	case code := <-stopDone:
		if code != 0 {
			t.Fatalf("cmdStop = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cmdStop did not finish after releasing controller shutdown")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not exit after cmdStop")
	}

	if pid := controllerAlive(dir); pid != 0 {
		t.Fatalf("controllerAlive after cmdStop = %d, want 0", pid)
	}
	if !strings.Contains(stdout.String(), "Controller stopping...") {
		t.Fatalf("stdout missing controller stop message: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestCmdStopWallClockTimeoutBoundsDirectStop(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	cityDir := shortSocketTempDir(t, "gc-stop-timeout-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "timeout-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "sleep 1"},
		},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newHangingProvider()
	t.Cleanup(sp.release)
	sessionName := lookupSessionNameOrLegacy(nil, loadedCityName(cfg, cityDir), cfg.Agents[0].QualifiedName(), cfg.Workspace.SessionTemplate)
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) runtime.Provider {
		return sp
	}

	var stdout, stderr lockedBuffer
	started := time.Now()
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 100*time.Millisecond, false)
	if code != 1 {
		t.Fatalf("cmdStop() = %d, want timeout code 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cmdStop returned after %s, want wall-clock cap near 100ms", elapsed)
	}
	if !strings.Contains(stderr.String(), "timed out after 100ms") {
		t.Fatalf("stderr = %q, want wall-clock timeout message", stderr.String())
	}
}

func TestCmdStopForceDelegatesImmediateControllerStop(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-force-stop-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "force-stop-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "250ms"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newRecordingStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		tryStopController(dir, &bytes.Buffer{})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	waitForControllerAvailable(t, dir)
	const sess = "force-stop-session"
	if err := sp.Start(context.Background(), sess, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{dir}, &stdout, &stderr, 2*time.Second, true)
	}()

	select {
	case interrupted := <-sp.interrupts:
		t.Fatalf("gc stop --force delegated interrupt for %q; want immediate stop", interrupted)
	case stopped := <-sp.stops:
		if stopped != sess {
			t.Fatalf("stopped = %q, want %q", stopped, sess)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delegated force stop")
	}

	select {
	case code := <-stopDone:
		if code != 0 {
			t.Fatalf("cmdStop = %d, want 0; stdout=%q stderr=%q controller stderr=%q", code, stdout.String(), stderr.String(), controllerStderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cmdStop did not finish after delegated force stop")
	}
}

func TestCmdStopForceEscalatesInProgressControllerStop(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-force-escalate-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "force-escalate-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "5s"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newGatedStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		tryStopControllerWithForce(dir, io.Discard, true)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	waitForControllerAvailable(t, dir)
	const sess = "force-escalate-session"
	if err := sp.Start(context.Background(), sess, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	var normalStdout, normalStderr lockedBuffer
	normalDone := make(chan int, 1)
	go func() {
		normalDone <- cmdStop([]string{dir}, &normalStdout, &normalStderr, 10*time.Second, false)
	}()

	interrupted := sp.waitForInterrupts(t, 1)
	if interrupted[0] != sess {
		t.Fatalf("interrupted = %q, want %q", interrupted[0], sess)
	}

	var forceStdout, forceStderr lockedBuffer
	forceDone := make(chan int, 1)
	go func() {
		forceDone <- cmdStop([]string{dir}, &forceStdout, &forceStderr, 10*time.Second, true)
	}()

	stopped := sp.waitForStops(t, 1)
	if stopped[0] != sess {
		t.Fatalf("stopped = %q, want %q", stopped[0], sess)
	}
	sp.release(stopped[0])
	sp.releaseInterrupt(interrupted[0])

	for _, result := range []struct {
		name string
		ch   <-chan int
		out  *lockedBuffer
		err  *lockedBuffer
	}{
		{name: "normal stop", ch: normalDone, out: &normalStdout, err: &normalStderr},
		{name: "force stop", ch: forceDone, out: &forceStdout, err: &forceStderr},
	} {
		select {
		case code := <-result.ch:
			if code != 0 {
				t.Fatalf("%s code = %d, want 0; stdout=%q stderr=%q controller stderr=%q",
					result.name, code, result.out.String(), result.err.String(), controllerStderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not finish after force escalation", result.name)
		}
	}
}

func TestDefaultStopWallClockTimeoutScalesWithConfiguredStopTargets(t *testing.T) {
	origStop := stopPerTargetTimeoutDefault
	origMargin := interruptPerTargetTimeoutMargin
	stopPerTargetTimeoutDefault = 10 * time.Second
	interruptPerTargetTimeoutMargin = time.Second
	t.Cleanup(func() {
		stopPerTargetTimeoutDefault = origStop
		interruptPerTargetTimeoutMargin = origMargin
	})

	cfg := &config.City{
		Daemon: config.DaemonConfig{ShutdownTimeout: "2s"},
	}
	for i := 0; i < 7; i++ {
		cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("worker-%d", i+1)})
	}

	got := defaultStopWallClockTimeout(cfg)
	// One stop pass budgets a 3s interrupt-dispatch cap, 2s graceful-exit
	// wait, and three 10s stop waves. The default cap allows two passes plus
	// one extra orphan-cleanup stop wave: 2*(3s+2s+30s)+10s.
	want := 80 * time.Second
	if got != want {
		t.Fatalf("defaultStopWallClockTimeout() = %s, want %s", got, want)
	}
}

func TestStopCityManagedBeadsProviderIfRunningStopsDefaultBD(t *testing.T) {
	skipSlowCmdGCTest(t, "exercises managed bd provider shutdown; run make test-cmd-gc-process for full coverage")
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := gcBeadsBdScriptPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> \""+logFile+"\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck

	state := doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	stateData, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json"), stateData, 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr lockedBuffer
	stopCityManagedBeadsProviderIfRunning(cityDir, &stderr)
	if stderr.String() != "" {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
	ops := readOpLog(t, logFile)
	if len(ops) != 1 || ops[0] != "stop" {
		t.Fatalf("provider ops = %v, want [stop]", ops)
	}
}

func TestMarkCityStopSessionSleepReasonSkipsCreatingSessions(t *testing.T) {
	store := beads.NewMemStore()
	active, err := store.Create(beads.Bead{
		Title:  "active",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	creating, err := store.Create(beads.Bead{
		Title:  "creating",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"state":                "creating",
			"session_name":         "creating",
			"pending_create_claim": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	markCityStopSessionSleepReason(store, ioDiscard{})

	activeUpdated, err := store.Get(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := activeUpdated.Metadata["sleep_reason"]; got != sleepReasonCityStop {
		t.Fatalf("active sleep_reason = %q, want %q", got, sleepReasonCityStop)
	}
	creatingUpdated, err := store.Get(creating.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := creatingUpdated.Metadata["sleep_reason"]; got != "" {
		t.Fatalf("creating sleep_reason = %q, want empty because create rollback owns this state", got)
	}
}

func TestCmdStopUsesTargetCitySessionProviderOutsideCityDir(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	cityDir := shortSocketTempDir(t, "gc-stop-city-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "bright-lights"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Session:   config.SessionConfig{Provider: "subprocess"},
		Agents: []config.Agent{
			{Name: "mayor", StartCommand: "sleep 1"},
		},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	otherDir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(otherDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })

	var gotPath, gotName, gotProvider string
	sessionProviderForStopCity = func(cfg *config.City, cityPath string) runtime.Provider {
		gotPath = cityPath
		if cfg != nil {
			gotName = cfg.Workspace.Name
			gotProvider = cfg.Session.Provider
		}
		return runtime.NewFake()
	}

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertSameTestPath(t, gotPath, cityDir)
	if gotName != "bright-lights" {
		t.Fatalf("session provider cityName = %q, want %q", gotName, "bright-lights")
	}
	if gotProvider != "subprocess" {
		t.Fatalf("session provider provider = %q, want %q", gotProvider, "subprocess")
	}
}

// TestCmdStopMarginExhaustion verifies that cmdStop tolerates slow controller
// shutdowns without timing out. With a non-zero ShutdownTimeout and a provider
// whose Stop blocks briefly (simulating CI scheduling delays or an in-flight
// tick), the increased wait margin must absorb the overhead.
//
// Regression test for gastownhall/gascity#572.
func TestCmdStopMarginExhaustion(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-margin-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-margin"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "1s"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newGatedStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, filepath.Join(dir, "city.toml"), cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		running, _ := sp.ListRunning("")
		for _, name := range running {
			sp.release(name)
		}
		tryStopController(dir, &bytes.Buffer{})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	waitForControllerAvailable(t, dir)

	const sess = "margin-session"
	if err := sp.Start(context.Background(), sess, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	go func() {
		sp.waitForInterrupts(t, 1)
		sp.releaseInterrupt(sess)
	}()

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{dir}, &stdout, &stderr, 0, false)
	}()

	stopped := sp.waitForStops(t, 1)
	if len(stopped) != 1 || stopped[0] != sess {
		t.Fatalf("stop targets = %v, want [%s]", stopped, sess)
	}

	time.AfterFunc(500*time.Millisecond, func() {
		sp.release(sess)
	})

	select {
	case code := <-stopDone:
		if code != 0 {
			t.Fatalf("cmdStop = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cmdStop did not finish within margin budget")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not exit after cmdStop")
	}

	if !strings.Contains(stdout.String(), "Controller stopping...") {
		t.Fatalf("stdout missing controller stop message: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
}

func waitForControllerAvailable(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if controllerAcceptsPing(dir, 100*time.Millisecond) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for controller socket to become available")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func controllerAcceptsPing(dir string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("unix", controllerSocketPath(dir), timeout)
	if err != nil {
		return false
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return false
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		return false
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	return err == nil && strings.TrimSpace(string(buf[:n])) != ""
}
