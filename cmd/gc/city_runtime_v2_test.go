package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The P2-8 wiring tests: the v2 runtime behind the exclusive switch. Unit
// tests run a phase fixture (newPhaseFixtureRuntime) made a v2 controller
// the way newCityRuntime makes one, with recording controllers; run-level
// tests drive run() itself on a fake provider, waiting with awaitClose and
// awaitCond, never sleeping.

// attachTestV2 makes cr a v2 controller exactly as newCityRuntime does for
// one that latched v2 (installPlanner, then the wiring's wake, which marks
// the planner dirty).
func attachTestV2(t *testing.T, cr *CityRuntime) *plannerRuntime {
	t.Helper()
	rt := newDefaultPlanner(cr.stderr)
	cr.reconcilerDrift.running = reconcilerV2
	cr.sessionDrains, cr.providerHealthGate = nil, nil
	cr.installPlanner(rt)
	wake := newLegacyWake(cr.pokeCh, cr.controlDispatcherCh)
	wake.planner = rt.planner
	cr.initWake(wake)
	if cr.cs != nil {
		wireControllerWakeSignals(cr.cs, cr.wake)
	}
	t.Cleanup(rt.stop)
	return rt
}

// primeTestV2 gives cr's controller state what the boot gate waits on
// besides a primed inventory: a primed cache over its city store, as a
// controller's is (CONTRACT v5 P2).
func primeTestV2(t *testing.T, cr *CityRuntime) {
	t.Helper()
	cr.cs.cityBeadStore = primeTestCache(t, cr.cs.cityBeadStore)
}

