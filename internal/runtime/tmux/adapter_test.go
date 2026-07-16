//go:build integration

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// The conformance fixture is a generic long-running command, not an agent
	// TUI with an observable idle prompt. Keep a short real timeout so the
	// Provider.Nudge wait/fallback branch stays covered without consuming the
	// production 30-second budget.
	cfg.NudgeIdleTimeout = 250 * time.Millisecond
	// Exercise the production construction path so one real tmux suite covers
	// both the Provider contract and the seam-backed cut-over.
	p := NewSeamBackedWithConfig(cfg)
	var counter int64

	runtimetest.RunProviderTestsWithOptions(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		name := fmt.Sprintf("gc-test-conform-%d", id)
		return p, runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, name
	}, runtimetest.Options{
		SkipStartError: func(err error) (string, bool) {
			if errors.Is(err, ErrServerDegraded) {
				return fmt.Sprintf("tmux test socket degraded before Start could run: %v", err), true
			}
			return "", false
		},
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

// TestProvider_RelaunchInWarmSession proves the un-weld relaunch path (B1):
// Relaunch respawns the agent with a NEW command inside the SAME box, the box is
// reused (its session env survives, since Relaunch never re-sets env), and a
// relaunch into a non-existent box is an error rather than a silent provision.
func TestProvider_RelaunchInWarmSession(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-relaunch-warm"
	_ = p.Stop(name)
	defer func() { _ = p.Stop(name) }()

	workDir := t.TempDir()
	marker := filepath.Join(workDir, "marker")
	// Single-string sh -c command, passed to tmux the same way the long-prompt
	// path already does (see ensureFreshSession), so tmux runs it intact.
	agentCmd := func(tag string) string {
		return fmt.Sprintf("sh -c 'echo %s > %s; sleep 300'", tag, marker)
	}

	// Provision the warm box (welded Start) with a sentinel session env value.
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: agentCmd("first"),
		WorkDir: workDir,
		Env:     map[string]string{"GC_RELAUNCH_TEST": "warm"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForMarker(t, marker, "first")

	// Relaunch the agent in the SAME box with a new command. Env is provision-half
	// and intentionally NOT re-passed; the box keeps its session environment.
	if err := p.Relaunch(context.Background(), name, runtime.Config{
		Command: agentCmd("second"),
		WorkDir: workDir,
	}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	waitForMarker(t, marker, "second")

	if !p.IsRunning(name) {
		t.Fatal("session should still be running after Relaunch")
	}

	// The box was reused, not recreated: the session env set at Start survives a
	// launch-only relaunch (Relaunch never re-sets env).
	val, err := p.Tmux().GetEnvironment(name, "GC_RELAUNCH_TEST")
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if val != "warm" {
		t.Fatalf("GC_RELAUNCH_TEST after relaunch = %q, want %q (warm box should be reused, not recreated)", val, "warm")
	}

	// Relaunch into a non-existent box is an error, not a silent provision.
	err = p.Relaunch(context.Background(), "gc-test-relaunch-absent", runtime.Config{Command: "sleep 300"})
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Relaunch of absent box = %v, want ErrSessionNotFound", err)
	}
}

func waitForMarker(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if last = strings.TrimSpace(string(b)); last == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("marker %q = %q, want %q (timed out)", path, last, want)
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
