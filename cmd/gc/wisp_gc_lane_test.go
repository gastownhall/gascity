package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
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