func primeTestCache(t *testing.T, store beads.Store) *beads.CachingStore {
	t.Helper()
	cache := beads.NewCachingStoreForTest(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	return cache
}

// newTestPlanner is a planner whose passes do nothing, for the wake's tests.
func newTestPlanner() *planner {
	return newPlanner(realPlannerClock{}, func() time.Duration { return time.Hour }, func(time.Time) passResult { return passResult{} }, newInflightMap(), nil, io.Discard)
}

// plannerWakes is how many times p was marked dirty for reason.
func plannerWakes(p *planner, reason string) uint64 {
	return p.metrics.snapshot(time.Now()).Wakes[reason]
}

// plannerPasses is how many passes p has run.
func plannerPasses(p *planner) uint64 {
	return p.metrics.snapshot(time.Now()).Passes
}

// newTestV2Wiring is newControllerWiring for a controller admitted to v2 by
// the developer override.
func newTestV2Wiring(t *testing.T, cfg *config.City, stderr io.Writer) *controllerWiring {
	t.Helper()
	latch := *cfg
	latch.Daemon.SessionReconciler = "v2"
	w, err := newControllerWiring(&latch, overrideEnv("1"), stderr)
	if err != nil {
		t.Fatalf("newControllerWiring: %v", err)
	}
	return w
}

func bootTestV2(t *testing.T, cr *CityRuntime) {
	t.Helper()
	primeTestV2(t, cr)
	if !cr.bootV2(context.Background()) {
		t.Fatal("bootV2 did not reach ready")
	}
}

// assertV2BootRefuses adds rows to a v2 phase fixture, whose own row is
// clean, and boots it through the city runtime's host. Boot must refuse at
// once, with want and the doctor command in the error, after a live read of
// the sessions leg (C4.5 item 1b), and before anything runs: the planner
// never starts, nothing is written, and the fixture's running session is not
// stopped.
func assertV2BootRefuses(t *testing.T, want string, rows ...beads.Bead) {
	t.Helper()
	cr, store := newPhaseFixtureRuntime(t, false, true)
	stderr := &lockedBuffer{}
	cr.stderr = stderr
	rt := attachTestV2(t, cr)
	for _, row := range rows {
		if _, err := store.Store.Create(row); err != nil {
			t.Fatalf("Create(%s): %v", row.ID, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if cr.bootV2(ctx) {
		t.Fatal("bootV2 reached ready over enterprise-era rows")
	}
	if ctx.Err() != nil {
		t.Fatal("bootV2 retried the refusal until the deadline")
	}
	if got := stderr.String(); !strings.Contains(got, want) || !strings.Contains(got, "run gc doctor --check v2-session-migration to list them") {
		t.Errorf("stderr = %q, want %q and the doctor command", got, want)
	}
	liveRead := false
	for _, op := range store.recorded() {
		liveRead = liveRead || (strings.HasPrefix(op, "List ") && strings.Contains(op, "live=true"))
		if !strings.HasPrefix(op, "List") && !strings.HasPrefix(op, "Get") {
			t.Errorf("store op %q during a refused boot", op)
		}
	}
	if !liveRead {
		t.Errorf("store ops = %q, want a live census read", store.recorded())
	}
	rt.mu.Lock()
	started := rt.started
	rt.mu.Unlock()
	if started || plannerPasses(rt.planner) != 0 {
		t.Errorf("started=%v passes=%d: a refused boot started the planner", started, plannerPasses(rt.planner))
	}
	if !cr.sp.IsRunning("worker") {
		t.Error("a refused boot stopped the running session")
	}
}

// Kills: the C11 boot preflight dropped, retried like a failed read, read
// from the cache, or left unwired from the city's host; and a drain-ack
// stop-pending row refused.
func TestV2BootRefusesUnknownStateRows(t *testing.T) {
	assertV2BootRefuses(t, `enterprise-era session rows: 2 open row(s) in a state main does not know ("archived"=1 "draining"=1)`,
		sessionRow("gc-a", "template", "worker", "state", "archived", "session_name", "a"),
		sessionRow("gc-b", "template", "worker", "state", "draining", "session_name", "b"),
		sessionRow("gc-c", "template", "worker", "state", "draining", "state_reason", session.DrainAckStopPendingReason, "session_name", "c"))
}

// Kills: shared slot-scoped names (P3 spec F8) left to the decide instead of
// refused at boot.
func TestV2BootRefusesSharedSlotNames(t *testing.T) {
	assertV2BootRefuses(t, "0 open row(s) in a state main does not know, 1 pool-slot session name(s) shared by open rows, 0 configured named",
		poolRow("gc-a", "worker", 2, "creating", "session_name", "worker-2-pool"),
		poolRow("gc-b", "worker", 2, "creating", "session_name", "worker-2-pool"))
}

// Kills: the legacy startup block (corpse cleanup, stale reap, desired state,
// sync, the boot beadReconcileTick) still run under v2, or the v2 boot left
// out of the startup step; or the boot env published without the boot
// config's revision (m27). The step runs the maintenance phases, writes
// nothing, never builds desired state, enters no guarded legacy path, and
// boots the planner, whose first pass completes before the step returns.
func TestCityRuntimeV2StartupSkipsLegacyBootReconcile(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, true)
	rt := attachTestV2(t, cr)
	primeTestV2(t, cr)
	var builds atomic.Int32
	cr.buildFn = func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
		builds.Add(1)
		return DesiredStateResult{}
	}

	if !cr.startupReconcile(context.Background()) {
		t.Fatal("the v2 startup step did not complete")
	}
	if n := cr.legacySessionEntries.Load(); n != 0 {
		t.Errorf("legacySessionEntries = %d, want 0: the v2 startup step entered a legacy session phase", n)
	}
	if n := builds.Load(); n != 0 {
		t.Errorf("desired state built %d time(s) under v2", n)
	}
	if writes := storeWrites(store.recorded()); len(writes) != 0 {
		t.Errorf("the v2 startup step wrote to the store: %v", writes)
	}
	if rec := rt.planner.out.record.Load(); rec == nil || rec.Err != "" || rt.bootState() != plannerBootReady {
		t.Errorf("pass record = %+v boot = %s, want a completed pass before the step returned", rec, rt.bootState())
	}
	if env := rt.env.Load(); env.ConfigRev == "" || env.ConfigRev != cr.configRev {
		t.Errorf("boot env revision = %q, want the boot config's %q", env.ConfigRev, cr.configRev)
	}
}

// v2RunFixture is a v2 city runtime driven through run() on a fake provider.
type v2RunFixture struct {
	cr       *CityRuntime
	rt       *plannerRuntime
	store    *beads.MemStore
	sp       *runtime.Fake
	stderr   *synchronizedBuffer
	cityPath string
	tomlPath string
	ready    chan struct{}
	done     chan struct{}
	cancel   context.CancelFunc
}

// newV2RunFixture builds a v2 city whose [daemon] section is daemon, on a
// primed cache over its store. setup, when set, runs before run() starts.
func newV2RunFixture(t *testing.T, daemon string, setup func(f *v2RunFixture)) *v2RunFixture {
	t.Helper()
	f := &v2RunFixture{stderr: &synchronizedBuffer{}, ready: make(chan struct{}), done: make(chan struct{})}
	f.cityPath = t.TempDir()
	f.tomlPath = filepath.Join(f.cityPath, "city.toml")
	writeCityRuntimeConfig(t, f.tomlPath, "fake")
	appendToFile(t, f.tomlPath, "\n[[agent]]\nname = \"worker\"\n\n[daemon]\n"+daemon)
	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, f.tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	f.sp = runtime.NewFake()
	f.store = beads.NewMemStore()
	wiring := newTestV2Wiring(t, cfg, f.stderr)
	f.rt = wiring.v2
	var readyOnce sync.Once
	f.cr = newTestCityRuntime(t, wiring.runtimeParams(CityRuntimeParams{
		CityPath:  f.cityPath,
		CityName:  "test-city",
		TomlPath:  f.tomlPath,
		ConfigRev: config.Revision(fsys.OSFS{}, prov, cfg, f.cityPath),
		Cfg:       cfg,
		SP:        f.sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:      newDrainOps(f.sp),
		Rec:       events.Discard,
		OnStarted: func() { readyOnce.Do(func() { close(f.ready) }) },
		Stdout:    io.Discard,
		Stderr:    f.stderr,
	}))
	cs := newControllerState(context.Background(), cfg, f.sp, events.NewFake(), "test-city", f.cityPath)
	cs.cityBeadStore = primeTestCache(t, f.store)
	wireControllerWakeSignals(cs, f.cr.wakeOf())
	cs.configDirty = f.cr.configDirty
	f.cr.setControllerState(cs)
	if setup != nil {
		setup(f)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() {
		defer close(f.done)
		f.cr.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		awaitClose(t, f.done, "run returning after cancel")
	})
	return f
}

func appendToFile(t *testing.T, path, text string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(data, text...), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// reload asks the running controller for a manual reload and returns its
// final reply. A reload replies just before it clears the active-reload slot,
// so it first waits for the previous one's slot to clear; a request that
// lands on a held slot is answered busy and never replied to.
func (f *v2RunFixture) reload(t *testing.T) reloadControlReply {
	t.Helper()
	awaitCond(t, func() bool {
		f.cr.reloadMu.Lock()
		defer f.cr.reloadMu.Unlock()
		return f.cr.activeReload == nil
	}, "the previous reload clearing its slot")
	req := reloadRequest{acceptedCh: make(chan reloadControlReply, 1), doneCh: make(chan reloadControlReply, 1)}
	select {
	case f.cr.reloadReqCh <- req:
	case <-time.After(hangBudget):
		t.Fatalf("the controller did not take the reload request within %s", hangBudget)
	}
	var accepted, reply reloadControlReply
	got := make(chan struct{})
	go func() {
		defer close(got)
		if accepted = <-req.acceptedCh; accepted.Outcome == reloadOutcomeAccepted {
			reply = <-req.doneCh
		}
	}()
	awaitClose(t, got, "the manual reload reply")
	if accepted.Outcome != reloadOutcomeAccepted {
		t.Fatalf("reload request answered %+v, want accepted", accepted)
	}
	return reply
}

// Kills: the planner booted before the adoption barrier or before the
// startup reload (MAINT-006, MAINT-007). At readiness the adopted row exists,
// and boot published the reloaded config's revision as env Gen 1.
func TestCityRuntimeV2AdoptionAndStartupReloadPrecedeBootEnqueue(t *testing.T) {
	var reloadedRev, runtimeName string
	f := newV2RunFixture(t, "patrol_interval = \"1h\"\n", func(f *v2RunFixture) {
		runtimeName = agent.SessionNameFor("test-city", "worker", f.cr.cfg.Workspace.SessionTemplate)
		if err := f.sp.Start(context.Background(), runtimeName, runtime.Config{Command: "run"}); err != nil {
			t.Fatalf("Start(%s): %v", runtimeName, err)
		}
		// A config edit made while the controller was down: the startup
		// reload applies it before the startup step.
		appendToFile(t, f.tomlPath, "\n[[agent]]\nname = \"fresh\"\n")
		cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, f.tomlPath)
		if err != nil {
			t.Fatalf("reload config: %v", err)
		}
		reloadedRev = config.Revision(fsys.OSFS{}, prov, cfg, f.cityPath)
	})
	// Stop the session before run's shutdown would wait out its graceful
	// stop budget on it.
	t.Cleanup(func() { _ = f.sp.Stop(runtimeName) })
	awaitClose(t, f.ready, "readiness")

	adopted, err := f.store.List(beads.ListQuery{Type: sessionBeadType})
	if err != nil || len(adopted) != 1 {
		t.Fatalf("adopted session beads = %v (err %v), want one for %s", adopted, err, runtimeName)
	}
	if env := f.rt.env.Load(); env.Gen != 1 || env.ConfigRev != reloadedRev {
		t.Errorf("boot env = gen %d rev %q, want gen 1 rev %q (the reloaded config)", env.Gen, env.ConfigRev, reloadedRev)
	}
}

// Kills: the control-dispatcher arm left live under v2 (MAINT-056, API-014):
// a stray signal would run the legacy controlDispatcherTick. Under v2 the arm
// selects on nil, and the socket's control-dispatch key marks the planner
// dirty without touching the legacy signal.
func TestCityRuntimeV2ControlDispatcherSignalNeverRunsLegacyPath(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	if got := cr.controlDispatcherSignal(); got == nil {
		t.Fatal("a legacy controller's control-dispatcher arm selects on nil")
	}
	rt := attachTestV2(t, cr)
	if got := cr.controlDispatcherSignal(); got != nil {
		t.Fatal("the control-dispatcher arm is live under v2")
	}

	cr.wakeOf().Enqueue(wakeReasonSocket, reconcilekey.ControlDispatch())
	if n := plannerWakes(rt.planner, wakeReasonSocket); n != 1 {
		t.Errorf("socket wakes = %d, want 1", n)
	}
	if len(cr.controlDispatcherCh) != 0 || len(cr.pokeCh) != 0 {
		t.Errorf("legacy signals (poke, dispatch) = (%d, %d), want none", len(cr.pokeCh), len(cr.controlDispatcherCh))
	}
	if n := cr.legacySessionEntries.Load(); n != 0 {
		t.Errorf("legacySessionEntries = %d, want 0", n)
	}
}

// Kills: keys still folded onto pokeCh under v2 (API-006..010). An API
// session key marks the planner dirty; the tick is not poked.
func TestCityRuntimeV2KeyedEnqueueMarksPlannerNotTick(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	rt := attachTestV2(t, cr)

	cr.cs.Enqueue(reconcilekey.Session("gc-1"))
	if n := plannerWakes(rt.planner, wakeReasonAPI); n != 1 {
		t.Errorf("api wakes = %d, want 1", n)
	}
	if len(cr.pokeCh) != 0 || len(cr.controlDispatcherCh) != 0 {
		t.Errorf("legacy signals (poke, dispatch) = (%d, %d), want none: the key reached the tick", len(cr.pokeCh), len(cr.controlDispatcherCh))
	}
}

// Kills: a config mutation that only marks the planner under v2, so the
// reload waits for the patrol (API-003, API-017), or a tick reload that
// publishes no env (CONTRACT v5 P7). The mutation pokes the maintenance tick,
// whose reload publishes the next env and marks the planner dirty.
func TestCityRuntimeV2ConfigMutationPublishesEnvThenMarksPlanner(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	rt := attachTestV2(t, cr)
	cr.cs.configDirty = cr.configDirty
	oldRev := rt.publishEnv().ConfigRev

	if err := cr.cs.mutateAndPoke(func() error {
		writePhaseFixtureConfig(t, cr.tomlPath, true)
		return nil
	}); err != nil {
		t.Fatalf("mutateAndPoke: %v", err)
	}
	if !drainSignal(cr.pokeCh) {
		t.Fatal("the config mutation did not wake the maintenance tick")
	}
	runFixtureTick(cr, "poke")

	env := rt.env.Load()
	if env.Gen != 2 || env.ConfigRev == oldRev {
		t.Fatalf("env after the reload = gen %d rev %q, want gen 2 with a new revision (old %q)", env.Gen, env.ConfigRev, oldRev)
	}
	if n := plannerWakes(rt.planner, "reload"); n != 1 {
		t.Errorf("reload wakes = %d, want 1", n)
	}
}

// Kills: the manual reload's reply left to the end of the tick, or never
// sent (MAINT-023); the reply amended before the planner's env is published,
// so a client is told what applied while the next pass reads the old config;
// or a hard reload told drift acceptance is unavailable (mhook2). The
// reply's amend sees env Gen 2, and by the next phase that reads the store
// the reply is delivered.
func TestCityRuntimeV2ReloadReplyAfterPublish(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, true, false)
	cr.activeReload.soft = false
	rt := attachTestV2(t, cr)
	rt.publishEnv()
	doneCh := cr.activeReload.doneCh
	var amendGen uint64
	rt.softReload = func(intent reloadIntent, r *reloadControlReply) {
		amendGen = rt.env.Load().Gen
		v2SoftReloadUnavailable(intent, r)
	}

	var mu sync.Mutex
	var firstRead *struct{ replied bool }
	store.onRecord = func(op string) {
		if !strings.HasPrefix(op, "List ") {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if firstRead == nil {
			firstRead = &struct{ replied bool }{replied: len(doneCh) == 1}
		}
	}
	runFixtureTick(cr, "reload")

	mu.Lock()
	defer mu.Unlock()
	if firstRead == nil {
		t.Fatal("the tick read nothing after the reload")
	}
	if !firstRead.replied {
		t.Error("the manual reply was not delivered before the phases after the reload")
	}
	if amendGen != 2 {
		t.Errorf("env gen when the reply was amended = %d, want 2", amendGen)
	}
	reply := <-doneCh
	if reply.Outcome != reloadOutcomeApplied {
		t.Errorf("reply = %+v, want applied", reply)
	}
	if slices.Contains(reply.Warnings, v2SoftReloadUnavailableWarning) {
		t.Errorf("a hard reload's reply carries the soft-reload notice: %q", reply.Warnings)
	}
}

// Kills: the provider event pump still poking the tick under v2 (MAINT-015,
// API-018), or a burst logging a line per event. Every session event marks
// the planner dirty; a burst reports one landed wake.
func TestCityRuntimeV2ProviderEventMarksPlanner(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	rt := attachTestV2(t, cr)
	now := time.Unix(1_800_000_000, 0)
	cr.wakeOf().now = func() time.Time { return now }
	var stderr bytes.Buffer
	pump := newSessionEventPump(context.Background(), cr.wakeOf(), &stderr, "gc test")

	for range 3 {
		pump.poke("died", "worker")
	}
	awaitCond(t, func() bool { return plannerWakes(rt.planner, wakeReasonProviderEvent) == 3 }, "three provider-event wakes")
	if len(cr.pokeCh) != 0 {
		t.Error("the provider event poked the tick under v2")
	}
	if n := strings.Count(stderr.String(), "reconcile poke"); n != 1 {
		t.Errorf("the burst logged %d wake lines, want 1:\n%s", n, stderr.String())
	}
}

// Kills: the inventory pass hook never installed under v2, or one that marks
// nothing. A runtime that goes away marks the planner dirty.
func TestCityRuntimeV2InventoryFlipMarksPlanner(t *testing.T) {
	sp := newScriptedInventoryProvider("ghost-runtime")
	sp.env["ghost-runtime"] = map[string]string{"GC_SESSION_ID": "gc-owner"}
	cr := inventoryLaneTestRuntime(t, sp, nil)
	rt := attachTestV2(t, cr)
	rt.publishEnv()
	if !rt.start(context.Background()) {
		t.Fatal("the planner did not start")
	}

	runTestInventoryPass(cr) // listed, attributed
	listed := plannerWakes(rt.planner, "inventory")
	sp.mu.Lock()
	sp.names = nil
	sp.mu.Unlock()
	runTestInventoryPass(cr) // gone
	if n := plannerWakes(rt.planner, "inventory"); n != listed+1 {
		t.Errorf("inventory wakes = %d after the runtime went away, want %d", n, listed+1)
	}
}

// Kills: the planner started on a city with no bead store (MAINT-003). The
// boot step logs that the planner is disabled and lets readiness proceed
// without reading a census.
func TestCityRuntimeV2NoStoreDisablesPlanner(t *testing.T) {
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityName:  "test-city",
		cfg:       &config.City{Workspace: config.Workspace{Name: "test-city"}},
		logPrefix: "gc test",
		stdout:    io.Discard,
		stderr:    &stderr,
	}
	rt := attachTestV2(t, cr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a boot that tried would fail at once instead of retrying
	if !cr.bootV2(ctx) {
		t.Fatal("a store-less v2 city did not proceed to readiness")
	}
	rt.mu.Lock()
	started := rt.started
	rt.mu.Unlock()
	if started {
		t.Error("the planner started with no bead store")
	}
	if !strings.Contains(stderr.String(), "no bead store; the planner is disabled") {
		t.Errorf("stderr = %q, want the disabled-planner notice", stderr.String())
	}
}

// Kills: tick_debounce honored under v2 (a poke waits out the debounce), or
// warned about on every tick (MAINT-017). With an hour of debounce and an
// hour of patrol, two manual reloads still reply at once, and the warning
// appears once.
func TestCityRuntimeV2TickDebounceIgnoredWithOneWarning(t *testing.T) {
	f := newV2RunFixture(t, "patrol_interval = \"1h\"\ntick_debounce = \"1h\"\n", nil)
	awaitClose(t, f.ready, "readiness")
	for i := range 2 {
		if reply := f.reload(t); reply.Outcome != reloadOutcomeNoChange {
			t.Fatalf("reload %d reply = %+v, want no change", i, reply)
		}
	}
	if n := strings.Count(f.stderr.String(), "tick_debounce is ignored"); n != 1 {
		t.Errorf("tick_debounce warnings = %d, want 1:\n%s", n, f.stderr.String())
	}
}

// panicOnLabelStore panics on a List of label, as a maintenance phase's
// broken read would.
type panicOnLabelStore struct {
	beads.Store
	label string
}

func (s *panicOnLabelStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Label == s.label {
		panic("maintenance read exploded")
	}
	return s.Store.List(q)
}

// Kills: a panic in the maintenance tick reaching the planner. The tick's
// safeTick swallows it, and the planner keeps running passes.
func TestCityRuntimeV2PanicInMaintenanceTickLeavesPlannerRunning(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, true)
	rt := attachTestV2(t, cr)
	bootTestV2(t, cr)
	cr.cs.cityBeadStore = &panicOnLabelStore{Store: store, label: "gc:extmsg-binding"}

	lastProviderName := "fake"
	var prevPoolRunning map[string]bool
	if !cr.safeTick(func() {
		cr.tick(context.Background(), cr.configDirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
	}, "patrol") {
		t.Fatal("the maintenance tick did not panic; the test needs it to")
	}
	passes := plannerPasses(rt.planner)
	cr.cs.Enqueue(reconcilekey.Session("gc-1"))
	awaitCond(t, func() bool { return plannerPasses(rt.planner) > passes }, "a pass after the panic")
	rt.mu.Lock()
	stopped := rt.stopped
	rt.mu.Unlock()
	if stopped {
		t.Error("the maintenance panic stopped the planner")
	}
}

// Kills: a soft reload under v2 answered as if drift acceptance ran (a
// silent zero), or not answered (MAINT-025 is off until C7c). The reply
// applies and says acceptance is unavailable; no session row is rewritten.
func TestCityRuntimeV2SoftReloadRepliesUnavailable(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, true, false)
	attachTestV2(t, cr)
	doneCh := cr.activeReload.doneCh

	runFixtureTick(cr, "reload")
	reply := <-doneCh
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("soft reload reply = %+v, want applied", reply)
	}
	if reply.AcceptedDriftCount != nil {
		t.Errorf("AcceptedDriftCount = %d, want unset: v2 accepted no drift", *reply.AcceptedDriftCount)
	}
	if !slices.Contains(reply.Warnings, v2SoftReloadUnavailableWarning) {
		t.Errorf("warnings = %q, want the unavailable notice", reply.Warnings)
	}
	if writes := storeWrites(store.recorded()); len(writes) != 0 {
		t.Errorf("the v2 soft reload wrote to the store: %v", writes)
	}
}

