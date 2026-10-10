package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// quiescenceRuntime is a CityRuntime over providerOwnedHealthFixture's city:
// one provider-owned rig whose provider script logs every op.
func quiescenceRuntime(t *testing.T, rigExtra string) (*CityRuntime, *runtime.Fake, string, string) {
	t.Helper()
	city, rig, logPath := providerOwnedHealthFixture(t, rigExtra)
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	resolveRigPaths(city, cfg.Rigs)
	sp := runtime.NewFake()
	return &CityRuntime{cityPath: city, cfg: cfg, sp: sp, stdout: io.Discard, stderr: io.Discard}, sp, rig, logPath
}

// A suspended city with nothing running is quiescent: the tick stops before
// its phases, every cache pauses, and the provider-owned scopes are stopped
// once. A session still running keeps it out of quiescence.
func TestBeadsQuiescenceRetiresASuspendedCityOnceItHasDrained(t *testing.T) {
	cr, sp, _, logPath := quiescenceRuntime(t, "")
	t.Setenv("GC_SUSPENDED", "1")
	cr.cs = &controllerState{beadsQuiescent: new(atomic.Bool)}

	if err := sp.Start(context.Background(), "city--worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if cr.enterBeadsQuiescenceIfDue(context.Background()) {
		t.Fatal("quiescent while a session is still running")
	}
	if ops := providerOpsLogged(t, logPath); ops != "" {
		t.Fatalf("scopes stopped before the sessions drained: %q", ops)
	}

	if err := sp.Stop("city--worker"); err != nil {
		t.Fatal(err)
	}
	if !cr.enterBeadsQuiescenceIfDue(context.Background()) {
		t.Fatal("a suspended city with nothing running is not quiescent")
	}
	if !cr.beadsQuiescent.Load() || !cr.cs.beadsQuiescent.Load() {
		t.Fatal("quiescence was not published to the runtime and its caches")
	}
	// The drain's own bead bookkeeping gets a whole tick before the pair goes.
	if ops := providerOpsLogged(t, logPath); ops != "" {
		t.Fatalf("provider ops on the first drained tick = %q, want none yet", ops)
	}
	cr.enterBeadsQuiescenceIfDue(context.Background())
	if ops := providerOpsLogged(t, logPath); ops != "stop" {
		t.Fatalf("provider ops = %q, want one stop", ops)
	}
	cr.enterBeadsQuiescenceIfDue(context.Background())
	if ops := providerOpsLogged(t, logPath); ops != "stop" {
		t.Fatalf("provider ops after another quiescent tick = %q, want still one stop", ops)
	}

	t.Setenv("GC_SUSPENDED", "")
	if cr.enterBeadsQuiescenceIfDue(context.Background()) || cr.cs.beadsQuiescent.Load() {
		t.Fatal("a resumed city is still quiescent")
	}
	if cr.retiredScopes.done(cr.beadsScopeRoots()[1], time.Time{}) {
		t.Fatal("a resumed scope is still recorded as retired")
	}
}

// A quiescent city retires its pairs only once the controller's asynchronous
// store work has finished. An autoclose run dispatched before the city went
// quiescent is still issuing bd reads: retired under it, the pair would be
// restarted by its next read and stay up until the next retire
// (acceptance: TestProxiedSuspensionIsQuiescenceSuspendedCity, whose
// shortened window caught a drained wisp's autoclose reads after the stop).
func TestBeadsQuiescenceRetiresOnlyAfterInFlightStoreWorkFinishes(t *testing.T) {
	cr, _, _, logPath := quiescenceRuntime(t, "")
	t.Setenv("GC_SUSPENDED", "1")
	cr.cs = &controllerState{beadsQuiescent: new(atomic.Bool)}

	end := cr.cs.beginStoreWork()
	for i := 0; i < 3; i++ {
		if !cr.enterBeadsQuiescenceIfDue(context.Background()) {
			t.Fatal("a suspended city with nothing running is not quiescent")
		}
	}
	if ops := providerOpsLogged(t, logPath); ops != "" {
		t.Fatalf("provider ops while an autoclose run was in flight = %q, want none", ops)
	}

	end()
	cr.enterBeadsQuiescenceIfDue(context.Background())
	if ops := providerOpsLogged(t, logPath); ops != "" {
		t.Fatalf("provider ops on the first tick with no store work in flight = %q, want none yet", ops)
	}
	cr.enterBeadsQuiescenceIfDue(context.Background())
	if ops := providerOpsLogged(t, logPath); ops != "stop" {
		t.Fatalf("provider ops = %q, want one stop once the store work finished", ops)
	}
}

