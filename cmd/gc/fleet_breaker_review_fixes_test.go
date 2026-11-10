package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/resilience"
)

// pinFleetJitter makes every backoff wait exactly half its cap, so a test
// knows whether a probe is due instead of depending on the random draw.
func pinFleetJitter(g *fleetStartGuard) {
	g.registry.SetJitterForTest(func(capDur time.Duration) time.Duration { return capDur / 2 })
}

// tripFleetOnClock fails three sessions across three templates a second apart.
func tripFleetOnClock(t *testing.T, g *fleetStartGuard, clk *clock.Fake) {
	t.Helper()
	failFleetStart(t, g, "x1", "x")
	clk.Advance(time.Second)
	failFleetStart(t, g, "y1", "y")
	clk.Advance(time.Second)
	failFleetStart(t, g, "z1", "z")
	if got := fleetState(g); got != resilience.StateOpen {
		t.Fatalf("premise: breaker = %v, want open", got)
	}
}

// reopenFleetBreaker takes the probe that is due and fails it, which opens the
// breaker again with a fresh backoff: a long-running outage seen at a later
// tick, with no probe due.
func reopenFleetBreaker(t *testing.T, g *fleetStartGuard) {
	t.Helper()
	failFleetStart(t, g, "probe", "probe")
	if got := fleetState(g); got != resilience.StateOpen || g.Eligible() {
		t.Fatalf("premise: breaker = %v, eligible = %v, want open with no probe due", got, g.Eligible())
	}
}

// fleetDeferralHarness is the pending-create deferral harness with a fleet
// guard on its clock and no per-tick wake budget, so the only gate between a
// create and its start is the fleet breaker.
func fleetDeferralHarness(t *testing.T, seed int64, n int) (*deferralHarness, *fleetStartGuard) {
	t.Helper()
	d := newDeferralHarness(t, seed, n, true)
	d.env.cfg.Daemon.MaxWakesPerTick = nil
	g := newFleetStartGuard(d.env.clk.Now, fleetTestStartupTimeout)
	pinFleetJitter(g)
	d.env.startOptions = append(d.env.startOptions, withFleetStartGuard(g))
	return d, g
}

// requireClaimsKept fails unless every create still holds its pending-create
// claim: the hold must keep the row as it was, not close it and mint another.
func (d *deferralHarness) requireClaimsKept(t *testing.T, when string) {
	t.Helper()
	d.requireNoneClosed(t, when)
	for _, id := range d.ids {
		if got := d.bead(t, id); got.Metadata["pending_create_claim"] != "true" {
			t.Fatalf("%s: create %s lost its claim: pending_create_claim=%q state=%q", when, id, got.Metadata["pending_create_claim"], got.Metadata["state"])
		}
	}
}

// A never-started create the open fleet breaker refused at the reconciler's
// gate has done nothing wrong and was never offered a start. The gate's
// deferral must reach the start-deferral memo, or the lease rolls the create
// back at ten minutes and mints a replacement the breaker refuses in turn.
// Every candidate is refused at the gate on both ticks, so no candidate ever
// reaches the executor to record anything.
func TestFleetBreakerGateDeferredPendingCreateSurvivesTheLeaseAndIsReleasedAtTheCeiling(t *testing.T) {
	d, g := fleetDeferralHarness(t, 20261110, 3)
	tripFleetOnClock(t, g, d.env.clk)

	if got := d.tick(t); got != 0 {
		t.Fatalf("pass 1 started %d with the breaker open, want 0", got)
	}
	if got := d.notRunning(); len(got) != 3 {
		t.Fatalf("premise: %d creates waiting after pass 1, want 3", len(got))
	}
	if g.Eligible() {
		t.Fatal("premise: a probe is due, so the executor, not the reconciler's gate, would turn the candidates away")
	}

	// Past the ten-minute lease, the breaker still open.
	d.env.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
	reopenFleetBreaker(t, g)
	if got := d.tick(t); got != 0 {
		t.Fatalf("pass 2 started %d with the breaker open, want 0", got)
	}
	d.requireClaimsKept(t, "pass 2, past the lease")
	if !strings.Contains(d.env.stderr.String(), "keeping pending create") {
		t.Fatalf("no \"keeping pending create\" log for the held creates; stderr:\n%s", d.env.stderr.String())
	}

	// Past the hold ceiling the hold is released, even with the breaker open.
	d.env.clk.Advance(startDeferralHoldCeiling)
	reopenFleetBreaker(t, g)
	d.tick(t)
	for _, id := range d.ids {
		if got := d.bead(t, id); got.Status != "closed" {
			t.Fatalf("create %s deferred by the fleet breaker for more than %s has status %q, want closed: the hold is bounded", id, startDeferralHoldCeiling, got.Status)
		}
	}
}

