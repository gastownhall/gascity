package acp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

// holdProcessEnvProjection takes execenv.ProcessEnvLock, as a native beads
// store open does for its whole projection, and returns the idempotent
// release, which also runs at cleanup.
func holdProcessEnvProjection(t *testing.T) (release func()) {
	t.Helper()
	execenv.ProcessEnvLock.Lock()
	held := true
	release = func() {
		if held {
			held = false
			execenv.ProcessEnvLock.Unlock()
		}
	}
	t.Cleanup(release)
	return release
}

// startWaitingOutTheProjection starts name on p in the background and returns
// Start's result channel once Start is reading the parent env, the read that
// waits while a projection holds execenv.ProcessEnvLock.
func startWaitingOutTheProjection(ctx context.Context, t *testing.T, p *Provider, name string) <-chan error {
	t.Helper()
	reading := make(chan struct{})
	signal := sync.OnceFunc(func() { close(reading) })
	p.environFunc = func() []string {
		signal()
		return execenv.Environ()
	}
	started := make(chan error, 1)
	go func() { started <- p.Start(ctx, name, runtime.Config{Command: fakeACPShellCommand()}) }()
	select {
	case <-reading:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("Start did not reach its parent-env read")
	}
	return started
}

// A Start waiting out a process-env projection (a native beads store open)
// must hold nothing another call needs: the wait lasts the whole open and
// cannot be canceled. Stop for the waiting name, from this provider or from
// another process's, and observations must not wait with it.
func TestStartWaitingOutAProcessEnvProjectionLeavesTheProviderServing(t *testing.T) {
	for _, stopper := range []string{"same-provider", "cross-provider"} {
		t.Run(stopper, func(t *testing.T) {
			p := newTestProvider(t)
			name := testName()
			// Registered first so it runs after the projection is released.
			t.Cleanup(func() { _ = p.Stop(name) })
			release := holdProcessEnvProjection(t)
			started := startWaitingOutTheProjection(testingContext(t), t, p, name)

			observed := make(chan bool, 1)
			go func() { observed <- p.IsRunning(testName()) }()
			select {
			case running := <-observed:
				if running {
					t.Error("IsRunning of an unrelated session = true, want false")
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				release()
				t.Fatal("IsRunning waited behind a Start that was waiting out a process-env projection")
			}

			stop := p.Stop
			if stopper == "cross-provider" {
				stop = NewProviderWithDir(p.dir, Config{}).Stop
			}
			stopped := make(chan error, 1)
			go func() { stopped <- stop(name) }()
			select {
			case err := <-stopped:
				if err != nil {
					t.Errorf("Stop: %v", err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				release()
				t.Fatal("Stop waited behind a Start that was waiting out a process-env projection")
			}

			// That Stop found no session to stop, so the start completes once
			// the projection restores.
			release()
			select {
			case err := <-started:
				if err != nil {
					t.Fatalf("Start after the projection restored: %v", err)
				}
			case <-time.After(testutil.ExecRaceTimeout):
				t.Fatal("Start did not return after the projection restored")
			}
		})
	}
}

// A start canceled while it waits out a process-env projection must end
// without spawning its agent once the projection restores: it holds no
// reservation for Stop to cancel yet, so its context is how a caller
// abandons it.
func TestStartCanceledWhileWaitingOutAProcessEnvProjectionSpawnsNothing(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	var handshakes atomic.Int32
	p.handshakeFunc = func(ctx context.Context, sc *sessionConn, wd string, servers []runtime.MCPServerConfig) error {
		handshakes.Add(1)
		return p.handshake(ctx, sc, wd, servers)
	}
	// Registered first so it runs after the projection is released.
	t.Cleanup(func() { _ = p.Stop(name) })
	release := holdProcessEnvProjection(t)
	ctx, cancel := context.WithCancel(testingContext(t))
	defer cancel()
	started := startWaitingOutTheProjection(ctx, t, p, name)

	cancel()
	release()
	select {
	case err := <-started:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("canceled Start did not return after the projection restored")
	}
	if n := handshakes.Load(); n != 0 {
		t.Errorf("canceled Start spawned its agent: %d handshake(s)", n)
	}
	if NewProviderWithDir(p.dir, Config{}).IsRunning(name) {
		t.Error("canceled Start left a running session")
	}
}
