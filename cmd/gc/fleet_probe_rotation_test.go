package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/resilience"
)

// fleetProbeRun is nine asleep sessions, each its own template, slept at
// strictly increasing times so w0 sorts first by wake fairness. Every wake
// fails except the one of healthy. One fake clock drives the executor and the
// guard. A failed wake restores the session's pre-wake state, so the same
// oldest session sorts first again unless the fleet path rotates the probe;
// pending-create rows would not show that, because a failed pending-create
// start closes the row and advances the queue by itself.
type fleetProbeRun struct {
	e     *capacityEnv
	g     *fleetStartGuard
	cands []startCandidate
	names []string
}

func newFleetProbeRun(t *testing.T, healthy string) *fleetProbeRun {
	t.Helper()
	r := &fleetProbeRun{}
	for i := 0; i < 9; i++ {
		r.names = append(r.names, fmt.Sprintf("w%d", i))
	}
	r.e = newCapacityEnv(t, false, r.names...)
	r.e.limitToOneWake()
	r.g = newFleetStartGuard(r.e.clk.Now, fleetTestStartupTimeout)
	r.e.startOptions = append(r.e.startOptions, withFleetStartGuard(r.g))
	base := r.e.clk.Now()
	for i, n := range r.names {
		r.cands = append(r.cands, r.e.session(t, n, "", map[string]string{
			"session_name":       n,
			"template":           n,
			"state":              "asleep",
			"generation":         "1",
			"continuation_epoch": "1",
			"instance_token":     "tok-" + n,
			"slept_at":           base.Add(-time.Duration(len(r.names)-i) * time.Hour).UTC().Format(time.RFC3339),
		}))
		if n != healthy {
			r.e.sp.StartErrors[n] = errors.New("boom")
		}
	}
	return r
}

// tick runs one executor pass over the sessions the reconciler would hand it:
// not running, and not in a wake-failure quarantine.
func (r *fleetProbeRun) tick(t *testing.T) {
	t.Helper()
	var live []startCandidate
	for _, c := range r.cands {
		if r.e.sp.IsRunning(c.name()) {
			continue
		}
		rc := r.e.refreshed(c)
		if q := r.e.bead(t, c.info.ID).Metadata["quarantined_until"]; q != "" {
			if qt, err := time.Parse(time.RFC3339, q); err == nil && qt.After(r.e.clk.Now()) {
				continue
			}
		}
		live = append(live, rc)
	}
	r.e.start(context.Background(), live...)
}

func (r *fleetProbeRun) attempts() int {
	n := 0
	for _, name := range r.names {
		n += r.e.sp.CountCalls("Start", name)
	}
	return n
}

// The half-open probe goes to a session that has not failed this episode, so a
// healthy template behind broken older ones is probed, and the breaker closes
// on it. Without rotation the oldest failing sessions take every probe and the
// breaker stays open.
func TestExecutePlannedStarts_FleetProbeRotatesToTheHealthyTemplate(t *testing.T) {
	r := newFleetProbeRun(t, "w5")
	const tickEvery = 5 * time.Second
	const budget = 10 * time.Minute

	tripAttempts := -1
	probes := 0
	var healthyAt time.Duration = -1
	var closedAt time.Duration = -1
	prev := 0
	for el := time.Duration(0); el <= budget; el += tickEvery {
		r.tick(t)
		if r.e.sp.IsRunning("w5") {
			// The probe session stays up: feed its liveness so the stability
			// window can confirm it.
			if healthyAt < 0 {
				healthyAt = el
			}
			r.g.ObserveLiveness(r.cands[5].info.ID, true)
		}
		if got := r.attempts(); got > prev {
			if fleetState(r.g) != resilience.StateClosed && tripAttempts < 0 {
				tripAttempts = prev + (got - prev)
			}
			if tripAttempts >= 0 && got > tripAttempts {
				probes += got - prev
			}
			prev = got
		}
		if fleetState(r.g) == resilience.StateClosed && healthyAt >= 0 {
			closedAt = el
			break
		}
		r.e.clk.Advance(tickEvery)
	}
	if tripAttempts != 3 {
		t.Fatalf("provider attempts when the breaker opened = %d, want 3", tripAttempts)
	}
	if healthyAt < 0 {
		t.Fatalf("the healthy w5 was never probed in %s; Start calls per session: %v", budget, r.startCounts())
	}
	if closedAt < 0 {
		t.Fatalf("the breaker did not close after the healthy probe; state = %v", fleetState(r.g))
	}
	if probes > 7 {
		t.Errorf("probes before the breaker closed = %d, want at most 7 in %s", probes, budget)
	}
	if got := r.attempts(); got > 10 {
		t.Errorf("provider attempts = %d, want at most 10 until the breaker closed", got)
	}
}

func (r *fleetProbeRun) startCounts() map[string]int {
	m := map[string]int{}
	for _, n := range r.names {
		m[n] = r.e.sp.CountCalls("Start", n)
	}
	return m
}

// With the breaker closed, the executor attempts sessions in wake fairness
// order even when some of them already failed a start inside the trip window:
// the oldest session goes first, failed or not. Only an open episode rotates
// the probe.
func TestExecutePlannedStarts_ClosedBreakerKeepsWakeOrderAfterPreTripFailures(t *testing.T) {
	r := newFleetProbeRun(t, "")
	failFleetStart(t, r.g, r.cands[0].info.ID, "w0")
	failFleetStart(t, r.g, r.cands[1].info.ID, "w1")
	if got := fleetState(r.g); got != resilience.StateClosed {
		t.Fatalf("premise: state = %v, want closed", got)
	}
	r.tick(t)
	// The tick attempts sessions oldest first until the third distinct failure
	// trips the breaker. A sort that demoted the sessions which failed before
	// the trip would attempt w2, w3 and w4 first and leave w0 untried.
	counts := r.startCounts()
	if counts["w0"] != 1 {
		t.Fatalf("Start calls after one tick = %v, want the oldest session w0 attempted although it failed before the trip", counts)
	}
	for _, n := range r.names[3:] {
		if counts[n] != 0 {
			t.Fatalf("Start calls after one tick = %v, want %s untried: sessions behind the oldest three must wait their turn", counts, n)
		}
	}
}