// Store work is counted from dispatch, not from when its goroutine starts, so
// a run queued just before the city goes quiescent holds the retire too.
func TestTrackStoreWorkCountsFromDispatch(t *testing.T) {
	cs := &controllerState{}
	if cs.storeWorkInFlight() {
		t.Fatal("store work in flight on a new controller state")
	}
	run := cs.trackStoreWork(func() {})
	if !cs.storeWorkInFlight() {
		t.Fatal("dispatched store work is not counted until it runs")
	}
	run()
	if cs.storeWorkInFlight() {
		t.Fatal("store work still counted after it finished")
	}
	var nilState *controllerState
	if nilState.storeWorkInFlight() {
		t.Fatal("a nil controller state reports store work in flight")
	}
}

// A suspended rig's pair is stopped once its sessions have drained, and only
// then.
func TestRetireSuspendedRigScopeAfterItsSessionsDrain(t *testing.T) {
	cr, sp, rig, logPath := quiescenceRuntime(t, "suspended_on_start = true\n")
	t.Setenv("GC_SUSPENDED", "")
	p := &tickPass{ctx: context.Background(), sessionBeads: newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{
		ID: "gc-1", SessionName: "city--r1-worker", WorkDir: filepath.Join(rig, "work"), State: "active",
	}})}
	if err := sp.Start(context.Background(), "city--r1-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	cr.tickRetireSuspendedRigScopes(p)
	if ops := providerOpsLogged(t, logPath); ops != "" {
		t.Fatalf("the rig was stopped while its session ran: %q", ops)
	}
	if err := sp.Stop("city--r1-worker"); err != nil {
		t.Fatal(err)
	}
	cr.tickRetireSuspendedRigScopes(p)
	cr.tickRetireSuspendedRigScopes(p)
	if ops := providerOpsLogged(t, logPath); ops != "stop" {
		t.Fatalf("provider ops = %q, want exactly one stop", ops)
	}
}

// A suspended rig's store is not primed: any read would go to bd and restart
// its retired proxy.
func TestWrapWithCachingStoreLeavesASuspendedRigCold(t *testing.T) {
	backing := &primeCountingStore{Store: beads.NewMemStore()}
	wrapWithCachingStore(context.Background(), backing, nil, false)
	if backing.lists != 0 {
		t.Fatalf("a suspended rig's store was listed %d time(s) at wrap", backing.lists)
	}
}

type primeCountingStore struct {
	beads.Store
	lists int
}

func (s *primeCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	return s.Store.List(q)
}

// The order-tracking watchdogs and `gc order sweep-tracking` leave a
// suspended rig alone.
func TestOrderTrackingSweepTargetsSkipSuspendedRigs(t *testing.T) {
	cr, _, _, _ := quiescenceRuntime(t, "suspended_on_start = true\n")
	t.Setenv("GC_SUSPENDED", "")
	for _, target := range orderTrackingSweepTargetsForConfig(cr.cityPath, cr.cfg) {
		if strings.Contains(target.label, "r1") {
			t.Fatalf("sweep targets include the suspended rig: %+v", target)
		}
	}
}

