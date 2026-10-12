package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The planner loop's tests run on fakePlannerClock: time moves only when a
// test advances it, so every pass's start time is exact. waitArmed and
// expectPass wait on channels the loop signals; their real-time guard only
// turns a hang into a failure.

const plannerTestGuard = 10 * time.Second

var plannerT0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// fakePlannerClock is a manual clock. Its timers fire when Advance reaches
// their deadline. In auto mode every armed timer fires at once and the clock
// jumps to its deadline, so the loop runs passes back to back.
type fakePlannerClock struct {
	mu      sync.Mutex
	now     time.Time
	auto    bool
	timers  []*fakePlannerTimer
	changed chan struct{} // closed and replaced whenever a timer changes
	arms    uint64        // timer arms so far
	passArm uint64        // arms when the last pass ended
	inPass  bool
}

type fakePlannerTimer struct {
	c      *fakePlannerClock
	ch     chan time.Time
	at     time.Time
	active bool
	arm    uint64 // the clock's arms count when this timer was last armed
}

func newFakePlannerClock(now time.Time) *fakePlannerClock {
	return &fakePlannerClock{now: now, changed: make(chan struct{})}
}

func (c *fakePlannerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer arms the new timer, but waitArmed does not count that arm: the
// loop re-arms before it waits.
func (c *fakePlannerClock) NewTimer(d time.Duration) plannerTimer {
	t := &fakePlannerTimer{c: c, ch: make(chan time.Time, 1)}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timers = append(c.timers, t)
	t.resetLocked(d)
	t.arm = 0
	return t
}

func (c *fakePlannerClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if t.active && !t.at.After(c.now) {
			t.fireLocked()
		}
	}
	c.signalLocked()
}

func (c *fakePlannerClock) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// passStarted and passEnded bracket a pass. waitArmed ignores the arms made
// before the last pass ended.
func (c *fakePlannerClock) passStarted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inPass = true
}

func (c *fakePlannerClock) passEnded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.passArm, c.inPass = c.arms, false
	c.signalLocked()
}

// waitArmed waits until some timer is armed for want after the last pass
// ended: the loop is then idle, waiting on that timer or a mark.
func (c *fakePlannerClock) waitArmed(t *testing.T, want time.Time) {
	t.Helper()
	guard := time.After(plannerTestGuard)
	for {
		c.mu.Lock()
		var armed []time.Time
		for _, tm := range c.timers {
			if tm.active {
				if tm.at.Equal(want) && tm.arm > c.passArm && !c.inPass {
					c.mu.Unlock()
					return
				}
				armed = append(armed, tm.at)
			}
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("no timer armed for %s; armed: %v", want.Sub(plannerT0), armed)
		}
	}
}

func (t *fakePlannerTimer) C() <-chan time.Time { return t.ch }

func (t *fakePlannerTimer) Reset(d time.Duration) {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	t.resetLocked(d)
}

func (t *fakePlannerTimer) resetLocked(d time.Duration) {
	c := t.c
	t.drainLocked()
	c.arms++
	t.at, t.active, t.arm = c.now.Add(d), true, c.arms
	if c.auto && t.at.After(c.now) {
		c.now = t.at
	}
	if !t.at.After(c.now) {
		t.fireLocked()
	}
	c.signalLocked()
}

func (t *fakePlannerTimer) Stop() {
	c := t.c
	c.mu.Lock()
	defer c.mu.Unlock()
	t.drainLocked()
	t.active = false
	c.signalLocked()
}

func (t *fakePlannerTimer) fireLocked() {
	t.active = false
	t.ch <- t.at // drained on every Reset and Stop, and fired once per arm
}

func (t *fakePlannerTimer) drainLocked() {
	select {
	case <-t.ch:
	default:
	}
}

// fakeInflight counts settlements without a lock: only the planner goroutine
// may touch it, which -race checks.
type fakeInflight struct{ settled []settlement }

func (f *fakeInflight) settle(s settlement) { f.settled = append(f.settled, s) }

// plannerHarness runs a planner on a fake clock. Every pass reports its start
// on passes, then runs the test's onPass if it has one.
type plannerHarness struct {
	clk      *fakePlannerClock
	p        *planner
	inflight *fakeInflight
	stderr   *bytes.Buffer
	passes   chan time.Time
	patrol   atomic.Int64
	stops    []time.Time // stopEffects deadlines
}

