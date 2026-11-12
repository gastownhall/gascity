package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// wispGCLaneTestWait bounds every wait on a lane goroutine in these tests.
const wispGCLaneTestWait = 10 * time.Second

// blockingWispGC is a tracker that is always due and whose purge blocks until
// the test releases it. Each purge sends on started first.
type blockingWispGC struct {
	started chan struct{}
	release chan struct{}
	runs    *atomic.Int32
}

func newBlockingWispGC() *blockingWispGC {
	return &blockingWispGC{started: make(chan struct{}, 8), release: make(chan struct{}), runs: &atomic.Int32{}}
}

func (b *blockingWispGC) shouldRun(time.Time) bool { return true }

func (b *blockingWispGC) runGC(wispGCLegs, beads.GraphStore, beads.SessionStore, beads.MailStore, time.Time) (int, error) {
	b.runs.Add(1)
	b.started <- struct{}{}
	<-b.release
	return 0, nil
}

// awaitPurgeStart waits for b's next purge to start.
func (b *blockingWispGC) awaitPurgeStart(t *testing.T) {
	t.Helper()
	select {
	case <-b.started:
	case <-time.After(wispGCLaneTestWait):
		t.Fatal("no wisp GC purge started")
	}
}

// countingWispGC is a tracker that is always due and counts its purges.
type countingWispGC struct {
	runs *atomic.Int32
}

func (c countingWispGC) shouldRun(time.Time) bool { return true }

func (c countingWispGC) runGC(wispGCLegs, beads.GraphStore, beads.SessionStore, beads.MailStore, time.Time) (int, error) {
	c.runs.Add(1)
	return 0, nil
}

// newWispGCLaneRuntime is a bare runtime with a city store and wg as its
// wisp GC tracker: enough for a tick and for a lane pass.
func newWispGCLaneRuntime(t *testing.T, wg wispGC) (*CityRuntime, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "test-city",
		cfg:                 &config.City{},
		sp:                  runtime.NewFake(),
		standaloneCityStore: beads.NewMemStore(),
		wg:                  wg,
		rec:                 events.Discard,
		logPrefix:           "test-city",
		stdout:              &stdout,
		stderr:              &stderr,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}
	return cr, &stdout, &stderr
}

func runBareTick(cr *CityRuntime, trigger string) {
	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, trigger)
}

// runLanePass runs one lane pass on the test goroutine and reports whether it
// purged.
func runLanePass(cr *CityRuntime, wake bool) bool {
	ran := false
	cr.runWispGCLanePass(context.Background(), wake, &ran)
	return ran
}

// completesWithin runs fn and fails unless it returns within the wait.
func completesWithin(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(wispGCLaneTestWait):
		t.Fatalf("%s did not complete while a wisp GC purge was blocked", what)
	}
}

// Kills: the purge back in the tick. A due tracker gets no purge from a tick,
// only a wake of its lane.
func TestCityRuntimeTick_DoesNotRunWispGCPurge(t *testing.T) {
	var runs atomic.Int32
	cr, _, _ := newWispGCLaneRuntime(t, countingWispGC{runs: &runs})

	runBareTick(cr, "patrol")

	if got := runs.Load(); got != 0 {
		t.Fatalf("the tick ran the wisp GC purge %d time(s); want 0", got)
	}
	select {
	case <-cr.wispGCLaneOf().wakeCh:
	default:
		t.Fatal("the tick did not wake the wisp GC lane")
	}
}

// Kills: a tick that waits for a purge. With the lane's purge blocked in its
// store call, a legacy tick and a v2 maintenance tick each run every phase
// to the end, and neither calls the purge itself.
func TestWispGCLane_TickCompletesWhilePurgeBlocked(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		wg := newBlockingWispGC()
		cr, _, _ := newWispGCLaneRuntime(t, wg)
		stop := cr.startWispGCLane(context.Background())
		defer func() {
			close(wg.release)
			stop()
		}()

		runBareTick(cr, "patrol")
		wg.awaitPurgeStart(t)
		completesWithin(t, "a legacy tick", func() { runBareTick(cr, "poke") })
		if got := wg.runs.Load(); got != 1 {
			t.Fatalf("purges = %d, want 1 (the lane's; the ticks run none)", got)
		}
	})
	t.Run("v2-maintenance", func(t *testing.T) {
		cr, store := newPhaseFixtureRuntime(t, false, true)
		armMaintenanceLanes(t, cr, store)
		wg := newBlockingWispGC()
		cr.wg = wg
		stop := cr.startWispGCLane(context.Background())
		defer func() {
			close(wg.release)
			stop()
		}()
		runV2Phases := func() bool {
			p := &tickPass{ctx: context.Background(), dirty: cr.configDirty, trigger: "patrol", prevPoolRunning: new(map[string]bool)}
			return cr.runTickPhases(p, v2TickPhases)
		}

		runV2Phases()
		wg.awaitPurgeStart(t)
		completed := false
		completesWithin(t, "a v2 maintenance tick", func() { completed = runV2Phases() })
		if !completed {
			t.Fatal("the v2 maintenance tick stopped before its last phase")
		}
		if got := wg.runs.Load(); got != 1 {
			t.Fatalf("purges = %d, want 1 (the lane's; the ticks run none)", got)
		}
	})
}

