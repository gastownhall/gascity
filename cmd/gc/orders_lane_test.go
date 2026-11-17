package main

import (
	"context"
	"fmt"
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
	"github.com/gastownhall/gascity/internal/fsys"
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

// A forced shutdown that overlaps a lane pass still holding passMu skips the
// order dispatcher drain instead of blocking on the pass or racing it for the
// dispatchers the lane owns.
func TestOrdersLaneForcedShutdownSkipsDrainWhileLaneHoldsPass(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.ShutdownTimeout = "1s"
	od := &recordingOrderDispatcher{}
	forceStop := &atomic.Bool{}
	forceStop.Store(true)
	var stderr syncWriter
	cr := &CityRuntime{
		cfg:               cfg,
		sp:                runtime.NewFake(),
		od:                od,
		rec:               events.Discard,
		logPrefix:         "gc start",
		stdout:            io.Discard,
		stderr:            &stderr,
		forceStopShutdown: forceStop,
	}
	// Stand in for a lane pass still in flight.
	lane := cr.ordersLaneOf()
	lane.passMu.Lock()
	var unlockOnce sync.Once
	endPass := func() { unlockOnce.Do(lane.passMu.Unlock) }
	t.Cleanup(endPass)

	done := make(chan struct{})
	go func() {
		defer close(done)
		cr.shutdown()
	}()
	awaitClose(t, done, "forced shutdown while the orders lane holds its pass lock")
	endPass()

	stderr.mu.Lock()
	out := stderr.buf.String()
	stderr.mu.Unlock()
	if !strings.Contains(out, "skipping order dispatcher drain") {
		t.Fatalf("stderr = %q, want the skipped-drain notice", out)
	}
	if od.drainCalls != 0 {
		t.Fatalf("order dispatcher drain calls = %d, want 0 while the lane holds the dispatchers", od.drainCalls)
	}
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

// waitForCalls waits until calls reaches want, failing after the hang budget.
func waitForCalls(t *testing.T, calls func() int32, want int32) {
	t.Helper()
	awaitCond(t, func() bool { return calls() >= want }, fmt.Sprintf("order dispatch reaching %d calls", want))
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

// Every tick that used to dispatch orders now wakes the lane instead. A lane
// that has idled at least as long as its last pass ran — here, one that has
// not passed yet — runs the wake's pass at once, so a poke-driven tick still
// gets its orders evaluated promptly rather than at the lane's next
// patrol-interval pass.
func TestOrdersLaneWakesOnTick(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}
	startOrdersLaneForTest(t, cr)

	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "poke")

	waitForCalls(t, func() int32 { return od.calls.Load() }, 1)
	// With a 1h backstop the lane cannot pass on its own inside this test, so
	// the pass must be the tick's wake.
	lane := cr.ordersLaneOf()
	if cadence, wakes := lane.cadencePasses.Load(), lane.wakePasses.Load(); cadence != 0 || wakes < 1 {
		t.Fatalf("lane passes: cadence=%d wake=%d, want cadence=0 wake>=1", cadence, wakes)
	}
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
	awaitClose(t, done, "the orders lane exiting after cancellation")
	// The lane goroutine is the only consumer of wakes; once it has exited a
	// wake stays buffered and no further pass can run.
	after := od.calls.Load()
	lane := cr.ordersLaneOf()
	lane.wake()
	if pending := len(lane.wakeCh); pending != 1 {
		t.Fatalf("buffered wakes after lane stop = %d, want 1 (nothing consumes them)", pending)
	}
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

// The tick trace reports the lane's last-pass age so a stuck lane is visible.
// A lane that has never finished a pass reports no age at all, and a finished
// pass latches its time and trigger.
func TestOrdersLaneLastPassFeedsTheTickTrace(t *testing.T) {
	cr := ordersLaneTestRuntime(t, &recordingOrderDispatcher{}, "1h", nil)
	lane := cr.ordersLaneOf()
	if _, _, ran := lane.lastPass(); ran {
		t.Fatal("a lane that never passed reports a pass")
	}
	before := time.Now()
	cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonWake)
	at, reason, ran := lane.lastPass()
	if !ran || reason != ordersLaneReasonWake || at.Before(before) {
		t.Fatalf("lastPass = (%s, %q, %t), want the wake pass just run", at, reason, ran)
	}
	fields := map[string]any{}
	addBackstopAgeFields(fields, at, reason, ran)
	if fields["backstop_ran"] != true || fields["backstop_last_reason"] != ordersLaneReasonWake {
		t.Fatalf("tick fields = %v, want the lane's last pass", fields)
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

// M1: a lane rescan that started before a reload must not overwrite the
// reload's newer dispatcher or revert the order set when it finishes. The scan
// seam parks the lane's rescan mid-scan while a reload stages its result.
func TestOrdersLaneStaleRescanDoesNotOverwriteNewerReload(t *testing.T) {
	oldOD := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, oldOD, "1h", nil)
	cr.orderRescanEnabled = true
	cr.tomlPath = filepath.Join(cr.cityPath, "city.toml")
	cr.orderSetSignature = "sig-boot"

	scanEntered := make(chan struct{})
	scanRelease := make(chan struct{})
	var once, releaseOnce sync.Once
	releaseScan := func() { releaseOnce.Do(func() { close(scanRelease) }) }
	cr.orderSetScan = func(string, *config.City, string) (orderSetSnapshot, error) {
		once.Do(func() { close(scanEntered) })
		<-scanRelease
		// What the lane read before the reload landed: a different, older set.
		return orderSetSnapshot{Signature: "sig-stale"}, nil
	}

	passDone := make(chan struct{})
	go func() {
		defer close(passDone)
		cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)
	}()
	// Release the parked scan even if a wait below fails, so the pass never
	// outlives the test.
	t.Cleanup(func() {
		releaseScan()
		awaitClose(t, passDone, "the parked rescan pass finishing")
	})
	awaitClose(t, scanEntered, "the lane pass starting its rescan")

	reloadOD := &recordingOrderDispatcher{}
	reloadSet := []orders.Order{{Name: "from-reload", Trigger: "cooldown", Interval: "1h", Exec: "true"}}
	cr.stageOrderDispatcher(reloadOD, reloadSet, "sig-reload", time.Now())
	cr.tryInstallPendingOrderDispatcher(context.Background())

	releaseScan()
	awaitClose(t, passDone, "the released rescan pass finishing")

	if cr.od != orderDispatcher(reloadOD) {
		t.Fatalf("cr.od = %#v, want the reload's dispatcher; a stale rescan overwrote it", cr.od)
	}
	if got := reloadOD.calls.Load(); got != 1 {
		t.Fatalf("reload dispatcher calls = %d, want 1 (installed before this pass dispatched)", got)
	}
	if cr.orderSetSignature != "sig-reload" || len(cr.orderSet) != 1 || cr.orderSet[0].Name != "from-reload" {
		t.Fatalf("order set = %q %#v, want the reload's set; the stale rescan reverted it", cr.orderSetSignature, cr.orderSet)
	}
}

// A pass's config is never read before its order-set generation. The hook
// lands a reload that has just published its config and staged (bumping the
// generation) inside orderPassConfig's critical section, before either read:
// the pass must read the reloaded config with the bumped generation, so its
// rescan stages. A config read before the generation would rescan the old
// config under the new generation and stage it over the reload. The test does
// not prove the two reads are atomic against every interleaving; it pins that
// neither read escapes the section the hook runs in.
func TestOrdersLanePassConfigIsNeverReadBeforeGeneration(t *testing.T) {
	cr := ordersLaneTestRuntime(t, &recordingOrderDispatcher{}, "1h", nil)
	cr.orderRescanEnabled = true
	cr.tomlPath = filepath.Join(cr.cityPath, "city.toml")
	cr.orderSetSignature = "sig-boot"
	cr.cfg.Daemon.ShutdownTimeout = "5s"
	var scanned []string
	cr.orderSetScan = func(_ string, cfg *config.City, _ string) (orderSetSnapshot, error) {
		scanned = append(scanned, cfg.Daemon.ShutdownTimeout)
		return orderSetSnapshot{Signature: "sig-" + cfg.Daemon.ShutdownTimeout}, nil
	}
	var once sync.Once
	cr.inOrderPassConfig = func(lane *ordersLane) {
		once.Do(func() {
			reloaded := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Daemon:    config.DaemonConfig{PatrolInterval: "1h", ShutdownTimeout: "6s"},
			}
			cr.serviceStateMu.Lock()
			cr.cfg = reloaded
			cr.serviceStateMu.Unlock()
			lane.generation++ // the reload's stage; setMu is held here
		})
	}

	cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)

	if len(scanned) != 1 || scanned[0] != "6s" {
		t.Fatalf("rescan read shutdown_timeout %v, want [6s]: the config was read outside the generation pair", scanned)
	}
	if cr.orderSetSignature != "sig-6s" {
		t.Fatalf("order set signature = %q, want %q: a rescan of the pair's config must stage", cr.orderSetSignature, "sig-6s")
	}
}

