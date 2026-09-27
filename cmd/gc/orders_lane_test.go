package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
)

// run() waits for the orders lane before returning. A startup step that gives
// up returns from run() with the city context still live, so the lane must be
// stopped by run() itself — otherwise run() never returns.
func TestCityRuntimeRunReturnsWhenStartupGivesUpWithOrdersLaneRunning(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	oneAttempt := 1
	cfg.Daemon.MaxRestarts = &oneAttempt
	sp := runtime.NewFake()
	cr, runtimeErr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			panic("startup reconcile keeps failing")
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if runtimeErr != nil {
		t.Fatalf("building the city runtime: %v", runtimeErr)
	}
	cr.od = &recordingOrderDispatcher{}
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		cr.run(ctx)
	}()
	awaitClose(t, runDone, "run() returning after startup gave up (the orders lane must not keep it waiting)")
}

// Order dispatch must keep running through a long cold-start reconcile, not
// just before it: the lane starts right after the synchronous startup pass
// (MAINT-008), ahead of the startup session reconcile. With the reconcile
// parked in its desired-state build, the lane must add passes of its own.
func TestOrdersLaneDispatchesDuringColdStartReconcile(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "100")
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Daemon.PatrolInterval = "10ms"
	sp := runtime.NewFake()
	reconcileEntered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseStartup := func() { releaseOnce.Do(func() { close(release) }) }
	cr, runtimeErr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			enterOnce.Do(func() { close(reconcileEntered) })
			<-release
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if runtimeErr != nil {
		t.Fatalf("building the city runtime: %v", runtimeErr)
	}
	od := &recordingOrderDispatcher{}
	cr.od = od
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		cr.run(ctx)
	}()
	t.Cleanup(func() {
		releaseStartup()
		cancel()
		awaitClose(t, runDone, "run() returning after cancellation")
	})
	awaitClose(t, reconcileEntered, "the cold-start reconcile reaching its desired-state build")

	// One dispatch is the synchronous startup pass; the rest must be lane
	// passes, landing while the reconcile stays parked.
	waitForCalls(t, od.calls.Load, 3)
}

// ordersLaneTestRuntime is a directly-constructed runtime with a fast patrol
// cadence and FS pressure pinned off, so a lane test depends on neither the
// host's /proc/pressure/io nor a 30s default interval.
func ordersLaneTestRuntime(t *testing.T, od orderDispatcher, patrol string, stderr io.Writer) *CityRuntime {
	t.Helper()
	// A threshold of 100 can never be exceeded, so the lane's pressure gate
	// always proceeds regardless of the machine running the test.
	t.Setenv(fsPressureThresholdEnv, "100")
	if stderr == nil {
		stderr = io.Discard
	}
	sp := runtime.NewFake()
	return &CityRuntime{
		cityName: "test-city",
		cityPath: t.TempDir(),
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Daemon:    config.DaemonConfig{PatrolInterval: patrol},
		},
		sp:                  sp,
		standaloneCityStore: beads.NewMemStore(),
		od:                  od,
		rec:                 events.Discard,
		logPrefix:           "gc test",
		stdout:              io.Discard,
		stderr:              stderr,
	}
}

// startOrdersLaneForTest starts the lane and registers a cleanup that stops it
// and waits for its goroutine, so no pass outlives the test.
func startOrdersLaneForTest(t *testing.T, cr *CityRuntime) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := cr.startOrdersLane(ctx, cr.cityPath)
	t.Cleanup(func() {
		cancel()
		awaitClose(t, done, "the orders lane stopping after cancellation")
	})
	return cancel, done
}