func TestWispGCLane_LogsPurgeCountWithNonFatalError(t *testing.T) {
	cr, stdout, stderr := newWispGCLaneRuntime(t, fixedWispGC{purged: 2, err: fmt.Errorf("delete failed")})

	if !runLanePass(cr, true) {
		t.Fatal("the lane did not purge on a wake with a due tracker")
	}

	if !strings.Contains(stderr.String(), "test-city: wisp gc: delete failed") {
		t.Fatalf("stderr = %q, want wisp gc error", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Bead GC: purged 2 expired bead(s)") {
		t.Fatalf("stdout = %q, want purge count despite non-fatal error", stdout.String())
	}
}

func TestWispGCLane_PrefixesEachJoinedErrorLine(t *testing.T) {
	cr, _, stderr := newWispGCLaneRuntime(t, fixedWispGC{err: fmt.Errorf(
		"%s\n%s",
		"deleting expired bead \"mol-1\": delete failed",
		"listing closed molecule roots: list failed",
	)})
	cr.stdout = io.Discard

	runLanePass(cr, true)

	got := stderr.String()
	for _, want := range []string{
		"test-city: wisp gc: deleting expired bead \"mol-1\": delete failed\n",
		"test-city: wisp gc: listing closed molecule roots: list failed\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stderr = %q, want line %q", got, want)
		}
	}
}

// notDueWispGC is a tracker whose interval has not elapsed.
type notDueWispGC struct{ runs *atomic.Int32 }

func (n notDueWispGC) shouldRun(time.Time) bool { return false }

func (n notDueWispGC) runGC(wispGCLegs, beads.GraphStore, beads.SessionStore, beads.MailStore, time.Time) (int, error) {
	n.runs.Add(1)
	return 0, nil
}

// Kills: a purge on a pass that must decline. Each case is one lane pass that
// starts no purge and does not count toward the lane's pacing.
func TestWispGCLane_DeclinesWithoutPurge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(cr *CityRuntime, runs *atomic.Int32)
		ctx   func() context.Context
		wake  bool
	}{
		{name: "canceled", wake: true, ctx: func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
		{name: "disabled", wake: true, setup: func(cr *CityRuntime, _ *atomic.Int32) { cr.wg = nil }},
		{name: "not-due", wake: true, setup: func(cr *CityRuntime, runs *atomic.Int32) { cr.wg = notDueWispGC{runs: runs} }},
		{name: "fs-pressure-shedding", wake: true, setup: func(cr *CityRuntime, _ *atomic.Int32) { cr.fsPressureShedding.Store(true) }},
		{name: "city-quiescent", wake: true, setup: func(cr *CityRuntime, _ *atomic.Int32) { cr.beadsQuiescent.Store(true) }},
		{name: "no-graph-store", wake: true, setup: func(cr *CityRuntime, _ *atomic.Int32) { cr.standaloneCityStore = nil }},
		{name: "timer-only", wake: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var runs atomic.Int32
			cr, _, _ := newWispGCLaneRuntime(t, countingWispGC{runs: &runs})
			if tc.setup != nil {
				tc.setup(cr, &runs)
			}
			ctx := context.Background()
			if tc.ctx != nil {
				ctx = tc.ctx()
			}
			ran := false
			cr.runWispGCLanePass(ctx, tc.wake, &ran)
			if got := runs.Load(); got != 0 || ran {
				t.Fatalf("purges = %d, ran = %v; want no purge", got, ran)
			}
		})
	}
}

// orderRecordingWispGC is a tracker that is always due and records each purge
// in order.
type orderRecordingWispGC struct{ order *orderedRuntimeEvents }

func (o orderRecordingWispGC) shouldRun(time.Time) bool { return true }

func (o orderRecordingWispGC) runGC(wispGCLegs, beads.GraphStore, beads.SessionStore, beads.MailStore, time.Time) (int, error) {
	o.order.record("purge")
	return 0, nil
}

// Kills: a purge that relies on its tick's managed-Dolt preflight, and a
// preflight moved ahead of the quiescence check. A wake the duty cycle
// deferred runs a purge-length after its tick, so the pass runs the preflight
// itself, before the purge; a pass on a quiescent city runs neither, so it
// never restarts a retired pair.
func TestWispGCLane_PreflightsManagedDoltBeforeItsPurge(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	cleanupManagedDoltTestCity(t, cityPath)
	order := &orderedRuntimeEvents{}
	cr, _, _ := newWispGCLaneRuntime(t, orderRecordingWispGC{order: order})
	cr.cityPath = cityPath
	cr.managedDoltOwned = func(string) (bool, error) { return true, nil }
	cr.managedDoltPort = func(string) string { return "" }
	cr.managedDoltHealth = func(context.Context, string) error {
		order.record("preflight")
		return nil
	}

	cr.beadsQuiescent.Store(true)
	if runLanePass(cr, true) {
		t.Fatal("a woken pass purged on a quiescent city")
	}
	if got := order.snapshot(); len(got) != 0 {
		t.Fatalf("a pass on a quiescent city ran %v, want neither the preflight nor a purge", got)
	}

	cr.beadsQuiescent.Store(false)
	if !runLanePass(cr, true) {
		t.Fatal("a woken pass with a due tracker did not purge")
	}
	if got, want := order.snapshot(), []string{"preflight", "purge"}; !slices.Equal(got, want) {
		t.Fatalf("a woken pass ran %v, want %v", got, want)
	}
}

