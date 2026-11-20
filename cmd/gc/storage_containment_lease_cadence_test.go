package main

// Cadence and no-lapse checks (design doc §4.3, carry-over 1) plus the
// panic-as-failed-attempt rule (carry-over 6): a contained panic in
// strandRecheck.tick() must extend the last published lease exactly like a
// transient recheck fault, not leave it to age on the normal cadence as
// though nothing happened.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// TestStrandRecheckNextInterval pins the adaptive formula itself:
// max(strandRecheckIntervalFloor, 10×lastCheckDuration).
func TestStrandRecheckNextInterval(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want time.Duration
	}{
		{0, strandRecheckIntervalFloor},
		{time.Millisecond, strandRecheckIntervalFloor},
		{strandRecheckIntervalFloor / 10, strandRecheckIntervalFloor},
		// Just past the point 10d crosses the floor.
		{strandRecheckIntervalFloor/10 + time.Second, 10 * (strandRecheckIntervalFloor/10 + time.Second)},
		{time.Minute, 10 * time.Minute},
		{time.Hour, 10 * time.Hour},
	}
	for _, c := range cases {
		if got := strandRecheckNextInterval(c.d); got != c.want {
			t.Errorf("strandRecheckNextInterval(%s) = %s, want %s", c.d, got, c.want)
		}
	}
}

// TestStrandRecheckFreshness pins
// min(strandRecheckFreshCap, d+interval+dNext+margin) where
// dNext = max(d,last)+margin.
func TestStrandRecheckFreshness(t *testing.T) {
	cases := []struct {
		interval, d, last time.Duration
		want              time.Duration
	}{
		// 0 + 60s + max(0,0) + 2x1s margin = 62s.
		{strandRecheckIntervalFloor, 0, 0, 62 * time.Second},
		// 1s + 60s + max(1s,1s) + 2s = 64s.
		{time.Minute, time.Second, time.Second, 64 * time.Second},
		// last greater than d: dNext is bound by last, not d.
		// 1s + 60s + max(1s,10s) + 2s = 73s.
		{time.Minute, time.Second, 10 * time.Second, 73 * time.Second},
		// Large enough that the uncapped formula would exceed the cap.
		{time.Hour, time.Hour, time.Hour, strandRecheckFreshCap},
		{strandRecheckFreshCap, 0, 0, strandRecheckFreshCap},
	}
	for _, c := range cases {
		if got := strandRecheckFreshness(c.interval, c.d, c.last); got != c.want {
			t.Errorf("strandRecheckFreshness(%s, %s, %s) = %s, want %s", c.interval, c.d, c.last, got, c.want)
		}
	}
}

// TestStrandRecheckFreshnessNeverExceedsTheCap is the property the formula
// must hold for EVERY interval, check duration and previous duration, not
// just the pinned cases above: the cap is an upper bound, full stop.
func TestStrandRecheckFreshnessNeverExceedsTheCap(t *testing.T) {
	for _, interval := range []time.Duration{0, time.Second, strandRecheckIntervalFloor, time.Minute, time.Hour, 24 * time.Hour} {
		for _, d := range []time.Duration{0, time.Millisecond, time.Second, time.Minute, time.Hour} {
			for _, last := range []time.Duration{0, time.Millisecond, time.Second, time.Minute, time.Hour} {
				if got := strandRecheckFreshness(interval, d, last); got > strandRecheckFreshCap {
					t.Errorf("strandRecheckFreshness(%s, %s, %s) = %s, exceeds the cap %s", interval, d, last, got, strandRecheckFreshCap)
				}
			}
		}
	}
}

// strandRecheckLapseEdge is the true steady-state no-lapse edge the formula's
// own doc comment derives (last == d, interval == 10d once d is past the
// floor's own 6s crossover): 12d+2×margin == strandRecheckFreshCap.
var strandRecheckLapseEdge = (strandRecheckFreshCap - 2*strandRecheckLapseMargin) / 12