// The executor's Admit denial is the other site: the probe is due, so the
// reconciler's gate lets every candidate through, and the guard admits one and
// turns the rest away. Those refused at Admit are recorded for the next pass,
// which rolls back before the executor runs and reads that record.
func TestFleetBreakerAdmitDeniedPendingCreateSurvivesTheLease(t *testing.T) {
	d, g := fleetDeferralHarness(t, 20261111, 3)
	tripFleetOnClock(t, g, d.env.clk)
	d.env.clk.Advance(pendingCreateNeverStartedTimeout + time.Minute)
	if !g.Eligible() {
		t.Fatal("premise: no probe is due after ten minutes, so the reconciler's gate would stop every candidate")
	}

	if got := d.tick(t); got != 1 {
		t.Fatalf("pass 1 started %d, want exactly the probe", got)
	}
	if got := d.notRunning(); len(got) != 2 {
		t.Fatalf("premise: %d creates waiting after pass 1, want the 2 the guard refused", len(got))
	}
	if g.Eligible() {
		t.Fatal("premise: a probe is still due after pass 1, so pass 2's gate would let candidates through")
	}

	// Short of the probe's stability window, so the breaker is still not closed.
	d.env.clk.Advance(time.Second)
	d.tick(t)
	d.requireNoneClosed(t, "pass 2, past the lease")
	if got := d.notRunning(); len(got) != 2 {
		t.Fatalf("%d creates waiting after pass 2, want the same 2 kept", len(got))
	}
}

// operatorStart is one way an operator asks for a session to start, as the
// reconciler sees it in the session's own metadata.
type operatorStart struct {
	name string
	meta func(now time.Time) map[string]string
}

// operatorStarts are the start shapes an operator asks for: an explicit wake
// (gc session wake, a nudge that wakes, the wake API) and a session the
// operator made by hand (gc session new, materialize).
var operatorStarts = []operatorStart{
	{"explicit wake", func(now time.Time) map[string]string {
		return map[string]string{"wake_request": "explicit", "wake_requested_at": now.UTC().Format(time.RFC3339)}
	}},
	{"user-made create", func(time.Time) map[string]string {
		return map[string]string{"session_origin": "manual"}
	}},
}

// controllerCreate is a create the controller made on its own account.
func controllerCreate() map[string]string { return map[string]string{"session_origin": "ephemeral"} }

// openedGuardFor returns a guard on e's clock, tripped open with the clock
// inside the backoff, so no probe is due.
func openedGuardFor(t *testing.T, e *capacityEnv) *fleetStartGuard {
	t.Helper()
	g := newFleetStartGuard(e.clk.Now, fleetTestStartupTimeout)
	pinFleetJitter(g)
	e.startOptions = append(e.startOptions, withFleetStartGuard(g))
	tripFleetOnClock(t, g, e.clk)
	return g
}

// An operator's own request to start a session is not an automatic start: the
// executor admits it while the breaker is open, and a controller-made create
// beside it is still refused.
func TestExecutePlannedStarts_FleetGuardAdmitsOperatorRequestedStartsWhileOpen(t *testing.T) {
	for _, tc := range operatorStarts {
		t.Run(tc.name, func(t *testing.T) {
			e := newCapacityEnv(t, false, "op", "auto")
			openedGuardFor(t, e)
			op := e.pendingCreate(t, "op", "", tc.meta(e.clk.Now()))
			auto := e.pendingCreate(t, "auto", "", controllerCreate())

			e.start(context.Background(), op, auto)

			if n := e.sp.CountCalls("Start", "op"); n != 1 {
				t.Errorf("operator-requested start: provider Start calls = %d, want 1 while the breaker is open\n%s", n, e.log.String())
			}
			if n := e.sp.CountCalls("Start", "auto"); n != 0 {
				t.Errorf("controller-made create: provider Start calls = %d, want 0 while the breaker is open", n)
			}
		})
	}
}