// Kills: one purge per queued wake. Wakes that land before the lane delivers
// one join it, and the pass that takes it purges once.
func TestWispGCLane_RunsAtMostOnePurgePerWake(t *testing.T) {
	wg := newBlockingWispGC()
	cr, _, _ := newWispGCLaneRuntime(t, wg)
	lane := cr.wispGCLaneOf()
	for range 3 {
		lane.wake()
	}

	stop := cr.startWispGCLane(context.Background())
	wg.awaitPurgeStart(t)
	close(wg.release)
	stop()

	if got := wg.runs.Load(); got != 1 {
		t.Fatalf("purges = %d, want 1 for the coalesced wakes", got)
	}
}

// Kills: a purge that waits for the lane's timer, or a trigger that no longer
// reaches it. The lane is paced at its 10m fallback, so only a tick's wake
// can start the purge inside the wait: each kind of tick that used to reach
// the purge starts exactly one.
func TestWispGCLane_TickWakeStartsPurgeWithoutTimer(t *testing.T) {
	for _, trigger := range []string{"startup-poke", "poke", "patrol"} {
		t.Run(trigger, func(t *testing.T) {
			wg := newBlockingWispGC()
			cr, _, _ := newWispGCLaneRuntime(t, wg)
			stop := cr.startWispGCLane(context.Background())

			runBareTick(cr, trigger)
			wg.awaitPurgeStart(t)
			close(wg.release)
			stop()

			if got := wg.runs.Load(); got != 1 {
				t.Fatalf("purges = %d, want 1", got)
			}
		})
	}
}

// Kills: a wake from a tick that stopped before the wake slot. A tick whose
// context ends during the managed-Dolt preflight stops there and leaves no
// wake queued for the lane.
func TestWispGCLane_StoppedTickEnqueuesNoWake(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "100")
	var runs atomic.Int32
	cr, _, _ := newWispGCLaneRuntime(t, countingWispGC{runs: &runs})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &tickPass{ctx: ctx, dirty: new(atomic.Bool), trigger: "patrol", prevPoolRunning: new(map[string]bool)}

	if cr.runTickPhases(p, legacyTickPhases) {
		t.Fatal("the tick ran past the preflight on an ended context")
	}
	select {
	case <-cr.wispGCLaneOf().wakeCh:
		t.Fatal("a tick stopped before the wake slot woke the wisp GC lane")
	default:
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("purges = %d, want 0", got)
	}
}

