package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/resilience"
	"github.com/gastownhall/gascity/internal/runtime"
)

// stalenessFake is a Fake whose state cache reports a settable staleness, the
// way the tmux provider's cache does.
type stalenessFake struct {
	*runtime.Fake
	age   time.Duration
	known bool
}

func (s *stalenessFake) StateCacheStaleness() (time.Duration, bool) { return s.age, s.known }

// fleetReconcileEnv is a reconciler env whose fleet start breaker runs on the
// env's clock, with two sessions of other templates already failed so one more
// start failure would trip it.
type fleetReconcileEnv struct {
	*reconcilerTestEnv
	guard *fleetStartGuard
}

func newFleetReconcileEnv(t *testing.T) *fleetReconcileEnv {
	t.Helper()
	env := newReconcilerTestEnv(t)
	guard := newFleetStartGuard(env.clk.Now, fleetTestStartupTimeout)
	env.startOptions = append(env.startOptions, withFleetStartGuard(guard))
	return &fleetReconcileEnv{reconcilerTestEnv: env, guard: guard}
}

// primeTwoFailures records failed starts of two other templates, so the next
// counted failure is the third distinct session across a third template.
func (e *fleetReconcileEnv) primeTwoFailures(t *testing.T) {
	t.Helper()
	failFleetStart(t, e.guard, "other-1", "alpha")
	e.clk.Advance(time.Second)
	failFleetStart(t, e.guard, "other-2", "beta")
	if got := fleetState(e.guard); got != resilience.StateClosed {
		t.Fatalf("breaker after two failures = %v, want closed", got)
	}
}

// startedLiveWorker sets up an awake worker session whose automatic start the
// guard is confirming.
func (e *fleetReconcileEnv) startedLiveWorker(t *testing.T) beads.Bead {
	t.Helper()
	e.cfg.Agents = append(e.cfg.Agents, config.Agent{Name: "worker"})
	e.addDesired("worker", "worker", true)
	session := e.createSessionBead("worker", "worker")
	e.markSessionActive(&session)
	ticket, ok := e.guard.Admit(session.ID, "worker")
	if !ok {
		t.Fatal("Admit(worker) denied, want admitted")
	}
	ticket.Resolve(fleetStartOK)
	return session
}

func (e *fleetReconcileEnv) tracked() int {
	e.guard.mu.Lock()
	defer e.guard.mu.Unlock()
	return len(e.guard.starts)
}

// reconcileWithProvider runs one reconcile tick of sessions over sp instead of
// the env's own fake.
func (e *fleetReconcileEnv) reconcileWithProvider(sp runtime.Provider, sessions []beads.Bead) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	reconcileSessionBeadsAtPath(
		context.Background(), e.city, sessions, e.desiredState, cfgNames, e.cfg, sp,
		e.store, nil, nil, nil, nil, e.dt, poolDesired, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr,
		e.startOptions...,
	)
}

// A session the reconciler is draining on purpose is not a session that
// failed to stay up: its runtime ending inside the stability window must not
// count toward the breaker.
func TestReconcile_FleetGuardIgnoresDeathOfADrainingSession(t *testing.T) {
	env := newFleetReconcileEnv(t)
	env.primeTwoFailures(t)
	session := env.startedLiveWorker(t)

	if !beginSessionDrainInfo(env.sessionInfo(session.ID), env.sp, env.dt, "config-drift", env.clk, defaultDrainTimeout) {
		t.Fatal("begin config-drift drain")
	}
	if err := env.sp.Stop("worker"); err != nil {
		t.Fatalf("stop worker: %v", err)
	}
	env.clk.Advance(5 * time.Second)
	env.reconcile([]beads.Bead{session})

	if got := fleetState(env.guard); got != resilience.StateClosed {
		t.Fatalf("breaker after a draining session's runtime ended = %v, want closed: a deliberate drain is not a start failure", got)
	}
	if n := env.tracked(); n != 0 {
		t.Fatalf("guard still confirms %d start(s) of a drained session, want none", n)
	}
}