// Kills: any v2 construction under legacy. A legacy wiring, even with the
// developer override set, builds no planner, prints no banner, and a legacy
// city runtime installs no inventory hook.
func TestLegacyRuntimeConstructsNoV2State(t *testing.T) {
	var stderr bytes.Buffer
	w, err := newControllerWiring(&config.City{}, overrideEnv("1"), &stderr)
	if err != nil {
		t.Fatalf("newControllerWiring: %v", err)
	}
	if w.v2 != nil || w.wake.planner != nil || w.mode != reconcilerLegacy {
		t.Errorf("legacy wiring = mode %v v2 %v planner %v, want legacy with neither", w.mode, w.v2, w.wake.planner)
	}
	if stderr.Len() != 0 {
		t.Errorf("legacy wiring printed %q", stderr.String())
	}

	cr, _ := newPhaseFixtureRuntime(t, false, true)
	runFixtureTick(cr, "patrol")
	if cr.v2 != nil || cr.wakeOf().planner != nil {
		t.Errorf("legacy city runtime: v2 %v planner %v, want neither", cr.v2, cr.wakeOf().planner)
	}
	if cr.inventoryLane.passHook.Load() != nil {
		t.Error("a legacy city runtime installed an inventory pass hook")
	}
	if cr.sessionDrains == nil {
		t.Error("the legacy drain tracker is gone")
	}
}