// firstListGateStore blocks its first List until the test releases it.
type firstListGateStore struct {
	beads.Store
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func newFirstListGateStore() *firstListGateStore {
	return &firstListGateStore{Store: beads.NewMemStore(), started: make(chan struct{}), release: make(chan struct{})}
}

func (s *firstListGateStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.once.Do(func() {
		close(s.started)
		<-s.release
	})
	return s.Store.List(q)
}

// installedWispGC is the tracker the lane's next purge would use: it runs a
// timer-only pass, which installs any policy a reload staged and purges
// nothing, and reads the lane's tracker.
func installedWispGC(cr *CityRuntime) wispGC {
	runLanePass(cr, false)
	lane := cr.wispGCLaneOf()
	lane.passMu.Lock()
	defer lane.passMu.Unlock()
	return lane.active
}

// reloadWispGCConfigWithin rewrites the city config to
// wispGCReloadCityBase+extra and applies it, failing unless the reload
// completes within the wait, whatever a purge is doing, and is applied.
func reloadWispGCConfigWithin(t *testing.T, cr *CityRuntime, tomlPath, extra string) {
	t.Helper()
	if err := os.WriteFile(tomlPath, []byte(wispGCReloadCityBase+extra), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var reply reloadControlReply
	completesWithin(t, "a config reload", func() {
		lastProviderName := "fake"
		reply = cr.reloadConfigTraced(context.Background(), &lastProviderName, cr.cityPath, nil, reloadSourceManual)
	})
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reply.Outcome = %q (err %q), want %q", reply.Outcome, reply.Error, reloadOutcomeApplied)
	}
}

// Kills: a reload that waits for a purge, that writes the tracker under it,
// or whose policy never reaches the lane. A reload during a purge returns
// while the purge is held in its store call, and the tracker the lane
// installs next has the reloaded policy and the finished purge's lastRun and
// session purge cursor.
func TestWispGCLane_ReloadDuringPurgeCarriesItsProgress(t *testing.T) {
	cr, tomlPath := newWispGCReloadRuntime(t, "\n[daemon]\nwisp_gc_interval = \"1h\"\nwisp_ttl = \"24h\"\n")
	store := newFirstListGateStore()
	cr.cs.cityBeadStore = store
	purging, ok := installedWispGC(cr).(*memoryWispGC)
	if !ok {
		t.Fatalf("wisp GC tracker = %T, want *memoryWispGC", installedWispGC(cr))
	}
	purging.sessionPurgeCursor = &beads.SeekBoundary{CreatedAt: time.Now().Add(-48 * time.Hour), ID: "gcg-session-p"}
	passDone := make(chan bool, 1)
	go func() { passDone <- runLanePass(cr, true) }()
	select {
	case <-store.started:
	case <-time.After(wispGCLaneTestWait):
		t.Fatal("the purge never reached the store")
	}

	reloadWispGCConfigWithin(t, cr, tomlPath, "\n[daemon]\nwisp_gc_interval = \"2h\"\nwisp_ttl = \"48h\"\n")
	close(store.release)
	select {
	case ran := <-passDone:
		if !ran {
			t.Fatal("the lane pass did not purge")
		}
	case <-time.After(wispGCLaneTestWait):
		t.Fatal("the purge did not finish after its store call was released")
	}

	next, ok := installedWispGC(cr).(*memoryWispGC)
	if !ok || next == purging {
		t.Fatalf("installed tracker = %T (same as the purging one: %v), want the reloaded *memoryWispGC", installedWispGC(cr), next == purging)
	}
	if next.interval != 2*time.Hour || next.ttl != 48*time.Hour {
		t.Fatalf("installed interval/ttl = %v/%v, want 2h/48h", next.interval, next.ttl)
	}
	if next.lastRun.IsZero() || !next.lastRun.Equal(purging.lastRun) {
		t.Fatalf("installed lastRun = %v, want the finished purge's %v", next.lastRun, purging.lastRun)
	}
	if next.sessionPurgeCursor != purging.sessionPurgeCursor {
		t.Fatalf("installed session purge cursor = %+v, want the finished purge's %+v", next.sessionPurgeCursor, purging.sessionPurgeCursor)
	}
}

// Kills: a disable that leaves the lane purging, and a re-enable that
// inherits the cadence and cursor from before the disable. With a tracker due
// and mid-sweep, a disabling reload stops the next woken pass from purging,
// and a later enabling reload, whether a pass installed the disable first or
// both reloads land between passes, installs a fresh tracker: due at once,
// with no session purge cursor.
func TestWispGCLane_DisableThenEnableStartsAfresh(t *testing.T) {
	const enabled = "\n[daemon]\nwisp_gc_interval = \"1h\"\nwisp_ttl = \"24h\"\n"
	const disabled = "\n[daemon]\nshutdown_timeout = \"1s\"\n"
	for _, tc := range []struct {
		name          string
		passInBetween bool
	}{
		{name: "pass-between-reloads", passInBetween: true},
		{name: "both-reloads-between-passes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr, tomlPath := newWispGCReloadRuntime(t, enabled)
			prev := stampWispGCLastRun(t, cr, time.Now().Add(-2*time.Hour))
			prev.sessionPurgeCursor = &beads.SeekBoundary{CreatedAt: time.Now().Add(-48 * time.Hour), ID: "gcg-session-p"}

			applyWispGCReload(t, cr, tomlPath, disabled)
			if tc.passInBetween && runLanePass(cr, true) {
				t.Fatal("a woken pass purged after a reload disabled wisp GC")
			}
			applyWispGCReload(t, cr, tomlPath, enabled)

			next, ok := installedWispGC(cr).(*memoryWispGC)
			if !ok || next == prev {
				t.Fatalf("installed tracker = %T (same as before the disable: %v), want a new *memoryWispGC", installedWispGC(cr), next == prev)
			}
			if !next.lastRun.IsZero() || next.sessionPurgeCursor != nil {
				t.Fatalf("re-enabled tracker lastRun/cursor = %v/%+v, want a fresh tracker (zero, nil)", next.lastRun, next.sessionPurgeCursor)
			}
		})
	}
}

// startLanePassAsync runs one woken lane pass on its own goroutine and
// returns where it reports whether it purged.
func startLanePassAsync(cr *CityRuntime) <-chan bool {
	done := make(chan bool, 1)
	go func() { done <- runLanePass(cr, true) }()
	return done
}

// awaitLanePass waits for a pass startLanePassAsync started.
func awaitLanePass(t *testing.T, done <-chan bool) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(wispGCLaneTestWait):
		t.Fatal("the lane pass did not finish after its purge was released")
	}
}

// newQuiescenceLaneRuntime is quiescenceRuntime's city with a controller
// state over a memory city store and wg as its wisp GC tracker. The lane's
// managed-Dolt preflight checks health through a hook that logs no provider
// op, so the provider log holds only the retirement's ops; the preflight's
// own test is TestWispGCLane_PreflightsManagedDoltBeforeItsPurge.
func newQuiescenceLaneRuntime(t *testing.T, rigExtra string, wg wispGC) (*CityRuntime, string) {
	t.Helper()
	cr, _, _, logPath := quiescenceRuntime(t, rigExtra)
	cr.cs = &controllerState{beadsQuiescent: new(atomic.Bool), cityBeadStore: beads.NewMemStore()}
	cr.wg = wg
	cr.managedDoltHealth = func(context.Context, string) error { return nil }
	return cr, logPath
}