func newPlannerHarness(t *testing.T, onPass func(h *plannerHarness, now time.Time) passResult) *plannerHarness {
	t.Helper()
	h := &plannerHarness{
		clk:      newFakePlannerClock(plannerT0),
		inflight: &fakeInflight{},
		stderr:   &bytes.Buffer{},
		passes:   make(chan time.Time, 64),
	}
	h.patrol.Store(int64(15 * time.Second))
	pass := func(now time.Time) passResult {
		h.clk.passStarted()
		defer h.clk.passEnded()
		h.passes <- now
		if onPass != nil {
			return onPass(h, now)
		}
		return passResult{}
	}
	h.p = newPlanner(h.clk, func() time.Duration { return time.Duration(h.patrol.Load()) }, pass, h.inflight,
		func(deadline time.Time) { h.stops = append(h.stops, deadline) }, h.stderr)
	return h
}

func (h *plannerHarness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go h.p.run(ctx)
	t.Cleanup(func() {
		cancel()
		<-h.p.done
	})
}

// expectPass waits for the next pass and checks that it started at want.
func (h *plannerHarness) expectPass(t *testing.T, want time.Time) {
	t.Helper()
	select {
	case got := <-h.passes:
		if !got.Equal(want) {
			t.Fatalf("pass started at T0+%s, want T0+%s", got.Sub(plannerT0), want.Sub(plannerT0))
		}
	case <-time.After(plannerTestGuard):
		t.Fatalf("no pass started; want one at T0+%s", want.Sub(plannerT0))
	}
}

func (h *plannerHarness) expectNoPass(t *testing.T) {
	t.Helper()
	select {
	case got := <-h.passes:
		t.Fatalf("unexpected pass at T0+%s", got.Sub(plannerT0))
	default:
	}
}

// TestPlannerIdlePassStartsImmediately: a mark on an idle planner starts a
// pass without the clock moving, the first time and after an idle gap.
//
// Kills: added latency when idle (a pass that always waits the minimum gap).
func TestPlannerIdlePassStartsImmediately(t *testing.T) {
	h := newPlannerHarness(t, nil)
	h.start(t)
	h.p.markDirty("test")
	h.expectPass(t, plannerT0)

	h.clk.waitArmed(t, plannerT0.Add(15*time.Second))
	h.clk.Advance(time.Second)
	h.p.markDirty("test")
	h.expectPass(t, plannerT0.Add(time.Second))
}

// TestPlannerPacingFourTimesLastPass: a mark that arrives while the planner
// is busy waits until max(250ms, 4 × the last pass) after the last start.
//
// Kills: a duty cycle above about 20% (a smaller factor, pacing from the end,
// or no pacing), and a long pass's minimum gap below 250ms.
func TestPlannerPacingFourTimesLastPass(t *testing.T) {
	for _, tc := range []struct {
		name string
		ran  time.Duration
		next time.Duration // from the first pass's start
	}{
		{"four times the last pass", time.Second, 4 * time.Second},
		{"floor of 250ms", 10 * time.Millisecond, 250 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := true
			h := newPlannerHarness(t, func(h *plannerHarness, _ time.Time) passResult {
				if first {
					first = false
					h.clk.Advance(tc.ran)
					h.p.markDirty("test")
				}
				return passResult{}
			})
			h.start(t)
			h.p.markDirty("test")
			h.expectPass(t, plannerT0)

			h.clk.waitArmed(t, plannerT0.Add(tc.next))
			h.expectNoPass(t)
			h.clk.Advance(tc.next - tc.ran)
			h.expectPass(t, plannerT0.Add(tc.next))
			if d := h.p.metrics.snapshot(plannerT0.Add(tc.next)).Duty; d > 0.26 {
				t.Errorf("duty cycle %.2f, want at most about 0.25", d)
			}
		})
	}
}

// TestPlannerFoldsDirtyDuringPass: many marks during a pass give exactly one
// follow-up pass; the next after that is the patrol's.
//
// Kills: pass storms (a mark queue deeper than one, or a pass per mark).
func TestPlannerFoldsDirtyDuringPass(t *testing.T) {
	passes := 0
	h := newPlannerHarness(t, func(h *plannerHarness, _ time.Time) passResult {
		if passes++; passes == 1 {
			for i := 0; i < 100; i++ {
				h.p.markDirty("test")
			}
		}
		return passResult{}
	})
	h.start(t)
	h.p.markDirty("test")
	h.expectPass(t, plannerT0)

	follow := plannerT0.Add(plannerMinGap)
	h.clk.waitArmed(t, follow)
	h.clk.Advance(plannerMinGap)
	h.expectPass(t, follow)

	patrol := follow.Add(15 * time.Second)
	h.clk.waitArmed(t, patrol)
	h.expectNoPass(t)
	h.clk.Advance(15 * time.Second)
	h.expectPass(t, patrol)
	if got := h.p.metrics.snapshot(patrol).Wakes; got["patrol"] != 1 || got["test"] != 101 {
		t.Fatalf("wakes = %v, want 101 test marks folded into one pass and the third pass from the patrol", got)
	}
}

