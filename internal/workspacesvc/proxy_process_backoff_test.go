package workspacesvc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// These tests pin the one escalating backoff every proxy_process respawn loop
// shares (ga-ydr5p3, ruling R1 of ga-5hnydz). Every window is driven through
// Tick's now and every failed start fails fast: the python helper exits at once
// when GC_SERVICE_FAIL_ONCE_FILE names an existing file, and it deletes the file
// as it goes, so the file's absence after a Tick means a start was attempted
// and its presence means none was.

const (
	backoffURLOne   = "https://bridge--acme--11111111.apps.example.com"
	backoffURLTwo   = "https://bridge--acme--deadbeef.apps.example.com"
	backoffURLThree = "https://bridge--acme--33333333.apps.example.com"
)

type backoffFixture struct {
	rt       *testRuntime
	mgr      *Manager
	failOnce string
}

// newBackoffFixture starts one published proxy_process service ("bridge") backed
// by the python helper and returns it ready, serving backoffURLOne.
func newBackoffFixture(t *testing.T) *backoffFixture {
	t.Helper()
	requirePython3(t)
	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "demo-app"},
			Services: []config.Service{{
				Name:        "bridge",
				Kind:        "proxy_process",
				Publication: config.ServicePublicationConfig{Visibility: "public"},
				Process: config.ServiceProcessConfig{
					Command:    []string{"python3", "-c", proxyProcessPythonHelper},
					HealthPath: "/healthz",
				},
			}},
		},
		pubCfg: supervisor.PublicationConfig{
			Provider:         "hosted",
			TenantSlug:       "acme",
			PublicBaseDomain: "apps.example.com",
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	f := &backoffFixture{
		rt:       rt,
		failOnce: filepath.Join(rt.cityPath, ".gc", "services", "bridge", "fail-once"),
	}
	if err := os.MkdirAll(filepath.Dir(f.failOnce), 0o750); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(f.failOnce), err)
	}
	t.Setenv("GC_SERVICE_FAIL_ONCE_FILE", f.failOnce)
	f.publish(t, backoffURLOne)
	f.mgr = NewManager(rt)
	t.Cleanup(func() { _ = f.mgr.Close() })
	if err := f.mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := f.servedURL(t); got != backoffURLOne {
		t.Fatalf("initial served URL = %q, want %q", got, backoffURLOne)
	}
	return f
}

func (f *backoffFixture) publish(t *testing.T, url string) {
	t.Helper()
	writePublicationStoreForTest(t, f.rt.cityPath, `[
  {
    "service_name": "bridge",
    "visibility": "public",
    "url": "`+url+`"
  }
]`)
}

// arm makes the next helper start exit at once.
func (f *backoffFixture) arm(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.failOnce, []byte("1"), 0o640); err != nil {
		t.Fatalf("WriteFile(%q): %v", f.failOnce, err)
	}
}

// consumed reports whether a helper started since the last arm.
func (f *backoffFixture) consumed(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(f.failOnce)
	if err == nil {
		return false
	}
	if !os.IsNotExist(err) {
		t.Fatalf("Stat(%q): %v", f.failOnce, err)
	}
	return true
}

// servedURL asks the live instance which GC_SERVICE_PUBLIC_URL it was started with.
func (f *backoffFixture) servedURL(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	if ok := f.mgr.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/svc/bridge/env", nil)); !ok || rec.Code != http.StatusOK {
		status, _ := f.mgr.Get("bridge")
		t.Fatalf("env endpoint: handled=%v code=%d status=%+v", ok, rec.Code, status)
	}
	var env map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode env: %v", err)
	}
	return env["GC_SERVICE_PUBLIC_URL"]
}

func (f *backoffFixture) entry() *entry {
	f.mgr.mu.RLock()
	defer f.mgr.mu.RUnlock()
	return f.mgr.entries["bridge"]
}

// seed runs fn on the bridge entry under the manager lock.
func (f *backoffFixture) seed(fn func(e *entry)) {
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	fn(f.mgr.entries["bridge"])
}