func overrideEnv(value string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if key == v2SkeletonEnv {
			return value, true
		}
		return "", false
	}
}

// Kills: the developer override honored when absent or not exactly "1", or
// the refusal skipped (OQ-1); and the planner missing from the wiring. An
// admitted v2 wiring carries an unstarted planner that is already on the
// wake, and announces it trace-only.
func TestNewControllerWiringV2AdmissionGate(t *testing.T) {
	v2 := &config.City{Workspace: config.Workspace{Name: "gate"}, Daemon: config.DaemonConfig{SessionReconciler: "v2"}}
	for _, tc := range []struct {
		name  string
		env   func(string) (string, bool)
		admit bool
	}{
		{name: "nil lookup"},
		{name: "absent", env: func(string) (string, bool) { return "", false }},
		{name: "zero", env: overrideEnv("0")},
		{name: "true", env: overrideEnv("true")},
		{name: "padded", env: overrideEnv(" 1")},
		{name: "one", env: overrideEnv("1"), admit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			w, err := newControllerWiring(v2, tc.env, &stderr)
			if !tc.admit {
				if err == nil || !strings.Contains(err.Error(), "not available in this build") {
					t.Fatalf("wiring err = %v, want the v2 refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("wiring err = %v, want v2 admitted", err)
			}
			if w.mode != reconcilerV2 || w.v2 == nil || w.wake.planner != w.v2.planner {
				t.Fatalf("wiring = mode %v v2 %v, want v2 with its planner on the wake", w.mode, w.v2)
			}
			if w.v2.started {
				t.Error("the wiring started the planner")
			}
			if !strings.Contains(stderr.String(), "session reconciler: v2 (planner: trace-only)") {
				t.Errorf("stderr = %q, want the trace-only banner", stderr.String())
			}
		})
	}
}

// Kills: the planner put on the wake after newControllerWiring returns, so a
// key the socket delivers before the city runtime exists is lost; or the
// city runtime building its own wake or planner instead of the wiring's, so
// its follow-ups, lanes and pump miss the planner.
func TestV2WiringRetainsKeysEnqueuedBeforeTheRuntime(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	cfg := *cr.cfg
	cfg.Daemon.SessionReconciler = "v2"
	w, err := newControllerWiring(&cfg, overrideEnv("1"), io.Discard)
	if err != nil {
		t.Fatalf("newControllerWiring: %v", err)
	}
	// The socket starts before the runtime: this key arrives first.
	w.wake.Enqueue(wakeReasonSocket, reconcilekey.Session("gc-1"))
	if drainSignal(w.pokeCh) {
		t.Fatal("an early socket key reached the legacy tick")
	}

	sp := runtime.NewFake()
	wired := newTestCityRuntime(t, w.runtimeParams(CityRuntimeParams{
		CityPath: cr.cityPath,
		CityName: "test-city",
		Cfg:      &cfg,
		SP:       sp,
		Dops:     newDrainOps(sp),
		Rec:      events.Discard,
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	}))
	t.Cleanup(w.v2.stop)
	if wired.wakeOf() != w.wake || wired.v2 != w.v2 {
		t.Fatal("the city runtime does not use the wiring's wake and planner")
	}
	if got := wired.v2.host.gather.CityPath; got != cr.cityPath {
		t.Errorf("bound host city path = %q, want %q", got, cr.cityPath)
	}
	// The mark waits for the first pass, which boot runs at once.
	if len(w.v2.planner.dirty) != 1 || plannerWakes(w.v2.planner, wakeReasonSocket) != 1 {
		t.Errorf("planner dirty = %d, socket wakes = %d, want the early key's mark pending", len(w.v2.planner.dirty), plannerWakes(w.v2.planner, wakeReasonSocket))
	}
}

// Kills: params whose wake, planner and mode disagree accepted, so the
// planner is woken through a wake the socket and API do not use; or a v2
// controller accepted without its wiring's wake and planner (an entry point
// that forgot them would build a private planner the socket never reaches).
func TestNewCityRuntimeRefusesMismatchedReconcilerWiring(t *testing.T) {
	rt := newDefaultPlanner(io.Discard)
	other := newDefaultPlanner(io.Discard)
	routed := newLegacyWake(nil, nil)
	routed.planner = rt.planner
	for _, tc := range []struct {
		name string
		p    CityRuntimeParams
		ok   bool
	}{
		{name: "legacy bare", ok: true},
		{name: "legacy wake", p: CityRuntimeParams{Wake: newLegacyWake(nil, nil)}, ok: true},
		{name: "v2 wired", p: CityRuntimeParams{ReconcilerMode: reconcilerV2, V2: rt, Wake: routed}, ok: true},
		{name: "v2 bare", p: CityRuntimeParams{ReconcilerMode: reconcilerV2}},
		{name: "v2 planner without a wake", p: CityRuntimeParams{ReconcilerMode: reconcilerV2, V2: rt}},
		{name: "v2 planner under legacy", p: CityRuntimeParams{V2: rt}},
		{name: "planner wake under legacy", p: CityRuntimeParams{Wake: routed}},
		{name: "v2 wake without its planner", p: CityRuntimeParams{ReconcilerMode: reconcilerV2, Wake: routed}},
		{name: "v2 wake to another planner", p: CityRuntimeParams{ReconcilerMode: reconcilerV2, V2: other, Wake: routed}},
		{name: "v2 legacy wake", p: CityRuntimeParams{ReconcilerMode: reconcilerV2, V2: rt, Wake: newLegacyWake(nil, nil)}},
	} {
		if err := checkReconcilerWiring(tc.p); (err == nil) != tc.ok {
			t.Errorf("%s: checkReconcilerWiring = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// Kills: the bead event watcher not reaching the planner, or reaching it
// unfiltered (F7's watcher has no recover, so the planner's half is a pure
// filter and a non-blocking mark). A session row and assigned work mark the
// planner dirty; unrouted, unassigned work does not; the watcher applies all
// three.
func TestBeadEventWatcherMarksPlannerForRelevantEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newTestPlanner()
		evt := func(typ string, b beads.Bead, seq uint64) events.Event {
			e := beadEvent(t, typ, b)
			e.Seq = seq
			return e
		}
		ep := &scriptedEventProvider{watches: []scriptedWatch{{events: []events.Event{
			evt(events.BeadUpdated, routerSessionBead("gc-1", map[string]string{"session_name": "s-1"}), 1),
			evt(events.BeadUpdated, routerWorkBead("w-1", "open", ""), 2),
			evt(events.BeadUpdated, routerWorkBead("w-2", "open", "worker-1"), 3),
		}}}}
		cs := &controllerState{eventProv: ep, beadEventStartSeqOK: true, cityBeadStore: beads.NewMemStore(), wake: &controllerWake{planner: p}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cs.startBeadEventWatcher(ctx)
		synctest.Wait()
		if n := plannerWakes(p, "bead-event"); n != 2 {
			t.Fatalf("bead-event wakes = %d, want 2 (the session row and the assigned work)", n)
		}
	})
}

// Kills: a planner pass run before run owns the city and arms the config
// watcher (MAINT-001); the planner's lifetime tied to the startup step rather
// than run (it would be gone once the city is ready); the planner not stopped
// when run returns; the drain tracker built under v2.
func TestV2Boot_OwnsCityAndArmsWatcherBeforeLanes(t *testing.T) {
	var violations []string
	var mu sync.Mutex
	f := newV2RunFixture(t, "patrol_interval = \"1h\"\n", func(f *v2RunFixture) {
		inner := f.rt.planner.pass
		f.rt.planner.pass = func(now time.Time) passResult {
			f.cr.watchMu.Lock()
			armed := f.cr.watchCleanup != nil
			f.cr.watchMu.Unlock()
			if !f.cr.ownedCity.Load() || !armed {
				mu.Lock()
				violations = append(violations, fmt.Sprintf("pass with ownedCity=%v watcher=%v", f.cr.ownedCity.Load(), armed))
				mu.Unlock()
			}
			return inner(now)
		}
	})
	awaitClose(t, f.ready, "readiness")
	mu.Lock()
	if len(violations) != 0 {
		t.Errorf("planner passes ran before run owned the city: %v", violations)
	}
	mu.Unlock()

	passes := plannerPasses(f.rt.planner)
	f.cr.wakeOf().Enqueue(wakeReasonAPI, reconcilekey.Session("gc-after-ready"))
	awaitCond(t, func() bool { return plannerPasses(f.rt.planner) > passes }, "a pass after readiness")

	f.cancel()
	awaitClose(t, f.done, "run returning")
	f.rt.mu.Lock()
	stopped := f.rt.stopped
	f.rt.mu.Unlock()
	if !stopped {
		t.Error("run returned without stopping the planner")
	}
	if f.cr.sessionDrains != nil {
		t.Error("run built the legacy drain tracker under v2")
	}
}

// Kills: a non-idempotent v2 boot, or a boot panic that stops the city
// instead of retrying (MAINT-005). A boot census that panics once is retried
// after the patrol and the city starts; one that always panics gives up after
// max_restarts.
func TestV2Boot_FirstPassRetriesOnPanicBoundedByMaxRestarts(t *testing.T) {
	panickingCensus := func(f *v2RunFixture, times int) *atomic.Int32 {
		var calls atomic.Int32
		inner := f.rt.host.bootCensus
		f.rt.host.bootCensus = func() (v2SessionMigration, error) {
			if int(calls.Add(1)) <= times {
				panic("census exploded")
			}
			return inner()
		}
		return &calls
	}

	t.Run("retries", func(t *testing.T) {
		var calls *atomic.Int32
		f := newV2RunFixture(t, "patrol_interval = \"10ms\"\nmax_restarts = 3\n", func(f *v2RunFixture) {
			calls = panickingCensus(f, 1)
		})
		awaitClose(t, f.ready, "readiness after the retried boot")
		if !strings.Contains(f.stderr.String(), "census exploded") {
			t.Errorf("stderr = %q, want the recovered boot panic", f.stderr.String())
		}
		if n := calls.Load(); n != 2 {
			t.Errorf("boot census reads = %d, want 2 (one panic, one retry)", n)
		}
	})

	t.Run("bounded", func(t *testing.T) {
		f := newV2RunFixture(t, "patrol_interval = \"10ms\"\nmax_restarts = 2\n", func(f *v2RunFixture) {
			panickingCensus(f, 1<<30)
		})
		awaitClose(t, f.done, "run giving up")
		select {
		case <-f.ready:
			t.Fatal("a city whose boot always panics reported ready")
		default:
		}
		if !strings.Contains(f.stderr.String(), "startup did not complete after 2 attempt(s)") {
			t.Errorf("stderr = %q, want the bounded give-up", f.stderr.String())
		}
	})
}

// Kills: a plannerHost closure reading a CityRuntime field a reload writes
// unlocked (F2). Under -race, each host closure loops on its own goroutine
// (so one closure's locking cannot order another's reads) against a tick that
// applies a new config, with the planner's passes running around it.
func TestV2HostReadsRaceFreeAgainstReload(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, true, true)
	cr.activeReload.soft = false
	rt := attachTestV2(t, cr)
	bootTestV2(t, cr)

	h, g := rt.host, rt.host.gather
	probes := []func(){
		func() { _ = g.Sessions() },
		func() { _ = g.RigStores() },
		func() { _ = g.Observations() },
		func() { _, _ = g.Episodes() },
		func() { _, _ = h.bootCensus() },
		func() { _ = rt.publishEnv() },
		func() { h.beginTrace("race-probe").end(TraceCompletionCompleted, traceRecordPayload{"phase": "probe"}) },
		func() { rt.planner.markDirty("race-probe") },
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, probe := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					probe()
				}
			}
		}()
	}
	runFixtureTick(cr, "reload")
	close(stop)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	awaitClose(t, done, "the host probes")
	if env := rt.env.Load(); env.Gen != 2 {
		t.Errorf("env gen = %d after the reload, want 2", env.Gen)
	}
}

