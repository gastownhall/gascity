package main

import (
	"context"
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