func (f *backoffFixture) instance(t *testing.T) *proxyProcessInstance {
	t.Helper()
	inst, ok := f.entry().inst.(*proxyProcessInstance)
	if !ok {
		t.Fatalf("bridge instance is %T, want *proxyProcessInstance", f.entry().inst)
	}
	return inst
}

// TestManagerTickRefreshBackoffEscalates: a failing publication refresh waits
// 1s, 2s, then 4s between attempts, and the old instance keeps serving the old
// URL throughout.
func TestManagerTickRefreshBackoffEscalates(t *testing.T) {
	f := newBackoffFixture(t)
	f.publish(t, backoffURLTwo)

	t0 := time.Now().UTC()
	steps := []struct {
		at           time.Duration
		attempt      bool
		wantFailures int
		wantWindow   time.Duration // from the tick, checked when attempt
	}{
		{0, true, 1, time.Second},
		{500 * time.Millisecond, false, 1, 0},
		{time.Second, true, 2, 2 * time.Second},
		{2 * time.Second, false, 2, 0},
		{3 * time.Second, true, 3, 4 * time.Second},
	}
	for _, step := range steps {
		f.arm(t)
		now := t0.Add(step.at)
		f.mgr.Tick(context.Background(), now)
		if got := f.consumed(t); got != step.attempt {
			t.Fatalf("tick at t0+%v: start attempted = %v, want %v", step.at, got, step.attempt)
		}
		e := f.entry()
		if e.refreshFailures != step.wantFailures {
			t.Fatalf("tick at t0+%v: refreshFailures = %d, want %d", step.at, e.refreshFailures, step.wantFailures)
		}
		if step.attempt && !e.nextRefreshRetry.Equal(now.Add(step.wantWindow)) {
			t.Fatalf("tick at t0+%v: nextRefreshRetry = %v, want tick+%v", step.at, e.nextRefreshRetry, step.wantWindow)
		}
		if status, _ := f.mgr.Get("bridge"); status.URL != backoffURLOne {
			t.Fatalf("tick at t0+%v: status URL = %q, want old URL %q", step.at, status.URL, backoffURLOne)
		}
		if got := f.servedURL(t); got != backoffURLOne {
			t.Fatalf("tick at t0+%v: served URL = %q, want old URL %q", step.at, got, backoffURLOne)
		}
	}
}

// TestManagerTickRefreshBackoffCapsAtMax: the tenth failure arms the 5 minute
// ceiling. Seeded white-box rather than spawned ten times.
func TestManagerTickRefreshBackoffCapsAtMax(t *testing.T) {
	f := newBackoffFixture(t)
	f.publish(t, backoffURLTwo)
	f.seed(func(e *entry) {
		e.refreshFailures = 9
		e.nextRefreshRetry = time.Time{}
	})
	f.arm(t)

	now := time.Now().UTC()
	f.mgr.Tick(context.Background(), now)
	if !f.consumed(t) {
		t.Fatal("a due refresh window did not attempt the refresh")
	}
	e := f.entry()
	if e.refreshFailures != 10 {
		t.Fatalf("refreshFailures = %d, want 10", e.refreshFailures)
	}
	if got := e.nextRefreshRetry.Sub(now); got != 5*time.Minute {
		t.Fatalf("nextRefreshRetry - now = %v, want 5m", got)
	}
}

// TestManagerTickRefreshRecoveryResetsWindow: once a refresh succeeds both
// refresh fields are zero, so the next context change is attempted at once.
func TestManagerTickRefreshRecoveryResetsWindow(t *testing.T) {
	f := newBackoffFixture(t)
	f.publish(t, backoffURLTwo)
	f.arm(t)

	t0 := time.Now().UTC()
	f.mgr.Tick(context.Background(), t0)
	if !f.consumed(t) {
		t.Fatal("first refresh attempt did not run")
	}

	due := t0.Add(proxyProcessRestartBackoff)
	f.mgr.Tick(context.Background(), due)
	if got := f.servedURL(t); got != backoffURLTwo {
		t.Fatalf("served URL after a due, unarmed refresh = %q, want %q", got, backoffURLTwo)
	}
	e := f.entry()
	if e.refreshFailures != 0 || !e.nextRefreshRetry.IsZero() {
		t.Fatalf("after a successful swap refreshFailures = %d, nextRefreshRetry = %v; want 0 and zero", e.refreshFailures, e.nextRefreshRetry)
	}

	f.publish(t, backoffURLThree)
	f.mgr.Tick(context.Background(), due)
	if got := f.servedURL(t); got != backoffURLThree {
		t.Fatalf("served URL after a second context change at the same tick time = %q, want %q", got, backoffURLThree)
	}
}