// A lane rescan that runs after a real reload has staged its dispatcher must
// read the reload's config, not the pre-reload one. The reload publishes its
// config before it stages, so the one generation bump at stage covers a
// rescan that captures the generation afterwards. The hook runs a lane pass in
// exactly that window, with orderRescanLast reset so the rescan is due.
func TestOrdersLaneRescanAfterReloadStageReadsReloadedConfig(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "100")
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeSoftReloadConfig(t, tomlPath, "5s")
	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: configRev,
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	var scannedTimeouts []string
	cr.orderSetScan = func(cityRoot string, cfg *config.City, cmdName string) (orderSetSnapshot, error) {
		scannedTimeouts = append(scannedTimeouts, cfg.Daemon.ShutdownTimeout)
		if cfg.Daemon.ShutdownTimeout != "6s" {
			return orderSetSnapshot{Signature: "sig-stale"}, nil
		}
		return scanOrderSetSnapshotFS(fsys.OSFS{}, cityRoot, cfg, io.Discard, cmdName)
	}
	cr.afterReloadStagesOrders = func() {
		cr.markOrderRescan(time.Time{})
		cr.runOrdersLanePass(context.Background(), cityPath, ordersLaneReasonCadence)
	}

	writeCityRuntimeSoftReloadConfig(t, tomlPath, "6s")
	lastProviderName := "fake"
	cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceWatch)

	if len(scannedTimeouts) != 1 {
		t.Fatalf("lane rescans in the post-stage window = %v, want exactly one", scannedTimeouts)
	}
	if scannedTimeouts[0] != "6s" {
		t.Fatalf("lane rescan read shutdown_timeout=%q, want the reloaded \"6s\": config must be published before the reload stages", scannedTimeouts[0])
	}
	if cr.orderSetSignature == "sig-stale" {
		t.Fatal("a rescan of the pre-reload config staged over the reload's dispatcher")
	}
}