// TestPlannerPatrolBoundsLostWake: with no mark at all, a pass runs one
// patrol interval after the last one ended, at the interval read after each
// pass.
//
// Kills: a hang after a lost wake (R-6), and a patrol fixed at construction.
func TestPlannerPatrolBoundsLostWake(t *testing.T) {
	h := newPlannerHarness(t, func(h *plannerHarness, _ time.Time) passResult {
		h.patrol.Store(int64(5 * time.Second))
		return passResult{}
	})
	h.start(t)
	first := plannerT0.Add(15 * time.Second)
	h.clk.waitArmed(t, first)
	h.clk.Advance(15 * time.Second)
	h.expectPass(t, first)

	second := first.Add(5 * time.Second)
	h.clk.waitArmed(t, second)
	h.clk.Advance(5 * time.Second)
	h.expectPass(t, second)
}

// TestPlannerEarliestDeadlineTimer: a pass that reports a row deadline gets
// a pass at that deadline; a deadline past the patrol leaves the patrol.
//
// Kills: a missed row timer, and a row timer that delays the patrol.
func TestPlannerEarliestDeadlineTimer(t *testing.T) {
	passes := 0
	h := newPlannerHarness(t, func(_ *plannerHarness, now time.Time) passResult {
		passes++
		switch passes {
		case 1:
			return passResult{Next: now.Add(2 * time.Second)}
		default:
			return passResult{Next: now.Add(time.Hour)}
		}
	})
	h.start(t)
	h.p.markDirty("test")
	h.expectPass(t, plannerT0)

	deadline := plannerT0.Add(2 * time.Second)
	h.clk.waitArmed(t, deadline)
	h.clk.Advance(2 * time.Second)
	h.expectPass(t, deadline)

	patrol := deadline.Add(15 * time.Second)
	h.clk.waitArmed(t, patrol)
	h.clk.Advance(15 * time.Second)
	h.expectPass(t, patrol)
	if got := h.p.metrics.snapshot(patrol).Wakes; got["deadline"] != 1 || got["patrol"] != 1 {
		t.Fatalf("wakes = %v, want one deadline and one patrol", got)
	}
}

// TestPlannerPassPanicRecovered: a panicking pass is logged and recorded, the
// settlements it drained are kept, and a follow-up pass is owed and runs.
//
// Kills: a crash, a loop that stops after a panic, and a panic that waits
// for the patrol.
func TestPlannerPassPanicRecovered(t *testing.T) {
	passes := 0
	h := newPlannerHarness(t, func(_ *plannerHarness, _ time.Time) passResult {
		if passes++; passes == 1 {
			panic("boom")
		}
		return passResult{}
	})
	h.p.settlements.post(settlement{Key: rowKey{Leg: "s", ID: "a"}})
	h.start(t)
	h.expectPass(t, plannerT0)

	follow := plannerT0.Add(plannerMinGap)
	h.clk.waitArmed(t, follow)
	h.clk.Advance(plannerMinGap)
	h.expectPass(t, follow)
	h.clk.waitArmed(t, follow.Add(15*time.Second))

	h.stop(t)
	if out := h.stderr.String(); !strings.Contains(out, "pass panicked: boom") {
		t.Fatalf("stderr = %q, want the panic logged", out)
	}
	if m := h.p.metrics.snapshot(follow); m.Passes != 2 || m.Panics != 1 || m.Wakes["panic"] != 1 {
		t.Fatalf("metrics: passes %d, panics %d, wakes %v; want 2 passes, 1 panic, 1 panic wake", m.Passes, m.Panics, m.Wakes)
	}
	if h.p.last.Panicked || len(h.inflight.settled) != 1 {
		t.Fatalf("last pass panicked = %t, settled = %d; want a clean last pass and the settlement kept", h.p.last.Panicked, len(h.inflight.settled))
	}
}

func (h *plannerHarness) stop(t *testing.T) {
	t.Helper()
	stopped := make(chan struct{})
	go func() {
		h.p.stop(plannerT0.Add(time.Hour))
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(plannerTestGuard):
		t.Fatal("stop did not return")
	}
}

