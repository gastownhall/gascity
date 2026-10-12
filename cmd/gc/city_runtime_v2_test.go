package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workqueue"
)

// The P2-8 wiring tests: the v2 runtime behind the exclusive switch. Unit
// tests run a phase fixture (newPhaseFixtureRuntime) made a v2 controller
// the way newCityRuntime makes one, with recording controllers; run-level
// tests drive run() itself on a fake provider, waiting with awaitClose and
// awaitCond, never sleeping.

// attachTestV2 makes cr a v2 controller exactly as newCityRuntime does for
// one that latched v2 (installV2, then the wiring's wake, which routes over
// the runtime's signals to its router), with this build's runtime and
// recording controllers in place of the trace-only ones.
func attachTestV2(t *testing.T, cr *CityRuntime) (*v2Runtime, *v2Recorder) {
	t.Helper()
	rt, rec := newTestDefaultV2Runtime(cr.cfg, cr.stderr)
	cr.reconcilerDrift.running = reconcilerV2
	cr.sessionDrains, cr.providerHealthGate = nil, nil
	cr.installV2(rt)
	wake := newLegacyWake(cr.pokeCh, cr.controlDispatcherCh)
	wake.router = rt.router
	cr.initWake(wake)
	if cr.cs != nil {
		wireControllerWakeSignals(cr.cs, cr.wake)
	}
	t.Cleanup(rt.stop)
	return rt, rec
}

// newTestDefaultV2Runtime is newDefaultV2Runtime with recording controllers,
// no jitter, and a worker FS gate that never reads host pressure.
func newTestDefaultV2Runtime(cfg *config.City, stderr io.Writer) (*v2Runtime, *v2Recorder) {
	rec := &v2Recorder{}
	rt := newDefaultV2Runtime(v2SessionsLeg(cfg), stderr)
	rt.ctrl = rec.controllers()
	rt.rand = func() float64 { return 0.5 }
	rt.fs.sample = func() (fsPressureStatus, bool) { return fsPressureStatus{}, false }
	return rt, rec
}

// newTestV2Wiring is newControllerWiring for a controller admitted to v2 by
// the developer override, with newTestDefaultV2Runtime's runtime on its wake.
func newTestV2Wiring(t *testing.T, cfg *config.City, stderr io.Writer) (*controllerWiring, *v2Recorder) {
	t.Helper()
	latch := *cfg
	latch.Daemon.SessionReconciler = "v2"
	w, err := newControllerWiring(&latch, overrideEnv("1"), io.Discard)
	if err != nil {
		t.Fatalf("newControllerWiring: %v", err)
	}
	rt, rec := newTestDefaultV2Runtime(cfg, stderr)
	w.v2 = rt
	w.wake.router = rt.router
	return w, rec
}

func bootTestV2(t *testing.T, cr *CityRuntime) {
	t.Helper()
	if !cr.bootV2(context.Background(), nil) {
		t.Fatal("bootV2 did not reach ready")
	}
}

// awaitReconcile waits until key has been reconciled with a reason of kind.
func awaitReconcile(t *testing.T, rec *v2Recorder, key, kind string) {
	t.Helper()
	awaitCond(t, func() bool {
		for _, c := range rec.callsFor(key) {
			if slices.Contains(c.kinds, kind) {
				return true
			}
		}
		return false
	}, fmt.Sprintf("a %s reconcile of %s", kind, key))
}

func awaitAllocatorReason(t *testing.T, rec *v2Recorder, kind string) {
	t.Helper()
	awaitCond(t, func() bool {
		for _, pass := range rec.allocatorPasses() {
			if slices.Contains(pass, kind) {
				return true
			}
		}
		return false
	}, "an allocator pass for "+kind)
}