// Kills: a poked v2 tick reading the session snapshot live. Under v2 a poke
// is a maintenance wake; the snapshot feeds maintenance only, so it is the
// cached read.
func TestCityRuntimeV2PokedTickReadsSessionSnapshotFromCache(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, false)
	attachTestV2(t, cr)
	runFixtureTick(cr, "poke")
	for _, op := range store.recorded() {
		if strings.Contains(op, "live=true") {
			t.Errorf("the poked v2 tick read live: %s", op)
		}
	}
}

// Kills: the drain tracker built under v2 on a reload that finds a store
// (the second exclusivity layer), or the tick trace still named a controller
// tick.
func TestCityRuntimeV2ReloadBuildsNoDrainTracker(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, true, false)
	cr.activeReload.soft = false
	attachTestV2(t, cr)
	runFixtureTick(cr, "reload")
	if cr.sessionDrains != nil || cr.providerHealthGate != nil {
		t.Error("a v2 reload built the legacy drain tracker")
	}
}

// Kills: a store-less or feedless city leaving the planner blind: a watcher
// that cannot start (no provider, or an unresolved start cursor) reports a
// gap, which marks the planner dirty.
func TestBeadEventWatcherWithoutFeedReportsGap(t *testing.T) {
	for _, tc := range []struct {
		name string
		ep   events.Provider
	}{
		{name: "no provider"},
		{name: "unresolved cursor", ep: latestSeqFailingProvider{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPlanner()
			cs := &controllerState{wake: &controllerWake{planner: p}}
			if tc.ep != nil {
				cs.eventProv = tc.ep
			}
			cs.startBeadEventWatcher(context.Background())
			if gaps := plannerWakes(p, "event-gap"); gaps != 1 {
				t.Errorf("gaps = %d, want 1", gaps)
			}
		})
	}
}

