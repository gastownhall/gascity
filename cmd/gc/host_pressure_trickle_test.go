package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// heldStartsOutcomes lists the outcomes of rec's held_starts events.
func heldStartsOutcomes(t *testing.T, rec *events.Fake) []string {
	t.Helper()
	var out []string
	for _, ev := range rec.Events {
		if ev.Type != events.SupervisorCPUPressureHeldStarts {
			continue
		}
		var p events.SupervisorCPUPressureHeldStartsPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("decoding %s payload: %v", ev.Type, err)
		}
		out = append(out, p.Outcome)
	}
	return out
}

// AC-5: the episode opens the trickle window at its start and after each
// forced start; once the hold has lasted 5 patrol intervals the next pass
// may trickle, and the outcomes are held at the start, released at the end,
// and nothing on a plain held pass.
func TestHostPressureEpisodeTricklesAfterFiveIntervals(t *testing.T) {
	const patrol = 10 * time.Second
	t0 := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	var e hostPressureEpisode
	low := hostPressure{Avg10: 10, Threshold: 50}
	step := func(p hostPressure, at time.Duration, wantTrickle bool, wantOutcome string) {
		t.Helper()
		trickle, outcome := e.observe(p, t0.Add(at), patrol)
		if trickle != wantTrickle || outcome != wantOutcome {
			t.Fatalf("at +%s: trickle %t outcome %q, want %t %q", at, trickle, outcome, wantTrickle, wantOutcome)
		}
	}
	step(low, 0, false, "")
	step(highHostPressure, 0, false, hostPressureOutcomeHeld)
	step(highHostPressure, 4*patrol, false, "")
	step(highHostPressure, 5*patrol, true, "")
	step(highHostPressure, 6*patrol, true, "") // nothing forced yet: still due
	e.forced(t0.Add(6 * patrol))
	step(highHostPressure, 7*patrol, false, "")
	step(highHostPressure, 11*patrol, true, "")
	step(low, 12*patrol, false, hostPressureOutcomeReleased)
	step(low, 13*patrol, false, "")
	step(highHostPressure, 14*patrol, false, hostPressureOutcomeHeld)
}

// AC-5 (v2 admission): a trickling hold admits exactly one start or create,
// the first that clears the remaining gates, and reports it forced.
func TestAdmitTrickleAdmitsExactlyOneStartOrCreate(t *testing.T) {
	intents := func() []intent {
		return []intent{startOn(1, "", 0), startOn(2, "", 1), poolCreate("w", ""), rowIntent(intentStop, 3)}
	}
	in := admitIn(5, 4)
	in.Hold = startHold{Cause: causeHostPressure, Trickle: true}
	res := admit(in, intents())
	var startsOrCreates int
	for _, it := range res.Admitted {
		if it.Kind == intentStart || it.Kind == intentCreate {
			startsOrCreates++
		}
	}
	if startsOrCreates != 1 || len(res.Admitted) != 2 || !res.Forced {
		t.Fatalf("admitted %v deferred %v forced %t: want one start or create forced through and the stop", keysOf(res.Admitted), causesOf(res.Deferred), res.Forced)
	}
	for key, cause := range causesOf(res.Deferred) {
		if cause != causeHostPressure {
			t.Fatalf("deferred %s with cause %q, want %q", key, cause, causeHostPressure)
		}
	}
	in.Hold.Trickle = false
	if res := admit(in, intents()); res.Forced || len(res.Admitted) != 1 {
		t.Fatalf("no trickle: admitted %v forced %t, want only the stop", keysOf(res.Admitted), res.Forced)
	}
}

// AC-5 (v2 pass): under sustained pressure the pass holds the creates, after
// 5 patrol intervals forces exactly one through and restarts the window, and
// records held, forced and released, nothing on a plain held pass.
func TestPassTricklesOneCreateUnderSustainedHostPressure(t *testing.T) {
	f := newGatherFixture(t, routedDemandBead("gc-r1"), routedDemandBead("gc-r2"))
	rec := events.NewFake()
	f.p.rec = rec
	pressure := highHostPressure
	f.env.HostPressure = func() hostPressure { return pressure }
	patrol := f.p.patrol()
	creates := func(at time.Duration) int {
		t.Helper()
		f.p.tracePass(f.env, gatherNow.Add(at))
		r := f.p.out.record.Load()
		if r == nil || r.Err != "" {
			t.Fatalf("pass at +%s: record %+v", at, r)
		}
		n := 0
		for _, it := range r.Admitted {
			if it.Kind == intentCreate {
				n++
			}
		}
		return n
	}
	if n := creates(0); n != 0 {
		t.Fatalf("episode start admitted %d create(s), want 0", n)
	}
	if n := creates(4 * patrol); n != 0 {
		t.Fatalf("held pass admitted %d create(s), want 0", n)
	}
	if n := creates(5 * patrol); n != 1 {
		t.Fatalf("pass after 5 intervals admitted %d create(s), want exactly 1", n)
	}
	if n := creates(5*patrol + time.Second); n != 0 {
		t.Fatalf("pass after the forced create admitted %d create(s), want 0: the window restarts", n)
	}
	pressure = hostPressure{Avg10: 10, Threshold: defaultHostPressureThreshold}
	if n := creates(6 * patrol); n == 0 {
		t.Fatal("pass after pressure released admitted no create")
	}
	want := []string{hostPressureOutcomeHeld, hostPressureOutcomeForced, hostPressureOutcomeReleased}
	if got := heldStartsOutcomes(t, rec); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("held_starts outcomes %v, want %v", got, want)
	}
}