// Kills: a purge the retirement does not wait for. A purge in flight when
// the city is suspended with nothing running holds the stop of its stores
// until it returns, and a pass after the stop declines.
func TestWispGCLane_PurgeHoldsSuspendedCityRetirement(t *testing.T) {
	wg := newBlockingWispGC()
	cr, logPath := newQuiescenceLaneRuntime(t, "", wg)
	passDone := startLanePassAsync(cr)
	wg.awaitPurgeStart(t)

	t.Setenv("GC_SUSPENDED", "1")
	for range 3 {
		if !cr.enterBeadsQuiescenceIfDue(context.Background()) {
			t.Fatal("a suspended city with nothing running is not quiescent")
		}
	}
	if ops := providerOpsLogged(t, logPath); ops != "" {
		t.Fatalf("provider ops while a wisp GC purge was in flight = %q, want none", ops)
	}

	close(wg.release)
	awaitLanePass(t, passDone)
	cr.enterBeadsQuiescenceIfDue(context.Background())
	cr.enterBeadsQuiescenceIfDue(context.Background())
	if ops := providerOpsLogged(t, logPath); ops != "stop" {
		t.Fatalf("provider ops = %q, want one stop once the purge finished", ops)
	}
	if runLanePass(cr, true) {
		t.Fatal("a woken pass purged on a retired, quiescent city")
	}
	if got := wg.runs.Load(); got != 1 {
		t.Fatalf("purges = %d, want 1", got)
	}
}

// Kills: a suspended rig retired under a purge. A purge in flight when a rig
// is suspended and its sessions have drained holds the stop of the rig's
// store until it returns.
func TestWispGCLane_PurgeHoldsSuspendedRigRetirement(t *testing.T) {
	wg := newBlockingWispGC()
	cr, logPath := newQuiescenceLaneRuntime(t, "suspended_on_start = true\n", wg)
	t.Setenv("GC_SUSPENDED", "")
	passDone := startLanePassAsync(cr)
	wg.awaitPurgeStart(t)

	p := &tickPass{ctx: context.Background(), sessionBeads: newSessionBeadSnapshotFromInfos(nil)}
	for range 3 {
		cr.tickRetireSuspendedRigScopes(p)
	}
	if ops := providerOpsLogged(t, logPath); ops != "" {
		t.Fatalf("the suspended rig was stopped while a wisp GC purge was in flight: %q", ops)
	}

	close(wg.release)
	awaitLanePass(t, passDone)
	cr.tickRetireSuspendedRigScopes(p)
	cr.tickRetireSuspendedRigScopes(p)
	if ops := providerOpsLogged(t, logPath); ops != "stop" {
		t.Fatalf("provider ops = %q, want one stop once the purge finished", ops)
	}
}