type latestSeqFailingProvider struct{ events.Provider }

func (latestSeqFailingProvider) LatestSeq() (uint64, error) { return 0, errors.New("log unreadable") }

// Kills: the planner's wake reporting every enqueue landed (a replay burst
// logs a line per event, API-018) or never. It reports landed at most once
// per interval of its clock.
func TestRoutedWakeReportsLandedAtMostOncePerInterval(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	w := &controllerWake{planner: newTestPlanner(), now: func() time.Time { return now }}
	var got []bool
	for _, step := range []time.Duration{0, 0, routedLandedEvery / 2, routedLandedEvery / 2, 0, routedLandedEvery} {
		now = now.Add(step)
		got = append(got, w.Enqueue(wakeReasonProviderEvent, reconcilekey.Session("gc-1")))
	}
	if want := []bool{true, false, false, true, false, true}; !slices.Equal(got, want) {
		t.Errorf("landed = %v, want %v", got, want)
	}
}

// Kills: a bead event under v2 still poking the legacy tick, or not reaching
// the planner, replays included; or the relevance filter dropped, so any work
// event runs a pass (C2c1's dirty filter). A session row's event and its
// replay mark the planner dirty; unrouted, unassigned work does not, until a
// pass has read it as demand.
func TestControllerWakeOnBeadEventMarksPlanner(t *testing.T) {
	p := newTestPlanner()
	pokeCh, dispatchCh := make(chan struct{}, 1), make(chan struct{}, 1)
	w := newLegacyWake(pokeCh, dispatchCh)
	w.planner = p

	row := beadEvent(t, events.BeadUpdated, routerSessionBead("gc-1", map[string]string{"session_name": "worker"}))
	w.OnBeadEvent(row, false)
	w.OnBeadEvent(row, true)
	work := beadEvent(t, events.BeadUpdated, routerWorkBead("w-1", "open", ""))
	w.OnBeadEvent(work, false)
	if drainSignal(pokeCh) || drainSignal(dispatchCh) {
		t.Error("a bead event under v2 poked the legacy reconciler")
	}
	if n := plannerWakes(p, "bead-event"); n != 2 {
		t.Errorf("bead-event wakes = %d, want 2 (the row's event and its replay)", n)
	}
	p.out.relevant.Store(&relevantSet{"w-1": true}) // a pass read w-1 as demand
	w.OnBeadEvent(work, false)
	if n := plannerWakes(p, "bead-event"); n != 3 {
		t.Errorf("bead-event wakes = %d after w-1 became demand, want 3", n)
	}
}

// Kills: gc start running its one-shot reconcile, which calls the legacy
// session reconciler directly, under a v2 latch. Even with v2 admitted by the
// developer override, the one-shot refuses before any init.
func TestStartStandaloneRefusesV2OneShot(t *testing.T) {
	cityPath, opsLog := newRefusedSessionReconcilerCity(t)
	prevEnv := reconcilerModeLookupEnv
	reconcilerModeLookupEnv = overrideEnv("1")
	oldDryRun := dryRunMode
	dryRunMode = false
	t.Cleanup(func() {
		reconcilerModeLookupEnv = prevEnv
		dryRunMode = oldDryRun
	})

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, false, &stdout, &stderr); code != 1 {
		t.Fatalf("doStartStandalone exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "has no one-shot reconcile") {
		t.Errorf("stderr = %q, want the one-shot refusal", stderr.String())
	}
	assertRefusedCityUntouched(t, cityPath, opsLog)
}

// Kills: doctor and the reload drift judging admission without the developer
// override the controller latched with, so they disagree with controller
// start.
func TestSessionReconcilerOverrideFeedsDoctorAndDrift(t *testing.T) {
	cfg := &config.City{Daemon: config.DaemonConfig{SessionReconciler: "v2"}}
	if r := newSessionReconcilerDoctorCheck(cfg, overrideEnv("1")).Run(&doctor.CheckContext{}); r.Status != doctor.StatusWarning {
		t.Errorf("doctor with the override = %v (%s), want a warning", r.Status, r.Message)
	}
	if r := newSessionReconcilerDoctorCheck(cfg, nil).Run(&doctor.CheckContext{}); r.Status != doctor.StatusError {
		t.Errorf("doctor without the override = %v (%s), want an error", r.Status, r.Message)
	}
	d := reconcilerModeDrift{lookupEnv: overrideEnv("1")}
	if got := d.observe(cfg); !strings.HasPrefix(got, "pending restart: ") || strings.Contains(got, "will refuse it") {
		t.Errorf("drift with the override = %q, want pending restart without a refusal", got)
	}
}

// assertV2WakeWired checks a v2 entry point's composition: the API state's
// wake is the runtime's, and it marks the runtime's own planner.
func assertV2WakeWired(t *testing.T, runtimes []*CityRuntime) {
	t.Helper()
	if len(runtimes) == 0 {
		t.Fatal("no controllerState was wired")
	}
	for _, cr := range runtimes {
		if cr.v2 == nil || cr.wake == nil || cr.wake.planner == nil {
			t.Fatalf("v2 city runtime: v2 %v wake %v, want a planner on the wake", cr.v2, cr.wake)
		}
		if cs := cr.cs; cs == nil || cs.wake != cr.wake || cs.wake.planner != cr.v2.planner {
			t.Fatal("the API state's wake is not the runtime's planner wake: API keys would miss the planner")
		}
	}
}

// admitV2 admits v2 at the composition edges for one test, through the
// developer override. Its callers must not call t.Parallel().
func admitV2(t *testing.T) {
	t.Helper()
	prev := reconcilerModeLookupEnv
	reconcilerModeLookupEnv = overrideEnv("1")
	t.Cleanup(func() { reconcilerModeLookupEnv = prev })
}

// Kills: the supervisor's startOneCity building its city runtime without
// the wiring's wake and v2 runtime (msup). Admitted by the developer
// override, the city starts and its runtime and API state share the wiring's
// routed wake.
func TestSupervisorStartOneCityV2SharesTheWiringsWake(t *testing.T) {
	admitV2(t)
	wired := captureWiredControllerStates(t)
	launchSupervisorWireCity(t, "session_reconciler = \"v2\"\n")
	assertV2WakeWired(t, wired())
}

// Kills: the v2 tick running the legacy phase list (m14), whose session
// phases the guard would then refuse and count, or the tick traced as a
// controller tick (m15). A full tick() enters no legacy session path, runs
// beadReconcileTick's maintenance sub-steps (the detached-orphan sweep and
// the nudge fallback trace under bead_reconcile.*; the usage facts and the
// historical transcript pass leave their own marks), and traces as a
// maintenance tick.
func TestCityRuntimeV2FullTickRunsOnlyMaintenance(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, true)
	armMaintenanceLanes(t, cr, store)
	attachTestV2(t, cr)

	runFixtureTick(cr, "patrol")
	if n := cr.legacySessionEntries.Load(); n != 0 {
		t.Errorf("legacySessionEntries = %d after a v2 tick, want 0", n)
	}
	records := closeTrace(t, cr)
	var subSteps []string
	for _, op := range operationRecords(records) {
		if name, ok := strings.CutPrefix(op, string(TraceSiteControllerTickPhase)+" "); ok && strings.HasPrefix(name, "bead_reconcile.") {
			subSteps = append(subSteps, name)
		}
	}
	assertLinesEqual(t, "v2 tick bead_reconcile sub-steps", subSteps, []string{
		"bead_reconcile.sweep_detached_handoff_orphans",
		"bead_reconcile.nudge_dispatch_tick",
	})
	if !transcriptMetaStarted(cr) {
		t.Error("the v2 tick did not start the historical transcript pass")
	}
	if !slices.Contains(store.recorded(), "Get gc-1") {
		t.Errorf("the v2 tick did not read the awake session for its usage facts: %v", store.recorded())
	}
	details := map[string]bool{}
	for _, r := range records {
		if r.TriggerDetail != "" {
			details[r.TriggerDetail] = true
		}
	}
	if !details["maintenance_tick"] || details["controller_tick"] {
		t.Errorf("trace trigger details = %v, want maintenance_tick and no controller_tick", details)
	}
}

