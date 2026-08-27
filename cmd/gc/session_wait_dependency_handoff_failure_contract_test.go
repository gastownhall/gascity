package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// These are contract tests for the Slice 1 reservation-to-controller handoff.
// They deliberately use the controller's blocking seam rather than time so a
// duplicate hint, an index rebuild, and the first provider-effect boundary
// have a deterministic ordering.
func TestSessionWaitDependencyHandoff_DuplicateHintAndUnchangedRebuildKeepOperationAndStartOnce(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	started := make(chan sessionStartAdmission, 1)
	reconciled := make(chan struct{})
	var beforeOnce sync.Once
	var starts atomic.Int64

	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 2,
		MaxRetries:  0,
		Stderr:      io.Discard,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			starts.Add(1)
			started <- admission
			close(reconciled)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.beforeMarkInFlightForTest = func() {
		beforeOnce.Do(func() {
			close(entered)
			<-release
		})
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Stop)

	first := handoffFailureLease("wait-a", "session-a", 1, "operation-first")
	if outcome, err := controller.AdmitWaitDependency(first); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("first AdmitWaitDependency = %q, %v; want accepted, nil", outcome, err)
	}
	awaitClose(t, entered, "first admission before provider-effect boundary")

	// This models the same durable wait/session certificate observed through a
	// rebuilt routing index. The second token is intentionally different: it
	// must never supersede the operation minted by the first durable claim.
	rebuilt := first
	rebuilt.IndexGeneration = 2
	rebuilt.Operation = "operation-rebuilt"
	if outcome, err := controller.AdmitWaitDependency(rebuilt); err != nil || outcome != sessionStartAdmissionCoalesced {
		t.Fatalf("rebuilt AdmitWaitDependency = %q, %v; want coalesced, nil", outcome, err)
	}
	close(release)
	awaitClose(t, reconciled, "reconciler provider-effect boundary")
	admission := <-started
	if admission.WaitDependency == nil {
		t.Fatal("reconciled admission lost its durable wait lease")
	}
	if got := admission.WaitDependency.Operation; got != first.Operation {
		t.Fatalf("durable operation = %q, want first operation %q", got, first.Operation)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("provider Start boundaries = %d, want 1", got)
	}
}

func TestSessionWaitDependencyHandoff_StaleReservationCannotTransferAfterWaitRebind(t *testing.T) {
	index := newSessionWaitDependencyIndex()
	if err := index.Rebuild([]sessionpkg.WaitInfo{{
		ID: "wait-a", SessionID: "session-b", Kind: "deps", Status: "open",
		State: waitStatePending, DepIDs: []string{"dependency-a"}, DepMode: "all",
	}}); err != nil {
		t.Fatal(err)
	}
	staleTarget := sessionWaitDependencyTarget{
		WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dependency-a"}, DepMode: "all", generation: 1,
	}
	cr := &CityRuntime{
		sessionWaitDependencyIndex:           index,
		sessionWaitDependencyIndexGeneration: 1,
		sessionWaitDependencyReservations: map[string]sessionWaitDependencyStartLease{
			"session-a": handoffFailureLease("wait-a", "session-a", 1, "operation-stale"),
		},
	}
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers: 1, MaxDistinct: 1, MaxRetries: 0, Stderr: io.Discard,
		Reconcile: func(context.Context, sessionStartAdmission) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	if cr.sessionWaitDependencyTargetCertified(staleTarget) {
		t.Fatal("rebound wait left the old session reservation certified")
	}
	if _, err := cr.transferSessionWaitDependencyReservation(staleTarget, handoffFailureLease("wait-a", "session-a", 1, "operation-stale"), controller); err == nil {
		t.Fatal("stale reservation transferred after wait rebind")
	}
	if !cr.ownsReservedSessionWaitDependencyStart("session-a") {
		t.Fatal("failed stale transfer discarded its reservation before the caller chose Auto or Require recovery")
	}
}

