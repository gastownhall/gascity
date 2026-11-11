package main

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The wisp GC purge on its lane (wisp_gc_lane.go): the tick only wakes it, a
// tick that reaches the wake slot completes whatever the purge is doing, and
// the lane's own rules — due-time gating, policy staging across reloads,
// suspension interplay, and the stop/join — hold pass by pass.

// gatedWispGC is a wisp GC tracker whose purge blocks inside runGC until the
// test releases it, counting its runs. With block unset it runs like a
// tracker that purges nothing, at once.
type gatedWispGC struct {
	runs  atomic.Int32
	block bool
	// started is closed when the first purge enters runGC; a blocked purge
	// waits for release.
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func newGatedWispGC(block bool) *gatedWispGC {
	return &gatedWispGC{block: block, started: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedWispGC) shouldRun(time.Time) bool { return true }

func (g *gatedWispGC) runGC(wispGCLegs, beads.GraphStore, beads.SessionStore, beads.MailStore, time.Time) (int, error) {
	g.runs.Add(1)
	g.startOnce.Do(func() { close(g.started) })
	if g.block {
		<-g.release
	}
	return 0, nil
}

// TestWispGCLane_TickCompletesWhilePurgeBlocked is the lane's reason to exist
// (#2604, PR #8431): a due purge blocked in a store call delays no session
// decision. A whole legacy tick — and a v2 maintenance tick — completes while
// the lane's purge is blocked, and stopping the lane waits for the purge to
// finish rather than abandoning it.
func TestWispGCLane_TickCompletesWhilePurgeBlocked(t *testing.T) {
	t.Run("legacy tick", func(t *testing.T) {
		cr, _ := newPhaseFixtureRuntime(t, false, false)
		gated := newGatedWispGC(true)
		cr.wg = gated
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		laneDone := cr.startWispGCLane(ctx)

		runTick := func() <-chan struct{} {
			done := make(chan struct{})
			go func() {
				defer close(done)
				var dirty atomic.Bool
				var lastProviderName string
				var prevPoolRunning map[string]bool
				cr.tick(ctx, &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
			}()
			return done
		}
		<-runTick() // the tick wakes the lane and returns
		<-gated.started

		select {
		case <-runTick():
		case <-time.After(60 * time.Second):
			t.Fatal("a tick waited for the blocked wisp GC purge; on its lane the purge must delay no tick")
		}

		cancel()
		select {
		case <-laneDone:
			t.Fatal("the lane exited while its purge was in flight")
		case <-time.After(2 * time.Second):
		}
		close(gated.release)
		select {
		case <-laneDone:
		case <-time.After(60 * time.Second):
			t.Fatal("the lane did not stop after its released purge finished")
		}
	})

	t.Run("v2 maintenance tick", func(t *testing.T) {
		cr, store := newPhaseFixtureRuntime(t, false, true)
		armMaintenanceLanes(t, cr, store)
		gated := newGatedWispGC(true)
		cr.wg = gated
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		laneDone := cr.startWispGCLane(ctx)

		runTick := func() <-chan struct{} {
			done := make(chan struct{})
			go func() {
				defer close(done)
				p := &tickPass{ctx: ctx, dirty: cr.configDirty, trigger: "patrol", prevPoolRunning: new(map[string]bool)}
				if cr.runTickPhases(p, maintenancePhases(legacyTickPhases)) {
					p.completed = true
				}
			}()
			return done
		}
		<-runTick()
		<-gated.started

		// The maintenance tick must reach its end on its own goroutine; the
		// guard fails the pass if it waits out the blocked purge.
		runFinished := runTick()
		select {
		case <-runFinished:
		case <-time.After(60 * time.Second):
			t.Fatal("a v2 maintenance tick waited for the blocked wisp GC purge")
		}

		cancel()
		select {
		case <-laneDone:
			t.Fatal("the lane exited while its purge was in flight")
		case <-time.After(2 * time.Second):
		}
		close(gated.release)
		select {
		case <-laneDone:
		case <-time.After(60 * time.Second):
			t.Fatal("the lane did not stop after its released purge finished")
		}
	})
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

// TestWispGCLane_TickReachingTheSlotEnqueuesOneWake: every controller tick
// that reaches the wake slot — the exact slot the purge used to run in —
// hands the lane exactly one wake, whatever trigger ran the tick and without
// the lane needing to be running to receive it.
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

// TestWispGCLane_StoppedBeforeTheSlotEnqueuesNoWake: a tick stopped before
// the wake slot never enqueues a wake. The store read that stops the pass
// is in a reap phase well before the slot (see the recorded abort in
// TestCityRuntimeTickCancelledMidPassAborts), so the lane stays asleep.
func TestWispGCLane_StoppedBeforeTheSlotEnqueuesNoWake(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.onRecord = func(string) { cancel() }
	runFixtureTickCtx(ctx, cr, "patrol")
	closeTrace(t, cr)
	if n := drainWispGCWakes(cr.wispGCLaneOf()); n != 0 {
		t.Errorf("a tick stopped before the wake slot enqueued %d wakes, want 0", n)
	}
}

// TestWispGCLane_WakeRunsThePurgeAtOnce: an arrived wake starts the purge
// immediately. The fixture's config leaves wisp GC off, so the lane's pacing
// falls back to the work arm's 10-minute backstop; a purge that starts well
// inside it proves the lane never sits on an arrived wake waiting out its
// timer — the tick's wake is what runs the GC, and it runs without a
// patrol-timer wait in between.
func TestWispGCLane_WakeRunsThePurgeAtOnce(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	gated := newGatedWispGC(true)
	cr.wg = gated
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	laneDone := cr.startWispGCLane(ctx)

	cr.wispGCLaneOf().wake()
	select {
	case <-gated.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the lane did not run the woken purge; it waited for its backstop instead")
	}
	if runs := gated.runs.Load(); runs != 1 {
		t.Errorf("the woken pass ran %d purges, want 1", runs)
	}

	cancel()
	select {
	case <-laneDone:
		t.Fatal("the lane exited while its purge was in flight")
	case <-time.After(2 * time.Second):
	}
	close(gated.release)
	select {
	case <-laneDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the lane did not stop after its released purge finished")
	}
}

// blockedMoleculeStore pauses the purge's first molecule-root enumeration:
// the wisp GC purge lists molecule roots almost as soon as it starts (see
// wispGCRootSelectors / closeAbandonedRoots), so a List that parks there
// leaves a purge in flight on the lane's goroutine.
type blockedMoleculeStore struct {
	beads.Store
	entered chan struct{}
	release chan struct{}
	armed   atomic.Bool
}

func (s *blockedMoleculeStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Type == "molecule" && s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
	return s.Store.List(q)
}

// TestWispGCLane_ReloadDuringAPurgeStagesWithoutWaiting: a config reload
// that lands while a purge is in flight applies at once — it only stages the
// rebuilt policy for the lane to install between passes, so it never waits
// for the in-flight purge and never reads the tracker underneath it — and
// the install after the purge carries the completed purge's lastRun and
// session cursor onto the new policy.
func TestWispGCLane_ReloadDuringAPurgeStagesWithoutWaiting(t *testing.T) {
	cr, tomlPath := newWispGCReloadRuntime(t, "\n[daemon]\nwisp_gc_interval = \"1h\"\nwisp_ttl = \"24h\"\n")
	store := &blockedMoleculeStore{Store: beads.NewMemStore(), entered: make(chan struct{}), release: make(chan struct{})}
	store.armed.Store(true)
	cr.cs.mu.Lock()
	cr.cs.cityBeadStore = store
	cr.cs.mu.Unlock()

	lane := cr.wispGCLaneOf()
	old, ok := lane.active.(*memoryWispGC)
	if !ok {
		t.Fatalf("active wisp GC tracker = %T, want *memoryWispGC", lane.active)
	}
	// The cursor the next install must carry: the purge on this fixture has no
	// session arm (the sessions class is not relocated), so it leaves it alone.
	cursor := &beads.SeekBoundary{CreatedAt: time.Now().Add(-48 * time.Hour), ID: "gcg-midpurge"}
	old.sessionPurgeCursor = cursor

	purgeNow := time.Now()
	passDone := make(chan struct{})
	go func() {
		defer close(passDone)
		if !cr.wispGCLanePass(context.Background(), purgeNow, true) {
			close(store.release) // never block the fatal below
		}
	}()
	select {
	case <-store.entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the purge never reached its molecule enumeration")
	}
	if !old.lastRun.Equal(purgeNow) {
		t.Fatalf("in-flight purge stamped lastRun=%v, want %v: purge not actually running", old.lastRun, purgeNow)
	}

	// The reload applies while the purge is still parked in the store.
	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		applyWispGCReload(t, cr, tomlPath, "\n[daemon]\nwisp_gc_interval = \"15m\"\nwisp_ttl = \"12h\"\n")
	}()
	select {
	case <-reloadDone:
	case <-time.After(60 * time.Second):
		t.Fatal("the reload waited for the in-flight purge; a reload must stage its policy without touching the lane's pass")
	}
	if !lane.stagedPending() {
		t.Fatal("the applied reload left no staged policy for the lane to install")
	}

	// Let the purge finish; the lane installs the staged policy between
	// passes, as the next pass would.
	close(store.release)
	select {
	case <-passDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the purge never finished after its release")
	}
	newT := installedWispGC(t, cr, old)
	if newT.interval != 15*time.Minute || newT.ttl != 12*time.Hour {
		t.Errorf("installed tracker interval=%s ttl=%s, want 15m/12h from the reloaded policy", newT.interval, newT.ttl)
	}
	if !newT.lastRun.Equal(purgeNow) {
		t.Errorf("installed tracker lastRun=%v, want %v (the completed purge's lastRun)", newT.lastRun, purgeNow)
	}
	if newT.sessionPurgeCursor != cursor {
		t.Errorf("installed tracker sessionPurgeCursor=%v, want the carried cursor", newT.sessionPurgeCursor)
	}
}

// TestWispGCLane_RejectedReloadStagesNothing: a reload that never validates
// stages nothing — the next install leaves the deployed policy running,
// exactly as it was.
func TestWispGCLane_RejectedReloadStagesNothing(t *testing.T) {
	cr, tomlPath := newWispGCReloadRuntime(t, "\n[daemon]\nwisp_gc_interval = \"1h\"\nwisp_ttl = \"24h\"\n")
	lane := cr.wispGCLaneOf()
	before := lane.active
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
	if lane.stagedPending() {
		t.Error("a reload that failed validation staged a policy for the lane")
	}
	lane.installStaged()
	if lane.active != before {
		t.Errorf("a rejected reload changed the deployed tracker: %T -> %T", before, lane.active)
	}
}
