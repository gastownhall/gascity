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
	"testing/synctest"
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

// TestStrandRecheckFreshness pins the approved formula itself (design doc
// §4.3): min(strandRecheckFreshCap, 2×interval + d).
func TestStrandRecheckFreshness(t *testing.T) {
	cases := []struct {
		interval, d time.Duration
		want        time.Duration
	}{
		{strandRecheckIntervalFloor, 0, 2 * strandRecheckIntervalFloor},
		{time.Minute, time.Second, 2*time.Minute + time.Second},
		{10 * time.Minute, 0, strandRecheckFreshCap},
		// Large enough that the uncapped formula would exceed the cap.
		{time.Hour, time.Hour, strandRecheckFreshCap},
		{strandRecheckFreshCap, 0, strandRecheckFreshCap},
	}
	for _, c := range cases {
		if got := strandRecheckFreshness(c.interval, c.d); got != c.want {
			t.Errorf("strandRecheckFreshness(%s, %s) = %s, want %s", c.interval, c.d, got, c.want)
		}
	}
}

// TestStrandRecheckFreshnessNeverExceedsTheCap is the property the formula
// must hold for EVERY interval and check duration, not just the pinned
// cases above: the cap is an upper bound, full stop.
func TestStrandRecheckFreshnessNeverExceedsTheCap(t *testing.T) {
	for _, interval := range []time.Duration{0, time.Second, strandRecheckIntervalFloor, time.Minute, time.Hour, 24 * time.Hour} {
		for _, d := range []time.Duration{0, time.Millisecond, time.Second, time.Minute, time.Hour} {
			if got := strandRecheckFreshness(interval, d); got > strandRecheckFreshCap {
				t.Errorf("strandRecheckFreshness(%s, %s) = %s, exceeds the cap %s", interval, d, got, strandRecheckFreshCap)
			}
		}
	}
}

// strandRecheckLapseEdge is the true steady-state no-lapse edge the
// formula's own doc comment derives (interval == 10d once d is past the
// floor's own crossover, d_next == d): 12d == strandRecheckFreshCap.
const strandRecheckLapseEdge = strandRecheckFreshCap / 12

// TestStrandRecheckNoLapseUpToTheDocumentedEdge is carry-over 1's property:
// at steady state (d_next == d), for every check duration d strictly below
// strandRecheckLapseEdge, a lease taken right as a tick starts does not
// lapse before the NEXT tick (which runs after
// strandRecheckNextInterval(d), and is assumed — the steady-state case — to
// take no longer than d itself) publishes a fresh one — i.e. the freshness
// budget strictly exceeds interval+d. This is the real requirement,
// fresh >= d+interval+d_next, not a stronger strict-inequality guarantee
// independent of d_next, across a range of d values the formula is
// designed to hold everywhere below the edge, not merely at the specific
// values pinned above.
func TestStrandRecheckNoLapseUpToTheDocumentedEdge(t *testing.T) {
	for d := time.Duration(0); d < strandRecheckLapseEdge; d += time.Second {
		interval := strandRecheckNextInterval(d)
		fresh := strandRecheckFreshness(interval, d)
		dNext := d
		if fresh < d+interval+dNext {
			t.Fatalf("d=%s: freshness %s does not outlast the wait until the next tick (d+interval+dNext = %s); a lease taken at this d would lapse before an on-time next tick republishes it", d, fresh, d+interval+dNext)
		}
	}
}