// A failed operator-requested start is not evidence the fleet cannot start
// sessions: it does not count toward tripping. The control is the same
// failure on a controller-made create, which is the third and trips it.
func TestExecutePlannedStarts_FleetGuardDoesNotCountOperatorRequestedStartFailures(t *testing.T) {
	primed := func(t *testing.T) (*capacityEnv, *fleetStartGuard) {
		t.Helper()
		e := newCapacityEnv(t, false, "w")
		g := newFleetStartGuard(e.clk.Now, fleetTestStartupTimeout)
		pinFleetJitter(g)
		e.startOptions = append(e.startOptions, withFleetStartGuard(g))
		failFleetStart(t, g, "x1", "x")
		e.clk.Advance(time.Second)
		failFleetStart(t, g, "y1", "y")
		e.sp.StartErrors["w"] = errors.New("boom")
		return e, g
	}

	control, cg := primed(t)
	control.start(context.Background(), control.pendingCreate(t, "w", "", controllerCreate()))
	if got := fleetState(cg); got != resilience.StateOpen {
		t.Fatalf("control: a third failed controller-made start left the breaker %v, want open", got)
	}

	for _, tc := range operatorStarts {
		t.Run(tc.name, func(t *testing.T) {
			e, g := primed(t)
			e.start(context.Background(), e.pendingCreate(t, "w", "", tc.meta(e.clk.Now())))
			if n := e.sp.CountCalls("Start", "w"); n != 1 {
				t.Fatalf("premise: provider Start calls = %d, want 1\n%s", n, e.log.String())
			}
			if got := fleetState(g); got != resilience.StateClosed {
				t.Fatalf("breaker after a failed operator-requested start = %v, want closed: it is not an automatic start", got)
			}
		})
	}
}

// The reconciler's gate lets an operator's own request through while the
// breaker is open and no probe is due: the wake reaches the provider.
func TestReconcile_FleetGateLetsOperatorRequestedStartsThrough(t *testing.T) {
	for _, tc := range operatorStarts {
		t.Run(tc.name, func(t *testing.T) {
			env := newFleetReconcileEnv(t)
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", false)
			session := env.createSessionBead("worker", "worker")
			env.markSessionCreating(&session)
			meta := tc.meta(env.clk.Now())
			if meta["session_origin"] == "manual" {
				// A user-made create is a never-started pending create.
				meta["pending_create_claim"] = "true"
			}
			env.setSessionMetadata(&session, meta)
			pinFleetJitter(env.guard)
			env.openFleetBreaker(t)

			env.reconcile([]beads.Bead{session})

			if n := env.startCallsFor("worker"); n == 0 {
				t.Fatalf("provider Start calls = 0: the reconciler's gate held back an operator-requested start (stderr: %s)", env.stderr.String())
			}
		})
	}
}

// The text that tells an operator what the breaker gates must say what the
// code does: an operator's own request to start a session is exempt, not
// "attach, new, nudge", which is false for the commands that go through the
// controller's reconciler.
func TestFleetBreakerPublishedTextDescribesTheOperatorRequestedExemption(t *testing.T) {
	const falseClaim = "User initiated starts (attach, new, nudge) are never gated"
	const exemption = "Operator-requested starts"
	long := strings.Join(strings.Fields(newBreakerCmd(&strings.Builder{}, &strings.Builder{}).Long), " ")
	if strings.Contains(long, falseClaim) {
		t.Errorf("gc breaker --help still says user-initiated starts (attach, new, nudge) are never gated:\n%s", long)
	}
	if !strings.Contains(long, exemption) {
		t.Errorf("gc breaker --help does not state the %q exemption:\n%s", exemption, long)
	}
}