// Kills: a lane stop that abandons a purge in flight. run() defers the stop
// after its shutdown, so a stop that returns early lets shutdown close the
// stores and the trace under the purge. The stop returns only after the
// purge has returned and its pass is recorded.
func TestWispGCLane_StopWaitsForPurgeInFlight(t *testing.T) {
	wg := newBlockingWispGC()
	cr, _, _ := newWispGCLaneRuntime(t, wg)
	stop := cr.startWispGCLane(context.Background())
	cr.wispGCLaneOf().wake()
	wg.awaitPurgeStart(t)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	select {
	case <-stopped:
		t.Fatal("the lane stop returned while its purge was in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(wg.release)
	select {
	case <-stopped:
	case <-time.After(wispGCLaneTestWait):
		t.Fatal("the lane stop did not return after its purge finished")
	}
	if _, ran := cr.wispGCLaneOf().lastPassAge(time.Now()); !ran {
		t.Fatal("the lane stop returned before the purge's pass was recorded")
	}
}

// routeRecordingWispGC is an always-due tracker that records the stores and
// legs its purge was handed, and returns a fixed count and error.
type routeRecordingWispGC struct {
	purged int
	err    error
	got    *wispGCPurgeArgs
}

type wispGCPurgeArgs struct {
	legs  wispGCLegs
	graph beads.GraphStore
	mail  beads.MailStore
}

func (r routeRecordingWispGC) shouldRun(time.Time) bool { return true }

func (r routeRecordingWispGC) runGC(legs wispGCLegs, graph beads.GraphStore, _ beads.SessionStore, mail beads.MailStore, _ time.Time) (int, error) {
	*r.got = wispGCPurgeArgs{legs: legs, graph: graph, mail: mail}
	return r.purged, r.err
}

// Kills: a lane purge that drops its reporting. A completed purge with
// deletions and a joined error keeps its log lines, its typed store routing,
// the relocated work-arm wake and the wisp_gc trace operation, recorded in a
// purge cycle of its own rather than in a tick's.
func TestWispGCLane_ReportsPurgeInItsOwnTraceCycle(t *testing.T) {
	cr, _ := laneRuntime(t, true)
	got := &wispGCPurgeArgs{}
	cr.wg = routeRecordingWispGC{purged: 2, err: fmt.Errorf("%s\n%s", "delete failed", "list failed"), got: got}
	cr.logPrefix = "test-city"
	cr.sp = runtime.NewFake()
	cr.buildFn = func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}
	cr.trace = newSessionReconcilerTraceManager(cr.cityPath, "test-city", io.Discard)

	runBareTick(cr, "patrol")
	if !runLanePass(cr, true) {
		t.Fatalf("the lane did not purge on a wake with a due tracker; stderr: %s", cr.stderr.(*bytes.Buffer).String())
	}

	stderr := cr.stderr.(*bytes.Buffer).String()
	for _, want := range []string{"test-city: wisp gc: delete failed\n", "test-city: wisp gc: list failed\n"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want line %q", stderr, want)
		}
	}
	if stdout := cr.stdout.(*bytes.Buffer).String(); !strings.Contains(stdout, "Bead GC: purged 2 expired bead(s)") {
		t.Errorf("stdout = %q, want the purge count", stdout)
	}

	if got.graph.Store == nil || got.graph.Store != cr.graphBeadStore().Store {
		t.Errorf("purge graph store = %v, want the typed graph-class store %v", got.graph.Store, cr.graphBeadStore().Store)
	}
	if got.mail.Store != cr.mailBeadStore().Store {
		t.Errorf("purge mail store = %v, want the typed messaging-class store %v", got.mail.Store, cr.mailBeadStore().Store)
	}
	if got.legs == nil {
		t.Error("purge on a relocated city got no work legs")
	}
	select {
	case <-cr.workGCLaneOf().wakeCh:
	default:
		t.Error("the purge on a relocated city did not wake the work-arm lane")
	}

	records := closeTrace(t, cr)
	var purgeOp *SessionReconcilerTraceRecord
	// A cycle's trigger is on its cycle_start record.
	cycleStarts := map[string]SessionReconcilerTraceRecord{}
	for i := range records {
		r := &records[i]
		if r.RecordType == TraceRecordCycleStart {
			cycleStarts[r.TickID] = *r
		}
		if r.RecordType == TraceRecordOperation && fmt.Sprint(r.Fields["operation_name"]) == "wisp_gc" {
			if purgeOp != nil {
				t.Fatalf("more than one wisp_gc operation record: %+v and %+v", *purgeOp, *r)
			}
			purgeOp = r
		}
	}
	if purgeOp == nil {
		t.Fatalf("no wisp_gc operation record; operations: %v", operationRecords(records))
	}
	if purgeOp.SiteCode != TraceSiteControllerTickPhase {
		t.Errorf("wisp_gc site = %q, want %q", purgeOp.SiteCode, TraceSiteControllerTickPhase)
	}
	start, ok := cycleStarts[purgeOp.TickID]
	if !ok {
		t.Fatalf("wisp_gc cycle %q has no cycle_start record", purgeOp.TickID)
	}
	if start.TickTrigger != wispGCLaneTraceTrigger || start.TriggerDetail != wispGCLaneTraceDetail {
		t.Errorf("wisp_gc cycle trigger = %q/%q, want %q/%q", start.TickTrigger, start.TriggerDetail, wispGCLaneTraceTrigger, wispGCLaneTraceDetail)
	}
	tickCycles := 0
	for _, c := range cycleStarts {
		if c.TickTrigger != wispGCLaneTraceTrigger {
			tickCycles++
		}
	}
	if tickCycles == 0 {
		t.Error("the tick recorded no cycle of its own")
	}
	if fmt.Sprint(purgeOp.Fields["purged"]) != "2" {
		t.Errorf("wisp_gc payload purged = %v, want 2; fields: %v", purgeOp.Fields["purged"], purgeOp.Fields)
	}
	completed := false
	for _, r := range records {
		if r.RecordType == TraceRecordCycleResult && r.TickID == purgeOp.TickID {
			completed = r.CompletionStatus == TraceCompletionCompleted
		}
	}
	if !completed {
		t.Errorf("purge cycle %q did not complete", purgeOp.TickID)
	}
}

// drainWispGCWakes removes every wake queued on the lane and reports how
// many there were.
func drainWispGCWakes(lane *wispGCLane) int {
	n := 0
	for {
		select {
		case <-lane.wakeCh:
			n++
		default:
			return n
		}
	}
}

// Kills: a trigger whose tick no longer reaches the wake slot, or a tick that
// wakes the lane more than once. Every controller tick that reaches the slot
// the purge used to run in hands the lane exactly one wake, whatever trigger
// ran it and without the lane running to receive it.
func TestWispGCLane_TickReachingTheSlotEnqueuesOneWake(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	lane := cr.wispGCLaneOf()
	if n := drainWispGCWakes(lane); n != 0 {
		t.Fatalf("a fresh lane queued %d wakes before any tick, want 0", n)
	}
	for _, trigger := range []string{"patrol", "poke", "startup-poke", "control-dispatcher", "reload"} {
		t.Run(trigger, func(t *testing.T) {
			runFixtureTick(cr, trigger)
			if n := drainWispGCWakes(lane); n != 1 {
				t.Errorf("a %s tick reaching the wake slot enqueued %d wakes, want 1", trigger, n)
			}
		})
	}
}

