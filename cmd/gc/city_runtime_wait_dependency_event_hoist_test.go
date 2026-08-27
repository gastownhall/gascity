package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// waitDependencyEventHoistFixture is the ga-zo9h3 shape: one asleep wait-held
// session, one pending dependency wait, one dependency that is about to close.
// The cached wait-dependency index is DELIBERATELY not seeded — that is the
// state the instrumented journey run captured ("reserve dep=zn-r54 targets=0"),
// because the census the index is built from is a CACHE observation and the
// wait was registered after it.
type waitDependencyEventHoistFixture struct {
	cr         *CityRuntime
	env        *reconcilerTestEnv
	waitID     string
	sessionID  string
	dependency string
}

func newWaitDependencyEventHoistFixture(t *testing.T) *waitDependencyEventHoistFixture {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	dependency, err := env.store.Create(beads.Bead{Title: "dependency"})
	if err != nil {
		t.Fatalf("create dependency: %v", err)
	}
	target := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&target, map[string]string{
		"state":              string(sessionpkg.StateAsleep),
		"continuation_epoch": "7",
		"wait_hold":          "true",
		"sleep_intent":       string(sessionpkg.SleepReasonWaitHold),
		"sleep_reason":       string(sessionpkg.SleepReasonWaitHold),
	})
	wait, err := env.store.Create(sessionWaitShadowBead(target.ID, dependency.ID))
	if err != nil {
		t.Fatalf("create dependency wait: %v", err)
	}
	if err := env.store.SetMetadata(wait.ID, "registered_epoch", "7"); err != nil {
		t.Fatalf("stamp wait epoch: %v", err)
	}
	if err := env.store.Close(dependency.ID); err != nil {
		t.Fatalf("close dependency: %v", err)
	}

	cs := &controllerState{
		cfg:                         env.cfg,
		sp:                          env.sp,
		cityPath:                    t.TempDir(),
		cityBeadStore:               env.store,
		eventProv:                   events.NewFake(),
		rolloutFlags:                rollout.ForTest(rollout.WithSessionReconciler(rollout.Auto)),
		sessionStartGeneration:      1,
		sessionStartStoreGeneration: 1,
	}
	cr := &CityRuntime{
		cs: cs, cfg: env.cfg, stderr: io.Discard,
		sessionStartOwnership: sessionStartOwnershipKeyed,
		sessionStartMode:      rollout.Auto,
	}
	return &waitDependencyEventHoistFixture{
		cr: cr, env: env, waitID: wait.ID, sessionID: target.ID, dependency: dependency.ID,
	}
}

// TestBeadClosedReservesWaitingDependentWithoutTheCachedIndex is ga-zo9h3
// option (b)'s primary RED. At the dependency's bead.closed the cached index
// answers ZERO targets, so before the hoist no reservation and no certificate
// existed — and the poke's legacy tick then advanced the durable wait into
// legacy shape (ready_owner="", ready_operation=""), which is exactly what left
// the keyed wait_dependency commit with nothing to consume. Resolving the
// waiting dependents live AT THE EVENT is what makes the certificate exist
// before the poke runs a tick.
func TestBeadClosedReservesWaitingDependentWithoutTheCachedIndex(t *testing.T) {
	fixture := newWaitDependencyEventHoistFixture(t)

	// Premise: the cached index is the state the journey captured.
	if targets := fixture.cr.sessionWaitDependencyTargetsForDependency(fixture.dependency); len(targets) != 0 {
		t.Fatalf("premise broken: cached index already answers %+v for the dependency", targets)
	}

	reserved := fixture.cr.reserveSessionWaitDependencyTargets(t.Context(), fixture.dependency)
	if len(reserved) != 1 || reserved[0].WaitID != fixture.waitID {
		t.Fatalf("reserved targets at bead.closed = %+v, want exactly the waiting dependent %q", reserved, fixture.waitID)
	}
	if !fixture.cr.ownsReservedSessionWaitDependencyStart(fixture.sessionID) {
		t.Fatal("bead.closed installed no keyed start reservation; there is nothing for legacy to stand down for")
	}
	waitInfo, err := sessionFrontDoor(fixture.env.store).GetWait(fixture.waitID)
	if err != nil {
		t.Fatalf("read wait: %v", err)
	}
	if !fixture.cr.ownsReservedSessionWaitDependencyWait(waitInfo) {
		t.Fatal("bead.closed certified no durable wait; the certificate must exist before the poke runs a tick")
	}
}