// The real lane running concurrently with real config reloads, for the race
// detector: reloads stage dispatchers and swap config while lane passes rescan,
// install and dispatch.
func TestOrdersLaneConcurrentWithReloadConfigTraced(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "100")
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeSoftReloadConfig(t, tomlPath, "5s")
	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)
	// The backstop runs a pass a patrol interval after the last one; a 1ms
	// interval keeps passes flowing through the reloads rather than one per
	// 30s default.
	cfg.Daemon.PatrolInterval = "1ms"
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: configRev,
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	lane := cr.ordersLaneOf()
	startOrdersLaneForTest(t, cr)

	// Wake a rescanning pass before each reload and again right after each
	// stage, so passes overlap both the reload's preparation and the rest of
	// the reload after it stages.
	wakeRescan := func() {
		cr.markOrderRescan(time.Time{}) // make the pass rescan
		lane.wake()
	}
	cr.afterReloadStagesOrders = wakeRescan

	lastProviderName := "fake"
	for i := 0; i < 6; i++ {
		timeout := "5s"
		if i%2 == 0 {
			timeout = "6s"
		}
		writeCityRuntimeSoftReloadConfig(t, tomlPath, timeout)
		wakeRescan()
		cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceWatch)
	}
	awaitCond(t, func() bool { return lane.wakePasses.Load()+lane.cadencePasses.Load() > 1 }, "orders lane passes during the reloads")
}
