package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	var controllerStdout, controllerStderr bytes.Buffer
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

	waitForControllerAvailable(t, dir, 15*time.Second)
	if err := sp.Start(context.Background(), seededSession, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{dir}, &stdout, &stderr)
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
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
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

	var stderr bytes.Buffer
	stopCityManagedBeadsProviderIfRunning(cityDir, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
	ops := readOpLog(t, logFile)
	if len(ops) != 1 || ops[0] != "stop" {
		t.Fatalf("provider ops = %v, want [stop]", ops)
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

	var stdout, stderr bytes.Buffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr)
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

	var controllerStdout, controllerStderr bytes.Buffer
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

	waitForControllerAvailable(t, dir, 15*time.Second)

	const sess = "margin-session"
	if err := sp.Start(context.Background(), sess, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	go func() {
		sp.waitForInterrupts(t, 1)
		sp.releaseInterrupt(sess)
	}()

	var stdout, stderr bytes.Buffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{dir}, &stdout, &stderr)
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

func waitForControllerAvailable(t *testing.T, dir string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
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
