package workspacesvc

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A starved host can take longer than the production readiness bound to start a
// healthy helper (ga-bhct0w). Reload and Tick return only once the helper has
// answered, so tests observe readiness and need a bound only a genuinely hung
// helper can outlive. Set here, before any test runs, so every test in the
// package, and the helper processes the test binary re-executes itself as,
// share it without a race. The bound gates a kill, so it is a guard: this
// package-global relaxation is allowlisted, with this reason, in
// tools/nogo/analyzers/testrelax.
func init() {
	proxyProcessReadyTimeout = time.Minute
}

// TestProxyProcessTickRestartReadyWindowStartsAtSpawn pins where the readiness
// clock starts. Manager.Tick hands every instance the same now, captured before
// the pass began, and a restart runs the orphan sweep (a scan of every process
// on the host) before it spawns anything, so now can already be older than the
// whole readiness window by the time the process exists. The window must be
// measured from the spawn: measured from now, a healthy helper is killed before
// it has been given any time at all.
func TestProxyProcessTickRestartReadyWindowStartsAtSpawn(t *testing.T) {
	t.Setenv("GC_SERVICE_HELPER", "1")
	setHelperPassthrough(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}

	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Services: []config.Service{{
				Name: "bridge",
				Kind: "proxy_process",
				Process: config.ServiceProcessConfig{
					Command:    []string{exe, "-test.run=^TestProxyProcessHelper$", "--"},
					HealthPath: "/healthz",
				},
			}},
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	mgr := NewManager(rt)
	defer mgr.Close() //nolint:errcheck // best-effort cleanup
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Crash the helper and wait for the instance's exit watcher to notice: it
	// closes doneCh only after it has cleared the instance's process.
	pid := proxyProcessInstancePID(t, mgr, "bridge")
	if pid == 0 {
		t.Fatal("bridge helper has no pid after Reload")
	}
	mgr.mu.RLock()
	inst, ok := mgr.entries["bridge"].inst.(*proxyProcessInstance)
	mgr.mu.RUnlock()
	if !ok {
		t.Fatal("bridge instance missing or wrong type")
	}
	inst.mu.Lock()
	doneCh := inst.doneCh
	inst.mu.Unlock()
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill helper group %d: %v", pid, err)
	}
	select {
	case <-doneCh:
	case <-time.After(time.Minute):
		t.Fatal("instance never observed the helper exit")
	}
	// The restart backoff has elapsed; clearing it avoids sleeping the test.
	inst.mu.Lock()
	inst.nextRestart = time.Time{}
	inst.mu.Unlock()

	// A tick time already older than the whole readiness window.
	stale := time.Now().UTC().Add(-2 * proxyProcessReadyTimeout)
	mgr.Tick(context.Background(), stale)

	status, ok := mgr.Get("bridge")
	if !ok {
		t.Fatal("service status missing")
	}
	if status.LocalState != "ready" {
		t.Fatalf("LocalState = %q (reason=%q), want ready: a restart whose tick time predates the readiness window must still get the window from the spawn", status.LocalState, status.Reason)
	}
}