// TestManagerTickConstructRetryBackoffEscalates: a service that cannot be
// constructed waits 1s after Reload, then 2s, 4s, 8s between Tick retries.
func TestManagerTickConstructRetryBackoffEscalates(t *testing.T) {
	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Services: []config.Service{{
				Name: "doomed",
				Kind: "proxy_process",
				Process: config.ServiceProcessConfig{
					Command:    []string{"/this/binary/does/not/exist"},
					HealthPath: "/healthz",
				},
			}},
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	mgr := NewManager(rt)
	defer mgr.Close() //nolint:errcheck // best-effort cleanup

	before := time.Now().UTC()
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	after := time.Now().UTC()

	read := func() (int, time.Time) {
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		e := mgr.entries["doomed"]
		return e.constructFailures, e.nextConstructionRetry
	}
	failures, next := read()
	if failures != 1 {
		t.Fatalf("constructFailures after Reload = %d, want 1", failures)
	}
	if next.Before(before.Add(time.Second)) || next.After(after.Add(time.Second)) {
		t.Fatalf("nextConstructionRetry after Reload = %v, want within [%v, %v]", next, before.Add(time.Second), after.Add(time.Second))
	}

	now := next
	for i, window := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
		mgr.Tick(context.Background(), now)
		failures, next = read()
		if failures != i+2 {
			t.Fatalf("retry %d: constructFailures = %d, want %d", i+1, failures, i+2)
		}
		if !next.Equal(now.Add(window)) {
			t.Fatalf("retry %d: nextConstructionRetry = %v, want tick+%v", i+1, next, window)
		}
		now = next
	}
}

// TestProxyProcessInstanceRestartBackoffEscalates: a crash after ready costs the
// base window, a restart that then keeps failing escalates on the schedule, a
// Tick inside the window makes no attempt, and a start that succeeds clears the
// count.
func TestProxyProcessInstanceRestartBackoffEscalates(t *testing.T) {
	f := newBackoffFixture(t)
	inst := f.instance(t)

	pid := proxyProcessInstancePID(t, f.mgr, "bridge")
	if pid == 0 {
		t.Fatal("bridge helper has no pid after Reload")
	}
	inst.mu.Lock()
	doneCh := inst.doneCh
	inst.mu.Unlock()
	killed := time.Now().UTC()
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill helper group %d: %v", pid, err)
	}
	select {
	case <-doneCh:
	case <-time.After(time.Minute):
		t.Fatal("instance never observed the helper exit")
	}
	exited := time.Now().UTC()

	read := func() (int, time.Time) {
		inst.mu.Lock()
		defer inst.mu.Unlock()
		return inst.restartFailures, inst.nextRestart
	}
	failures, next := read()
	if failures != 1 {
		t.Fatalf("restartFailures after a crash following ready = %d, want 1", failures)
	}
	if next.Before(killed.Add(time.Second)) || next.After(exited.Add(time.Second)) {
		t.Fatalf("nextRestart after a crash following ready = %v, want the base 1s window within [%v, %v]", next, killed.Add(time.Second), exited.Add(time.Second))
	}

	// The restart keeps failing: 2s, then 4s.
	tick := next
	f.arm(t)
	f.mgr.Tick(context.Background(), tick)
	if !f.consumed(t) {
		t.Fatal("a due restart did not attempt a start")
	}
	if failures, next = read(); failures != 2 || !next.Equal(tick.Add(2*time.Second)) {
		t.Fatalf("after the first failed restart: restartFailures = %d, nextRestart = %v; want 2 and tick+2s", failures, next)
	}

	// Inside the window: no attempt.
	f.arm(t)
	f.mgr.Tick(context.Background(), tick.Add(time.Second))
	if f.consumed(t) {
		t.Fatal("a Tick inside the restart window attempted a start")
	}
	if failures, next = read(); failures != 2 || !next.Equal(tick.Add(2*time.Second)) {
		t.Fatalf("after a not-due Tick: restartFailures = %d, nextRestart = %v; want unchanged 2 and tick+2s", failures, next)
	}

	// Window elapsed, file still armed: fails again, window 4s.
	tick = tick.Add(2 * time.Second)
	f.mgr.Tick(context.Background(), tick)
	if !f.consumed(t) {
		t.Fatal("a due restart did not attempt a start")
	}
	if failures, next = read(); failures != 3 || !next.Equal(tick.Add(4*time.Second)) {
		t.Fatalf("after the second failed restart: restartFailures = %d, nextRestart = %v; want 3 and tick+4s", failures, next)
	}

	// Not re-armed: the next due start succeeds and clears the count.
	f.mgr.Tick(context.Background(), tick.Add(4*time.Second))
	if failures, _ = read(); failures != 0 {
		t.Fatalf("restartFailures after a successful start = %d, want 0", failures)
	}
	if status, _ := f.mgr.Get("bridge"); status.LocalState != "ready" {
		t.Fatalf("LocalState after the successful restart = %q (reason=%q), want ready", status.LocalState, status.Reason)
	}
}