// TestStrandRecheckNoLapseUpToTheDocumentedEdge is carry-over 1's property,
// reworked for the new formula: at steady state (last == d), for every
// check duration d strictly below strandRecheckLapseEdge, a lease taken
// right as a tick starts does not lapse before the NEXT tick (which runs
// after strandRecheckNextInterval(d), and is conservatively bounded to take
// no longer than dNext = d+margin) publishes a fresh one — i.e. the
// freshness budget strictly exceeds interval+dNext. This is the no-lapse
// property across a range of d values the formula is designed to hold
// everywhere below the edge, not merely at the specific values pinned
// above.
func TestStrandRecheckNoLapseUpToTheDocumentedEdge(t *testing.T) {
	for d := time.Duration(0); d < strandRecheckLapseEdge; d += time.Second {
		interval := strandRecheckNextInterval(d)
		fresh := strandRecheckFreshness(interval, d, d)
		dNext := d + strandRecheckLapseMargin
		if fresh <= interval+dNext {
			t.Fatalf("d=%s: freshness %s does not outlast the wait until the next tick (interval %s + dNext %s = %s); a lease taken at this d would lapse before an on-time next tick republishes it", d, fresh, interval, dNext, interval+dNext)
		}
	}
}

// TestStrandRecheckLapseBeginsOnlyPastTheDocumentedEdge is the other half of
// carry-over 1: past strandRecheckLapseEdge the cap pins freshness, and the
// no-lapse guarantee is allowed to fail — but it must not fail EARLIER than
// the documented edge. This also kills a mutant that computes freshness
// from strandRecheckIntervalFloor instead of the real interval/d/last: such
// a mutant pins freshness far below the true formula's value once d grows
// past the floor, so the no-lapse property would break well before this
// test's edge instead of exactly at it.
func TestStrandRecheckLapseBeginsOnlyPastTheDocumentedEdge(t *testing.T) {
	// Just below the edge: still guaranteed.
	d := strandRecheckLapseEdge - time.Second
	interval := strandRecheckNextInterval(d)
	fresh := strandRecheckFreshness(interval, d, d)
	dNext := d + strandRecheckLapseMargin
	if fresh <= interval+dNext {
		t.Fatalf("d=%s (just below the documented edge %s): freshness %s does not outlast interval+dNext %s", d, strandRecheckLapseEdge, fresh, interval+dNext)
	}
	// Past the edge (by a full second, clear of any integer-division
	// rounding at the edge itself): the cap has taken over, and the cap is
	// no longer guaranteed to outlast interval+dNext.
	for _, probe := range []time.Duration{strandRecheckLapseEdge + time.Second, strandRecheckLapseEdge + 5*time.Second, time.Minute, time.Hour} {
		if got := strandRecheckFreshness(strandRecheckNextInterval(probe), probe, probe); got != strandRecheckFreshCap {
			t.Errorf("d=%s (at or past the documented edge): freshness = %s, want the cap %s", probe, got, strandRecheckFreshCap)
		}
	}
}

// TestStrandRecheckFirstInterval pins the first-tick seed: min(the
// (possibly test-shortened) strandRecheckInterval, the floor). A test that
// shortens strandRecheckInterval below the floor to get a prompt first
// tick must still see that shortened value, unmodified.
func TestStrandRecheckFirstInterval(t *testing.T) {
	prev := strandRecheckInterval
	t.Cleanup(func() { strandRecheckInterval = prev })

	strandRecheckInterval = time.Millisecond
	if got := strandRecheckFirstInterval(); got != time.Millisecond {
		t.Errorf("strandRecheckFirstInterval() = %s with a shortened interval, want %s unmodified", got, time.Millisecond)
	}

	strandRecheckInterval = time.Hour
	if got := strandRecheckFirstInterval(); got != strandRecheckIntervalFloor {
		t.Errorf("strandRecheckFirstInterval() = %s with a long interval, want the floor %s", got, strandRecheckIntervalFloor)
	}
}