// AC-5 (legacy): a trickling hold lets exactly one candidate through and
// reports it forced.
func TestLegacyTrickleLetsOneCandidateThrough(t *testing.T) {
	e := newPrepareBudgetEnv(t, "a", "b")
	a := e.candidate(t, "a", time.Second)
	b := e.candidate(t, "b", time.Second)
	var forced int
	opts := []startExecutionOption{withStartHold(startHold{Cause: causeHostPressure, Trickle: true}), withStartHoldForced(func() { forced++ })}
	if woken := e.runWith(opts, false, a, b); woken != 1 || forced != 1 {
		t.Fatalf("woken %d forced %d, want exactly one candidate forced through\n%s", woken, forced, e.log.String())
	}
}

// AC-5 (legacy): the trickle is spent only by a candidate that clears every
// gate behind the hold. A candidate a later gate defers (here the endpoint
// capacity breaker) leaves the trickle to the next candidate, and the window
// restarts (forced) only when a start is actually prepared.
func TestLegacyTrickleSkipsACandidateALaterGateDefers(t *testing.T) {
	trickle := withStartHold(startHold{Cause: causeHostPressure, Trickle: true})
	t.Run("the next candidate is forced", func(t *testing.T) {
		e := newCapacityEnv(t, true, "a", "b")
		a := e.pendingCreate(t, "a", "e", nil)
		b := e.pendingCreate(t, "b", "f", nil)
		e.openEndpoint(t)
		var forced int
		e.startOptions = append(e.startOptions, trickle, withStartHoldForced(func() { forced++ }))
		if woken := e.start(context.Background(), a, b); woken != 1 || forced != 1 {
			t.Fatalf("woken %d forced %d, want b forced through once a is deferred by the endpoint\n%s", woken, forced, e.log.String())
		}
		if e.sp.CountCalls("Start", "a") != 0 || e.sp.CountCalls("Start", "b") != 1 {
			t.Fatalf("Start calls = %+v, want only b", e.sp.SnapshotCalls())
		}
		if !strings.Contains(e.log.String(), "outcome=deferred_by_endpoint_capacity") {
			t.Fatalf("stderr = %q, want a deferred_by_endpoint_capacity", e.log.String())
		}
	})
	t.Run("nothing prepared leaves the window", func(t *testing.T) {
		e := newCapacityEnv(t, true, "a")
		a := e.pendingCreate(t, "a", "e", nil)
		e.openEndpoint(t)
		var forced int
		e.startOptions = append(e.startOptions, trickle, withStartHoldForced(func() { forced++ }))
		if woken := e.start(context.Background(), a); woken != 0 || forced != 0 {
			t.Fatalf("woken %d forced %d, want nothing forced while the only candidate is deferred\n%s", woken, forced, e.log.String())
		}
	})
}

// AC-5 (legacy tick): the runtime's tick hold tracks the episode: held at
// the start with one stderr line, a trickle after 5 patrol intervals whose
// forced start restarts the window, and released when pressure drops.
func TestCityRuntimeTickStartHoldTracksTheEpisode(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, false)
	var stderr strings.Builder
	cr.stderr = &stderr
	rec := events.NewFake()
	cr.rec = rec
	patrol := cr.cfg.Daemon.PatrolIntervalDuration()
	t0 := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	fakeCPUPressure(t, cpuPSI("80.00"), nil)
	hold := func(at time.Duration) (startHold, func()) {
		t.Helper()
		return cr.tickStartHold(t0.Add(at))
	}
	if h, _ := hold(0); h != (startHold{Cause: causeHostPressure}) {
		t.Fatalf("episode start: hold %+v", h)
	}
	if h, _ := hold(4 * patrol); h.Trickle {
		t.Fatalf("hold %+v trickles before 5 intervals", h)
	}
	h, forced := hold(5 * patrol)
	if !h.Trickle {
		t.Fatalf("hold %+v after 5 intervals, want a trickle", h)
	}
	forced()
	if h, _ := hold(5*patrol + time.Second); h.Trickle {
		t.Fatalf("hold %+v right after a forced start, want the window restarted", h)
	}
	fakeCPUPressure(t, cpuPSI("10.00"), nil)
	if h, _ := hold(6 * patrol); h.held() {
		t.Fatalf("hold %+v after pressure released", h)
	}
	want := []string{hostPressureOutcomeHeld, hostPressureOutcomeForced, hostPressureOutcomeReleased}
	if got := heldStartsOutcomes(t, rec); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("held_starts outcomes %v, want %v", got, want)
	}
	if n := strings.Count(stderr.String(), "CPU pressure"); n != 1 {
		t.Fatalf("stderr names CPU pressure %d time(s), want once per episode:\n%s", n, stderr.String())
	}
}