// waitForCalls polls until calls reaches want, failing after a generous bound.
func waitForCalls(t *testing.T, calls func() int32, want int32) {
	t.Helper()
	const within = 10 * time.Second
	deadline := time.Now().Add(within)
	for calls() < want {
		if time.Now().After(deadline) {
			t.Fatalf("order dispatch calls = %d, want at least %d within %s", calls(), want, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The defect this lane exists for: on maintainer-city dispatch_orders was ~32s
// of a tick the controller goroutine spent 97-100% of its time in, so a due
// order waited behind every session phase. The lane must dispatch on its own
// cadence while a tick is wedged in its session work.
func TestOrdersLaneDispatchesWhileTickIsBlocked(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "20ms", nil)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseTick := func() { releaseOnce.Do(func() { close(release) }) }
	tickEnteredSessionWork := make(chan struct{})
	var once sync.Once
	cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
		once.Do(func() { close(tickEnteredSessionWork) })
		<-release
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	startOrdersLaneForTest(t, cr)

	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		var dirty atomic.Bool
		var lastProviderName string
		var prevPoolRunning map[string]bool
		cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
	}()
	t.Cleanup(func() {
		releaseTick()
		awaitClose(t, tickDone, "the parked tick finishing")
	})
	awaitClose(t, tickEnteredSessionWork, "the tick reaching its session work")

	// The tick is now parked in its demand build. Two lane passes must land
	// while it stays parked: dispatch does not wait on the tick.
	waitForCalls(t, func() int32 { return od.calls.Load() }, 2)
}

// The tick no longer runs order dispatch itself; it only nudges the lane. A
// directly-driven tick with no lane running must therefore not dispatch.
func TestCityRuntimeTickDoesNotDispatchOrders(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}
	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
	if got := od.calls.Load(); got != 0 {
		t.Fatalf("order dispatch calls from tick = %d, want 0: dispatch belongs to the orders lane", got)
	}
}

// Every tick that used to dispatch orders now wakes the lane instead, so a
// poke-driven tick still gets its orders evaluated promptly rather than at the
// lane's next patrol-interval pass.
func TestOrdersLaneWakesOnTick(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}
	startOrdersLaneForTest(t, cr)

	// With a 1h cadence the lane would not pass on its own inside this test.
	time.Sleep(50 * time.Millisecond)
	if got := od.calls.Load(); got != 0 {
		t.Fatalf("order dispatch calls before any tick = %d, want 0", got)
	}

	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "poke")

	waitForCalls(t, func() int32 { return od.calls.Load() }, 1)
}

// A panicking dispatch must be recovered per pass (incident #663): the lane
// keeps running, the controller is untouched, and the panic is logged with the
// lane's trigger so an operator can tell which site fired.
func TestOrdersLanePanicDoesNotKillLane(t *testing.T) {
	stderr := &syncWriter{}
	od := &recordingOrderDispatcher{}
	od.onDispatch = func(context.Context, string, time.Time) {
		if od.calls.Load() == 1 {
			panic("orders lane boom")
		}
	}
	cr := ordersLaneTestRuntime(t, od, "20ms", stderr)
	_, done := startOrdersLaneForTest(t, cr)

	waitForCalls(t, func() int32 { return od.calls.Load() }, 3)
	select {
	case <-done:
		t.Fatal("orders lane exited after a recovered panic")
	default:
	}
	stderr.mu.Lock()
	out := stderr.buf.String()
	stderr.mu.Unlock()
	if !strings.Contains(out, "trigger="+ordersLaneSafeTickTrigger) {
		t.Fatalf("stderr = %q, want panic logged with trigger=%s", out, ordersLaneSafeTickTrigger)
	}
	if !strings.Contains(out, "orders lane boom") {
		t.Fatalf("stderr = %q, want recovered panic detail", out)
	}
}

// Cancellation stops the lane: its goroutine exits and no pass starts after.
func TestOrdersLaneStopsOnShutdown(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "10ms", nil)
	cancel, done := startOrdersLaneForTest(t, cr)
	waitForCalls(t, func() int32 { return od.calls.Load() }, 1)

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("orders lane did not exit after cancellation")
	}
	after := od.calls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := od.calls.Load(); got != after {
		t.Fatalf("order dispatch calls after lane stop = %d, want %d", got, after)
	}
}

