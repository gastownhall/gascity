package subprocess

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

// A session started while another goroutine holds a transient process-env
// projection (the native beads store open) must not inherit it: the session
// keeps its environment for its whole life, so a projected database selector
// or a withheld BD_* variable would outlive the projection that caused it.
func TestStartDoesNotInheritAnInFlightProcessEnvProjection(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("GC_SUBPROCESS_AMBIENT_PROBE", "ambient")
	t.Setenv("GC_SUBPROCESS_PROJECTED_PROBE", "")
	if err := os.Unsetenv("GC_SUBPROCESS_PROJECTED_PROBE"); err != nil {
		t.Fatal(err)
	}

	execenv.ProcessEnvLock.Lock()
	projected := true
	restore := func() {
		if !projected {
			return
		}
		projected = false
		_ = os.Unsetenv("GC_SUBPROCESS_PROJECTED_PROBE")
		_ = os.Setenv("GC_SUBPROCESS_AMBIENT_PROBE", "ambient")
		execenv.ProcessEnvLock.Unlock()
	}
	t.Cleanup(restore)
	if err := os.Setenv("GC_SUBPROCESS_PROJECTED_PROBE", "projected"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("GC_SUBPROCESS_AMBIENT_PROBE"); err != nil {
		t.Fatal(err)
	}

	p := newTestProvider(t)
	started := make(chan error, 1)
	go func() {
		started <- p.Start(context.Background(), "env-projection", runtime.Config{
			Command: "env | sort > " + marker,
		})
	}()
	select {
	case err := <-started:
		restore()
		t.Fatalf("Start returned (err=%v) while a process-env projection was in flight; it must wait for the projection to restore", err)
	case <-time.After(10 * time.Millisecond):
	}
	restore()

	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the projection restored")
	}
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

// A Start waiting out a process-env projection must not hold the provider's
// mutex while it waits: the projection lasts a whole native store open, and
// every other call on the provider (IsRunning, Stop, Interrupt) would wait
// with it.
func TestStartWaitingOutAProcessEnvProjectionLeavesTheProviderServing(t *testing.T) {
	p := newTestProvider(t)
	execenv.ProcessEnvLock.Lock()
	projected := true
	restore := func() {
		if projected {
			projected = false
			execenv.ProcessEnvLock.Unlock()
		}
	}
	t.Cleanup(restore)

	started := make(chan error, 1)
	go func() {
		started <- p.Start(context.Background(), "env-waiter", runtime.Config{Command: "sleep 30"})
	}()
	select {
	case err := <-started:
		restore()
		t.Fatalf("Start returned (err=%v) while a process-env projection was in flight", err)
	case <-time.After(20 * time.Millisecond):
	}

	observed := make(chan bool, 1)
	go func() { observed <- p.IsRunning("env-bystander") }()
	select {
	case running := <-observed:
		if running {
			t.Errorf("IsRunning(env-bystander) = true, want false")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		restore()
		t.Fatal("IsRunning waited behind a Start that was waiting out a process-env projection")
	}
	restore()
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop("env-waiter") //nolint:errcheck
}