// TestProxyProcessRecordExitOnlyPenalisesReadyCrash: the exit watcher owns the
// window only for a service that was ready. An exit while still starting is
// accounted by whoever ran the start, so the watcher must not also write it
// (ga-5hnydz F4).
func TestProxyProcessRecordExitOnlyPenalisesReadyCrash(t *testing.T) {
	t.Run("starting leaves counter and window alone", func(t *testing.T) {
		cmd := &exec.Cmd{}
		sentinel := time.Now().UTC().Add(time.Hour)
		p := &proxyProcessInstance{
			cmd:             cmd,
			restartFailures: 5,
			nextRestart:     sentinel,
			status:          Status{State: "starting", LocalState: "starting"},
		}
		p.recordExit(cmd, errors.New("boom"))
		if p.cmd != nil {
			t.Fatal("recordExit left the exited cmd in place")
		}
		if p.restartFailures != 5 || !p.nextRestart.Equal(sentinel) {
			t.Fatalf("restartFailures = %d, nextRestart = %v; want untouched 5 and %v", p.restartFailures, p.nextRestart, sentinel)
		}
		if p.status.LocalState != "degraded" || !strings.Contains(p.status.Reason, "boom") {
			t.Fatalf("status = %+v, want degraded with the exit reason", p.status)
		}
	})
	t.Run("ready restarts the count at one", func(t *testing.T) {
		cmd := &exec.Cmd{}
		p := &proxyProcessInstance{
			cmd:             cmd,
			restartFailures: 4,
			status:          Status{State: "ready", LocalState: "ready"},
		}
		before := time.Now().UTC()
		p.recordExit(cmd, nil)
		after := time.Now().UTC()
		if p.restartFailures != 1 {
			t.Fatalf("restartFailures = %d, want 1", p.restartFailures)
		}
		if p.nextRestart.Before(before.Add(proxyProcessRestartBackoff)) || p.nextRestart.After(after.Add(proxyProcessRestartBackoff)) {
			t.Fatalf("nextRestart = %v, want the base window within [%v, %v]", p.nextRestart, before.Add(proxyProcessRestartBackoff), after.Add(proxyProcessRestartBackoff))
		}
	})
}