// laneStagedPending reports whether a reload has staged a policy the lane has
// not installed yet.
func laneStagedPending(lane *wispGCLane) bool {
	lane.stageMu.Lock()
	defer lane.stageMu.Unlock()
	return lane.stagedPending
}

// Kills: a failed reload that reaches the lane. A reload that never
// validates stages nothing, and the next install leaves the deployed
// tracker running exactly as it was.
func TestWispGCLane_RejectedReloadStagesNothing(t *testing.T) {
	cr, tomlPath := newWispGCReloadRuntime(t, "\n[daemon]\nwisp_gc_interval = \"1h\"\nwisp_ttl = \"24h\"\n")
	before := installedWispGC(cr)
	if before == nil {
		t.Fatal("the deployed config left wisp GC disabled; want a running tracker")
	}
	// An unparseable table header fails the reload outright (an invalid
	// duration is only a non-strict warning, which still applies).
	if err := os.WriteFile(tomlPath, []byte(wispGCReloadCityBase+"\n[daemon\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cr.cityPath, nil, reloadSourceManual)
	if reply.Outcome == reloadOutcomeApplied {
		t.Fatalf("reload applied an unparseable config (%+v)", reply)
	}
	if laneStagedPending(cr.wispGCLaneOf()) {
		t.Error("a reload that failed validation staged a policy for the lane")
	}
	if after := installedWispGC(cr); after != before {
		t.Errorf("a rejected reload changed the deployed tracker: %T(%p) -> %T(%p)", before, before, after, after)
	}
}

// installLaneStaged installs the lane's staged policy the way a pass does,
// under passMu.
func installLaneStaged(lane *wispGCLane) {
	lane.passMu.Lock()
	defer lane.passMu.Unlock()
	lane.installStagedLocked()
}

// Kills: a wrong staging rule between reloads and installs. A disable clears
// the tracker; a disable coalesced with a later enable installs the fresh
// tracker's own cadence (the disable ended the old one, so nothing carries);
// the latest staged policy wins; and a plain enable-to-enable replacement
// carries the running tracker's lastRun and session purge cursor.
func TestWispGCLane_StageInstallTransitions(t *testing.T) {
	seedTracker := func(lastRun time.Time, cursor *beads.SeekBoundary) *memoryWispGC {
		return &memoryWispGC{interval: time.Hour, ttl: 24 * time.Hour, lastRun: lastRun, sessionPurgeCursor: cursor}
	}
	freshTracker := func(interval time.Duration) *memoryWispGC {
		return &memoryWispGC{interval: interval, ttl: 48 * time.Hour}
	}
	lastRun := time.Now().Add(-10 * time.Minute)
	cursor := &beads.SeekBoundary{CreatedAt: lastRun, ID: "gcg-carry"}

	t.Run("disable", func(t *testing.T) {
		lane := newWispGCLane(seedTracker(lastRun, cursor))
		lane.stage(nil)
		if !laneStagedPending(lane) {
			t.Fatal("a disable left nothing staged")
		}
		installLaneStaged(lane)
		if lane.active != nil {
			t.Errorf("after installing a disable, tracker = %T, want nil", lane.active)
		}
		if laneStagedPending(lane) {
			t.Error("the staged disable survived its install")
		}
	})

	t.Run("coalesced disable then enable installs fresh cadence", func(t *testing.T) {
		lane := newWispGCLane(seedTracker(lastRun, cursor))
		fresh := freshTracker(15 * time.Minute)
		lane.stage(nil)
		lane.stage(fresh)
		installLaneStaged(lane)
		if lane.active != wispGC(fresh) {
			t.Fatalf("installed tracker = %p, want the fresh enable %p", lane.active, fresh)
		}
		if !fresh.lastRun.IsZero() {
			t.Errorf("fresh tracker carried lastRun=%v from the retired cadence; a disable ends the cadence", fresh.lastRun)
		}
		if fresh.sessionPurgeCursor != nil {
			t.Errorf("fresh tracker carried sessionPurgeCursor=%v from the retired cadence", fresh.sessionPurgeCursor)
		}
	})

	t.Run("latest staged policy wins", func(t *testing.T) {
		lane := newWispGCLane(seedTracker(lastRun, cursor))
		first := freshTracker(15 * time.Minute)
		latest := freshTracker(30 * time.Minute)
		lane.stage(first)
		lane.stage(latest)
		installLaneStaged(lane)
		if lane.active != wispGC(latest) {
			t.Errorf("installed tracker = %p, want the latest staged policy %p", lane.active, latest)
		}
	})

	t.Run("enable carries the running cadence", func(t *testing.T) {
		lane := newWispGCLane(seedTracker(lastRun, cursor))
		fresh := freshTracker(15 * time.Minute)
		lane.stage(fresh)
		installLaneStaged(lane)
		if lane.active != wispGC(fresh) {
			t.Fatalf("installed tracker = %p, want %p", lane.active, fresh)
		}
		if !fresh.lastRun.Equal(lastRun) {
			t.Errorf("installed tracker lastRun=%v, want %v carried from the running tracker", fresh.lastRun, lastRun)
		}
		if fresh.sessionPurgeCursor != cursor {
			t.Errorf("installed tracker sessionPurgeCursor=%v, want the carried cursor", fresh.sessionPurgeCursor)
		}
	})
}

// Kills: a controller exit that abandons a purge in flight. run() stops the
// wisp GC lane on its way out and joins it, so the purge finishes with its
// stores, trace and output still available, and only then does run return.
// TestWispGCLane_StopWaitsForPurgeInFlight covers the lane's own stop.
func TestCityRuntimeRunJoinsAnInFlightWispGCPurgeOnExit(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	// A fast patrol so the controller's own tick loop wakes the purge in
	// seconds, not after the default interval.
	f, err := os.OpenFile(tomlPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}
	if _, err := f.WriteString("\n[daemon]\npatrol_interval = \"5ms\"\n"); err != nil {
		t.Fatalf("append config: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close config: %v", err)
	}
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	wg := newBlockingWispGC()
	cr.wg = wg

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		cr.run(ctx)
	}()
	select {
	case <-wg.started:
	case <-time.After(60 * time.Second):
		t.Fatal("the controller's patrol ticks never started the purge")
	}

	cancel()
	select {
	case <-runDone:
		t.Fatal("run returned while its purge was still running; the exit must join the lane")
	case <-time.After(5 * time.Second):
	}

	close(wg.release)
	select {
	case <-runDone:
	case <-time.After(60 * time.Second):
		t.Fatal("run did not return after the purge it was joining finished")
	}
}

// panickingWispGC is an always-due tracker whose purge panics inside runGC.
type panickingWispGC struct{}

func (panickingWispGC) shouldRun(time.Time) bool { return true }

func (panickingWispGC) runGC(wispGCLegs, beads.GraphStore, beads.SessionStore, beads.MailStore, time.Time) (int, error) {
	panic("purge boom")
}

// Kills: a purge panic that escapes the lane or leaks its cleanup. Driven
// through safeTick as startWispGCLane drives it, a panicking purge is
// recovered, still counts toward the lane's pacing (ran), releases its
// store-work registration, and ends its trace cycle aborted.
func TestWispGCLane_PurgePanicIsRecoveredAndAbortsItsCycle(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	cr.wg = panickingWispGC{}
	ran := false
	panicked := cr.safeTick(func() { cr.runWispGCLanePass(context.Background(), true, &ran) }, wispGCLaneSafeTickTrigger)
	if !panicked {
		t.Fatal("safeTick reported no panic from a purge that panicked")
	}
	if !ran {
		t.Fatal("a pass whose purge panicked did not report running a purge; a recovered panic still counts toward pacing")
	}
	if cr.cs.storeWorkInFlight() {
		t.Fatal("the purging pass's store-work registration outlived its recovered panic")
	}
	records := closeTrace(t, cr)
	if got := passCompletion(records, "wisp_gc"); got != TraceCompletionAborted {
		t.Errorf("a panicked purge's cycle completion = %q, want %q", got, TraceCompletionAborted)
	}
}

// dueOnceGatedWispGC is due only for its first pass: it blocks in its one
// purge and is not due once that has run, so any later pass is provably a
// decline. secondCheck closes on the second shouldRun call, the queued
// wake's pass reaching the due-time check.
type dueOnceGatedWispGC struct {
	runs        atomic.Int32
	started     chan struct{}
	secondCheck chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	secondOnce  sync.Once
}

func (g *dueOnceGatedWispGC) shouldRun(time.Time) bool {
	if g.runs.Load() > 0 {
		g.secondOnce.Do(func() { close(g.secondCheck) })
		return false
	}
	return true
}

func (g *dueOnceGatedWispGC) runGC(wispGCLegs, beads.GraphStore, beads.SessionStore, beads.MailStore, time.Time) (int, error) {
	g.runs.Add(1)
	g.startOnce.Do(func() { close(g.started) })
	<-g.release
	return 0, nil
}

// Kills: a queue of purges behind a burst of ticks. Three ticks reaching the
// wake slot while a purge runs leave one queued wake, whose pass reaches
// the due-time check and starts no second purge.
func TestWispGCLane_ABurstOfTicksIsNeverAQueueOfPurges(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	tracker := &dueOnceGatedWispGC{
		started:     make(chan struct{}),
		secondCheck: make(chan struct{}),
		release:     make(chan struct{}),
	}
	cr.wg = tracker
	stop := cr.startWispGCLane(context.Background())

	for range 3 {
		runFixtureTick(cr, "patrol")
	}
	select {
	case <-tracker.started:
	case <-time.After(wispGCLaneTestWait):
		t.Fatal("the first woken tick never started the purge")
	}
	close(tracker.release)

	select {
	case <-tracker.secondCheck:
	case <-time.After(wispGCLaneTestWait):
		t.Fatal("the queued wake never ran its pass after the first purge finished")
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	select {
	case <-stopped:
	case <-time.After(wispGCLaneTestWait):
		t.Fatal("the lane did not stop after its pass finished")
	}
	if got := tracker.runs.Load(); got != 1 {
		t.Fatalf("purges = %d, want 1: a burst of ticks during a purge queues at most one more pass, which is not due", got)
	}
}
