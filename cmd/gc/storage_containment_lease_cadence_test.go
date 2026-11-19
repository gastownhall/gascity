package main

// Cadence and no-lapse checks (design doc §4.3, carry-over 1) plus the
// panic-as-failed-attempt rule (carry-over 6): a contained panic in
// strandRecheck.tick() must extend the last published lease exactly like a
// transient recheck fault, not leave it to age on the normal cadence as
// though nothing happened.

import (
	"bytes"
	"strings"
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

// TestStrandRecheckFreshness pins min(strandRecheckFreshCap, 2×interval+d).
func TestStrandRecheckFreshness(t *testing.T) {
	cases := []struct {
		interval, d time.Duration
		want        time.Duration
	}{
		{strandRecheckIntervalFloor, 0, 2 * strandRecheckIntervalFloor},
		{time.Minute, time.Second, 2*time.Minute + time.Second},
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

// TestStrandRecheckNoLapseUpToTheDocumentedEdge is carry-over 1's property:
// for a check duration d up to roughly the ~55s edge storage_boot.go's own
// comment documents, a lease taken right as a tick starts does not lapse
// before the NEXT tick (which runs after strandRecheckNextInterval(d))
// publishes a fresh one — i.e. the freshness budget strictly exceeds the
// wait until the next tick. Past that edge the cap pins freshness and the
// property is allowed to fail, which is exactly what carry-over 1 asks to
// be documented rather than fixed.
func TestStrandRecheckNoLapseUpToTheDocumentedEdge(t *testing.T) {
	for d := time.Duration(0); d <= 54*time.Second; d += time.Second {
		interval := strandRecheckNextInterval(d)
		fresh := strandRecheckFreshness(interval, d)
		if fresh <= interval+d {
			t.Fatalf("d=%s: freshness %s does not outlast the wait until the next tick (interval %s + d %s = %s); a lease taken at this d would lapse before an on-time next tick republishes it", d, fresh, interval, d, interval+d)
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
// RecheckFailing, exactly as extendLeaseOnTransientFailureFor does for an
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