// TestStrandRecheckLapseBeginsOnlyPastTheDocumentedEdge is the other half of
// carry-over 1: past strandRecheckLapseEdge (d > 50s) the cap pins
// freshness, and the no-lapse guarantee is allowed to fail — but it must
// not fail EARLIER than the documented edge.
func TestStrandRecheckLapseBeginsOnlyPastTheDocumentedEdge(t *testing.T) {
	// Just below the edge: still guaranteed.
	d := strandRecheckLapseEdge - time.Second
	interval := strandRecheckNextInterval(d)
	fresh := strandRecheckFreshness(interval, d)
	if fresh < d+interval+d {
		t.Fatalf("d=%s (just below the documented edge %s): freshness %s does not outlast d+interval+d_next %s", d, strandRecheckLapseEdge, fresh, d+interval+d)
	}
	// Past the edge: the cap has taken over.
	for _, probe := range []time.Duration{strandRecheckLapseEdge + time.Second, strandRecheckLapseEdge + 5*time.Second, time.Minute, time.Hour} {
		if got := strandRecheckFreshness(strandRecheckNextInterval(probe), probe); got != strandRecheckFreshCap {
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

// TestStrandRecheckFirstIntervalHonoursTheKillSwitch (SHOULD 3, owner
// decision 2026-11-19): with the switch off, the FIRST tick runs at the
// fixed strandRecheckInterval, not the floor — the floor exists only to get
// a lease refreshed quickly, and with the switch off nothing is published.
func TestStrandRecheckFirstIntervalHonoursTheKillSwitch(t *testing.T) {
	resetContainmentLeaseKillSwitchForTest(t)
	t.Setenv(containmentLeaseKillSwitchEnv, "off")
	if got := strandRecheckFirstInterval(); got != strandRecheckInterval {
		t.Errorf("kill switch on: first recheck after %s, want the fixed %s", got, strandRecheckInterval)
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
// Driven from a CONVERGED city (not a genesis boot, which never publishes
// regardless of the switch) so the boot half actually exercises
// publishBootContainmentLease's own kill-switch check.
func TestStrandRecheckKillSwitchPublishesNoLease(t *testing.T) {
	cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = routes.close() })

	resetContainmentLeaseKillSwitchForTest(t)
	t.Setenv(containmentLeaseKillSwitchEnv, "off")

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

// TestPublishBootContainmentLeaseAccountsForTheBootCheckDuration pins the
// approved formula's boot seed: 2×strandRecheckIntervalFloor plus twice the
// boot gate's own check duration (routes.bootVerdictDuration), not a
// hardcoded zero. It also kills a mutant that seeds the boot lease from the
// 10-minute strandRecheckInterval instead of strandRecheckIntervalFloor
// (freshMS would then sit at the cap, which this test's upper bound
// catches).
func TestPublishBootContainmentLeaseAccountsForTheBootCheckDuration(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)

	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	if err != nil {
		t.Fatalf("the converged city refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	if routes.bootVerdictDuration <= 0 {
		t.Fatal("routes.bootVerdictDuration is 0; this test needs a real, positive boot check duration to tell seeds apart")
	}
	routes.bootVerdictDuration = 4 * time.Second // hand-set: avoids depending on the fixture's real timing.

	publishBootContainmentLease(routes)
	lease := routes.lease.Load()
	if lease == nil {
		t.Fatal("publishBootContainmentLease stored no lease")
	}
	want := strandRecheckFreshness(strandRecheckIntervalFloor, 2*routes.bootVerdictDuration).Milliseconds()
	if lease.FreshMS != want {
		t.Fatalf("FreshMS = %d, want %d computed as 2xfloor + 2xboot-duration from %s", lease.FreshMS, want, routes.bootVerdictDuration)
	}
	zeroDurationFreshMS := strandRecheckFreshness(strandRecheckIntervalFloor, 0).Milliseconds()
	if lease.FreshMS <= zeroDurationFreshMS {
		t.Fatalf("FreshMS = %d, want strictly more than the zero-duration seed %d now that the boot check took %s", lease.FreshMS, zeroDurationFreshMS, routes.bootVerdictDuration)
	}
	if lease.FreshMS >= strandRecheckFreshCap.Milliseconds() {
		t.Fatalf("FreshMS = %d, want well under the 10-minute cap %d for a boot check this cheap", lease.FreshMS, strandRecheckFreshCap.Milliseconds())
	}
}

// TestStrandRecheckRunReArmsTheTickerToTheAdaptiveInterval (P6, sleep-free,
// synctest): run()'s ticker.Reset(strandRecheckNextInterval(...)) after
// every tick, not just the fixed interval run() was seeded with. This
// kills three re-arm mutants: dropping the Reset call entirely (D3), one
// that re-arms to the original SEED interval unconditionally (D3b), and one
// that re-arms to the floor unconditionally regardless of the real adaptive
// interval (D3c) — the hand-set lastCheckDuration below makes the real
// adaptive interval exceed both the seed and the floor, so only the correct
// re-arm lands a second tick at the exact moment pinned.
func TestStrandRecheckRunReArmsTheTickerToTheAdaptiveInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &strandRecheck{stderr: io.Discard, logPrefix: "gc start"}
		var ticks atomic.Int64
		s.quiescent = func() bool { ticks.Add(1); return true } // skip the real read; keeps lastCheckDuration under test control.
		const d = 30 * time.Second                              // > strandRecheckIntervalFloor/10, so the adaptive interval (300s) exceeds both the floor and the seed.
		s.lastCheckDuration = d

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); s.run(ctx, 10*time.Millisecond) }()

		advance(10 * time.Millisecond)
		if got := ticks.Load(); got != 1 {
			t.Fatalf("after the seed interval: %d ticks, want 1", got)
		}

		adaptive := strandRecheckNextInterval(d)
		advance(adaptive - time.Millisecond)
		if got := ticks.Load(); got != 1 {
			t.Fatalf("before the adaptive interval (%s) elapsed: %d ticks, want still 1", adaptive, got)
		}

		advance(time.Millisecond)
		if got := ticks.Load(); got != 2 {
			t.Fatalf("at the adaptive interval: %d ticks, want 2", got)
		}

		cancel()
		<-done
	})
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
	s.publishLease(report, takenAt)

	lease := s.routes.lease.Load()
	if lease == nil {
		t.Fatal("publishLease stored no lease")
	}
	realInterval := strandRecheckNextInterval(slow)
	if realInterval <= strandRecheckIntervalFloor {
		t.Fatalf("fixture: strandRecheckNextInterval(%s) = %s, want it to exceed the floor %s for this test to tell the two seeds apart", slow, realInterval, strandRecheckIntervalFloor)
	}
	want := strandRecheckFreshness(realInterval, slow).Milliseconds()
	if lease.FreshMS != want {
		t.Fatalf("FreshMS = %d, want %d from the real adaptive interval %s", lease.FreshMS, want, realInterval)
	}
	floorSeeded := strandRecheckFreshness(strandRecheckIntervalFloor, slow).Milliseconds()
	if lease.FreshMS == floorSeeded {
		t.Fatalf("FreshMS = %d equals the floor-seeded value %d; the real adaptive interval must be used, not the floor", lease.FreshMS, floorSeeded)
	}
}

