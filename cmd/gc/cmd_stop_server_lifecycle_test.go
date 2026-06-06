package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// lifecycleOrderProvider wraps runtime.Fake and additionally implements
// runtime.ServerLifecycleProvider. It records the order of ListRunning and
// TeardownServer calls so ordering tests can verify the stop-path sequence.
type lifecycleOrderProvider struct {
	*runtime.Fake
	mu          sync.Mutex
	events      []string
	teardownErr error
}

func (p *lifecycleOrderProvider) ListRunning(prefix string) ([]string, error) {
	p.mu.Lock()
	p.events = append(p.events, "ListRunning")
	p.mu.Unlock()
	return p.Fake.ListRunning(prefix)
}

func (p *lifecycleOrderProvider) ConfigureServer() error {
	return nil
}

func (p *lifecycleOrderProvider) TeardownServer() error {
	p.mu.Lock()
	p.events = append(p.events, "TeardownServer")
	p.mu.Unlock()
	return p.teardownErr
}

// Compile-time assertions: lifecycleOrderProvider satisfies both interfaces.
var (
	_ runtime.Provider                = (*lifecycleOrderProvider)(nil)
	_ runtime.ServerLifecycleProvider = (*lifecycleOrderProvider)(nil)
)

// TestCmdStopBodyTeardownRunsAfterStopOrphansBeforeBeadsShutdown is a
// coordination test that verifies the stop-path ordering contract in
// cmdStopBody: TeardownServer is called after stopOrphans has run its
// ListRunning sweep AND before the bead-provider shutdown.
//
// Expected sequence: doStop -> stopOrphans (ListRunning) -> TeardownServer -> shutdownBeadsProvider
func TestCmdStopBodyTeardownRunsAfterStopOrphansBeforeBeadsShutdown(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "lifecycle-order-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)

	sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}

	var orderMu sync.Mutex
	var shutdownCalled bool
	var eventsAtShutdown []string
	overrideShutdownBeadsProviderForStop(t, func(string) error {
		sp.mu.Lock()
		snapshot := make([]string, len(sp.events))
		copy(snapshot, sp.events)
		sp.mu.Unlock()

		orderMu.Lock()
		shutdownCalled = true
		eventsAtShutdown = snapshot
		orderMu.Unlock()
		return nil
	})

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) runtime.Provider { return sp }

	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdStopBody() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	sp.mu.Lock()
	allProviderEvents := make([]string, len(sp.events))
	copy(allProviderEvents, sp.events)
	sp.mu.Unlock()

	orderMu.Lock()
	called := shutdownCalled
	snapshot := eventsAtShutdown
	orderMu.Unlock()

	// TeardownServer must be present.
	teardownIdx := -1
	for i, e := range allProviderEvents {
		if e == "TeardownServer" {
			teardownIdx = i
			break
		}
	}
	if teardownIdx < 0 {
		t.Fatalf("TeardownServer was never called; provider events = %v", allProviderEvents)
	}

	// TeardownServer must precede the bead-provider shutdown.
	if called {
		found := false
		for _, e := range snapshot {
			if e == "TeardownServer" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("TeardownServer must occur before bead-provider shutdown; events at shutdown = %v, all provider events = %v",
				snapshot, allProviderEvents)
		}
	}

	// TeardownServer must follow at least one ListRunning (the orphan sweep).
	listRunningBeforeTeardown := false
	for _, e := range allProviderEvents[:teardownIdx] {
		if e == "ListRunning" {
			listRunningBeforeTeardown = true
			break
		}
	}
	if !listRunningBeforeTeardown {
		t.Fatalf("TeardownServer called before any ListRunning (orphan sweep must precede teardown); events = %v", allProviderEvents)
	}
}

// TestCmdStopBodySkipsTeardownForNonLifecycleProvider verifies that cmdStopBody
// completes successfully when the session provider does not implement
// runtime.ServerLifecycleProvider. The type assertion must yield ok=false and
// skip teardown silently: no panic, no error in stderr.
//
// This is the normal case for non-tmux providers (subprocess, exec, K8s, Fake).
func TestCmdStopBodySkipsTeardownForNonLifecycleProvider(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "no-lifecycle-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)

	// Verify that runtime.Fake does NOT implement ServerLifecycleProvider.
	// This guards against a future change that accidentally adds the interface
	// to Fake (which would defeat the skip path and change non-tmux behavior).
	var baseProvider runtime.Provider = runtime.NewFake()
	if _, ok := baseProvider.(runtime.ServerLifecycleProvider); ok {
		t.Fatal("runtime.Fake must not implement runtime.ServerLifecycleProvider; " +
			"non-lifecycle providers must skip teardown via ok=false type assertion")
	}

	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) runtime.Provider {
		return runtime.NewFake()
	}

	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdStopBody() = %d, want 0 for non-lifecycle provider; stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout = %q, want City stopped.", stdout.String())
	}
	if strings.Contains(stderr.String(), "teardown server") {
		t.Fatalf("stderr = %q, unexpected teardown error for non-lifecycle provider", stderr.String())
	}
}

// TestCmdStopBodyReportsTeardownErrorWithoutFailing verifies that tmux server
// teardown is best-effort: a provider error is visible to the operator but does
// not change the stop exit code.
func TestCmdStopBodyReportsTeardownErrorWithoutFailing(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "lifecycle-error-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)

	sp := &lifecycleOrderProvider{
		Fake:        runtime.NewFake(),
		teardownErr: errors.New("provider-stop-failed"),
	}

	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) runtime.Provider { return sp }

	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdStopBody() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout = %q, want City stopped.", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc stop: teardown server: provider-stop-failed") {
		t.Fatalf("stderr = %q, want teardown server warning", stderr.String())
	}
}

func writeStopLifecycleCityConfig(t *testing.T, cityDir string, cfg *config.City) {
	t.Helper()
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