// The core maintenance orders enumerate rigs through scope_bd.sh and their own
// jq filters; every one of them leaves suspended rigs out.
func TestCoreMaintenanceScriptsSkipSuspendedRigs(t *testing.T) {
	core := filepath.Join(repoRootForLint(t), "internal", "bootstrap", "packs", "core", "assets", "scripts")
	dolt := filepath.Join(repoRootForLint(t), "examples", "bd", "dolt", "assets", "scripts")
	for _, path := range []string{
		filepath.Join(core, "scope_bd.sh"),
		filepath.Join(core, "orphan-sweep.sh"),
		filepath.Join(core, "renudge-stale-human-gates.sh"),
		filepath.Join(core, "cascade-nudge-on-blocker-close.sh"),
		filepath.Join(core, "notify-on-human-gate-creation.sh"),
		filepath.Join(core, "cross-rig-deps.sh"),
		filepath.Join(dolt, "mol-dog-backup.sh"),
	} {
		name := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), ".suspended") {
			t.Errorf("%s enumerates rigs without skipping suspended ones", name)
		}
	}
}

// A resume and a new suspension that no tick observed in between still stop
// the scope again: the retirement belongs to the suspension episode.
func TestRetireSuspendedRigScopeAgainInANewEpisode(t *testing.T) {
	cr, _, _, logPath := quiescenceRuntime(t, "")
	t.Setenv("GC_SUSPENDED", "")
	p := &tickPass{ctx: context.Background(), sessionBeads: newSessionBeadSnapshotFromInfos(nil)}
	suspend := func(v bool) {
		t.Helper()
		if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cr.cityPath, "r1", &v); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond) // distinct UpdatedAt stamps
	}
	suspend(true)
	cr.tickRetireSuspendedRigScopes(p)
	cr.tickRetireSuspendedRigScopes(p)
	suspend(false)
	suspend(true)
	cr.tickRetireSuspendedRigScopes(p)
	if ops := providerOpsLogged(t, logPath); ops != "stop\nstop" {
		t.Fatalf("provider ops = %q, want a stop per suspension episode", ops)
	}
}

// The completions sweep leaves a suspended rig's store out of its fan.
func TestCompletionReconcileInputsSkipSuspendedRigs(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "")
	cs := &controllerState{
		cityPath: t.TempDir(),
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Rigs:      []config.Rig{{Name: "alpha", Path: "alpha"}, {Name: "beta", Path: "beta", SuspendedOnStart: true}},
		},
		cityBeadStore: beads.NewMemStore(),
		beadStores:    map[string]beads.Store{"alpha": beads.NewMemStore(), "beta": beads.NewMemStore()},
		eventProv:     events.NewFake(),
	}
	_, fan := cs.completionReconcileInputs(reconcilePlane)
	if len(fan) != 2 {
		t.Fatalf("the sweep fans out to %d store(s), want 2 (city work + the unsuspended rig)", len(fan))
	}
}

// gc start's is_blocked repair leaves suspended scopes cold, and the
// controller runs it for a scope on the first tick after that scope resumes.
func TestBlockedRepairSkipsSuspendedScopesAndRunsOnResume(t *testing.T) {
	cr, _, rig, _ := quiescenceRuntime(t, "")
	t.Setenv("GC_SUSPENDED", "")
	scopes := []blockedRepairScope{{id: "city", root: cr.cityPath}, {id: "rig/r1", root: rig}}

	suspendRig := func(v bool) {
		t.Helper()
		if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cr.cityPath, "r1", &v); err != nil {
			t.Fatal(err)
		}
	}
	suspendRig(true)
	kept := withoutSuspendedRepairScopes(scopes, suspendedBeadsScopes(cr.cityPath, cr.cfg))
	if len(kept) != 1 || kept[0].id != "city" {
		t.Fatalf("start repair scopes = %+v, want only the city", kept)
	}

	oldRepair, oldCandidates := resumeRepairBlockedFlags, resumeRepairCandidates
	t.Cleanup(func() { resumeRepairBlockedFlags, resumeRepairCandidates = oldRepair, oldCandidates })
	resumeRepairCandidates = func(string, *config.City) []blockedRepairScope { return scopes }
	var repaired []string
	resumeRepairBlockedFlags = func(_ string, _ *config.City, got []blockedRepairScope, _ io.Writer, _ string) {
		for _, s := range got {
			repaired = append(repaired, s.id)
		}
	}
	cr.enterBeadsQuiescenceIfDue(context.Background()) // first tick: rig suspended
	cr.enterBeadsQuiescenceIfDue(context.Background()) // still suspended
	if len(repaired) != 0 {
		t.Fatalf("repaired %v while the rig was suspended", repaired)
	}
	suspendRig(false)
	cr.enterBeadsQuiescenceIfDue(context.Background())
	cr.enterBeadsQuiescenceIfDue(context.Background())
	if strings.Join(repaired, ",") != "rig/r1" {
		t.Fatalf("repaired %v after resume, want exactly rig/r1 once", repaired)
	}
}

