package main

import (
	"bytes"
	"context"
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
	t.Setenv("GC_HOME", t.TempDir())

	dir := t.TempDir()
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

	waitForController(t, dir, 5*time.Second)
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
