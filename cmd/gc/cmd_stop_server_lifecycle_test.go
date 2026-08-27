package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// lifecycleOrderProvider wraps runtime.Fake and additionally implements
// runtime.ServerLifecycleProvider. It records a teardown attempt so the
// managed-stop test can reject server destruction.
type lifecycleOrderProvider struct {
	*runtime.Fake
	mu     sync.Mutex
	events []string
	// listErr, when set, makes every ListRunning return it (empty slice),
	// modeling a persistently failed enumeration.
	listErr error
	// listErrors is the per-call error sequence, for tests that need a
	// specific call in the sequence to fail.
	listCalls  int
	listErrors []error
}

func (p *lifecycleOrderProvider) ListRunning(prefix string) ([]string, error) {
	p.mu.Lock()
	p.events = append(p.events, "ListRunning")
	always := p.listErr
	call := p.listCalls
	p.listCalls++
	var seq error
	if call < len(p.listErrors) {
		seq = p.listErrors[call]
	}
	p.mu.Unlock()
	if always != nil {
		return nil, always
	}
	names, err := p.Fake.ListRunning(prefix)
	if err != nil {
		return names, err
	}
	return names, seq
}

func (p *lifecycleOrderProvider) ConfigureServer() error {
	return nil
}

func (p *lifecycleOrderProvider) TeardownServer() error {
	p.mu.Lock()
	p.events = append(p.events, "TeardownServer")
	p.mu.Unlock()
	return nil
}

// Compile-time assertions: lifecycleOrderProvider satisfies both interfaces.
var (
	_ runtime.Provider                = (*lifecycleOrderProvider)(nil)
	_ runtime.ServerLifecycleProvider = (*lifecycleOrderProvider)(nil)
)

// TestCmdStopBodyPreservesDrainedServerInventory proves a managed stop drains
// sessions without tearing down a provider-owned server inventory.
func TestCmdStopBodyPreservesDrainedServerInventory(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "lifecycle-order-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)

	sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}
	sessionName := agent.SessionNameFor(cfg.Workspace.Name, cfg.Agents[0].QualifiedName(), cfg.Workspace.SessionTemplate)
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("start managed session: %v", err)
	}

	overrideShutdownBeadsProviderForStop(t, func(string) error {
		return nil
	})

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }

	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdStopBody() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if sp.IsRunning(sessionName) {
		t.Fatalf("managed session %q remains running after stop", sessionName)
	}

	sp.mu.Lock()
	allProviderEvents := make([]string, len(sp.events))
	copy(allProviderEvents, sp.events)
	sp.mu.Unlock()

	for _, event := range allProviderEvents {
		if event == "TeardownServer" {
			t.Fatalf("managed stop tore down the drained server inventory: %v", allProviderEvents)
		}
	}
}

func TestCmdStopBodyFailsClosedWhenRuntimeInventoryFails(t *testing.T) {
	for _, test := range []struct {
		name       string
		listErrors []error
	}{
		{name: "partial orphan inventory", listErrors: []error{nil, &runtime.PartialListError{Err: errors.New("partial runtime inventory unavailable")}}},
		{name: "hard orphan inventory error", listErrors: []error{nil, errors.New("runtime inventory unavailable")}},
		{name: "hard initial inventory error", listErrors: []error{errors.New("runtime inventory unavailable")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cityDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := &config.City{
				Workspace: config.Workspace{Name: "orphan-inventory-city"},
				Beads:     config.BeadsConfig{Provider: "file"},
				Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
			}
			writeStopLifecycleCityConfig(t, cityDir, cfg)

			provider := &lifecycleOrderProvider{
				Fake:       runtime.NewFake(),
				listErrors: test.listErrors,
			}
			overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
			oldFactory := sessionProviderForStopCity
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
				return provider, nil
			}

			var stdout, stderr lockedBuffer
			code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("cmdStopBody() = %d, want fail-closed 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), "City stopped.") {
				t.Fatalf("stdout reported terminal success after failed orphan inventory: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "runtime inventory unavailable") {
				t.Fatalf("stderr = %q, want runtime inventory failure detail", stderr.String())
			}
		})
	}
}

func TestCmdStopBodyStopsWithNonServerProvider(t *testing.T) {
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

	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
		return runtime.NewFake(), nil
	}

	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdStopBody() = %d, want 0 for non-server provider; stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout = %q, want City stopped.", stdout.String())
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