// A session that leaves the desired set while its start is being confirmed is
// forgotten: nothing observes it again, so its entry would otherwise stay in
// the guard for the life of the controller.
func TestReconcile_FleetGuardForgetsAnUndesiredSession(t *testing.T) {
	env := newFleetReconcileEnv(t)
	env.cfg.Agents = append(env.cfg.Agents, config.Agent{Name: "worker"})
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	ticket, ok := env.guard.Admit(session.ID, "worker")
	if !ok {
		t.Fatal("Admit(worker) denied, want admitted")
	}
	ticket.Resolve(fleetStartOK)
	if n := env.tracked(); n != 1 {
		t.Fatalf("guard confirms %d start(s) after the start, want 1", n)
	}

	env.reconcile([]beads.Bead{session}) // worker is not in the desired state

	if n := env.tracked(); n != 0 {
		t.Fatalf("guard still confirms %d start(s) of a session that left the desired set, want none", n)
	}
}

// A session that is not being stopped and whose runtime ends inside the
// stability window did not stay up: that is a start failure, whatever its exit
// status. Only the reconciler's own stops, drains and closes are excused.
func TestReconcile_FleetGuardCountsAnUnpromptedDeathInsideTheStabilityWindow(t *testing.T) {
	env := newFleetReconcileEnv(t)
	env.primeTwoFailures(t)
	session := env.startedLiveWorker(t)
	if err := env.sp.Stop("worker"); err != nil {
		t.Fatalf("stop worker: %v", err)
	}
	env.clk.Advance(5 * time.Second)
	env.reconcile([]beads.Bead{session})

	if got := fleetState(env.guard); got != resilience.StateOpen {
		t.Fatalf("breaker after a third session exited unprompted inside %s = %v, want open", stabilityThreshold, got)
	}
}

// Trigger B: a state cache that cannot refresh while sessions are expected up
// opens the breaker, through the reconciler's wiring.
func TestReconcile_FleetGuardOpensOnAStaleStateCache(t *testing.T) {
	env := newFleetReconcileEnv(t)
	sp := &stalenessFake{Fake: env.sp, age: time.Hour, known: true}
	session := env.startedLiveWorker(t)

	env.reconcileWithProvider(sp, []beads.Bead{session})

	st := env.guard.Status()
	if st.State != resilience.StateOpen || st.Reason != fleetReasonStateCache {
		t.Fatalf("breaker after a tick with a 1h-stale state cache = %v (%q), want open (%q)", st.State, st.Reason, fleetReasonStateCache)
	}
}

func TestReconcile_FleetGuardIgnoresAnUnknownOrFreshStateCache(t *testing.T) {
	for name, sp := range map[string]*stalenessFake{
		"cache age unknown": {age: time.Hour, known: false},
		"cache fresh":       {age: 0, known: true},
	} {
		t.Run(name, func(t *testing.T) {
			env := newFleetReconcileEnv(t)
			sp.Fake = env.sp
			session := env.startedLiveWorker(t)
			env.reconcileWithProvider(sp, []beads.Bead{session})
			if got := fleetState(env.guard); got != resilience.StateClosed {
				t.Fatalf("breaker = %v, want closed", got)
			}
		})
	}
}

// Sessions that are asleep, or not expected up, are not sessions the state
// cache is mistaken about: a city of them with a stale cache is idle, not
// broken.
func TestReconcile_FleetGuardStateCacheIgnoresSessionsNotExpectedRunning(t *testing.T) {
	env := newFleetReconcileEnv(t)
	sp := &stalenessFake{Fake: env.sp, age: time.Hour, known: true}
	env.cfg.Agents = append(env.cfg.Agents, config.Agent{Name: "worker"})
	env.addDesired("worker", "worker", false)
	asleep := env.createSessionBead("worker", "worker") // state "asleep", no wake reason

	env.reconcileWithProvider(sp, []beads.Bead{asleep})

	if got := fleetState(env.guard); got != resilience.StateClosed {
		t.Fatalf("breaker with only asleep sessions and a stale cache = %v, want closed", got)
	}
}
