//go:build integration

package tmux

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// Compile-time check.
var _ runtime.Provider = (*Provider)(nil)

func TestTmuxConformance(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		name := fmt.Sprintf("gc-test-conform-%d", id)
		// Safety cleanup for orphan prevention.
		t.Cleanup(func() { _ = p.Stop(name) })
		return p, runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, name
	})
}

func TestProvider_StartStopIsRunning(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-adapter"

	// Clean slate.
	_ = p.Stop(name)

	if p.IsRunning(name) {
		t.Fatal("session should not exist before Start")
	}

	if err := p.Start(context.Background(), name, runtime.Config{Command: "sleep 300"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Stop(name) }()

	if !p.IsRunning(name) {
		t.Fatal("session should be running after Start")
	}

	// Duplicate start returns an error.
	if err := p.Start(context.Background(), name, runtime.Config{}); err == nil {
		t.Fatal("duplicate Start should return error")
	}

	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if p.IsRunning(name) {
		t.Fatal("session should not be running after Stop")
	}

	// Idempotent stop.
	if err := p.Stop(name); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
}

func TestProvider_StartWithEnv(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-adapter-env"
	_ = p.Stop(name)

	err := p.Start(context.Background(), name, runtime.Config{
		Command: "sleep 300",
		Env:     map[string]string{"GC_TEST": "hello"},
	})
	if err != nil {
		t.Fatalf("Start with env: %v", err)
	}
	defer func() { _ = p.Stop(name) }()

	// Verify the env var was set.
	val, err := p.Tmux().GetEnvironment(name, "GC_TEST")
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if val != "hello" {
		t.Fatalf("GC_TEST: got %q, want %q", val, "hello")
	}
}

func TestProvider_RecyclesDeadPaneWithoutProcessNames(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-dead-pane-recycle"
	_ = p.Stop(name)
	defer func() { _ = p.Stop(name) }()

	if err := p.Start(context.Background(), name, runtime.Config{
		Command: "sleep 0.1",
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start first session: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		has, err := p.Tmux().HasSession(name)
		if err != nil {
			t.Fatalf("HasSession: %v", err)
		}
		if has && !p.IsRunning(name) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if p.IsRunning(name) {
		t.Fatal("IsRunning stayed true after one-shot command exited")
	}

	if err := p.Start(context.Background(), name, runtime.Config{
		Command: "sleep 300",
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start after dead pane: %v", err)
	}
	if !p.IsRunning(name) {
		t.Fatal("session should be running after dead-pane recycle")
	}
}

func TestProviderObserveLivenessKeepsZombieShellVisible(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-zombie-shell-liveness"
	_ = p.Stop(name)
	defer func() { _ = p.Stop(name) }()

	if err := p.Start(context.Background(), name, runtime.Config{
		Command:      "sh -c 'sleep 2; exec sh -i'",
		WorkDir:      t.TempDir(),
		ProcessNames: []string{"sleep"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		obs := runtime.ObserveLiveness(p, name, nil)
		if obs.Running && !obs.Alive {
			if !p.IsRunning(name) {
				t.Fatalf("IsRunning = false, want true while zombie shell pane is still present")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	obs := runtime.ObserveLiveness(p, name, nil)
	t.Fatalf("ObserveLiveness() = %#v, want running zombie shell with dead process", obs)
}

func TestProvider_StartCanceledCleansUpSession(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-adapter-canceled"
	_ = p.Stop(name)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := p.Start(ctx, name, runtime.Config{
		Command:           "sleep 300",
		WorkDir:           t.TempDir(),
		ProcessNames:      []string{"sleep"},
		ReadyPromptPrefix: "> ",
		ReadyDelayMs:      1,
	})
	if !errors.Is(err, context.Canceled) {
		_ = p.Stop(name)
		t.Fatalf("Start: got %v, want context canceled", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !p.IsRunning(name) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = p.Stop(name)
	t.Fatal("session should be cleaned up after canceled start")
}