// TestStrandRecheckPanicExtendsTheLeaseLikeATransientFailure is carry-over
// 6, at the lease itself: TestStrandRecheckIgnoresATransientFaultAndContainsAPanic
// (slice 1) already proves a panicked tick publishes no event and logs
// "panicked"; this proves the OTHER half slice 2 adds — the lease already
// published before the panic is kept, but re-dated and marked
// RecheckFailing, exactly as extendLeaseOnTransientFailure does for an
// ordinary transient fault, rather than left to age on the normal cadence.
func TestStrandRecheckPanicExtendsTheLeaseLikeATransientFailure(t *testing.T) {
	cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
	var boot bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &boot)
	if err != nil {
		t.Fatalf("boot refused: %v\n%s", err, boot.String())
	}
	t.Cleanup(func() { _ = routes.close() })

	rec := events.NewFake()
	var stderr bytes.Buffer
	s := newStrandRecheck(routes, cityPath, rec, &stderr, "gc start")
	s.tick() // a normal tick, to publish a real lease to extend.
	before := s.routes.lease.Load()
	if before == nil {
		t.Fatal("the control tick published no lease; the fixture never reached the scenario under test")
	}

	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = func(string) (beads.Store, error) { panic("boom") }
	beforeAttempt := time.Now()
	s.tick() // must not panic
	openInfraMigrationSource = prev

	if !strings.Contains(stderr.String(), "panicked") {
		t.Fatalf("the contained panic was not logged: %q", stderr.String())
	}
	after := s.routes.lease.Load()
	if after == nil {
		t.Fatal("the panicked tick withdrew the lease instead of extending it")
	}
	if !after.RecheckFailing {
		t.Error("a panicked tick must mark the extended lease RecheckFailing, same as an ordinary transient fault")
	}
	if after.FreshMS != strandRecheckFreshCap.Milliseconds() {
		t.Errorf("FreshMS after a panicked tick = %d, want the full cap %d", after.FreshMS, strandRecheckFreshCap.Milliseconds())
	}
	if after.LastAttemptAt.Before(beforeAttempt) {
		t.Errorf("LastAttemptAt = %s, want it taken at (or after) this panicked attempt (%s)", after.LastAttemptAt, beforeAttempt)
	}
	if after.Outcome != before.Outcome || after.Serving != before.Serving {
		t.Errorf("the panicked tick changed the verdict it was extending: before %+v, after %+v", before, after)
	}
}

// TestStrandRecheckNextIntervalKillSwitchIsFixedCadence (owner decision
// 2026-11-19): with GC_STORAGE_CONTAINMENT_LEASE=off, the recheck's
// cadence is the FIXED strandRecheckInterval, not the adaptive formula —
// the adaptive cadence exists to keep a published lease fresh, and with
// the switch off nothing is published for it to serve.
func TestStrandRecheckNextIntervalKillSwitchIsFixedCadence(t *testing.T) {
	resetContainmentLeaseKillSwitchForTest(t)
	t.Setenv(containmentLeaseKillSwitchEnv, "off")
	prev := strandRecheckInterval
	t.Cleanup(func() { strandRecheckInterval = prev })
	strandRecheckInterval = 7 * time.Minute

	for _, d := range []time.Duration{0, time.Second, time.Hour} {
		if got := strandRecheckNextInterval(d); got != strandRecheckInterval {
			t.Errorf("strandRecheckNextInterval(%s) with the switch off = %s, want the fixed %s", d, got, strandRecheckInterval)
		}
	}
}

// TestStrandRecheckKillSwitchPublishesNoLease (owner decision 2026-11-19):
// with the switch off, neither the boot publish nor a definitive recheck
// tick stores a lease — the controller publishes and serves none of it.
func TestStrandRecheckKillSwitchPublishesNoLease(t *testing.T) {
	resetContainmentLeaseKillSwitchForTest(t)
	t.Setenv(containmentLeaseKillSwitchEnv, "off")

	cityPath := t.TempDir()
	writeScopeProjectID(t, cityPath, "prj-switch-off")
	cfg := infraSplitConfig(t.TempDir())
	stubInfraMigrationSource(t)

	var boot bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &boot)
	if err != nil {
		t.Fatalf("genesis boot refused: %v\n%s", err, boot.String())
	}
	t.Cleanup(func() { _ = routes.close() })

	publishBootContainmentLease(routes)
	if l := routes.lease.Load(); l != nil {
		t.Fatalf("publishBootContainmentLease stored a lease with the kill switch set: %+v", l)
	}

	s := newStrandRecheck(routes, cityPath, events.NewFake(), io.Discard, "gc start")
	if s == nil {
		t.Fatal("no recheck")
	}
	s.tick()
	if l := routes.lease.Load(); l != nil {
		t.Fatalf("a definitive recheck tick stored a lease with the kill switch set: %+v", l)
	}
}