// TestManagerReloadRefreshAttemptsThroughOpenWindow: Reload is new information,
// so it recreates through an open refresh window, and a failure there is
// recorded into the reused entry instead of only logged.
func TestManagerReloadRefreshAttemptsThroughOpenWindow(t *testing.T) {
	t.Run("failure is recorded", func(t *testing.T) {
		f := newBackoffFixture(t)
		f.publish(t, backoffURLTwo)
		f.seed(func(e *entry) {
			e.refreshFailures = 3
			e.nextRefreshRetry = time.Now().UTC().Add(time.Hour)
		})
		f.arm(t)

		before := time.Now().UTC()
		if err := f.mgr.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		after := time.Now().UTC()
		if !f.consumed(t) {
			t.Fatal("Reload did not attempt the refresh through an open window")
		}
		e := f.entry()
		if e.refreshFailures != 4 {
			t.Fatalf("refreshFailures = %d, want 4", e.refreshFailures)
		}
		if e.nextRefreshRetry.Before(before.Add(8*time.Second)) || e.nextRefreshRetry.After(after.Add(8*time.Second)) {
			t.Fatalf("nextRefreshRetry = %v, want the 8s window within [%v, %v]", e.nextRefreshRetry, before.Add(8*time.Second), after.Add(8*time.Second))
		}
		if got := f.servedURL(t); got != backoffURLOne {
			t.Fatalf("served URL after a failed Reload refresh = %q, want old URL %q", got, backoffURLOne)
		}
	})
	t.Run("success leaves no window", func(t *testing.T) {
		f := newBackoffFixture(t)
		f.publish(t, backoffURLTwo)
		f.seed(func(e *entry) {
			e.refreshFailures = 3
			e.nextRefreshRetry = time.Now().UTC().Add(time.Hour)
		})

		if err := f.mgr.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if got := f.servedURL(t); got != backoffURLTwo {
			t.Fatalf("served URL after Reload through an open window = %q, want %q", got, backoffURLTwo)
		}
		e := f.entry()
		if e.refreshFailures != 0 || !e.nextRefreshRetry.IsZero() {
			t.Fatalf("refreshFailures = %d, nextRefreshRetry = %v; want 0 and zero", e.refreshFailures, e.nextRefreshRetry)
		}
	})
}

// TestManagerRestartSuccessZeroesBothWindows: an explicit Restart is new
// information, so a successful one clears the construct and refresh windows.
func TestManagerRestartSuccessZeroesBothWindows(t *testing.T) {
	f := newBackoffFixture(t)
	f.seed(func(e *entry) {
		e.constructFailures = 3
		e.nextConstructionRetry = time.Now().UTC().Add(time.Hour)
		e.refreshFailures = 2
		e.nextRefreshRetry = time.Now().UTC().Add(time.Hour)
	})

	if err := f.mgr.Restart("bridge"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	e := f.entry()
	if e.constructFailures != 0 || !e.nextConstructionRetry.IsZero() || e.refreshFailures != 0 || !e.nextRefreshRetry.IsZero() {
		t.Fatalf("after a successful Restart: constructFailures = %d, nextConstructionRetry = %v, refreshFailures = %d, nextRefreshRetry = %v; want all zero",
			e.constructFailures, e.nextConstructionRetry, e.refreshFailures, e.nextRefreshRetry)
	}
}

// TestManagerRestartFailureArmsConstructWindow: a failed Restart leaves a nil
// instance, and Tick must not retry it with no window at all.
func TestManagerRestartFailureArmsConstructWindow(t *testing.T) {
	f := newBackoffFixture(t)
	f.arm(t)

	before := time.Now().UTC()
	if err := f.mgr.Restart("bridge"); err == nil {
		t.Fatal("Restart succeeded, want the armed helper start to fail")
	}
	after := time.Now().UTC()
	if !f.consumed(t) {
		t.Fatal("Restart did not attempt a start")
	}
	e := f.entry()
	if e.inst != nil {
		t.Fatalf("inst = %T after a failed Restart, want nil", e.inst)
	}
	if e.constructFailures != 1 {
		t.Fatalf("constructFailures = %d, want 1", e.constructFailures)
	}
	if e.nextConstructionRetry.Before(before.Add(time.Second)) || e.nextConstructionRetry.After(after.Add(time.Second)) {
		t.Fatalf("nextConstructionRetry = %v, want the base window within [%v, %v]", e.nextConstructionRetry, before.Add(time.Second), after.Add(time.Second))
	}
}
