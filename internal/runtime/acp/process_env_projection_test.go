package acp

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

// processEnvProbe is the variable a held projection window changes: the
// ambient value is what a session must inherit, the projected one is what the
// window holder sees through the bare os env.
const processEnvProbe = "GC_ACP_ENV_PROJECTION_PROBE"

// holdProcessEnvProjection enters an execenv projection window and projects a
// different value for processEnvProbe, as a native beads store open does for
// its whole open, and returns the idempotent end, which also runs at cleanup.
func holdProcessEnvProjection(t *testing.T) (end func()) {
	t.Helper()
	t.Setenv(processEnvProbe, "ambient")
	exit, err := execenv.EnterProjectionWindow(context.Background())
	if err != nil {
		t.Fatalf("EnterProjectionWindow: %v", err)
	}
	if err := os.Setenv(processEnvProbe, "projected"); err != nil {
		exit()
		t.Fatalf("projecting the probe: %v", err)
	}
	var once sync.Once
	end = func() {
		once.Do(func() {
			_ = os.Setenv(processEnvProbe, "ambient")
			exit()
		})
	}
	t.Cleanup(end)
	return end
}

// startReadingTheParentEnv starts name on p in the background and returns
// Start's result channel once Start has read the parent env. The env Start read
// is stored in seen.
func startReadingTheParentEnv(ctx context.Context, t *testing.T, p *Provider, name string, seen *atomic.Value) <-chan error {
	t.Helper()
	reading := make(chan struct{})
	signal := sync.OnceFunc(func() { close(reading) })
	p.environFunc = func() []string {
		env := execenv.Environ()
		seen.Store(env)
		signal()
		return env
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

func probeValues(env []string) []string {
	var values []string
	for _, entry := range env {
		if v, ok := strings.CutPrefix(entry, processEnvProbe+"="); ok {
			values = append(values, v)
		}
	}
	return values
}

// A Start during a process-env projection (a native beads store open) is not
// held up by it: the parent env it reads is the ambient one, the provider
// keeps serving, and Stop for the starting name, from this provider or from
// another process's, and observations do not wait.
func TestStartDuringAProcessEnvProjectionLeavesTheProviderServing(t *testing.T) {
	for _, stopper := range []string{"same-provider", "cross-provider"} {
		t.Run(stopper, func(t *testing.T) {
			p := newTestProvider(t)
			name := testName()
			// Registered first so it runs after the projection is ended.
			t.Cleanup(func() { _ = p.Stop(name) })
			end := holdProcessEnvProjection(t)
			var seen atomic.Value
			started := startReadingTheParentEnv(testingContext(t), t, p, name, &seen)

			env, _ := seen.Load().([]string)
			if got := probeValues(env); len(got) != 1 || got[0] != "ambient" {
				t.Errorf("%s in the env Start read = %v, want [ambient]", processEnvProbe, got)
			}

			observed := make(chan bool, 1)
			go func() { observed <- p.IsRunning(testName()) }()
			select {
			case running := <-observed:
				if running {
					t.Error("IsRunning of an unrelated session = true, want false")
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				end()
				t.Fatal("IsRunning waited behind a Start during a process-env projection")
			}

			select {
			case err := <-started:
				if err != nil {
					t.Fatalf("Start during the projection: %v", err)
				}
			case <-time.After(testutil.ExecRaceTimeout):
				end()
				t.Fatal("Start did not return while a projection window stood")
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
				end()
				t.Fatal("Stop waited behind a process-env projection")
			}
			end()
		})
	}
}

// A start whose ctx ends while it reads the parent env must end without
// spawning its agent: it holds no reservation for Stop to cancel yet, so its
// context is how a caller abandons it, and the ctx is checked again after the
// read. The cancellation lands through the environFunc seam, after the read.
func TestStartCanceledDuringTheParentEnvReadSpawnsNothing(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	var handshakes atomic.Int32
	p.handshakeFunc = func(ctx context.Context, sc *sessionConn, wd string, servers []runtime.MCPServerConfig) error {
		handshakes.Add(1)
		return p.handshake(ctx, sc, wd, servers)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	holdProcessEnvProjection(t)
	ctx, cancel := context.WithCancel(testingContext(t))
	defer cancel()
	p.environFunc = func() []string {
		env := execenv.Environ()
		cancel()
		return env
	}

	err := p.Start(ctx, name, runtime.Config{Command: fakeACPShellCommand()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v, want %v", err, context.Canceled)
	}
	if n := handshakes.Load(); n != 0 {
		t.Errorf("canceled Start spawned its agent: %d handshake(s)", n)
	}
	if NewProviderWithDir(p.dir, Config{}).IsRunning(name) {
		t.Error("canceled Start left a running session")
	}
}