// Kills: a failed boot census swallowed into an empty one (m32): a failed
// read must never look like an empty city, and boot must not declare ready
// on it. Boot retries it and stays not ready until its context ends.
func TestV2SessionsCensusFailedReadBlocksReady(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, true)
	stderr := &synchronizedBuffer{}
	cr.stderr = stderr
	rt := attachTestV2(t, cr)
	cr.cs.cityBeadStore = failingListStore{Store: store}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- cr.bootV2(ctx) }()
	awaitCond(t, func() bool {
		return strings.Contains(stderr.String(), "boot census failed") || len(result) == 1
	}, "the boot census failing")
	if got := rt.bootState(); got != plannerBootCensus {
		t.Fatalf("boot state = %s on a failed census, want %s", got, plannerBootCensus)
	}
	cancel()
	select {
	case ok := <-result:
		if ok {
			t.Error("bootV2 reported ready after its context ended on a failing census")
		}
	case <-time.After(hangBudget):
		t.Fatalf("bootV2 did not return within %s of its context ending", hangBudget)
	}
}

// stopOrderProvider records whether the planner had stopped at each session
// listing; run's shutdown lists last.
type stopOrderProvider struct {
	*runtime.Fake
	rt                *plannerRuntime
	stoppedAtLastList atomic.Bool
}

func (p *stopOrderProvider) ListRunning(prefix string) ([]string, error) {
	p.rt.mu.Lock()
	stopped := p.rt.stopped
	p.rt.mu.Unlock()
	p.stoppedAtLastList.Store(stopped)
	return p.Fake.ListRunning(prefix)
}

// Kills: the planner stopped after run's shutdown (m33), so its passes
// would still run while shutdown stops the sessions. Shutdown's session
// listing finds the planner already stopped.
func TestCityRuntimeV2StopsBeforeShutdown(t *testing.T) {
	var sp *stopOrderProvider
	f := newV2RunFixture(t, "patrol_interval = \"1h\"\n", func(f *v2RunFixture) {
		sp = &stopOrderProvider{Fake: f.sp, rt: f.rt}
		f.cr.sp = sp
	})
	awaitClose(t, f.ready, "readiness")
	f.cancel()
	awaitClose(t, f.done, "run returning")
	if !sp.stoppedAtLastList.Load() {
		t.Error("shutdown listed the sessions to stop while the planner still ran")
	}
}

// Kills: a host bound to a planner that already started (mbind-nopanic),
// whose goroutines read the host unsynchronized with the bind.
func TestV2BindHostAfterStartPanics(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	rt := attachTestV2(t, cr)
	rt.publishEnv()
	rt.start(context.Background())
	defer func() {
		if recover() == nil {
			t.Error("bindHost on a started planner did not panic")
		}
	}()
	rt.bindHost(cr.newPlannerHost())
}

// Kills: the city runtime's reload drift judging admission without the
// environment its controller latched with (mdrift-noenv). A legacy
// controller latched with the developer override does not warn that a v2
// edit will be refused at the next start.
func TestCityRuntimeDriftJudgesWithTheWiringsEnv(t *testing.T) {
	w, err := newControllerWiring(&config.City{}, overrideEnv("1"), io.Discard)
	if err != nil {
		t.Fatalf("newControllerWiring: %v", err)
	}
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, w.runtimeParams(CityRuntimeParams{
		CityPath: t.TempDir(),
		CityName: "test-city",
		Cfg:      &config.City{},
		SP:       sp,
		Dops:     newDrainOps(sp),
		Rec:      events.Discard,
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	}))
	got := cr.reconcilerDrift.observe(&config.City{Daemon: config.DaemonConfig{SessionReconciler: "v2"}})
	if !strings.HasPrefix(got, "pending restart: ") || strings.Contains(got, "will refuse it") {
		t.Errorf("drift warning = %q, want pending restart without a refusal", got)
	}
}

// v2PassRecordFields are the reconcile_pass record's fields. They are
// pinned: a trace consumer reads them by name.
var v2PassRecordFields = []string{"admitted", "boot", "deferred", "legacy_session_entries", "passes", "wakes"}

// passRecords returns the reconcile_pass records, in order.
func passRecords(records []SessionReconcilerTraceRecord) []SessionReconcilerTraceRecord {
	var out []SessionReconcilerTraceRecord
	for _, r := range records {
		if r.RecordType == TraceRecordOperation && r.Fields["operation_name"] == "reconcile_pass" {
			out = append(out, r)
		}
	}
	return out
}