// A pass that finds the context already canceled returns before dispatching,
// the lane-side equivalent of the tick's early return.
func TestOrdersLanePassReturnsWhenCanceled(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cr.runOrdersLanePass(ctx, cr.cityPath, ordersLaneReasonCadence)
	if od.called.Load() {
		t.Fatal("order dispatch ran after the lane context was canceled")
	}
}

// #5990 lives inside the dispatcher, and the lane must hand it the same call
// the tick did: a due condition order fires even when clock-driven orders
// ahead of it in the rotation spend the per-pass budget.
func TestOrdersLaneFiresDueConditionOrderOutsideTheRotationBudget(t *testing.T) {
	aa := []orders.Order{
		cooldownBudgetOrder("sweep-a"),
		cooldownBudgetOrder("sweep-b"),
		conditionBudgetOrder("true"),
	}
	m, _, store := newConditionBudgetDispatcher(t, aa, 1)
	cr := ordersLaneTestRuntime(t, m, "1h", nil)

	cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)
	drainOrderDispatch(t, m)

	if got := runCountFor(t, store, "queue-c"); got != 1 {
		t.Fatalf("condition order runs after one lane pass = %d, want 1 (#5990)", got)
	}
}

// A reload that lands while a lane pass holds the dispatcher must not wait for
// the pass (reload-reply latency must not scale with order count, #3206). The
// new dispatcher is staged and installed by the next pass, carrying state from
// the outgoing one.
func TestOrdersLaneReloadDuringPassDoesNotBlock(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	releasePass := func() { releaseOnce.Do(func() { close(release) }) }
	oldOD := &recordingOrderDispatcher{onDispatch: func(context.Context, string, time.Time) {
		once.Do(func() { close(entered) })
		<-release
	}}
	newOD := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, oldOD, "1h", nil)

	passDone := make(chan struct{})
	go func() {
		defer close(passDone)
		cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)
	}()
	t.Cleanup(func() {
		releasePass()
		awaitClose(t, passDone, "the parked lane pass finishing")
	})
	awaitClose(t, entered, "the lane pass reaching dispatch")

	installed := make(chan struct{})
	go func() {
		defer close(installed)
		cr.stageOrderDispatcher(newOD, nil, "sig-new", time.Now())
		cr.tryInstallPendingOrderDispatcher(context.Background())
	}()
	// Staging never takes passMu, so it returns while the pass stays parked;
	// a stage that blocked behind the pass would wedge here.
	awaitClose(t, installed, "staging a reloaded dispatcher during an in-flight lane pass")
	releasePass()
	awaitClose(t, passDone, "the released lane pass finishing")

	cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)
	if got := newOD.calls.Load(); got != 1 {
		t.Fatalf("new dispatcher calls after the next pass = %d, want 1", got)
	}
	if got := oldOD.calls.Load(); got != 1 {
		t.Fatalf("old dispatcher calls = %d, want 1 (only the pass in flight at reload)", got)
	}
	if oldOD.drainCalls != 1 {
		t.Fatalf("old dispatcher drain calls = %d, want 1 before replacement", oldOD.drainCalls)
	}
}

// With no pass in flight, a staged dispatcher is installed synchronously, so a
// reload observed from the controller goroutine behaves exactly as before.
func TestOrdersLaneStagedDispatcherInstallsImmediatelyWhenIdle(t *testing.T) {
	oldOD := &recordingOrderDispatcher{}
	newOD := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, oldOD, "1h", nil)
	cr.stageOrderDispatcher(newOD, nil, "sig-new", time.Now())
	cr.tryInstallPendingOrderDispatcher(context.Background())
	if cr.od != orderDispatcher(newOD) {
		t.Fatalf("cr.od = %#v, want the staged dispatcher installed", cr.od)
	}
	if oldOD.drainCalls != 1 {
		t.Fatalf("old dispatcher drain calls = %d, want 1", oldOD.drainCalls)
	}
}