// TestPlannerSettlementQueueNeverBlocks: 10k posts with no pass running all
// return, and the next pass settles every one, in order.
//
// Kills: a bounded settlement queue, whose post blocks while no pass drains
// it (D-8).
func TestPlannerSettlementQueueNeverBlocks(t *testing.T) {
	const n = 10000
	h := newPlannerHarness(t, nil)
	posted := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			h.p.settlements.post(settlement{Key: rowKey{Leg: "s", ID: string(rune('a' + i%26))}, Kind: "start", Err: errors.New("x")})
		}
		close(posted)
	}()
	select {
	case <-posted:
	case <-time.After(plannerTestGuard):
		t.Fatal("posting 10k settlements with no pass running blocked")
	}
	h.start(t)
	h.expectPass(t, plannerT0)
	h.clk.waitArmed(t, plannerT0.Add(15*time.Second))
	h.stop(t)
	if got := len(h.inflight.settled); got != n {
		t.Fatalf("settled %d, want %d", got, n)
	}
	for i, s := range h.inflight.settled {
		if want := string(rune('a' + i%26)); s.Key.ID != want {
			t.Fatalf("settlement %d is %q, want %q: order lost", i, s.Key.ID, want)
		}
	}
	if got := h.p.settlements.drain(); len(got) != 0 {
		t.Fatalf("queue still holds %d settlements", len(got))
	}
}

// TestPlannerStopStopsAdmissionFirst: stop closes admission before it stops
// the executor, so no pass starts once stop is called, even for marks made
// during the running pass or the executor's stop; a pass already running
// finishes, and stop returns after run does.
//
// The run repeats because a loop that left the stop to select's random
// choice between quit and a ready mark or timer would start a pass only some
// of the time.
//
// Kills: a GUAR-013 regression (the executor stopped while the planner can
// still admit, or a pass after stop).
func TestPlannerStopStopsAdmissionFirst(t *testing.T) {
	for i := 0; i < 20; i++ {
		testPlannerStopStopsAdmissionFirst(t)
	}
}

func testPlannerStopStopsAdmissionFirst(t *testing.T) {
	t.Helper()
	inPass, release := make(chan struct{}), make(chan struct{})
	h := newPlannerHarness(t, func(h *plannerHarness, _ time.Time) passResult {
		close(inPass)
		<-release
		h.p.markDirty("test")
		return passResult{}
	})
	var admittingAtStop []bool
	h.p.stopEffects = func(deadline time.Time) {
		admittingAtStop = append(admittingAtStop, !h.p.stopping())
		h.p.markDirty("test")
		// From here every timer fires at once, so only the closed admission
		// keeps the loop from starting a follow-up pass.
		h.clk.mu.Lock()
		h.clk.auto = true
		h.clk.mu.Unlock()
		close(release)
		h.stops = append(h.stops, deadline)
	}
	h.start(t)
	h.p.markDirty("test")
	<-inPass
	h.stop(t)

	h.expectPass(t, plannerT0)
	h.expectNoPass(t)
	if len(admittingAtStop) != 1 || admittingAtStop[0] {
		t.Fatalf("executor stop calls saw admission open = %v, want one call with admission closed", admittingAtStop)
	}
	if want := plannerT0.Add(time.Hour); len(h.stops) != 1 || !h.stops[0].Equal(want) {
		t.Fatalf("executor stop deadlines = %v, want [%s]", h.stops, want)
	}
	select {
	case <-h.p.done:
	default:
		t.Fatal("stop returned before run did")
	}
	h.stop(t) // a second stop is harmless
}

// TestPlannerSingleOwnerRace runs passes back to back while many goroutines
// post settlements, mark dirty and pause starts. Under -race it fails if the
// planner-owned state is touched off the planner goroutine.
//
// Kills: shared mutable state (settling or tracing outside the pass).
func TestPlannerSingleOwnerRace(t *testing.T) {
	const posters, each = 8, 500
	reached := make(chan struct{})
	var once sync.Once
	h := newPlannerHarness(t, func(h *plannerHarness, now time.Time) passResult {
		<-h.passes // keep the report channel from filling
		h.p.rowTrace[rowKey{Leg: "s", ID: "a"}] = now.String()
		h.p.bucket = h.p.bucket.refill(now, 10, time.Minute)
		h.p.boot.CachePrimed = true
		_ = h.p.startsPaused()
		if len(h.inflight.settled) == posters*each {
			once.Do(func() { close(reached) })
		}
		return passResult{Counts: passCounts{InFlight: map[string]int{"start": len(h.inflight.settled)}}}
	})
	h.clk.auto = true
	h.start(t)
	var wg sync.WaitGroup
	for g := 0; g < posters; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				h.p.settlements.post(settlement{Kind: "start"})
				h.p.markDirty("poke")
				if i%50 == 0 {
					h.p.pauseStarts()
					h.p.resumeStarts()
					_ = h.p.metrics.snapshot(plannerT0)
				}
			}
		}()
	}
	wg.Wait()
	select {
	case <-reached:
	case <-time.After(plannerTestGuard):
		t.Fatal("the planner never settled every post")
	}
	h.stop(t)
}