// TestStrandRecheckPanicRecordsItsOwnCheckDuration kills a mutant that
// drops tick()'s panic-recover update of s.lastCheckDuration: the time a
// tick spent before panicking must still be recorded, exactly as a normal
// check's duration is, since run()'s ticker re-arm and the next
// publishLease both read it. A sentinel set before the panicking tick,
// rather than a time.Sleep inside the stub, tells the two cases apart: no
// real check ever takes 999 hours, so if it is still there after the tick,
// the recover path never recorded anything.
func TestStrandRecheckPanicRecordsItsOwnCheckDuration(t *testing.T) {
	_, _, s := workStoreStickyFixture(t)

	const sentinel = 999 * time.Hour
	s.lastCheckDuration = sentinel

	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		panic("boom")
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })

	s.tick() // must not panic

	if s.lastCheckDuration == sentinel {
		t.Errorf("lastCheckDuration after a panicked tick is still the sentinel %s set before it; the panic-recover path must record how long the check ran", sentinel)
	}
}

// TestStorageBootGateMeasuresABootVerdictDurationNoSleep (P7, sleep-free):
// the boot gate measures a real, positive check duration bounded by the
// call's own wall time, with no fixed sleep required to observe it.
func TestStorageBootGateMeasuresABootVerdictDurationNoSleep(t *testing.T) {
	cityPath, cfg, _, _ := convergedCityWithRecheckLease(t)
	start := time.Now()
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, io.Discard)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = routes.close() })
	if routes.bootVerdictDuration <= 0 || routes.bootVerdictDuration > elapsed {
		t.Fatalf("bootVerdictDuration = %s, want in (0, %s]", routes.bootVerdictDuration, elapsed)
	}

	routes.bootVerdictDuration = 30 * time.Second
	publishBootContainmentLease(routes)
	l := routes.lease.Load()
	if l == nil {
		t.Fatal("no boot lease")
	}
	if want := strandRecheckFreshness(strandRecheckIntervalFloor, 2*30*time.Second).Milliseconds(); l.FreshMS != want {
		t.Fatalf("FreshMS = %d, want %d", l.FreshMS, want)
	}
}