func TestSessionWaitDependencyHandoff_AutoFailureYieldsReservationAndLegacyPokeOnce(t *testing.T) {
	target := sessionWaitDependencyTarget{
		WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dependency-a"}, DepMode: "all", generation: 1,
	}
	index := newSessionWaitDependencyIndex()
	if err := index.Rebuild([]sessionpkg.WaitInfo{{
		ID: target.WaitID, SessionID: target.SessionID, Kind: "deps", Status: "open",
		State: waitStatePending, DepIDs: target.DepIDs, DepMode: target.DepMode,
	}}); err != nil {
		t.Fatal(err)
	}
	pokes := make(chan struct{}, 2)
	cr := &CityRuntime{
		pokeCh:                               pokes,
		sessionStartMode:                     rollout.Auto,
		sessionWaitDependencyIndex:           index,
		sessionWaitDependencyIndexGeneration: 1,
		sessionWaitDependencyReservations: map[string]sessionWaitDependencyStartLease{
			target.SessionID: handoffFailureLease(target.WaitID, target.SessionID, target.generation, "operation-auto"),
		},
	}
	hint := sessionWaitDependencyStartHint{Target: target, Cause: sessionWaitDependencyCauseDependency}

	// Controller-unavailable and queue-overflow callbacks can race/retry. The
	// first Auto failure must yield ownership exactly once; later observations
	// must not create a legacy poke storm after the reservation is gone.
	cr.handleSessionWaitDependencyAdmissionFailure(hint, rollout.Auto, errors.New("controller unavailable"))
	cr.handleSessionWaitDependencyAdmissionFailure(hint, rollout.Auto, errors.New("admission overflow"))
	if cr.ownsReservedSessionWaitDependencyStart(target.SessionID) {
		t.Fatal("Auto failure retained the exact reservation")
	}
	if got := drainHandoffFailurePokes(pokes); got != 1 {
		t.Fatalf("legacy fallback pokes = %d, want 1", got)
	}
}

func TestSessionWaitDependencyHandoff_AutoFailureCleansStaleReservationWithoutLegacyPoke(t *testing.T) {
	staleTarget := sessionWaitDependencyTarget{
		WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dependency-a"}, DepMode: "all", generation: 1,
	}
	currentTarget := staleTarget
	currentTarget.SessionID = "session-b"
	index := newSessionWaitDependencyIndex()
	if err := index.Rebuild([]sessionpkg.WaitInfo{{
		ID: currentTarget.WaitID, SessionID: currentTarget.SessionID, Kind: "deps", Status: "open",
		State: waitStatePending, DepIDs: currentTarget.DepIDs, DepMode: currentTarget.DepMode,
	}}); err != nil {
		t.Fatal(err)
	}
	pokes := make(chan struct{}, 1)
	cr := &CityRuntime{
		pokeCh:                               pokes,
		sessionStartMode:                     rollout.Auto,
		sessionWaitDependencyIndex:           index,
		sessionWaitDependencyIndexGeneration: 1,
		sessionWaitDependencyReservations: map[string]sessionWaitDependencyStartLease{
			staleTarget.SessionID: handoffFailureLease(staleTarget.WaitID, staleTarget.SessionID, staleTarget.generation, "operation-stale"),
		},
	}

	cr.handleSessionWaitDependencyAdmissionFailure(
		sessionWaitDependencyStartHint{Target: staleTarget, Cause: sessionWaitDependencyCauseDependency},
		rollout.Auto,
		errors.New("controller unavailable"),
	)
	if cr.ownsReservedSessionWaitDependencyStart(staleTarget.SessionID) {
		t.Fatal("stale reservation was not cleaned")
	}
	if !cr.sessionWaitDependencyTargetCertified(currentTarget) {
		t.Fatal("stale failure retired the rebound wait target")
	}
	if got := drainHandoffFailurePokes(pokes); got != 0 {
		t.Fatalf("legacy fallback pokes = %d, want 0", got)
	}
}