// TestPassMetricsRecordsPasses pins the metrics a pass feeds: durations, the
// idle gap, the duty cycle, and the admitted, deferred, in-flight and age
// readings.
//
// Kills: a gap measured from the start instead of the end, counts that
// replace instead of accumulate, and a duty cycle over the wrong window.
func TestPassMetricsRecordsPasses(t *testing.T) {
	m := newPassMetrics()
	m.recordPass(plannerT0, time.Second, false, passCounts{
		Admitted: map[string]int{"start": 2},
		Deferred: map[deferral]int{{Kind: "start", Cause: "bucket"}: 1},
	})
	m.recordPass(plannerT0.Add(4*time.Second), time.Second, true, passCounts{
		Admitted:  map[string]int{"start": 1, "drain": 1},
		InFlight:  map[string]int{"start": 3},
		InputAges: map[string]time.Duration{"inventory": 2 * time.Second},
	})
	s := m.snapshot(plannerT0.Add(10 * time.Second))
	if s.Passes != 2 || s.Panics != 1 || s.LastDuration != time.Second || s.GapP50 != 3*time.Second {
		t.Fatalf("passes %d panics %d last %s gap %s; want 2, 1, 1s, 3s", s.Passes, s.Panics, s.LastDuration, s.GapP50)
	}
	if s.Admitted["start"] != 3 || s.Admitted["drain"] != 1 || s.Deferred[deferral{"start", "bucket"}] != 1 {
		t.Fatalf("admitted %v deferred %v", s.Admitted, s.Deferred)
	}
	if s.InFlight["start"] != 3 || s.InputAges["inventory"] != 2*time.Second {
		t.Fatalf("in flight %v ages %v", s.InFlight, s.InputAges)
	}
	if s.Duty != 0.2 {
		t.Fatalf("duty %v, want 0.2 (2s busy in 10s)", s.Duty)
	}
	late := m.snapshot(plannerT0.Add(passDutyWindow + 10*time.Second))
	if late.Duty != 0 {
		t.Fatalf("duty %v past the window, want 0", late.Duty)
	}
}

// TestRealPlannerClockTimer pins the real clock's timer: it fires, a Reset
// re-arms it, and Stop disarms it.
//
// Kills: a real timer whose Reset or Stop does not reach time.Timer.
func TestRealPlannerClockTimer(t *testing.T) {
	var clk plannerClock = realPlannerClock{}
	if clk.Now().IsZero() {
		t.Fatal("Now is zero")
	}
	timer := clk.NewTimer(0)
	expectFire := func() {
		t.Helper()
		select {
		case <-timer.C():
		case <-time.After(plannerTestGuard):
			t.Fatal("the timer did not fire")
		}
	}
	expectFire()
	timer.Reset(time.Hour)
	timer.Stop()
	select {
	case <-timer.C():
		t.Fatal("a stopped timer fired")
	default:
	}
	timer.Reset(0)
	expectFire()
}

// Kills a zero settlement At reaching the in-flight map, where it would make
// an ambiguous create's hard bound already passed: the drain stamps it with
// the pass time, and keeps an effect's own At.
func TestPlannerStampsZeroSettlementAt(t *testing.T) {
	h := newPlannerHarness(t, nil)
	own := plannerT0.Add(-time.Minute)
	h.p.settlements.post(settlement{Kind: inflightCreate, Token: "tok-zero"})
	h.p.settlements.post(settlement{Kind: inflightCreate, Token: "tok-own", At: own})
	h.p.runPass(plannerT0)
	got := h.inflight.settled
	if len(got) != 2 || !got[0].At.Equal(plannerT0) || !got[1].At.Equal(own) {
		t.Fatalf("settled = %+v, want the zero At stamped %v and the effect's own At kept", got, plannerT0)
	}
}
