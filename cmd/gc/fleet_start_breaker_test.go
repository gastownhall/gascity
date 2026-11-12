package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/resilience"
	"github.com/gastownhall/gascity/internal/runtime"
)

// fleetTestStartupTimeout is the startup_timeout the fleet guard tests
// configure; the half-open probe window is this plus a 60 s grace.
const fleetTestStartupTimeout = 120 * time.Second

// newFleetTestGuard returns a guard on a fake clock with jitter pinned to half
// the backoff cap, so every wait is exactly cap/2.
func newFleetTestGuard(t *testing.T) (*fleetStartGuard, *clock.Fake) {
	t.Helper()
	clk := &clock.Fake{Time: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	g := newFleetStartGuard(clk.Now, fleetTestStartupTimeout)
	g.registry.SetJitterForTest(func(capDur time.Duration) time.Duration { return capDur / 2 })
	return g, clk
}

func fleetState(g *fleetStartGuard) resilience.State { return g.Status().State }

// failFleetStart admits one start of session in template and fails it.
func failFleetStart(t *testing.T, g *fleetStartGuard, session, template string) {
	t.Helper()
	ticket, ok := g.Admit(session, template)
	if !ok {
		t.Fatalf("Admit(%s) denied, want admitted", session)
	}
	ticket.Resolve(fleetStartFailed)
}

// tripFleet fails three sessions across two templates inside the window.
func tripFleet(t *testing.T, g *fleetStartGuard, clk *clock.Fake) {
	t.Helper()
	failFleetStart(t, g, "s1", "a")
	clk.Advance(time.Second)
	failFleetStart(t, g, "s2", "a")
	clk.Advance(time.Second)
	failFleetStart(t, g, "s3", "b")
	if got := fleetState(g); got != resilience.StateOpen {
		t.Fatalf("state after 3 sessions across 2 templates = %v, want open", got)
	}
}

// admitFleetProbe advances to the probe and returns its ticket.
func admitFleetProbe(t *testing.T, g *fleetStartGuard, clk *clock.Fake, session string) *fleetTicket {
	t.Helper()
	clk.Advance(g.Status().BackoffCap)
	if !g.Eligible() {
		t.Fatal("Eligible() = false once the probe is due, want true")
	}
	ticket, ok := g.Admit(session, "a")
	if !ok || !ticket.Probe() {
		t.Fatalf("Admit(%s) = (%v, probe %v), want the due probe", session, ok, ticket != nil && ticket.Probe())
	}
	return ticket
}

func TestFleetGuard_TripThreshold(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures [][2]string // session, template
		gap      time.Duration
		want     resilience.State
	}{
		{"three sessions one template", [][2]string{{"s1", "a"}, {"s2", "a"}, {"s3", "a"}}, time.Second, resilience.StateClosed},
		{"two sessions two templates", [][2]string{{"s1", "a"}, {"s2", "b"}}, time.Second, resilience.StateClosed},
		{"one session three times", [][2]string{{"s1", "a"}, {"s1", "a"}, {"s1", "b"}}, time.Second, resilience.StateClosed},
		{"three sessions two templates", [][2]string{{"s1", "a"}, {"s2", "a"}, {"s3", "b"}}, time.Second, resilience.StateOpen},
		{"spread past the window", [][2]string{{"s1", "a"}, {"s2", "a"}, {"s3", "b"}}, 70 * time.Second, resilience.StateClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, clk := newFleetTestGuard(t)
			for _, f := range tc.failures {
				failFleetStart(t, g, f[0], f[1])
				clk.Advance(tc.gap)
			}
			if got := fleetState(g); got != tc.want {
				t.Fatalf("state = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFleetGuard_ProbeScheduleIsBounded is the verification plan's headline:
// nine sessions over three templates whose every start fails cost at most
// three attempts to trip and at most seven probes in ten simulated minutes.
func TestFleetGuard_ProbeScheduleIsBounded(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	templates := []string{"a", "b", "c"}
	attempts, toTrip := 0, 0
	for sec := 0; sec < 600; sec++ {
		for i := 0; i < 9; i++ {
			if !g.Eligible() {
				break
			}
			ticket, ok := g.Admit(fmt.Sprintf("s%d", i), templates[i%3])
			if !ok {
				break
			}
			attempts++
			if fleetState(g) == resilience.StateClosed {
				toTrip = attempts
			}
			ticket.Resolve(fleetStartFailed)
		}
		clk.Advance(time.Second)
	}
	if toTrip > 3 || fleetState(g) == resilience.StateClosed {
		t.Fatalf("attempts before the trip = %d, state %v, want it open after at most 3", toTrip, fleetState(g))
	}
	if attempts > 10 {
		t.Fatalf("attempts over 10 simulated minutes = %d, want at most 10 (3 + 7 probes)", attempts)
	}
	if attempts < 4 {
		t.Fatalf("attempts = %d, want probes to keep happening", attempts)
	}
}

// TestFleetGuard_NilGuardIsTheKillSwitch pins today's behavior: with no
// guard every one of 45 attempts (nine sessions, five cycles) goes through.
func TestFleetGuard_NilGuardIsTheKillSwitch(t *testing.T) {
	var g *fleetStartGuard
	attempts := 0
	for cycle := 0; cycle < 5; cycle++ {
		for i := 0; i < 9; i++ {
			if !g.Eligible() {
				t.Fatal("nil guard not eligible")
			}
			ticket, ok := g.Admit(fmt.Sprintf("s%d", i), "a")
			if !ok {
				t.Fatal("nil guard denied a start")
			}
			ticket.Resolve(fleetStartFailed)
			attempts++
		}
	}
	if attempts != 45 {
		t.Fatalf("attempts = %d, want 45", attempts)
	}
	g.ObserveLiveness("s1", false)
	g.NoteStateCache(time.Hour, true, 5)
	if g.Reset() {
		t.Fatal("nil guard Reset() = true")
	}
}

func TestFleetGuard_ProbeClosesOnlyAfterAliveConfirmation(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	tripFleet(t, g, clk)
	probe := admitFleetProbe(t, g, clk, "p1")
	probe.Resolve(fleetStartOK)
	if got := fleetState(g); got != resilience.StateHalfOpen {
		t.Fatalf("state right after the probe's Start returned = %v, want half-open: Start OK is not recovery", got)
	}
	clk.Advance(10 * time.Second)
	g.ObserveLiveness("p1", true)
	if got := fleetState(g); got != resilience.StateHalfOpen {
		t.Fatalf("state after 10s alive = %v, want half-open", got)
	}
	clk.Advance(stabilityThreshold)
	g.ObserveLiveness("p1", true)
	if got := fleetState(g); got != resilience.StateClosed {
		t.Fatalf("state after the session stayed alive %s = %v, want closed", stabilityThreshold, got)
	}
}

func TestFleetGuard_ProbeDeathInsideStabilityReopens(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	tripFleet(t, g, clk)
	trips := g.Status().Trips
	probe := admitFleetProbe(t, g, clk, "p1")
	probe.Resolve(fleetStartOK)
	clk.Advance(5 * time.Second)
	g.ObserveLiveness("p1", false)
	st := g.Status()
	if st.State != resilience.StateOpen || st.Trips <= trips {
		t.Fatalf("after the probe's session died inside %s: state %v trips %d (was %d), want re-opened", stabilityThreshold, st.State, st.Trips, trips)
	}
}

func TestFleetGuard_StragglerSuccessWhileOpenDoesNotClose(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	straggler, ok := g.Admit("late", "a")
	if !ok {
		t.Fatal("Admit denied while closed")
	}
	tripFleet(t, g, clk)
	straggler.Resolve(fleetStartOK)
	clk.Advance(2 * stabilityThreshold)
	g.ObserveLiveness("late", true)
	if got := fleetState(g); got != resilience.StateOpen {
		t.Fatalf("state after a straggler's confirmed success = %v, want it to stay open", got)
	}
}

func TestFleetGuard_InconclusiveProbeIsReleased(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	tripFleet(t, g, clk)
	probe := admitFleetProbe(t, g, clk, "p1")
	probe.Resolve(fleetInconclusive)
	next, ok := g.Admit("p2", "b")
	if !ok || !next.Probe() {
		t.Fatalf("Admit after an inconclusive probe = (%v, probe %v), want a fresh probe at once", ok, next != nil && next.Probe())
	}
}

func TestFleetGuard_HalfOpenIntervalIsStartupTimeoutPlusGrace(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	tripFleet(t, g, clk)
	probe := admitFleetProbe(t, g, clk, "p1")
	probe.Resolve(fleetStartOK) // started, unconfirmed: the probe is still out
	window := fleetTestStartupTimeout + 60*time.Second
	clk.Advance(window - time.Second)
	if _, ok := g.Admit("p2", "b"); ok {
		t.Fatalf("second probe admitted %s after the first, before startup_timeout+60s = %s", window-time.Second, window)
	}
	clk.Advance(2 * time.Second)
	if next, ok := g.Admit("p2", "b"); !ok || !next.Probe() {
		t.Fatalf("no second probe after startup_timeout+60s = %s", window)
	}
}

func TestFleetGuard_StateCacheTrigger(t *testing.T) {
	for _, tc := range []struct {
		name     string
		age      time.Duration
		known    bool
		expected int
		want     resilience.State
	}{
		{"stale with sessions expected", 61 * time.Second, true, 1, resilience.StateOpen},
		{"stale with none expected", 61 * time.Second, true, 0, resilience.StateClosed},
		{"fresh with sessions expected", 59 * time.Second, true, 5, resilience.StateClosed},
		{"no cache known", time.Hour, false, 5, resilience.StateClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newFleetTestGuard(t)
			g.NoteStateCache(tc.age, tc.known, tc.expected)
			if got := fleetState(g); got != tc.want {
				t.Fatalf("state = %v, want %v", got, tc.want)
			}
			if tc.want == resilience.StateOpen && g.Status().Reason != "state-cache" {
				t.Fatalf("reason = %q, want state-cache", g.Status().Reason)
			}
		})
	}
}

func TestFleetGuard_ResetClosesAndForgetsTheWindow(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	if g.Reset() {
		t.Fatal("Reset() on a closed breaker = true, want false")
	}
	tripFleet(t, g, clk)
	if !g.Reset() {
		t.Fatal("Reset() on an open breaker = false, want true")
	}
	if got := fleetState(g); got != resilience.StateClosed {
		t.Fatalf("state after Reset = %v, want closed", got)
	}
	failFleetStart(t, g, "s4", "a")
	failFleetStart(t, g, "s5", "b")
	if got := fleetState(g); got != resilience.StateClosed {
		t.Fatalf("state = %v: failures from before the reset still counted", got)
	}
}

func TestFleetGuard_OneEventAndOneStderrLinePerEpisode(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	rec := events.NewFake()
	var stderr strings.Builder
	tripFleet(t, g, clk)
	// Two failed probes re-open the same episode.
	for i := 0; i < 2; i++ {
		admitFleetProbe(t, g, clk, fmt.Sprintf("p%d", i)).Resolve(fleetStartFailed)
	}
	g.Flush(rec, &stderr)
	for _, e := range rec.Events {
		if e.Type != breakerStateChangedEventType {
			t.Fatalf("event type %q, want %q", e.Type, breakerStateChangedEventType)
		}
	}
	if len(rec.Events) == 0 {
		t.Fatal("no breaker.state_changed event recorded")
	}
	first := string(rec.Events[0].Payload)
	if !strings.Contains(first, `"scope":"fleet"`) || !strings.Contains(first, `"op_class"`) {
		t.Fatalf("first payload = %s, want scope fleet with op_class set", first)
	}
	if n := strings.Count(stderr.String(), "fleet start breaker opened"); n != 1 {
		t.Fatalf("stderr opened-lines = %d, want exactly 1 for the episode\n%s", n, stderr.String())
	}
	g.Reset()
	g.Flush(rec, &stderr)
	if n := strings.Count(stderr.String(), "fleet start breaker closed"); n != 1 {
		t.Fatalf("stderr closed-lines = %d, want 1\n%s", n, stderr.String())
	}
}

func TestClassifyFleetOutcome(t *testing.T) {
	capacity := &runtime.CapacityError{ExitCode: runtime.ExitCodeTempFail, Source: runtime.CapacitySourceExitStatus, Err: errors.New("busy")}
	for _, tc := range []struct {
		name string
		r    startResult
		want fleetOutcome
	}{
		{"success", startResult{providerStartCalled: true, outcome: TraceOutcomeSuccess}, fleetStartOK},
		{"deadline exceeded", startResult{providerStartCalled: true, err: context.DeadlineExceeded, outcome: TraceOutcomeDeadlineExceeded}, fleetStartFailed},
		{"plain start error", startResult{providerStartCalled: true, err: errors.New("boom"), outcome: TraceOutcomeFailed}, fleetStartFailed},
		{"died during startup", startResult{providerStartCalled: true, err: fmt.Errorf("x: %w", runtime.ErrSessionDiedDuringStartup), diedDuringStartup: true, outcome: TraceOutcomeFailed}, fleetStartFailed},
		{"provider capacity", startResult{providerStartCalled: true, err: capacity, outcome: TraceOutcomeFailed}, fleetInconclusive},
		{"unresolved session exists", startResult{providerStartCalled: true, err: runtime.ErrSessionExists, outcome: TraceOutcomeSessionExists}, fleetInconclusive},
		{"deferred outcome", startResult{providerStartCalled: true, outcome: TraceOutcomeDeferred}, fleetInconclusive},
		{"runtime unavailable", startResult{providerStartCalled: true, err: runtime.ErrRuntimeUnavailable, outcome: TraceOutcomeDeferred}, fleetInconclusive},
		{"never reached the provider", startResult{err: errors.New("prepare"), outcome: TraceOutcomeFailed}, fleetInconclusive},
		{"canceled", startResult{providerStartCalled: true, err: context.Canceled, outcome: TraceOutcomeCanceled}, fleetInconclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFleetOutcome(tc.r); got != tc.want {
				t.Fatalf("classifyFleetOutcome = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStartConfirmTracker(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	tr := newStartConfirmTracker(clk.Now, 30*time.Second)
	if got := tr.Observe("x", true); got != confirmUntracked {
		t.Fatalf("untracked session = %v, want untracked", got)
	}
	tr.Begin("a")
	tr.Begin("b")
	tr.Begin("c")
	clk.Advance(10 * time.Second)
	if got := tr.Observe("a", true); got != confirmPending {
		t.Fatalf("alive at 10s = %v, want pending", got)
	}
	if got := tr.Observe("b", false); got != confirmDied {
		t.Fatalf("dead at 10s = %v, want died", got)
	}
	if got := tr.Observe("b", true); got != confirmUntracked {
		t.Fatalf("died session observed again = %v, want it forgotten", got)
	}
	clk.Advance(20 * time.Second)
	if got := tr.Observe("a", true); got != confirmAlive {
		t.Fatalf("alive at 30s = %v, want alive", got)
	}
	if got := tr.Observe("a", true); got != confirmUntracked {
		t.Fatalf("confirmed session observed again = %v, want it forgotten", got)
	}
	tr.Forget("c")
	if got := tr.Observe("c", false); got != confirmUntracked {
		t.Fatalf("forgotten session = %v, want untracked", got)
	}
}

// fleetWiringEnv is a reconciler env with nine pending-create sessions, each
// its own template, whose every start fails, and one wake per batch so each
// result is resolved before the next admission.
func fleetWiringEnv(t *testing.T, guard *fleetStartGuard, startErr error) (*capacityEnv, []startCandidate) {
	t.Helper()
	names := make([]string, 9)
	for i := range names {
		names[i] = fmt.Sprintf("w%d", i)
	}
	e := newCapacityEnv(t, false, names...)
	e.limitToOneWake()
	if guard != nil {
		e.startOptions = append(e.startOptions, withFleetStartGuard(guard))
	}
	cands := make([]startCandidate, len(names))
	for i, n := range names {
		cands[i] = e.pendingCreate(t, n, "", nil)
		e.sp.StartErrors[n] = startErr
	}
	return e, cands
}

func TestExecutePlannedStarts_FleetGuardStopsFailingFleetAfterThreeAttempts(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	g := newFleetStartGuard(clk.Now, fleetTestStartupTimeout)
	e, cands := fleetWiringEnv(t, g, errors.New("boom"))
	e.start(context.Background(), cands...)

	started := 0
	for _, c := range cands {
		if e.sp.CountCalls("Start", c.name()) > 0 {
			started++
			continue
		}
		got := e.bead(t, c.info.ID)
		if got.Metadata["wake_attempts"] != "" || got.Metadata["last_woke_at"] != "" {
			t.Fatalf("denied %s: wake_attempts=%q last_woke_at=%q, want no wake budget spent", c.name(), got.Metadata["wake_attempts"], got.Metadata["last_woke_at"])
		}
	}
	if started != 3 {
		t.Fatalf("provider Start attempts = %d, want 3 before the fleet breaker opens", started)
	}
	if got := fleetState(g); got != resilience.StateOpen {
		t.Fatalf("fleet breaker = %v, want open", got)
	}
}

func TestExecutePlannedStarts_WithoutFleetGuardEveryStartIsAttempted(t *testing.T) {
	e, cands := fleetWiringEnv(t, nil, errors.New("boom"))
	e.start(context.Background(), cands...)
	for _, c := range cands {
		if n := e.sp.CountCalls("Start", c.name()); n != 1 {
			t.Fatalf("%s Start calls = %d, want 1 with the kill switch off", c.name(), n)
		}
	}
}

func TestExecutePlannedStarts_CapacityRefusalsNeverTripTheFleetBreaker(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	g := newFleetStartGuard(clk.Now, fleetTestStartupTimeout)
	e, cands := fleetWiringEnv(t, g, capacityRefusal("w", "no capacity"))
	e.start(context.Background(), cands...)
	if got := fleetState(g); got != resilience.StateClosed {
		t.Fatalf("fleet breaker = %v after provider-capacity refusals, want closed", got)
	}
	for _, c := range cands {
		if n := e.sp.CountCalls("Start", c.name()); n != 1 {
			t.Fatalf("%s Start calls = %d, want 1: capacity errors are not fleet failures", c.name(), n)
		}
	}
}

func fleetTracked(g *fleetStartGuard) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.starts)
}

// A session the reconciler stopped on purpose is forgotten, so its death
// inside the stability window is not a failed start.
func TestFleetGuard_ForgetSessionExcusesADeliberateStop(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	failFleetStart(t, g, "s1", "a")
	clk.Advance(time.Second)
	failFleetStart(t, g, "s2", "b")
	for _, id := range []string{"s3", "s4", "s5"} {
		ticket, ok := g.Admit(id, "c")
		if !ok {
			t.Fatalf("Admit(%s) denied", id)
		}
		ticket.Resolve(fleetStartOK)
		g.ObserveLiveness(id, true)
		g.ForgetSession(id)
		clk.Advance(time.Second)
		g.ObserveLiveness(id, false)
	}
	if got := fleetState(g); got != resilience.StateClosed {
		t.Fatalf("state after three forgotten sessions ended = %v, want closed", got)
	}
	if n := fleetTracked(g); n != 0 {
		t.Fatalf("starts still tracked after ForgetSession = %d, want 0", n)
	}
}

// Without ForgetSession a death inside the window is a failure: the same three
// sessions, not forgotten, trip the breaker.
func TestFleetGuard_UnforgottenDeathInsideTheWindowTrips(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	failFleetStart(t, g, "s1", "a")
	clk.Advance(time.Second)
	ticket, ok := g.Admit("s2", "b")
	if !ok {
		t.Fatal("Admit(s2) denied")
	}
	ticket.Resolve(fleetStartOK)
	g.ObserveLiveness("s2", true)
	clk.Advance(time.Second)
	g.ObserveLiveness("s2", false)
	ticket, _ = g.Admit("s3", "c")
	ticket.Resolve(fleetStartOK)
	g.ObserveLiveness("s3", true)
	clk.Advance(time.Second)
	g.ObserveLiveness("s3", false)
	if got := fleetState(g); got != resilience.StateOpen {
		t.Fatalf("state after two unprompted deaths and a failed start = %v, want open", got)
	}
}

// A confirmation that resolves leaves nothing behind.
func TestFleetGuard_ConfirmedStartLeavesNoEntry(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	ticket, _ := g.Admit("s1", "a")
	ticket.Resolve(fleetStartOK)
	g.ObserveLiveness("s1", true)
	clk.Advance(stabilityThreshold + time.Second)
	g.ObserveLiveness("s1", true)
	if n := fleetTracked(g); n != 0 {
		t.Fatalf("starts after a confirmed start = %d, want 0", n)
	}
}

// A session that is never observed again (closed, removed, renamed) must not
// leave its start in the guard for the life of the controller.
func TestFleetGuard_AbandonedStartsArePruned(t *testing.T) {
	g, clk := newFleetTestGuard(t)
	for _, id := range []string{"gone-1", "gone-2"} {
		ticket, _ := g.Admit(id, "a")
		ticket.Resolve(fleetStartOK)
	}
	if n := fleetTracked(g); n != 2 {
		t.Fatalf("starts after two admitted starts = %d, want 2", n)
	}
	clk.Advance(time.Duration(fleetConfirmForgetFactor+1) * stabilityThreshold)
	ticket, _ := g.Admit("fresh", "a")
	ticket.Resolve(fleetStartOK)
	if n := fleetTracked(g); n != 1 {
		t.Fatalf("starts after the abandoned ones aged out = %d, want only the fresh one", n)
	}
}
