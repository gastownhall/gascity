package subprocess

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

// holdProcessEnvProjection enters an execenv projection window, as a native
// beads store open does, and projects the env a session must not inherit: the
// projected probe set and the ambient probe withheld. The returned end is
// idempotent and also runs at cleanup.
func holdProcessEnvProjection(t *testing.T) (end func()) {
	t.Helper()
	t.Setenv("GC_SUBPROCESS_AMBIENT_PROBE", "ambient")
	t.Setenv("GC_SUBPROCESS_PROJECTED_PROBE", "")
	if err := os.Unsetenv("GC_SUBPROCESS_PROJECTED_PROBE"); err != nil {
		t.Fatal(err)
	}
	exit, err := execenv.EnterProjectionWindow(context.Background())
	if err != nil {
		t.Fatalf("EnterProjectionWindow: %v", err)
	}
	if err := os.Setenv("GC_SUBPROCESS_PROJECTED_PROBE", "projected"); err != nil {
		exit()
		t.Fatal(err)
	}
	if err := os.Unsetenv("GC_SUBPROCESS_AMBIENT_PROBE"); err != nil {
		_ = os.Unsetenv("GC_SUBPROCESS_PROJECTED_PROBE")
		exit()
		t.Fatal(err)
	}
	var once sync.Once
	end = func() {
		once.Do(func() {
			_ = os.Unsetenv("GC_SUBPROCESS_PROJECTED_PROBE")
			_ = os.Setenv("GC_SUBPROCESS_AMBIENT_PROBE", "ambient")
			exit()
		})
	}
	t.Cleanup(end)
	return end
}

// A session started while another goroutine holds a transient process-env
// projection (the native beads store open) starts at once and must not inherit
// the projection: the session keeps its environment for its whole life, so a
// projected database selector or a withheld BD_* variable would outlive the
// projection that caused it.
func TestStartDuringAProcessEnvProjectionDoesNotInheritIt(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "env.txt")
	end := holdProcessEnvProjection(t)

	p := newTestProvider(t)
	started := make(chan error, 1)
	go func() {
		started <- p.Start(context.Background(), "env-projection", runtime.Config{
			Command: "env | sort > " + marker,
		})
	}()
	select {
	case err := <-started:
		if err != nil {
			end()
			t.Fatalf("Start during the projection: %v", err)
		}
	case <-time.After(5 * time.Second):
		end()
		t.Fatal("Start did not return while a process-env projection stood")
	}
	end()
	defer p.Stop("env-projection") //nolint:errcheck
	p.mu.Lock()
	conn := p.procs["env-projection"]
	p.mu.Unlock()
	if conn == nil {
		t.Fatal("environment child was not tracked")
	}
	select {
	case <-conn.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for environment child process")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "GC_SUBPROCESS_PROJECTED_PROBE=") {
		t.Fatalf("the in-flight projection reached the session: %q", got)
	}
	if !strings.Contains(got, "GC_SUBPROCESS_AMBIENT_PROBE=ambient\n") {
		t.Fatalf("the ambient variable the projection withheld did not reach the session: %q", got)
	}
}

// A Start during a process-env projection must not hold the provider's mutex
// while it reads the env: every other call on the provider (IsRunning, Stop,
// Interrupt) would wait with it. It starts at once and the provider keeps
// serving.
func TestStartDuringAProcessEnvProjectionLeavesTheProviderServing(t *testing.T) {
	p := newTestProvider(t)
	end := holdProcessEnvProjection(t)

	started := make(chan error, 1)
	go func() {
		started <- p.Start(context.Background(), "env-waiter", runtime.Config{Command: "sleep 30"})
	}()
	select {
	case err := <-started:
		if err != nil {
			end()
			t.Fatalf("Start during the projection: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		end()
		t.Fatal("Start did not return while a process-env projection stood")
	}
	defer p.Stop("env-waiter") //nolint:errcheck

	observed := make(chan bool, 1)
	go func() { observed <- p.IsRunning("env-bystander") }()
	select {
	case running := <-observed:
		if running {
			t.Errorf("IsRunning(env-bystander) = true, want false")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		end()
		t.Fatal("IsRunning waited during a process-env projection")
	}
	end()
}