func TestSessionWaitDependencyHandoff_AutoFailureWithoutReservationPokesOnce(t *testing.T) {
	target := sessionWaitDependencyTarget{
		WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dependency-a"}, DepMode: "all", generation: 1,
	}
	index := newSessionWaitDependencyIndex()
	if err := index.Rebuild([]sessionpkg.WaitInfo{{
		ID: target.WaitID, SessionID: target.SessionID, Kind: "deps", Status: "open",
		State: waitStatePending, DepIDs: target.DepIDs, DepMode: target.DepMode,
	}}); err != nil {
		t.Fatal(err)
	}
	pokes := make(chan struct{}, 2)
	cr := &CityRuntime{
		pokeCh:                               pokes,
		sessionStartMode:                     rollout.Auto,
		sessionWaitDependencyIndex:           index,
		sessionWaitDependencyIndexGeneration: 1,
	}
	hint := sessionWaitDependencyStartHint{Target: target, Cause: sessionWaitDependencyCauseDependency}

	cr.handleSessionWaitDependencyAdmissionFailure(hint, rollout.Auto, errors.New("controller unavailable"))
	cr.handleSessionWaitDependencyAdmissionFailure(hint, rollout.Auto, errors.New("admission overflow"))
	if cr.sessionWaitDependencyTargetCertified(target) {
		t.Fatal("Auto failure retained the certified target")
	}
	if got := drainHandoffFailurePokes(pokes); got != 1 {
		t.Fatalf("legacy fallback pokes = %d, want 1", got)
	}
}

func TestSessionWaitDependencyHandoff_AutoEnqueueFailurePokesOnce(t *testing.T) {
	target := sessionWaitDependencyTarget{
		WaitID: "wait-a", SessionID: "session-a", DepIDs: []string{"dependency-a"}, DepMode: "all", generation: 1,
	}
	index := newSessionWaitDependencyIndex()
	if err := index.Rebuild([]sessionpkg.WaitInfo{{
		ID: target.WaitID, SessionID: target.SessionID, Kind: "deps", Status: "open",
		State: waitStatePending, DepIDs: target.DepIDs, DepMode: target.DepMode,
	}}); err != nil {
		t.Fatal(err)
	}
	pokes := make(chan struct{}, 2)
	cr := &CityRuntime{
		pokeCh:                               pokes,
		stderr:                               io.Discard,
		sessionStartMode:                     rollout.Auto,
		sessionWaitDependencyIndex:           index,
		sessionWaitDependencyIndexGeneration: 1,
		sessionWaitDependencyReservations: map[string]sessionWaitDependencyStartLease{
			target.SessionID: handoffFailureLease(target.WaitID, target.SessionID, target.generation, "operation-auto"),
		},
	}

	cr.handleReservedSessionWaitDependencyEnqueueFailure(target, errors.New("queue full"))
	cr.handleReservedSessionWaitDependencyEnqueueFailure(target, errors.New("queue full"))
	if cr.ownsReservedSessionWaitDependencyStart(target.SessionID) {
		t.Fatal("Auto enqueue failure retained the exact reservation")
	}
	if cr.sessionWaitDependencyTargetCertified(target) {
		t.Fatal("Auto enqueue failure retained the certified target")
	}
	if got := drainHandoffFailurePokes(pokes); got != 1 {
		t.Fatalf("legacy fallback pokes = %d, want 1", got)
	}
}