// A suspended proxied scope's pair is stopped whenever it is found running,
// not once: a late touch that restarted it is undone on the next tick.
func TestRetireSuspendedProxiedScopeConvergesOnLiveState(t *testing.T) {
	cr, _, rig, logPath := quiescenceRuntime(t, "suspended_on_start = true\n")
	t.Setenv("GC_SUSPENDED", "")
	live := true
	old := suspendedScopePair
	t.Cleanup(func() { suspendedScopePair = old })
	suspendedScopePair = func(root string) (bool, bool) {
		if samePath(root, rig) {
			return true, live
		}
		return false, false
	}
	p := &tickPass{ctx: context.Background(), sessionBeads: newSessionBeadSnapshotFromInfos(nil)}
	cr.tickRetireSuspendedRigScopes(p) // drained once: settle
	cr.tickRetireSuspendedRigScopes(p) // stop
	live = false
	cr.tickRetireSuspendedRigScopes(p) // already down: nothing
	live = true                        // a straggler restarted it
	cr.tickRetireSuspendedRigScopes(p) // stop again
	if ops := providerOpsLogged(t, logPath); ops != "stop\nstop" {
		t.Fatalf("provider ops = %q, want a stop each time the pair was found running", ops)
	}
}

// Without a controller, gc rig suspend stops the rig's pair itself.
func TestRigSuspendWithoutControllerStopsThePair(t *testing.T) {
	cr, _, _, logPath := quiescenceRuntime(t, "")
	t.Setenv("GC_SUSPENDED", "")
	var stdout, stderr strings.Builder
	if code := doRigSuspend(fsys.OSFS{}, cr.cityPath, "r1", &stdout, &stderr); code != 0 {
		t.Fatalf("gc rig suspend = %d: %s", code, stderr.String())
	}
	if ops := providerOpsLogged(t, logPath); ops != "stop" {
		t.Fatalf("provider ops = %q, want the suspended rig stopped", ops)
	}
}

// A quiescent city's tick runs no bead-store phase: the fixture store sees no
// call at all.
func TestQuiescentCityTickTouchesNoStore(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, false)
	if err := cr.sp.Stop("worker"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_SUSPENDED", "1")
	before := len(store.recorded())
	runFixtureTick(cr, "patrol")
	if got := store.recorded()[before:]; len(got) != 0 {
		t.Fatalf("a quiescent tick touched the store: %v", got)
	}
	if !cr.beadsQuiescent.Load() {
		t.Fatal("the city was not quiescent")
	}
}

// countingOrderDispatcher counts dispatch passes.
type countingOrderDispatcher struct{ dispatches int }

func (d *countingOrderDispatcher) dispatch(context.Context, string, time.Time) { d.dispatches++ }
func (d *countingOrderDispatcher) drain(context.Context) bool                  { return true }

// A suspended city gets no order pass: neither dispatch nor the tracking and
// mail watchdogs, which read every scope's store.
func TestDispatchOrdersSkipsASuspendedCity(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, false)
	od := &countingOrderDispatcher{}
	cr.od = od
	cr.wispIndexMigrationApplied = true
	t.Setenv("GC_SUSPENDED", "1")
	before := len(store.recorded())
	cr.dispatchOrdersLocked(context.Background(), cr.cityPath, 0, cr.cfg)
	if od.dispatches != 0 {
		t.Fatalf("dispatched %d order pass(es) in a suspended city", od.dispatches)
	}
	if got := store.recorded()[before:]; len(got) != 0 {
		t.Fatalf("the order pass touched the store in a suspended city: %v", got)
	}
	t.Setenv("GC_SUSPENDED", "")
	cr.dispatchOrdersLocked(context.Background(), cr.cityPath, 0, cr.cfg)
	if od.dispatches != 1 {
		t.Fatalf("dispatches after resume = %d, want 1", od.dispatches)
	}
}