// TestPublishBootContainmentLeaseAccountsForTheBootCheckDuration pins item 2
// of the round-2 review: the boot gate's own check duration
// (routes.bootVerdictDuration), not a hardcoded zero, seeds the very first
// lease's freshness budget. It also kills two mutants: one that seeds the
// boot lease from a zero duration regardless of how long the boot check
// actually took (freshMS would then sit at the small floor-only value this
// test proves it does NOT), and one that seeds it from the 10-minute
// strandRecheckInterval instead of strandRecheckIntervalFloor (freshMS
// would then sit at the cap, which this test's upper bound also catches).
func TestPublishBootContainmentLeaseAccountsForTheBootCheckDuration(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)

	const bootDelay = 50 * time.Millisecond
	prevOpen := openInfraMigrationSource
	openInfraMigrationSource = func(path string) (beads.Store, error) {
		time.Sleep(bootDelay)
		return prevOpen(path)
	}
	t.Cleanup(func() { openInfraMigrationSource = prevOpen })

	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	if routes.bootVerdictDuration < bootDelay {
		t.Fatalf("routes.bootVerdictDuration = %s, want at least the %s the boot check was slowed by", routes.bootVerdictDuration, bootDelay)
	}

	publishBootContainmentLease(routes)
	lease := routes.lease.Load()
	if lease == nil {
		t.Fatal("publishBootContainmentLease stored no lease")
	}
	want := strandRecheckFreshness(strandRecheckIntervalFloor, routes.bootVerdictDuration, routes.bootVerdictDuration).Milliseconds()
	if lease.FreshMS != want {
		t.Fatalf("FreshMS = %d, want %d computed from the real boot check duration %s", lease.FreshMS, want, routes.bootVerdictDuration)
	}
	// A mutant seeding from a zero duration would sit near the floor's own
	// 62s; a mutant seeding from strandRecheckInterval (10 minutes) would
	// sit at the 10-minute cap. The real formula, fed a mere tens-of-ms
	// delay, lands well clear of both: comfortably above the zero-duration
	// floor value and comfortably below the cap.
	zeroDurationFreshMS := strandRecheckFreshness(strandRecheckIntervalFloor, 0, 0).Milliseconds()
	if lease.FreshMS <= zeroDurationFreshMS {
		t.Fatalf("FreshMS = %d, want strictly more than the zero-duration seed %d now that the boot check took %s", lease.FreshMS, zeroDurationFreshMS, routes.bootVerdictDuration)
	}
	if lease.FreshMS >= strandRecheckFreshCap.Milliseconds() {
		t.Fatalf("FreshMS = %d, want well under the 10-minute cap %d for a boot check slowed by only %s", lease.FreshMS, strandRecheckFreshCap.Milliseconds(), bootDelay)
	}
}