func TestSessionWaitDependencyHandoff_RequireFailureRetainsReservationUntilExplicitRedrive(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	dependency, err := env.store.Create(beads.Bead{Title: "dependency"})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.Close(dependency.ID); err != nil {
		t.Fatal(err)
	}
	targetSession := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&targetSession, map[string]string{
		"state":              string(sessionpkg.StateAsleep),
		"continuation_epoch": "epoch-a",
		"wait_hold":          "true",
		"sleep_intent":       string(sessionpkg.SleepReasonWaitHold),
		"sleep_reason":       string(sessionpkg.SleepReasonWaitHold),
	})
	wait, err := env.store.Create(sessionWaitShadowBead(targetSession.ID, dependency.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.SetMetadata(wait.ID, "registered_epoch", "epoch-a"); err != nil {
		t.Fatal(err)
	}
	target := sessionWaitDependencyTarget{
		WaitID: wait.ID, SessionID: targetSession.ID, DepIDs: []string{dependency.ID}, DepMode: "all", generation: 1,
	}
	index := newSessionWaitDependencyIndex()
	if err := index.Rebuild([]sessionpkg.WaitInfo{{
		ID: target.WaitID, SessionID: target.SessionID, Kind: "deps", Status: "open",
		State: waitStatePending, DepIDs: target.DepIDs, DepMode: target.DepMode,
	}}); err != nil {
		t.Fatal(err)
	}
	pokes := make(chan struct{}, 1)
	cs := &controllerState{
		cfg:                         env.cfg,
		sp:                          env.sp,
		cityPath:                    t.TempDir(),
		cityBeadStore:               env.store,
		eventProv:                   events.NewFake(),
		pokeCh:                      pokes,
		rolloutFlags:                rollout.ForTest(rollout.WithSessionReconciler(rollout.Require)),
		sessionStartGeneration:      1,
		sessionStartStoreGeneration: 1,
	}
	cr := &CityRuntime{
		cs:                                   cs,
		cfg:                                  env.cfg,
		pokeCh:                               pokes,
		stderr:                               io.Discard,
		sessionStartOwnership:                sessionStartOwnershipKeyed,
		sessionStartMode:                     rollout.Require,
		sessionWaitDependencyIndex:           index,
		sessionWaitDependencyIndexGeneration: 1,
	}
	if reserved := cr.reserveSessionWaitDependencyTargets(t.Context(), dependency.ID); len(reserved) != 1 {
		t.Fatalf("reserved targets = %d, want 1", len(reserved))
	}
	hint := sessionWaitDependencyStartHint{Target: target, Cause: sessionWaitDependencyCauseDependency}

	cr.handleSessionWaitDependencyAdmissionFailure(hint, rollout.Require, errors.New("controller unavailable"))
	cr.handleSessionWaitDependencyAdmissionFailure(hint, rollout.Require, errors.New("controller unavailable"))
	if !cr.ownsReservedSessionWaitDependencyStart(target.SessionID) {
		t.Fatal("Require failure yielded its exact reservation to legacy")
	}
	if got := drainHandoffFailurePokes(pokes); got != 0 {
		t.Fatalf("Require failure generated %d legacy pokes, want 0", got)
	}
	cr.sessionWaitDependencyMu.RLock()
	owed := cr.sessionWaitDependencyStartupCensusOwed
	cr.sessionWaitDependencyMu.RUnlock()
	if !owed {
		t.Fatal("Require failure did not rearm explicit patrol/redrive")
	}

	redriven := make(chan struct{})
	var redriveCount atomic.Int64
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers: 1, MaxDistinct: 1, MaxRetries: 0, Stderr: io.Discard,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			if admission.WaitDependency == nil {
				t.Error("explicit redrive lost its wait lease")
			}
			redriveCount.Add(1)
			close(redriven)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Stop)
	cr.sessionStartMu.Lock()
	cr.sessionStartController = controller
	cr.sessionStartMu.Unlock()

	// No background retry enters the controller. The retained lease advances
	// only when patrol explicitly invokes this redrive.
	cr.redriveSessionWaitDependencyReservations(t.Context())
	awaitClose(t, redriven, "explicit Require reservation redrive")
	if got := redriveCount.Load(); got != 1 {
		t.Fatalf("explicit redrive admissions = %d, want 1", got)
	}
	if got := drainHandoffFailurePokes(pokes); got != 0 {
		t.Fatalf("Require redrive generated %d legacy pokes, want 0", got)
	}
}

func handoffFailureLease(waitID, sessionID string, generation uint64, operation string) sessionWaitDependencyStartLease {
	return sessionWaitDependencyStartLease{
		WaitID: waitID, SessionID: sessionID, DepIDs: []string{"dependency-a"}, DepMode: "all",
		RegisteredEpoch: "epoch-a", WaitRevision: 1, SessionRevision: 1,
		IndexGeneration: generation, ControllerGeneration: 1, Operation: operation,
	}
}

func drainHandoffFailurePokes(pokes <-chan struct{}) int {
	count := 0
	for {
		select {
		case <-pokes:
			count++
		default:
			return count
		}
	}
}