// TestBeadClosedReservationComposesWithTheWaitAdvanceStandDown is the third
// ruled RED: the round-6 stand-down twins landed at e4fcb01f5f compose with the
// event hoist, and there is exactly one starter either way. With the
// reservation installed at the event, legacy's wait-advance boundary yields;
// once the reservation retires, legacy is free again on its very next pass.
func TestBeadClosedReservationComposesWithTheWaitAdvanceStandDown(t *testing.T) {
	fixture := newWaitDependencyEventHoistFixture(t)

	if fixture.cr.keyedWaitAdvanceExcluded(sessionpkg.WaitInfo{ID: fixture.waitID, SessionID: fixture.sessionID}) {
		t.Fatal("premise broken: legacy already stands down before any reservation exists")
	}

	reserved := fixture.cr.reserveSessionWaitDependencyTargets(t.Context(), fixture.dependency)
	if len(reserved) != 1 {
		t.Fatalf("reserved targets = %+v, want one", reserved)
	}
	if !fixture.cr.keyedWaitAdvanceExcluded(sessionpkg.WaitInfo{ID: fixture.waitID, SessionID: fixture.sessionID}) {
		t.Fatal("legacy did not stand down at the wait-advance boundary for the event-hoisted reservation")
	}

	fixture.cr.releaseSessionWaitDependencyReservation(reserved[0])
	if fixture.cr.keyedWaitAdvanceExcluded(sessionpkg.WaitInfo{ID: fixture.waitID, SessionID: fixture.sessionID}) {
		t.Fatal("legacy stayed fenced after the keyed reservation retired; the stand-down must be lease-triggered")
	}
}

// TestWarmIndexPathStillReservesAndSkipsTheLiveRead is the second ruled RED,
// stated as the property that actually matters: the event hoist ADDS a path, it
// does not replace the cached one. A city whose index already carries the wait
// reserves exactly as before -- so a bead.closed nobody delivered still recovers
// through the anti-entropy backstop the patrol census drives -- and the live
// durable read is not paid at all when the index can answer.
func TestWarmIndexPathStillReservesAndSkipsTheLiveRead(t *testing.T) {
	fixture := newWaitDependencyEventHoistFixture(t)
	fixture.cr.sessionWaitDependencyIndex = newSessionWaitDependencyIndex()
	if err := fixture.cr.sessionWaitDependencyIndex.Rebuild([]sessionpkg.WaitInfo{{
		ID: fixture.waitID, SessionID: fixture.sessionID, Kind: "deps", Status: "open",
		State: waitStatePending, DepIDs: []string{fixture.dependency}, DepMode: "all",
	}}); err != nil {
		t.Fatalf("rebuild warm index: %v", err)
	}
	fixture.cr.sessionWaitDependencyIndexGeneration = 1

	reserved := fixture.cr.reserveSessionWaitDependencyTargets(t.Context(), fixture.dependency)
	if len(reserved) != 1 || reserved[0].WaitID != fixture.waitID {
		t.Fatalf("warm-index reservation = %+v, want the same exact target", reserved)
	}
	if reserved[0].authoritative {
		t.Fatal("the warm-index path paid the live durable read; the hoist must only run when the index cannot answer")
	}
	if !fixture.cr.ownsReservedSessionWaitDependencyStart(fixture.sessionID) {
		t.Fatal("the warm-index backstop installed no keyed start reservation")
	}
}

// TestLiveDependentResolveIsThrottledOnACityWithNoPendingWaits pins the cost
// bound: with no pending waits at all, no bead.closed can name a waiting
// dependent, so repeating the durable listing per close buys nothing.
func TestLiveDependentResolveIsThrottledOnACityWithNoPendingWaits(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	cr := &CityRuntime{
		cs: &controllerState{
			cfg: env.cfg, sp: env.sp, cityPath: t.TempDir(), cityBeadStore: env.store,
			eventProv: events.NewFake(),
		},
		cfg: env.cfg, stderr: io.Discard,
	}

	if targets := cr.authoritativeSessionWaitDependencyTargetsForDependency("gc-none"); len(targets) != 0 {
		t.Fatalf("targets on a city with no pending waits = %+v, want none", targets)
	}
	if !cr.waitDependencyLiveResolveEmpty || cr.waitDependencyLiveResolveAt.IsZero() {
		t.Fatal("the empty result was not memoized; the throttle cannot engage")
	}
	before := cr.waitDependencyLiveResolveAt
	if targets := cr.authoritativeSessionWaitDependencyTargetsForDependency("gc-none"); len(targets) != 0 {
		t.Fatalf("throttled call returned %+v, want none", targets)
	}
	if !cr.waitDependencyLiveResolveAt.Equal(before) {
		t.Fatal("the throttle did not suppress a second durable listing inside its floor")
	}
}