// TestStrandRecheckRunReArmsTheTickerToTheAdaptiveInterval pins item 3 of
// the round-2 review: run()'s ticker.Reset(strandRecheckNextInterval(...))
// after every tick, not just the fixed interval run() was seeded with. This
// kills a "never re-arm" mutant (dropping the Reset call, or resetting to
// the original seed interval unconditionally): without it, a run seeded
// with a short first interval keeps ticking at that short interval forever,
// instead of settling onto the adaptive cadence (here, the floor, since the
// fixture's check is cheap) after its first tick.
func TestStrandRecheckRunReArmsTheTickerToTheAdaptiveInterval(t *testing.T) {
	cityPath, _, target, _ := convergedCityWithRecheckLease(t)
	s := newStrandRecheckFixture(cityPath, target)

	var ticks atomic.Int64
	s.quiescent = func() bool {
		ticks.Add(1)
		return false // never actually quiescent; just counting calls.
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Seeded with a very short first interval: with no re-arm, every
		// following tick would also fire this fast.
		s.run(ctx, 10*time.Millisecond)
	}()

	// Let the first tick land, and a little more, so a non-re-arming ticker
	// would have fired several more times in the window below.
	deadline := time.After(300 * time.Millisecond)
	for ticks.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the first tick never landed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	afterFirst := ticks.Load()

	// The fixture's check is cheap, so the re-armed interval is the floor
	// (strandRecheckIntervalFloor, 60s) — comfortably longer than this
	// window. A ticker still running at the 10ms seed would tick dozens of
	// times in it; a correctly re-armed one ticks zero more times.
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	if got := ticks.Load() - afterFirst; got != 0 {
		t.Errorf("%d more tick(s) landed after the first, within a 300ms window; want 0 once the ticker re-arms to the adaptive (floor) interval", got)
	}
}

// TestStrandRecheckPublishLeaseUsesTheRealAdaptiveIntervalNotTheFloor kills
// a mutant that seeds publishLease's freshness budget from
// strandRecheckIntervalFloor regardless of the actually-computed adaptive
// interval: for a check slow enough that the real interval
// (strandRecheckNextInterval) exceeds the floor, the published lease's
// FreshMS must reflect the REAL interval, not the floor. (Every other
// publishLease test in this package uses the fixture's own fast check, so
// the real and floor-seeded values coincide there and this mutant would
// slip past them; this test's hand-set, slow lastCheckDuration is what
// tells them apart.)
func TestStrandRecheckPublishLeaseUsesTheRealAdaptiveIntervalNotTheFloor(t *testing.T) {
	cityPath, _, target, _ := convergedCityWithRecheckLease(t)
	s := newStrandRecheckFixture(cityPath, target)

	report := confirmInfraContainment(cityPath, target, "gc start", io.Discard)
	report.Target = target
	takenAt := time.Now()
	report.CheckedAt = takenAt

	const slow = 30 * time.Second // > 6s, so the real interval (10x) exceeds the floor.
	s.lastCheckDuration = slow
	s.prevCheckDuration = 0
	s.publishLease(report, takenAt)

	lease := s.routes.lease.Load()
	if lease == nil {
		t.Fatal("publishLease stored no lease")
	}
	realInterval := strandRecheckNextInterval(slow)
	if realInterval <= strandRecheckIntervalFloor {
		t.Fatalf("fixture: strandRecheckNextInterval(%s) = %s, want it to exceed the floor %s for this test to tell the two seeds apart", slow, realInterval, strandRecheckIntervalFloor)
	}
	want := strandRecheckFreshness(realInterval, slow, 0).Milliseconds()
	if lease.FreshMS != want {
		t.Fatalf("FreshMS = %d, want %d from the real adaptive interval %s", lease.FreshMS, want, realInterval)
	}
	floorSeeded := strandRecheckFreshness(strandRecheckIntervalFloor, slow, 0).Milliseconds()
	if lease.FreshMS == floorSeeded {
		t.Fatalf("FreshMS = %d equals the floor-seeded value %d; the real adaptive interval must be used, not the floor", lease.FreshMS, floorSeeded)
	}
}

// TestStrandRecheckPanicRecordsItsOwnCheckDuration kills a mutant that
// drops tick()'s panic-recover update of s.lastCheckDuration: the time a
// tick spent before panicking must still be recorded, exactly as a normal
// check's duration is, since run()'s ticker re-arm and the next
// publishLease both read it.
func TestStrandRecheckPanicRecordsItsOwnCheckDuration(t *testing.T) {
	_, _, s := workStoreStickyFixture(t)

	const delay = 30 * time.Millisecond
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		time.Sleep(delay)
		panic("boom")
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })

	s.tick() // must not panic

	if s.lastCheckDuration < delay {
		t.Errorf("lastCheckDuration after a panicked tick = %s, want at least the %s the panicking check was slowed by", s.lastCheckDuration, delay)
	}
}