// assertV2BootRefuses adds rows to a v2 phase fixture, whose own row is
// clean, and boots it through the city runtime's host. Boot must refuse at
// once, with want and the doctor command in the error, after a live read of
// the sessions leg (C4.5 item 1b), and before anything runs: no key is
// reconciled, no allocator pass runs, nothing is written, and the fixture's
// running session is not stopped.
func assertV2BootRefuses(t *testing.T, want string, rows ...beads.Bead) {
	t.Helper()
	cr, store := newPhaseFixtureRuntime(t, false, true)
	stderr := &lockedBuffer{}
	cr.stderr = stderr
	rt, rec := attachTestV2(t, cr)
	for _, row := range rows {
		if _, err := store.Store.Create(row); err != nil {
			t.Fatalf("Create(%s): %v", row.ID, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if cr.bootV2(ctx, nil) {
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
	if rt.ready.Load() || len(rec.keys()) != 0 || len(rec.allocatorPasses()) != 0 {
		t.Errorf("ready=%v reconciled=%q allocator passes=%d: a refused boot ran its controllers", rt.ready.Load(), rec.keys(), len(rec.allocatorPasses()))
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
// boots the v2 runtime, which reconciles the open row.
func TestCityRuntimeV2StartupSkipsLegacyBootReconcile(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, true)
	rt, rec := attachTestV2(t, cr)
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
	if calls := rec.callsFor("gc-1"); len(calls) != 1 || !slices.Equal(calls[0].kinds, []string{v2ReasonBoot}) {
		t.Errorf("reconciles of gc-1 = %+v, want one boot reconcile before the step returned", calls)
	}
	if len(rec.allocatorPasses()) == 0 {
		t.Error("the startup step returned before the allocator primed")
	}
	if env := rt.env.Load(); env.ConfigRev == "" || env.ConfigRev != cr.configRev {
		t.Errorf("boot env revision = %q, want the boot config's %q", env.ConfigRev, cr.configRev)
	}
}

// v2RunFixture is a v2 city runtime driven through run() on a fake provider.
type v2RunFixture struct {
	cr       *CityRuntime
	rt       *v2Runtime
	rec      *v2Recorder
	store    *beads.MemStore
	sp       *runtime.Fake
	stderr   *synchronizedBuffer
	cityPath string
	tomlPath string
	ready    chan struct{}
	done     chan struct{}
	cancel   context.CancelFunc
}

// newV2RunFixture builds a v2 city whose [daemon] section is daemon. setup,
// when set, runs before run() starts.
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
	wiring, rec := newTestV2Wiring(t, cfg, f.stderr)
	f.rt, f.rec = wiring.v2, rec
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
	cs.cityBeadStore = f.store
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

// Kills: the v2 boot enqueue-all run before the adoption barrier or before
// the startup reload (MAINT-006, MAINT-007). The first reconciles already see
// the adopted row and the reloaded config's revision.
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
	calls := f.rec.callsFor(adopted[0].ID)
	if len(calls) == 0 || !slices.Contains(calls[0].kinds, v2ReasonBoot) {
		t.Fatalf("reconciles of the adopted row %s = %+v, want a boot reconcile first", adopted[0].ID, calls)
	}
	if calls[0].gen != 1 || calls[0].rev != reloadedRev {
		t.Errorf("the boot reconcile ran on env gen %d rev %q, want gen 1 rev %q (the reloaded config)", calls[0].gen, calls[0].rev, reloadedRev)
	}
}

// Kills: the control-dispatcher arm left live under v2 (MAINT-056, API-014):
// a stray signal would run the legacy controlDispatcherTick. Under v2 the arm
// selects on nil, and the socket's control-dispatch key reaches the allocator
// without touching the legacy signal.
func TestCityRuntimeV2ControlDispatcherSignalNeverRunsLegacyPath(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	if got := cr.controlDispatcherSignal(); got == nil {
		t.Fatal("a legacy controller's control-dispatcher arm selects on nil")
	}
	_, rec := attachTestV2(t, cr)
	if got := cr.controlDispatcherSignal(); got != nil {
		t.Fatal("the control-dispatcher arm is live under v2")
	}
	bootTestV2(t, cr)

	cr.wakeOf().Enqueue(wakeReasonSocket, reconcilekey.ControlDispatch())
	awaitAllocatorReason(t, rec, routeReasonControlDispatch)
	if len(cr.controlDispatcherCh) != 0 || len(cr.pokeCh) != 0 {
		t.Errorf("legacy signals (poke, dispatch) = (%d, %d), want none", len(cr.pokeCh), len(cr.controlDispatcherCh))
	}
	if n := cr.legacySessionEntries.Load(); n != 0 {
		t.Errorf("legacySessionEntries = %d, want 0", n)
	}
}

// Kills: keys still folded onto pokeCh under v2 (API-006..010). An API
// session key reaches the v2 queue and is reconciled; the tick is not poked.
func TestCityRuntimeV2KeyedEnqueueReachesQueueNotTick(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	_, rec := attachTestV2(t, cr)
	bootTestV2(t, cr)

	cr.cs.Enqueue(reconcilekey.Session("gc-1"))
	awaitReconcile(t, rec, "gc-1", wakeReasonAPI)
	if len(cr.pokeCh) != 0 || len(cr.controlDispatcherCh) != 0 {
		t.Errorf("legacy signals (poke, dispatch) = (%d, %d), want none: the key reached the tick", len(cr.pokeCh), len(cr.controlDispatcherCh))
	}
}

// Kills: a config mutation that only wakes the allocator under v2, so the
// reload waits for the patrol (API-003, API-017), or a tick reload that skips
// the barrier. The mutation pokes the maintenance tick, whose reload applies
// under the barrier, publishes the next env and then wakes the allocator.
func TestCityRuntimeV2ConfigMutationRunsBarrierThenWakesAllocator(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	rt, rec := attachTestV2(t, cr)
	cr.cs.configDirty = cr.configDirty
	bootTestV2(t, cr)
	oldRev := rt.env.Load().ConfigRev

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
	awaitAllocatorReason(t, rec, v2ReasonReload)
	if holds := rt.sessions.Stats().Holds; len(holds) != 0 {
		t.Errorf("holds after the reload = %v, want none", holds)
	}
}

// Kills: the manual reload's reply left to the end of the tick, or never
// sent (MAINT-023), or a hard reload told drift acceptance is unavailable
// (mhook2). The barrier answers it from inside: by the next phase that reads
// the store, the reply is already delivered and the reconciles have resumed.
func TestCityRuntimeV2ReloadReplyBeforeResume(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, true, false)
	cr.activeReload.soft = false
	rt, _ := attachTestV2(t, cr)
	bootTestV2(t, cr)
	doneCh := cr.activeReload.doneCh

	var mu sync.Mutex
	var firstRead *struct {
		replied bool
		holds   []string
	}
	store.onRecord = func(op string) {
		if !strings.HasPrefix(op, "List ") {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if firstRead == nil {
			firstRead = &struct {
				replied bool
				holds   []string
			}{replied: len(doneCh) == 1, holds: rt.sessions.Stats().Holds}
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
	if len(firstRead.holds) != 0 {
		t.Errorf("holds while the tick went on = %v, want none (resumed)", firstRead.holds)
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
// API-018), or a routed replay burst logging a line per event. A session
// event resolves through the router's name index to the row's key; a burst
// reports one landed wake.
func TestCityRuntimeV2ProviderEventEnqueuesSessionKey(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	_, rec := attachTestV2(t, cr)
	bootTestV2(t, cr)
	now := time.Unix(1_800_000_000, 0)
	cr.wakeOf().now = func() time.Time { return now }
	var stderr bytes.Buffer
	pump := newSessionEventPump(context.Background(), cr.wakeOf(), &stderr, "gc test")

	for range 3 {
		pump.poke("died", "worker")
	}
	awaitReconcile(t, rec, "gc-1", wakeReasonProviderEvent)
	if len(cr.pokeCh) != 0 {
		t.Error("the provider event poked the tick under v2")
	}
	if n := strings.Count(stderr.String(), "reconcile poke"); n != 1 {
		t.Errorf("the burst logged %d wake lines, want 1:\n%s", n, stderr.String())
	}
}

// Kills: the inventory pass hook never installed under v2. A runtime
// attributed to a session row, under a name no row answers to, goes away:
// the owner's key is reconciled.
func TestCityRuntimeV2InventoryFlipEnqueuesOwnerKey(t *testing.T) {
	sp := newScriptedInventoryProvider("ghost-runtime")
	sp.env["ghost-runtime"] = map[string]string{"GC_SESSION_ID": "gc-owner"}
	cr := inventoryLaneTestRuntime(t, sp, nil)
	_, rec := attachTestV2(t, cr)
	bootTestV2(t, cr)

	runTestInventoryPass(cr) // listed, attributed
	sp.mu.Lock()
	sp.names = nil
	sp.mu.Unlock()
	runTestInventoryPass(cr) // gone
	awaitReconcile(t, rec, "gc-owner", routeReasonInventory)
}

// Kills: v2 workers started on a city with no bead store (MAINT-003). The
// boot step logs that the workers are disabled and lets readiness proceed
// without reading a census.
func TestCityRuntimeV2NoStoreDisablesWorkers(t *testing.T) {
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityName:  "test-city",
		cfg:       &config.City{Workspace: config.Workspace{Name: "test-city"}},
		logPrefix: "gc test",
		stdout:    io.Discard,
		stderr:    &stderr,
	}
	rt, _ := attachTestV2(t, cr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a boot that tried would fail at once instead of retrying
	if !cr.bootV2(ctx, nil) {
		t.Fatal("a store-less v2 city did not proceed to readiness")
	}
	rt.mu.Lock()
	started := rt.started
	rt.mu.Unlock()
	if started {
		t.Error("the v2 workers started with no bead store")
	}
	if !strings.Contains(stderr.String(), "no bead store; reconcile workers disabled") {
		t.Errorf("stderr = %q, want the disabled-workers notice", stderr.String())
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

// Kills: a panic in the maintenance tick reaching the v2 runtime. The tick's
// safeTick swallows it, and the workers keep reconciling.
func TestCityRuntimeV2PanicInMaintenanceTickLeavesWorkersRunning(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, false)
	rt, rec := attachTestV2(t, cr)
	bootTestV2(t, cr)
	cr.cs.cityBeadStore = &panicOnLabelStore{Store: store, label: "gc:extmsg-binding"}

	lastProviderName := "fake"
	var prevPoolRunning map[string]bool
	if !cr.safeTick(func() {
		cr.tick(context.Background(), cr.configDirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
	}, "patrol") {
		t.Fatal("the maintenance tick did not panic; the test needs it to")
	}
	cr.cs.Enqueue(reconcilekey.Session("gc-1"))
	awaitReconcile(t, rec, "gc-1", wakeReasonAPI)
	rt.mu.Lock()
	stopped := rt.stopped
	rt.mu.Unlock()
	if stopped {
		t.Error("the maintenance panic stopped the v2 runtime")
	}
}

// Kills: a soft reload under v2 answered as if drift acceptance ran (a
// silent zero), or not answered (MAINT-025 is off until P4.4). The reply
// applies and says acceptance is unavailable; no session row is rewritten.
func TestCityRuntimeV2SoftReloadRepliesUnavailable(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, true, false)
	attachTestV2(t, cr)
	bootTestV2(t, cr)
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
// developer override set, builds no v2 runtime and no router, prints no
// banner, and a legacy city runtime installs no inventory hook.
func TestLegacyRuntimeConstructsNoV2State(t *testing.T) {
	var stderr bytes.Buffer
	w, err := newControllerWiring(&config.City{}, overrideEnv("1"), &stderr)
	if err != nil {
		t.Fatalf("newControllerWiring: %v", err)
	}
	if w.v2 != nil || w.wake.router != nil || w.mode != reconcilerLegacy {
		t.Errorf("legacy wiring = mode %v v2 %v router %v, want legacy with neither", w.mode, w.v2, w.wake.router)
	}
	if stderr.Len() != 0 {
		t.Errorf("legacy wiring printed %q", stderr.String())
	}

	cr, _ := newPhaseFixtureRuntime(t, false, true)
	runFixtureTick(cr, "patrol")
	if cr.v2 != nil || cr.wakeOf().router != nil {
		t.Errorf("legacy city runtime: v2 %v router %v, want neither", cr.v2, cr.wakeOf().router)
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
// the refusal skipped (OQ-1); and the router missing from the wiring. An
// admitted v2 wiring carries an unstarted runtime whose router is already on
// the wake, keyed by the sessions leg, and announces the skeleton.
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
			if w.mode != reconcilerV2 || w.v2 == nil || w.wake.router != w.v2.router {
				t.Fatalf("wiring = mode %v v2 %v, want v2 with its router on the wake", w.mode, w.v2)
			}
			if w.v2.router.sessionsLeg != "city:gate" {
				t.Errorf("sessions leg = %q, want city:gate", w.v2.router.sessionsLeg)
			}
			if w.v2.ctrl.complete() {
				t.Error("this build's v2 controllers report complete")
			}
			if !strings.Contains(stderr.String(), "session reconciler: v2 (skeleton: trace-only controllers)") {
				t.Errorf("stderr = %q, want the skeleton banner", stderr.String())
			}
		})
	}
}

// Kills: the router installed after newControllerWiring returns, so a key the
// socket delivers before the city runtime exists is lost; or the city runtime
// building its own wake instead of the wiring's, so its follow-ups, lanes and
// pump bypass the router.
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

	rec := &v2Recorder{}
	w.v2.ctrl = rec.controllers()
	w.v2.fs.sample = func() (fsPressureStatus, bool) { return fsPressureStatus{}, false }
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
		t.Fatal("the city runtime does not use the wiring's wake and v2 runtime")
	}
	if leg := wired.v2.host.sessionsLeg; leg == "" || leg != w.v2.router.sessionsLeg {
		t.Errorf("bound host sessions leg = %q, want the wiring's %q", leg, w.v2.router.sessionsLeg)
	}
	wired.setControllerState(cr.cs)
	bootTestV2(t, wired)
	awaitReconcile(t, rec, "gc-1", wakeReasonSocket)
}

// Kills: params whose wake, v2 runtime and mode disagree accepted, so the
// runtime reconciles through a wake the socket and API do not use; or a v2
// controller accepted without its wiring's wake and runtime (an entry point
// that forgot them would build a private router the socket never reaches).
func TestNewCityRuntimeRefusesMismatchedReconcilerWiring(t *testing.T) {
	rt := newDefaultV2Runtime("city:x", io.Discard)
	other := newDefaultV2Runtime("city:x", io.Discard)
	routed := newLegacyWake(nil, nil)
	routed.router = rt.router
	for _, tc := range []struct {
		name string
		p    CityRuntimeParams
		ok   bool
	}{
		{name: "legacy bare", ok: true},
		{name: "legacy wake", p: CityRuntimeParams{Wake: newLegacyWake(nil, nil)}, ok: true},
		{name: "v2 wired", p: CityRuntimeParams{ReconcilerMode: reconcilerV2, V2: rt, Wake: routed}, ok: true},
		{name: "v2 bare", p: CityRuntimeParams{ReconcilerMode: reconcilerV2}},
		{name: "v2 runtime without a wake", p: CityRuntimeParams{ReconcilerMode: reconcilerV2, V2: rt}},
		{name: "v2 runtime under legacy", p: CityRuntimeParams{V2: rt}},
		{name: "routed wake under legacy", p: CityRuntimeParams{Wake: routed}},
		{name: "v2 wake without its runtime", p: CityRuntimeParams{ReconcilerMode: reconcilerV2, Wake: routed}},
		{name: "v2 wake to another runtime", p: CityRuntimeParams{ReconcilerMode: reconcilerV2, V2: other, Wake: routed}},
		{name: "v2 legacy wake", p: CityRuntimeParams{ReconcilerMode: reconcilerV2, V2: rt, Wake: newLegacyWake(nil, nil)}},
	} {
		if err := checkReconcilerWiring(tc.p); (err == nil) != tc.ok {
			t.Errorf("%s: checkReconcilerWiring = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// Kills: a router mapping panic escaping applyBeadEventToStores into the bead
// event watcher, which has no recover of its own (F7). The panic is counted,
// a resync is requested, and the watcher keeps applying events.
func TestApplyBeadEventRouterPanicDoesNotKillWatcher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var resyncs []string
		var added atomic.Int32
		router := newReconcileRouter(routerTestLeg, routerSink{
			addSession: func(rowKey, routeReason) {
				added.Add(1)
				panic("sink exploded")
			},
			wakeAllocator: func(routeReason) {},
			requestResync: func(reason string) { resyncs = append(resyncs, reason) },
		}, io.Discard)
		row := func(id string, seq uint64) events.Event {
			evt := beadEvent(t, events.BeadUpdated, routerSessionBead(id, map[string]string{"session_name": "s-" + id}))
			evt.Seq = seq
			return evt
		}
		ep := &scriptedEventProvider{watches: []scriptedWatch{{events: []events.Event{row("gc-1", 1), row("gc-2", 2)}}}}
		cs := &controllerState{eventProv: ep, beadEventStartSeqOK: true, cityBeadStore: beads.NewMemStore(), wake: &controllerWake{router: router}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cs.startBeadEventWatcher(ctx)
		synctest.Wait()
		if n := added.Load(); n != 2 {
			t.Fatalf("session rows routed = %d, want 2: the watcher stopped at the first panic", n)
		}
		if got := router.stats().Panics; got != 2 {
			t.Errorf("router panics = %d, want 2", got)
		}
		if n := strings.Count(strings.Join(resyncs, ","), "router-panic"); n != 2 {
			t.Errorf("resyncs = %v, want a router-panic resync per panic", resyncs)
		}
	})
}

// Kills: v2 lanes started before run owns the city and arms the config
// watcher (MAINT-001); the runtime's lifetime tied to the startup step rather
// than run (its workers would be gone once the city is ready); the worker FS
// gate left unarmed after readiness; the runtime not stopped when run returns;
// the drain tracker built under v2.
func TestV2Boot_OwnsCityAndArmsWatcherBeforeLanes(t *testing.T) {
	var violations []string
	var mu sync.Mutex
	f := newV2RunFixture(t, "patrol_interval = \"1h\"\n", func(f *v2RunFixture) {
		f.rec.setAllocator(func() error {
			f.cr.watchMu.Lock()
			armed := f.cr.watchCleanup != nil
			f.cr.watchMu.Unlock()
			if !f.cr.ownedCity.Load() || !armed {
				mu.Lock()
				violations = append(violations, fmt.Sprintf("allocator pass with ownedCity=%v watcher=%v", f.cr.ownedCity.Load(), armed))
				mu.Unlock()
			}
			return nil
		})
	})
	awaitClose(t, f.ready, "readiness")
	mu.Lock()
	if len(violations) != 0 {
		t.Errorf("v2 lanes ran before run owned the city: %v", violations)
	}
	mu.Unlock()

	id := "gc-after-ready"
	f.cr.wakeOf().Enqueue(wakeReasonAPI, reconcilekey.Session(id))
	awaitReconcile(t, f.rec, id, wakeReasonAPI)
	// run arms the gate just after it reports ready.
	awaitCond(t, func() bool {
		f.rt.mu.Lock()
		defer f.rt.mu.Unlock()
		return f.rt.fsArmed
	}, "the worker FS gate armed after readiness")

	f.cancel()
	awaitClose(t, f.done, "run returning")
	f.rt.mu.Lock()
	stopped := f.rt.stopped
	f.rt.mu.Unlock()
	if !stopped {
		t.Error("run returned without stopping the v2 runtime")
	}
	if f.cr.sessionDrains != nil {
		t.Error("run built the legacy drain tracker under v2")
	}
}

// Kills: a non-idempotent v2 boot, or a boot-pass panic that stops the city
// instead of retrying (MAINT-005). A census that panics once is retried after
// the patrol and the city starts with one boot; a census that always panics
// gives up after max_restarts.
func TestV2Boot_FirstPassRetriesOnPanicBoundedByMaxRestarts(t *testing.T) {
	panickingCensus := func(f *v2RunFixture, times int) *atomic.Int32 {
		var calls atomic.Int32
		inner := f.rt.host.sessions
		f.rt.host.sessions = func() ([]session.Info, error) {
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
			if _, err := f.store.Create(v2TestRow("gc-row")); err != nil {
				t.Fatalf("Create: %v", err)
			}
			calls = panickingCensus(f, 1)
		})
		awaitClose(t, f.ready, "readiness after the retried boot")
		if !strings.Contains(f.stderr.String(), "census exploded") {
			t.Errorf("stderr = %q, want the recovered boot panic", f.stderr.String())
		}
		if n := calls.Load(); n != 2 {
			t.Errorf("boot census reads = %d, want 2 (one panic, one retry)", n)
		}
		rows, err := f.store.List(beads.ListQuery{Type: sessionBeadType})
		if err != nil || len(rows) != 1 {
			t.Fatalf("session rows = %v (err %v)", rows, err)
		}
		if got := f.rec.callsFor(rows[0].ID); len(got) != 1 {
			t.Errorf("boot reconciles of %s = %+v, want exactly one", rows[0].ID, got)
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

// Kills: a v2Host closure reading a CityRuntime field a reload writes
// unlocked (F2). Under -race, each host closure loops on its own goroutine
// (so one closure's locking cannot order another's reads) against a tick that
// applies a new config, with resync passes running around it.
func TestV2HostReadsRaceFreeAgainstReload(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, true, true)
	cr.activeReload.soft = false
	rt, _ := attachTestV2(t, cr)
	bootTestV2(t, cr)

	h := rt.host
	probes := []func(){
		func() { _, _ = h.sessions() },
		func() { _, _ = h.censusLegs() },
		func() { _ = h.cityStore() },
		func() { _ = h.rigStores() },
		func() { h.beginTrace("race-probe").end(TraceCompletionCompleted, traceRecordPayload{"phase": "probe"}) },
		func() { rt.requestResync("race-probe") },
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
	bootTestV2(t, cr)
	runFixtureTick(cr, "reload")
	if cr.sessionDrains != nil || cr.providerHealthGate != nil {
		t.Error("a v2 reload built the legacy drain tracker")
	}
}

// Kills: a store-less or feedless city leaving the v2 router blind: a
// watcher that cannot start (no provider, or an unresolved start cursor)
// reports a gap, so the router resyncs.
func TestBeadEventWatcherWithoutFeedReportsGap(t *testing.T) {
	for _, tc := range []struct {
		name string
		ep   events.Provider
	}{
		{name: "no provider"},
		{name: "unresolved cursor", ep: latestSeqFailingProvider{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gaps int
			router := newReconcileRouter(routerTestLeg, routerSink{
				requestResync: func(reason string) {
					if reason == "bead-event-gap" {
						gaps++
					}
				},
			}, io.Discard)
			cs := &controllerState{wake: &controllerWake{router: router}}
			if tc.ep != nil {
				cs.eventProv = tc.ep
			}
			cs.startBeadEventWatcher(context.Background())
			if gaps != 1 {
				t.Errorf("gaps = %d, want 1", gaps)
			}
		})
	}
}

type latestSeqFailingProvider struct{ events.Provider }

func (latestSeqFailingProvider) LatestSeq() (uint64, error) { return 0, errors.New("log unreadable") }

// Kills: the routed wake reporting every enqueue landed (a replay burst logs
// a line per event, API-018) or never. It reports landed at most once per
// interval of its clock.
func TestRoutedWakeReportsLandedAtMostOncePerInterval(t *testing.T) {
	router := newReconcileRouter(routerTestLeg, routerSink{
		addSession:    func(rowKey, routeReason) {},
		wakeAllocator: func(routeReason) {},
		requestResync: func(string) {},
	}, io.Discard)
	now := time.Unix(1_800_000_000, 0)
	w := &controllerWake{router: router, now: func() time.Time { return now }}
	var got []bool
	for _, step := range []time.Duration{0, 0, routedLandedEvery / 2, routedLandedEvery / 2, 0, routedLandedEvery} {
		now = now.Add(step)
		got = append(got, w.Enqueue(wakeReasonProviderEvent, reconcilekey.Session("gc-1")))
	}
	if want := []bool{true, false, false, true, false, true}; !slices.Equal(got, want) {
		t.Errorf("landed = %v, want %v", got, want)
	}
}

// Kills: a routed bead event still poking the legacy tick, or not reaching
// the router; replays included (F1). The event's row is enqueued as a replay.
func TestControllerWakeOnBeadEventRoutesUnderRouter(t *testing.T) {
	var rows []rowKey
	var kinds []string
	router := newReconcileRouter(routerTestLeg, routerSink{
		addSession: func(k rowKey, r routeReason) {
			rows = append(rows, k)
			kinds = append(kinds, r.Kind)
		},
		wakeAllocator: func(routeReason) {},
		requestResync: func(string) {},
	}, io.Discard)
	pokeCh, dispatchCh := make(chan struct{}, 1), make(chan struct{}, 1)
	w := newLegacyWake(pokeCh, dispatchCh)
	w.router = router

	evt := beadEvent(t, events.BeadUpdated, routerSessionBead("gc-1", map[string]string{"session_name": "worker"}))
	w.OnBeadEvent(evt, false, true)
	w.OnBeadEvent(evt, true, true)
	if drainSignal(pokeCh) || drainSignal(dispatchCh) {
		t.Error("a routed bead event poked the legacy reconciler")
	}
	if want := []rowKey{{Leg: routerTestLeg, ID: "gc-1"}, {Leg: routerTestLeg, ID: "gc-1"}}; !slices.Equal(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	if want := []string{routeReasonEvent, routeReasonReplay}; !slices.Equal(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
}

// Kills: the bead event watcher handing the router appliedToSessions=false
// for a session bead in the sessions store, so v2 treats its own rows as
// relics on another leg.
func TestApplyBeadEventMarksSessionsStoreEvents(t *testing.T) {
	var rows []rowKey
	router := newReconcileRouter(routerTestLeg, routerSink{
		addSession:    func(k rowKey, _ routeReason) { rows = append(rows, k) },
		wakeAllocator: func(routeReason) {},
		requestResync: func(string) {},
	}, io.Discard)
	cs := &controllerState{cityBeadStore: beads.NewMemStore(), wake: &controllerWake{router: router}}
	cs.applyBeadEventToStores(beadEvent(t, events.BeadUpdated, routerSessionBead("gc-7", map[string]string{"session_name": "w"})))
	if want := []rowKey{{Leg: routerTestLeg, ID: "gc-7"}}; !slices.Equal(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
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

// Kills: v2SessionsLeg drifting from the sessions census's own label for the
// sessions-class store, on a city that relocates nothing.
func TestV2SessionsLegIsTheSessionCensusLabel(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "leg-city"}}
	legs, err := sessionCensusStoreCandidates(t.TempDir(), cfg, beads.NewMemStore(), nil, nil)
	if err != nil || len(legs) == 0 {
		t.Fatalf("session census legs = %v, err %v", legs, err)
	}
	if got := v2SessionsLeg(cfg); got != legs[0].ref {
		t.Errorf("v2SessionsLeg = %q, want the session census's leading label %q", got, legs[0].ref)
	}
}

// assertV2WakeWired checks a v2 entry point's composition: the API state's
// wake is the runtime's, and it routes to the runtime's own v2 router.
func assertV2WakeWired(t *testing.T, runtimes []*CityRuntime) {
	t.Helper()
	if len(runtimes) == 0 {
		t.Fatal("no controllerState was wired")
	}
	for _, cr := range runtimes {
		if cr.v2 == nil || cr.wake == nil || cr.wake.router == nil {
			t.Fatalf("v2 city runtime: v2 %v wake %v, want a v2 runtime whose router is on the wake", cr.v2, cr.wake)
		}
		if cs := cr.cs; cs == nil || cs.wake != cr.wake || cs.wake.router != cr.v2.router {
			t.Fatal("the API state's wake is not the runtime's v2-routed wake: API keys would miss the v2 queue")
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

// Kills: a failed sessions census swallowed into an empty one (m32): a
// failed read must never look like an empty city, and boot must not declare
// ready on it. The census returns the error; boot retries it and stays not
// ready until its context ends.
func TestV2SessionsCensusFailedReadBlocksReady(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, false)
	stderr := &synchronizedBuffer{}
	cr.stderr = stderr
	rt, _ := attachTestV2(t, cr)
	cr.cs.cityBeadStore = failingListStore{Store: store}

	if infos, err := cr.v2SessionsCensus(); err == nil {
		t.Fatalf("v2SessionsCensus = %v, nil on a failing store, want its error", infos)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- cr.bootV2(ctx, nil) }()
	awaitCond(t, func() bool {
		return strings.Contains(stderr.String(), "boot census failed") || len(result) == 1
	}, "the boot census failing")
	if rt.ready.Load() {
		t.Fatal("boot declared ready on a failed sessions census")
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

// Kills: appliedToSessions reported for an event whose bead landed in
// another store (m23a). A session-shaped bead under a rig's prefix is a relic
// on the rig's leg: a census input that wakes the allocator, never a row key
// on the sessions leg. The same bead under the city prefix is a row.
func TestApplyBeadEventRigSessionBeadRoutesCensusOnly(t *testing.T) {
	var rows []rowKey
	var allocs int
	router := newReconcileRouter(routerTestLeg, routerSink{
		addSession:    func(k rowKey, _ routeReason) { rows = append(rows, k) },
		wakeAllocator: func(routeReason) { allocs++ },
		requestResync: func(string) {},
	}, io.Discard)
	cs := identityTestController(beads.NewMemStore(), beads.NewMemStore())
	cs.wake = &controllerWake{router: router}

	cs.applyBeadEventToStores(beadEvent(t, events.BeadUpdated, routerSessionBead("rw-7", map[string]string{"session_name": "w"})))
	if len(rows) != 0 || allocs != 1 {
		t.Fatalf("rig session bead: rows %v allocator wakes %d, want no row and one census wake", rows, allocs)
	}
	cs.applyBeadEventToStores(beadEvent(t, events.BeadUpdated, routerSessionBead("ct-7", map[string]string{"session_name": "w"})))
	if want := []rowKey{{Leg: routerTestLeg, ID: "ct-7"}}; !slices.Equal(rows, want) {
		t.Errorf("city session bead: rows %v, want %v", rows, want)
	}
}

// Kills: the manual reload answered after the barrier resumed (m28), which
// TestCityRuntimeV2ReloadReplyBeforeResume cannot see because both orders
// reply before the next phase. A post-reload hook takes reloadMu, so the
// reply's slot clear blocks in whichever frame sends the reply; when the reply
// lands, the reconciles must still be held under the reload's name.
func TestCityRuntimeV2ReloadReplySentInsideTheBarrier(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, true, false)
	cr.activeReload.soft = false
	rt, _ := attachTestV2(t, cr)
	bootTestV2(t, cr)
	doneCh := cr.activeReload.doneCh
	locked := make(chan struct{})
	rt.barrier.afterPublish = append(rt.barrier.afterPublish, func(context.Context, reloadIntent, *reconcileEnv, *reloadControlReply) {
		cr.reloadMu.Lock() // the test unlocks it
		close(locked)
	})

	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		runFixtureTick(cr, "reload")
	}()
	awaitClose(t, locked, "the post-reload hook")
	awaitCond(t, func() bool { return len(doneCh) == 1 }, "the manual reload reply")
	holds := rt.sessions.Stats().Holds
	cr.reloadMu.Unlock()
	awaitClose(t, ticked, "the tick")
	if !slices.Contains(holds, v2HoldReload) {
		t.Errorf("holds when the reply landed = %v, want %q: the reply went out after the barrier resumed", holds, v2HoldReload)
	}
}

// stopOrderProvider records whether the v2 runtime had stopped at each
// session listing; run's shutdown lists last.
type stopOrderProvider struct {
	*runtime.Fake
	rt                *v2Runtime
	stoppedAtLastList atomic.Bool
}

func (p *stopOrderProvider) ListRunning(prefix string) ([]string, error) {
	p.rt.mu.Lock()
	stopped := p.rt.stopped
	p.rt.mu.Unlock()
	p.stoppedAtLastList.Store(stopped)
	return p.Fake.ListRunning(prefix)
}

// Kills: the v2 runtime stopped after run's shutdown (m33), so its workers
// would still reconcile while shutdown stops the sessions. Shutdown's
// session listing finds the runtime already stopped.
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
		t.Error("shutdown listed the sessions to stop while the v2 runtime still ran")
	}
}

// Kills: v2SessionsLeg drifting from the session census's label for the
// sessions-class store on a city that moves the infrastructure classes to a
// shared binding (mleg-nosplit): the leg is the binding's class ref, which
// the census, planned over the city's registered routes, leads with.
func TestV2SessionsLegIsTheSplitCensusLabel(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(cityPath)
	cfg.Workspace.Name = "leg-city"
	work, binding := beads.NewMemStore(), beads.NewMemStore()
	routes := &storageRoutes{binding: "infra", stores: map[coordclass.Class]beads.Store{
		coordclass.ClassGraph:     binding,
		coordclass.ClassSessions:  binding,
		coordclass.ClassMessaging: binding,
		coordclass.ClassOrders:    binding,
		coordclass.ClassNudges:    binding,
	}}
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return work })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	legs, err := sessionCensusStoreCandidates(cityPath, cfg, binding, nil, nil)
	if err != nil || len(legs) < 2 {
		t.Fatalf("split session census legs = %v, err %v, want the binding and the work store", legs, err)
	}
	if got := v2SessionsLeg(cfg); got != legs[0].ref || !strings.HasPrefix(got, "class:") {
		t.Errorf("v2SessionsLeg = %q, want the census's leading binding label %q", got, legs[0].ref)
	}
}

// Pins what P3-7's L1 obligation rests on: the sessions leg is spelled from
// Workspace.Name, not the resolved city name, so a reload that renames the
// workspace moves the census's label while the router keeps the leg it
// latched at boot. Both agree for any one config.
func TestV2SessionsLegFollowsWorkspaceName(t *testing.T) {
	var labels []string
	for _, name := range []string{"old", "new"} {
		cfg := &config.City{Workspace: config.Workspace{Name: name}, ResolvedWorkspaceName: "site-name"}
		legs, err := sessionCensusStoreCandidates(t.TempDir(), cfg, beads.NewMemStore(), nil, nil)
		if err != nil || len(legs) == 0 {
			t.Fatalf("%s: session census legs = %v, err %v", name, legs, err)
		}
		if got := v2SessionsLeg(cfg); got != legs[0].ref {
			t.Errorf("%s: v2SessionsLeg = %q, census leads with %q", name, got, legs[0].ref)
		}
		labels = append(labels, legs[0].ref)
	}
	if want := []string{"city:old", "city:new"}; !slices.Equal(labels, want) {
		t.Errorf("census labels across a rename = %v, want %v", labels, want)
	}
}

// Kills: a host bound to a runtime that already started (mbind-nopanic),
// whose goroutines read the host unsynchronized with the bind.
func TestV2BindHostAfterStartPanics(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	rt, _ := attachTestV2(t, cr)
	bootTestV2(t, cr)
	defer func() {
		if recover() == nil {
			t.Error("bindHost on a started runtime did not panic")
		}
	}()
	rt.bindHost(cr.newV2Host())
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

// v2QueueRecordFields are the reconcile_queue record's fields (§4.13). They
// are pinned: a trace consumer reads them by name.
var v2QueueRecordFields = []string{
	"adds", "allocator_duty", "allocator_failures", "allocator_last_pass_ms", "allocator_passes", "allocator_wakes",
	"allocator_wakes_suppressed",
	"bead_event_latency_p50_ms", "bead_event_latency_p99_ms", "boot", "boot_ms",
	"deferred", "depth_hot", "depth_resync", "dirty", "dropped_adds", "fs_gate", "holds", "keys",
	"latency_p50_ms", "latency_p99_ms", "legacy_session_entries", "longest_in_flight_ms",
	"oldest_hot_ms", "oldest_resync_ms", "processing", "reconcile_failures", "reconcile_panics",
	"reconciles", "resync_requests", "resync_superseded", "resync_sweep_ms",
	"router_events", "router_keys_out", "router_panics", "router_replays", "router_undecodable",
	"router_unresolved", "session_reasons", "timers", "work_p50_ms", "work_p99_ms",
}

// queueRecords returns the reconcile_queue records, in order.
func queueRecords(records []SessionReconcilerTraceRecord) []SessionReconcilerTraceRecord {
	var out []SessionReconcilerTraceRecord
	for _, r := range records {
		if r.RecordType == TraceRecordOperation && r.Fields["operation_name"] == "reconcile_queue" {
			out = append(out, r)
		}
	}
	return out
}

// Kills: the reconcile_queue record missing from the v2 maintenance tick,
// recorded twice in one, its fields renamed, its adds cumulative instead of
// since the last tick, or recorded by a legacy tick (whose trace must not
// change); and the m14/m15 mutations of TestCityRuntimeV2FullTickRunsOnlyMaintenance
// as they show in the record: the v2 tick running the legacy phase list (the
// guard counts its refusals) or tracing as a controller tick.
func TestV2MaintenanceTraceRecordsReconcileQueue(t *testing.T) {
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
		recs := queueRecords(records)
		if len(recs) != 2 || recs[0].TickID == recs[1].TickID {
			t.Fatalf("reconcile_queue records = %d over two v2 ticks, want one per tick", len(recs))
		}
		first, second := recs[0], recs[1]
		details := map[string]bool{}
		for _, r := range records {
			if r.TickID == first.TickID && r.TriggerDetail != "" {
				details[r.TriggerDetail] = true
			}
		}
		if first.SiteCode != TraceSiteReconcileQueue || !details["maintenance_tick"] || details["controller_tick"] {
			t.Errorf("record site %q in a trace with details %v, want %q in a maintenance_tick trace", first.SiteCode, details, TraceSiteReconcileQueue)
		}
		var names []string
		for name := range first.Fields {
			if name != "operation_name" {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		assertLinesEqual(t, "reconcile_queue fields", names, v2QueueRecordFields)
		if first.Fields["boot"] != v2BootReady || first.Fields["legacy_session_entries"] != float64(0) || first.Fields["reconciles"] != float64(1) {
			t.Errorf("record boot=%v legacy_session_entries=%v reconciles=%v, want ready, 0, 1 (the boot reconcile of the open row)",
				first.Fields["boot"], first.Fields["legacy_session_entries"], first.Fields["reconciles"])
		}
		if adds, _ := first.Fields["adds"].(map[string]any); adds["boot"] != float64(1) {
			t.Errorf("first record adds = %v, want the boot add", first.Fields["adds"])
		}
		if adds, _ := second.Fields["adds"].(map[string]any); len(adds) != 0 {
			t.Errorf("second record adds = %v, want none since the first", second.Fields["adds"])
		}
	})

	t.Run("legacy", func(t *testing.T) {
		cr, _ := newPhaseFixtureRuntime(t, false, true)
		runFixtureTick(cr, "patrol")
		if recs := queueRecords(closeTrace(t, cr)); len(recs) != 0 {
			t.Errorf("a legacy tick recorded reconcile_queue %d times, want never", len(recs))
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
	recs := queueRecords(closeTrace(t, cr))
	if len(recs) != 1 {
		t.Fatalf("reconcile_queue records = %d, want 1", len(recs))
	}
	if got := recs[0].Fields["legacy_session_entries"]; got != float64(2) {
		t.Errorf("legacy_session_entries = %v, want 2", got)
	}
}

// Kills: a store-less v2 city's record reading as a boot stuck on its census
// (MAINT-003: boot never runs there, and readiness proceeds). bootV2 latches
// no-store; the record does not re-read the store.
func TestCityRuntimeV2NoStoreQueueRecordSaysNoStore(t *testing.T) {
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
	if !cr.bootV2(context.Background(), nil) {
		t.Fatal("a store-less v2 city did not proceed to readiness")
	}
	trace := cr.beginTraceCycle("patrol", "maintenance_tick", nil)
	cr.recordV2Queue(trace)
	trace.end(TraceCompletionCompleted, traceRecordPayload{"phase": "tick"})
	recs := queueRecords(closeTrace(t, cr))
	if len(recs) != 1 {
		t.Fatalf("reconcile_queue records = %d, want 1", len(recs))
	}
	if got := recs[0].Fields["boot"]; got != "no-store" {
		t.Errorf("boot = %v, want no-store", got)
	}
}

// startupQueueRecords returns the boot values of the reconcile_queue records
// in the startup step's trace cycle, in order.
func startupQueueRecords(records []SessionReconcilerTraceRecord) []string {
	var tick string
	for _, r := range records {
		if r.RecordType == TraceRecordCycleResult && r.Fields["phase"] == "startup" {
			tick = r.TickID
		}
	}
	var boots []string
	for _, r := range queueRecords(records) {
		if r.TickID == tick {
			boots = append(boots, fmt.Sprint(r.Fields["boot"]))
		}
	}
	return boots
}

// awaitQueueRecord waits until the v2 runtime builds its next reconcile_queue
// record.
func awaitQueueRecord(t *testing.T, rt *v2Runtime) {
	t.Helper()
	rt.report.mu.Lock()
	rt.report.adds = nil
	rt.report.mu.Unlock()
	awaitCond(t, func() bool {
		rt.report.mu.Lock()
		defer rt.report.mu.Unlock()
		return rt.report.adds != nil
	}, "a reconcile_queue record")
}

// Kills: a boot stuck before ready leaving no record (records only on
// maintenance ticks, which start after ready), the boot patrol recording
// outside the startup step's trace, or the record reading no-store for a
// city that has a store (the store re-read, or no-store whenever not ready:
// N1); and the startup watchdog saying nothing of v2. A boot stuck on a boot
// key's first reconcile records coverage at every patrol, and a boot stuck on
// the allocator records allocator, in the startup step's trace; a boot whose
// census keeps failing records census while it waits to retry.
func TestV2BootPatrolRecordsWhatBootWaitsOn(t *testing.T) {
	setup := func(t *testing.T) (*CityRuntime, *v2Runtime, *v2Recorder, *synchronizedBuffer) {
		cr, _ := newPhaseFixtureRuntime(t, false, false)
		stderr := &synchronizedBuffer{}
		cr.stderr = stderr
		cr.cfg.Daemon.PatrolInterval = "10ms"
		rt, rec := attachTestV2(t, cr)
		return cr, rt, rec, stderr
	}
	startup := func(cr *CityRuntime) <-chan bool {
		done := make(chan bool, 1)
		go func() { done <- cr.startupReconcile(context.Background()) }()
		return done
	}
	finish := func(t *testing.T, done <-chan bool) {
		t.Helper()
		select {
		case ok := <-done:
			if !ok {
				t.Fatal("the v2 startup step did not complete")
			}
		case <-time.After(hangBudget):
			t.Fatalf("the v2 startup step did not complete within %s", hangBudget)
		}
	}

	t.Run("census", func(t *testing.T) {
		cr, rt, _, _ := setup(t)
		cr.cs.cityBeadStore = failingListStore{Store: cr.cs.cityBeadStore}
		trace := cr.beginTraceCycle("startup", "initial_reconcile", nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan bool, 1)
		go func() { done <- cr.bootV2(ctx, trace) }()
		awaitQueueRecord(t, rt)
		cancel()
		select {
		case ok := <-done:
			if ok {
				t.Fatal("bootV2 reported ready on a failing census")
			}
		case <-time.After(hangBudget):
			t.Fatalf("bootV2 did not return within %s of its context ending", hangBudget)
		}
		trace.end(TraceCompletionAborted, traceRecordPayload{"phase": "startup"})
		boots := startupQueueRecords(closeTrace(t, cr))
		if len(boots) == 0 || slices.ContainsFunc(boots, func(b string) bool { return b != v2BootCensus }) {
			t.Fatalf("startup trace boot records = %v, want census at every patrol while the census fails", boots)
		}
	})

	t.Run("coverage", func(t *testing.T) {
		cr, rt, rec, stderr := setup(t)
		release := make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(release) })
		t.Cleanup(releaseOnce)
		rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) {
			<-release
			return 0, nil
		})
		done := startup(cr)
		awaitQueueRecord(t, rt)
		awaitQueueRecord(t, rt)
		cr.startupReadinessWatchdog(context.Background(), make(chan struct{}), 0, time.Minute)
		if got := stderr.String(); !strings.Contains(got, "session reconciler v2: boot=coverage depth_hot=0 depth_resync=0 processing=1 longest_in_flight=") {
			t.Errorf("startup watchdog = %q, want the v2 boot state with a reconcile in flight", got)
		}
		releaseOnce()
		finish(t, done)
		boots := startupQueueRecords(closeTrace(t, cr))
		if len(boots) < 2 || boots[0] != v2BootCoverage || boots[1] != v2BootCoverage {
			t.Fatalf("startup trace boot records = %v, want coverage at every patrol while the boot key reconciles", boots)
		}
		for _, b := range boots {
			if b != v2BootCoverage && b != v2BootReady {
				t.Fatalf("startup trace boot records = %v, want coverage until ready", boots)
			}
		}
	})

	t.Run("allocator", func(t *testing.T) {
		cr, rt, rec, _ := setup(t)
		var primed atomic.Bool
		rec.setAllocator(func() error {
			if !primed.Load() {
				return errors.New("not yet")
			}
			return nil
		})
		done := startup(cr)
		awaitCond(t, func() bool { return rt.bootState() == v2BootAllocator }, "boot waiting on the allocator")
		awaitQueueRecord(t, rt)
		primed.Store(true)
		rt.router.Enqueue(wakeReasonAPI) // urgent: the next pass skips the failed pass's backoff
		finish(t, done)
		boots := startupQueueRecords(closeTrace(t, cr))
		if len(boots) == 0 || !slices.Contains(boots, v2BootAllocator) {
			t.Fatalf("startup trace boot records = %v, want allocator while the allocator fails", boots)
		}
		for _, b := range boots {
			if b != v2BootCoverage && b != v2BootAllocator && b != v2BootReady {
				t.Fatalf("startup trace boot records = %v, want coverage, then allocator, until ready", boots)
			}
		}
	})
}

// Kills: the record left out of a maintenance tick that panics (recorded only
// at the tick's normal end, or only when it completed). The aborted cycle
// still carries exactly one record.
func TestV2PanickedMaintenanceTickRecordsTheQueue(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, false)
	attachTestV2(t, cr)
	bootTestV2(t, cr)
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
	for _, r := range queueRecords(records) {
		if r.TickID == tick {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("reconcile_queue records in the aborted tick = %d, want 1", n)
	}
}