// A suspended rig's convergence loops wait: its store is not read.
func TestConvergenceTickSkipsASuspendedRig(t *testing.T) {
	cr, _, _, _ := quiescenceRuntime(t, "suspended_on_start = true\n")
	t.Setenv("GC_SUSPENDED", "")
	store := &opRecordingStore{Store: beads.NewMemStore()}
	scope := cr.newConvergenceScope("r1", store, "", nil, false)
	scope.needsStartupReconcile = true
	cr.convScopes = map[string]*convergenceScope{"r1": scope}
	cr.convergenceReqCh = make(chan convergenceRequest, 1)
	cr.convergenceTick(context.Background())
	cr.convergenceStartupReconcile(context.Background())
	if got := store.recorded(); len(got) != 0 {
		t.Fatalf("convergence read a suspended rig's store: %v", got)
	}
}

// The closed-bead worktree reaper is not handed a suspended rig's store.
func TestReapClosedBeadWorktreesSkipsASuspendedRig(t *testing.T) {
	cr, _, _, _ := quiescenceRuntime(t, "suspended_on_start = true\n")
	t.Setenv("GC_SUSPENDED", "")
	cr.standaloneRigStores = map[string]beads.Store{"r1": beads.NewMemStore()}
	enabled := true
	cr.cfg.Daemon.AutoReapClosedBeadWorktrees = &enabled
	old := tickReapClosedBeadWorktreesFn
	t.Cleanup(func() { tickReapClosedBeadWorktreesFn = old })
	var handed map[string]beads.Store
	tickReapClosedBeadWorktreesFn = func(_ string, _ *config.City, rigStores map[string]beads.Store, _ []string, _ bool, _ events.Recorder, _ *reapSkipTracker, _ io.Writer) reapReport {
		handed = rigStores
		return reapReport{}
	}
	p := &tickPass{ctx: context.Background(), sessionBeads: newSessionBeadSnapshotFromInfos(nil)}
	cr.tickReapClosedBeadWorktrees(p)
	if _, ok := handed["r1"]; ok {
		t.Fatalf("the worktree reaper was handed the suspended rig's store: %v", handed)
	}
}

// A rig suspended at runtime stops its cache's periodic full scan from the
// next tick on, with no reload; resuming lets it scan again.
func TestRigCacheReconcilePausesWhileTheRigIsSuspended(t *testing.T) {
	cs := &controllerState{beadsQuiescent: new(atomic.Bool)}
	backing := &primeCountingStore{Store: beads.NewMemStore()}
	cache := beads.NewCachingStore(backing, nil, cs.rigReconcileGate("r1"))
	cs.setSuspendedRigs(map[string]bool{"r1": true})
	cache.ReconcileIfDueForTest()
	if backing.lists != 0 {
		t.Fatalf("a suspended rig's cache scanned its store %d time(s)", backing.lists)
	}
	cs.setSuspendedRigs(map[string]bool{})
	cache.ReconcileIfDueForTest()
	if backing.lists == 0 {
		t.Fatal("a resumed rig's cache did not scan")
	}
}

// A quiescent city still supervises its workspace services: they are not
// suspended with the city, so a crashed one must still be seen.
func TestQuiescentCityTickStillTicksWorkspaceServices(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	if cr.svc == nil {
		t.Fatal("the fixture runtime has no workspace service manager")
	}
	if err := cr.sp.Stop("worker"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_SUSPENDED", "1")
	runFixtureTick(cr, "patrol")
	if !cr.beadsQuiescent.Load() {
		t.Fatal("the city was not quiescent")
	}
	found := false
	for _, op := range tickOperationRecords(t, cr) {
		found = found || strings.HasSuffix(op, " workspace_service_tick")
	}
	if !found {
		t.Fatal("a quiescent tick did not tick workspace services")
	}
}