// Kills: the reconcile_pass record missing from the v2 maintenance tick,
// recorded twice in one, its fields renamed, or recorded by a legacy tick
// (whose trace must not change); and the m14/m15 mutations of
// TestCityRuntimeV2FullTickRunsOnlyMaintenance as they show in the record:
// the v2 tick running the legacy phase list (the guard counts its refusals)
// or tracing as a controller tick.
func TestV2MaintenanceTraceRecordsReconcilePass(t *testing.T) {
	t.Run("v2", func(t *testing.T) {
		cr, _ := newPhaseFixtureRuntime(t, false, true)
		attachTestV2(t, cr)
		bootTestV2(t, cr)
		runFixtureTick(cr, "patrol")
		runFixtureTick(cr, "patrol")
		if n := cr.legacySessionEntries.Load(); n != 0 {
			t.Errorf("legacySessionEntries = %d after v2 ticks, want 0", n)
		}
		records := closeTrace(t, cr)
		recs := passRecords(records)
		if len(recs) != 2 || recs[0].TickID == recs[1].TickID {
			t.Fatalf("reconcile_pass records = %d over two v2 ticks, want one per tick", len(recs))
		}
		first := recs[0]
		details := map[string]bool{}
		for _, r := range records {
			if r.TickID == first.TickID && r.TriggerDetail != "" {
				details[r.TriggerDetail] = true
			}
		}
		if first.SiteCode != TraceSiteReconcilePass || !details["maintenance_tick"] || details["controller_tick"] {
			t.Errorf("record site %q in a trace with details %v, want %q in a maintenance_tick trace", first.SiteCode, details, TraceSiteReconcilePass)
		}
		var names []string
		for name := range first.Fields {
			if name != "operation_name" {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		assertLinesEqual(t, "reconcile_pass fields", names, v2PassRecordFields)
		if first.Fields["boot"] != plannerBootReady || first.Fields["legacy_session_entries"] != float64(0) {
			t.Errorf("record boot=%v legacy_session_entries=%v, want ready, 0", first.Fields["boot"], first.Fields["legacy_session_entries"])
		}
	})

	t.Run("legacy", func(t *testing.T) {
		cr, _ := newPhaseFixtureRuntime(t, false, true)
		runFixtureTick(cr, "patrol")
		if recs := passRecords(closeTrace(t, cr)); len(recs) != 0 {
			t.Errorf("a legacy tick recorded reconcile_pass %d times, want never", len(recs))
		}
	})
}

// Kills: the exclusivity counter not surfaced: the record reports a constant
// instead of the city runtime's count of refused legacy session entries.
func TestV2LegacySessionEntriesReportedInTrace(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, true)
	attachTestV2(t, cr)
	cr.beadReconcileTick(context.Background(), DesiredStateResult{}, nil, nil, false) // refused and counted
	cr.controlDispatcherTick(context.Background())                                    // refused and counted
	runFixtureTick(cr, "patrol")
	recs := passRecords(closeTrace(t, cr))
	if len(recs) != 1 {
		t.Fatalf("reconcile_pass records = %d, want 1", len(recs))
	}
	if got := recs[0].Fields["legacy_session_entries"]; got != float64(2) {
		t.Errorf("legacy_session_entries = %v, want 2", got)
	}
}

// Kills: a store-less v2 city's record reading as a boot stuck on its census
// (MAINT-003: boot never runs there, and readiness proceeds). bootV2 latches
// no-store; the record does not re-read the store.
func TestCityRuntimeV2NoStorePassRecordSaysNoStore(t *testing.T) {
	cityPath := t.TempDir()
	cr := &CityRuntime{
		cityName:  "test-city",
		cityPath:  cityPath,
		cfg:       &config.City{Workspace: config.Workspace{Name: "test-city"}},
		logPrefix: "gc test",
		stdout:    io.Discard,
		stderr:    io.Discard,
		trace:     newSessionReconcilerTraceManager(cityPath, "test-city", io.Discard),
	}
	attachTestV2(t, cr)
	if !cr.bootV2(context.Background()) {
		t.Fatal("a store-less v2 city did not proceed to readiness")
	}
	trace := cr.beginTraceCycle("patrol", "maintenance_tick", nil)
	cr.recordV2Pass(trace)
	trace.end(TraceCompletionCompleted, traceRecordPayload{"phase": "tick"})
	recs := passRecords(closeTrace(t, cr))
	if len(recs) != 1 {
		t.Fatalf("reconcile_pass records = %d, want 1", len(recs))
	}
	if got := recs[0].Fields["boot"]; got != "no-store" {
		t.Errorf("boot = %v, want no-store", got)
	}
}

// Kills: the startup watchdog saying nothing of the planner, or not what
// boot waits on: a boot whose sessions cache never primes waits on the gate.
func TestV2StartupWatchdogReportsTheBootGate(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, true)
	stderr := &synchronizedBuffer{}
	cr.stderr = stderr
	rt := attachTestV2(t, cr) // no primed cache: the gate stays shut
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- cr.bootV2(ctx) }()
	awaitCond(t, func() bool { return plannerPasses(rt.planner) > 0 }, "a pass that hit the shut gate")
	cr.startupReadinessWatchdog(context.Background(), make(chan struct{}), 0, time.Minute)
	if got := stderr.String(); !strings.Contains(got, "session reconciler v2: boot=gate") {
		t.Errorf("startup watchdog = %q, want the planner's shut boot gate", got)
	}
	cancel()
	if <-result {
		t.Error("bootV2 reported ready with the cache unprimed")
	}
}

// Kills: the record left out of a maintenance tick that panics (recorded only
// at the tick's normal end, or only when it completed). The aborted cycle
// still carries exactly one record.
func TestV2PanickedMaintenanceTickRecordsThePass(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, false)
	attachTestV2(t, cr)
	cr.cs.cityBeadStore = &panicOnLabelStore{Store: store, label: "gc:extmsg-binding"}
	if !cr.safeTick(func() { runFixtureTick(cr, "patrol") }, "patrol") {
		t.Fatal("the maintenance tick did not panic; the test needs it to")
	}
	records := closeTrace(t, cr)
	if got := passCompletion(records, "tick"); got != TraceCompletionAborted {
		t.Fatalf("tick completion = %q, want aborted", got)
	}
	var tick string
	for _, r := range records {
		if r.RecordType == TraceRecordCycleResult && r.Fields["phase"] == "tick" {
			tick = r.TickID
		}
	}
	var n int
	for _, r := range passRecords(records) {
		if r.TickID == tick {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("reconcile_pass records in the aborted tick = %d, want 1", n)
	}
}

// Kills: the trace sink unwired (the pass's changed rows never reach the
// trace) or one record per row per pass instead of per change (v5 R6). The
// first pass records the fixture row's decision under its template and key;
// a second, unchanged pass records nothing. The worker template is armed for
// detail, as an operator's gc trace arms it.
func TestPlannerTraceSinkRecordsChangedRows(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, true)
	rt := attachTestV2(t, cr)
	primeTestV2(t, cr)
	now := time.Now()
	if _, err := cr.trace.armStore.upsertArm(TraceArm{
		ScopeType: TraceArmScopeTemplate, ScopeValue: "worker", Source: TraceArmSourceManual, Level: TraceModeDetail,
		ArmedAt: now, ExpiresAt: now.Add(time.Hour), LastExtendedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert arm: %v", err)
	}
	rt.publishEnv()
	rt.pass(time.Now())
	if rec := rt.planner.out.record.Load(); rec.Err != "" || len(rec.Rows) != 1 {
		t.Fatalf("first pass record = %+v, want the worker row traced", rec)
	}
	rt.pass(time.Now())
	var decisions []string
	for _, r := range closeTrace(t, cr) {
		if r.RecordType == TraceRecordDecision && r.SiteCode == TraceSiteV2SessionDecision {
			decisions = append(decisions, fmt.Sprintf("%s %s %v", r.Template, r.SessionName, r.Fields["key"]))
		}
	}
	if len(decisions) != 1 || !strings.HasPrefix(decisions[0], "worker worker city:") {
		t.Fatalf("decision records = %q, want one for the worker row", decisions)
	}
}

// v2PlannerSeams are the reconcile_*.go production files that are the city
// runtime's side of the switch and so name CityRuntime by design. Every
// other reconcile_*.go and allocator_*.go file is v2 code, a new one
// included, and reaches the city only through plannerHost.
var v2PlannerSeams = map[string]bool{"reconcile_maintenance.go": true}

// v2PlannerSeamDecls are the only declarations inside a v2 file that may name
// CityRuntime: the wake's runtime accessors.
var v2PlannerSeamDecls = map[string]bool{
	"reconcile_wake.go:(*CityRuntime).initWake": true,
	"reconcile_wake.go:(*CityRuntime).wakeOf":   true,
}

// Kills: planner code reaching CityRuntime (F2), or a pass reading the
// host's config directly instead of the published env. A reload writes
// CityRuntime fields without a lock, so the v2 files never name the type,
// and only publishEnv calls host.snapshotEnv.
func TestPlannerDoesNotReferenceCityRuntime(t *testing.T) {
	var files []string
	for _, pattern := range []string{"reconcile_*.go", "allocator_*.go"} {
		names, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, name := range names {
			if !strings.HasSuffix(name, "_test.go") && !v2PlannerSeams[name] {
				files = append(files, name)
			}
		}
	}
	for _, want := range []string{"reconcile_planner.go", "reconcile_planner_runtime.go", "reconcile_pass.go", "reconcile_gather.go", "reconcile_wiring.go", "reconcile_wake.go"} {
		if !slices.Contains(files, want) {
			t.Fatalf("v2 files %v miss %s", files, want)
		}
	}
	fset := token.NewFileSet()
	var bad []string
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			if v2PlannerSeamDecls[name+":"+topLevelDeclName(decl)] {
				continue
			}
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.Ident:
					if n.Name == "CityRuntime" {
						bad = append(bad, fset.Position(n.Pos()).String()+": names CityRuntime")
					}
				case *ast.SelectorExpr:
					if n.Sel.Name == "snapshotEnv" && (fn == nil || fn.Name.Name != "publishEnv") {
						bad = append(bad, fset.Position(n.Pos()).String()+": reads host.snapshotEnv outside publishEnv")
					}
				}
				return true
			})
		}
	}
	if len(bad) > 0 {
		t.Fatalf("planner code must reach the city only through plannerHost and the published env (F2):\n  %s", strings.Join(bad, "\n  "))
	}
}
