package session

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func createTestSession(t *testing.T, m *Manager, template string) string {
	t.Helper()
	sp := m.sp.(*runtime.Fake)
	_ = sp // ensure fake provider available

	b, err := m.store.Create(beads.Bead{
		Title: template,
		Type:  BeadType,
		Labels: []string{
			LabelSession,
			"template:" + template,
		},
		Metadata: map[string]string{
			"template":     template,
			"state":        string(StateActive),
			"session_name": "s-test-" + template,
		},
	})
	if err != nil {
		t.Fatalf("creating test bead: %v", err)
	}
	return b.ID
}

// seedDrainAckStopPending puts a row into draining the way production does:
// the controller's drain-ack path is the only writer of state=draining.
func seedDrainAckStopPending(t *testing.T, m *Manager, id string) {
	t.Helper()
	if err := m.PersistedStore().ApplyPatch(id, DrainAckStopPendingPatch(time.Now())); err != nil {
		t.Fatalf("seeding drain-ack stop-pending: %v", err)
	}
}

func getState(t *testing.T, m *Manager, id string) State {
	t.Helper()
	b, err := m.store.Get(id)
	if err != nil {
		t.Fatalf("getting bead: %v", err)
	}
	return State(b.Metadata["state"])
}

func TestConformance_CreatingState(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := newTestManager(t, store, sp)

	// Create a bead in creating state.
	b, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"template":             "worker",
			"state":                string(StateCreating),
			"pending_create_claim": "true",
			"sleep_reason":         "idle-timeout",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Confirm creation transitions to active.
	if err := m.ConfirmCreation(b.ID); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, b.ID); s != StateActive {
		t.Errorf("state = %q, want %q", s, StateActive)
	}
	// Check state_reason.
	got, _ := store.Get(b.ID)
	if got.Metadata["state_reason"] != "creation_complete" {
		t.Errorf("state_reason = %q, want creation_complete", got.Metadata["state_reason"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Errorf("pending_create_claim = %q, want cleared", got.Metadata["pending_create_claim"])
	}
	if got.Metadata["sleep_reason"] != "" {
		t.Errorf("sleep_reason = %q, want cleared", got.Metadata["sleep_reason"])
	}
}

func TestConformance_SuspendDrainingTearsDownRuntimeWithoutRewritingState(t *testing.T) {
	// ga-rxhu2: `gc stop` and `gc restart` issue suspend on every session bead
	// with no state pre-filter. Suspend from draining used to return
	// ErrIllegalTransition, so every restart SKIPPED the draining seats and they
	// survived as live panes still holding their pool slot names. Handle draining
	// the way failed-create is already handled (#2597, same function): tear the
	// runtime down best-effort and report success.
	//
	// Deliberately NOT a new state-machine edge. Adding
	// StateDraining -> StateSuspended to CmdSuspend would release nothing (an OPEN
	// bead owns its session_name whatever its state) and would let an operator
	// "suspend" a seat mid-drain, losing the drain reason. So the early return
	// must leave `state` alone for the drain machinery to finish or the reconciler
	// to reap.
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := newTestManager(t, store, sp)

	id := createTestSession(t, m, "worker")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}
	seedDrainAckStopPending(t, m, id)

	if err := m.Suspend(context.Background(), testActor(m, ActorSweep), id, false); err != nil {
		t.Fatalf("sweep Suspend(draining) = %v, want nil (must not block gc stop and leave a live pane holding a pool name)", err)
	}
	if sp.CountCalls("Stop", sessName) == 0 {
		t.Errorf("sweep Suspend(draining) did not tear down the runtime session %q", sessName)
	}
	if sp.IsRunning(sessName) {
		t.Errorf("runtime session %q still running after sweep Suspend(draining)", sessName)
	}
	if got := getState(t, m, id); got != StateDraining {
		t.Errorf("state = %q, want %q — the early return must not rewrite state and lose the drain", got, StateDraining)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if after.Metadata["suspended_at"] != "" {
		t.Errorf("suspended_at = %q, want empty: this path performs no suspension", after.Metadata["suspended_at"])
	}
	if after.Metadata["drain_at"] == "" {
		t.Error("drain_at was cleared; the drain reason must survive a city stop")
	}
}

// The draining latitude belongs to the city-stop SWEEP, not to an operator
// naming one session. A targeted Suspend of a draining seat has none of the
// gates the reconciler's terminal escalation insists on for the same class of
// kill, so it must keep returning the illegal transition instead of quietly
// killing a live agent mid-drain and reporting 200.
func TestConformance_OperatorSuspendStillRejectsDraining(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := newTestManager(t, store, sp)

	id := createTestSession(t, m, "worker")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}
	seedDrainAckStopPending(t, m, id)

	err = m.Suspend(context.Background(), testActor(m, ActorOperator), id, false)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Suspend(draining) = %v, want ErrIllegalTransition — an operator suspend must not become an ungated mid-drain kill", err)
	}
	if sp.CountCalls("Stop", sessName) != 0 {
		t.Errorf("operator Suspend(draining) tore down runtime %q anyway", sessName)
	}
	if !sp.IsRunning(sessName) {
		t.Errorf("runtime session %q was killed by a rejected operator suspend", sessName)
	}
}

// A teardown that FAILED must not be reported as success. Callers read nil as
// "the seat stopped" — gc stop prints "Stopped agent", counts it and records
// session.stopped — so swallowing the error makes the sweep claim success over a
// pane still alive holding its pool slot name: the exact wedge class here.
func TestConformance_SuspendForShutdownPropagatesDrainingTeardownFailure(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := newTestManager(t, store, sp)

	id := createTestSession(t, m, "worker")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}
	sp.StopErrors = map[string]error{sessName: errors.New("provider refused the stop")}
	seedDrainAckStopPending(t, m, id)

	err = m.Suspend(context.Background(), testActor(m, ActorSweep), id, false)
	if err == nil {
		t.Fatal("sweep Suspend(draining) = nil despite a failed teardown; gc stop would report a live pane as stopped")
	}
	if !strings.Contains(err.Error(), "stopping runtime session") {
		t.Errorf("err = %v, want it to name the runtime teardown failure", err)
	}
}

// ga-fqdzdy: the shutdown sweep can reach a live runtime whose row still reads
// creating (only the start's commit moves the row to active, and the city stop
// cancels its starts first). Rejecting that row made the stop fail and the
// live runtime survive the shutdown, so the sweep tears the runtime down like
// it does a draining seat.
//
// Like draining, it writes nothing, last_woke_at included: a start the stop
// canceled never commits, so the next start settles the row the sweep leaves
// and reads its marker. Its stuck-creating reap keeps a claimless row for the
// relaunch (TestSweepStopOfAnAbandonedStartKeepsItForTheNextStart), and a
// pending create, like this one, keeps its started-create lease.
func TestConformance_SweepSuspendCreatingTearsDownRuntimeWithoutRewritingState(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := newTestManager(t, store, sp)

	id := createTestSession(t, m, "worker")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}
	// Land the row the way the async-start window leaves it: creating, claim
	// held, identity minted, wake stamp fresh.
	seed := map[string]string{
		"state":                     string(StateCreating),
		"pending_create_claim":      "true",
		"pending_create_started_at": "2026-10-30T12:00:00Z",
		"instance_token":            "tok-worker",
		"generation":                "1",
		"last_woke_at":              "2026-10-30T12:00:01Z",
	}
	for k, v := range seed {
		if err := store.SetMetadata(id, k, v); err != nil {
			t.Fatalf("seeding %s: %v", k, err)
		}
	}
	before, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}

	if err := m.Suspend(context.Background(), testActor(m, ActorSweep), id, false); err != nil {
		t.Fatalf("sweep Suspend(creating) = %v, want nil (the runtime would survive the city stop)", err)
	}
	if sp.CountCalls("Stop", sessName) == 0 {
		t.Errorf("sweep Suspend(creating) did not tear down the runtime session %q", sessName)
	}
	if sp.IsRunning(sessName) {
		t.Errorf("runtime session %q still running after sweep Suspend(creating)", sessName)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if got, want := after.Metadata["last_woke_at"], seed["last_woke_at"]; got != want {
		t.Errorf("last_woke_at = %q, want %q kept: it marks a start that has not committed, which the next start settles", got, want)
	}
	if !maps.Equal(after.Metadata, before.Metadata) {
		t.Errorf("metadata = %v, want it unchanged %v: the row is its start's, and the sweep writes nothing to it", after.Metadata, before.Metadata)
	}
}

// The creating latitude belongs to the city-stop SWEEP alone, like draining: an
// operator naming ONE session in creating, and the idle auto-suspend, still get
// the illegal transition, because the row is its in-flight start's to finish
// (TestConformance_SuspendStillRejectsOtherIllegalStates pins the operator arm
// generically; this pins both intents against a live runtime).
func TestConformance_OperatorSuspendStillRejectsCreating(t *testing.T) {
	for _, tc := range []struct {
		name string
		soft bool
	}{
		{"operator", false},
		{"idle auto-suspend", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			m := newTestManager(t, store, sp)

			id := createTestSession(t, m, "worker")
			b, err := store.Get(id)
			if err != nil {
				t.Fatalf("get bead: %v", err)
			}
			sessName := b.Metadata["session_name"]
			if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
				t.Fatalf("seeding runtime: %v", err)
			}
			if err := store.SetMetadata(id, "state", string(StateCreating)); err != nil {
				t.Fatalf("set state creating: %v", err)
			}

			err = m.Suspend(context.Background(), testActor(m, ActorOperator), id, tc.soft)
			if !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("Suspend(creating) = %v, want ErrIllegalTransition — the row is its start's to finish", err)
			}
			var ite *IllegalTransitionError
			if !errors.As(err, &ite) || ite.From != StateCreating || ite.Command != CmdSuspend {
				t.Fatalf("err = %v (%T), want *IllegalTransitionError{From: creating, Command: suspend}", err, err)
			}
			if sp.CountCalls("Stop", sessName) != 0 {
				t.Errorf("%s Suspend(creating) tore down runtime %q anyway", tc.name, sessName)
			}
			if !sp.IsRunning(sessName) {
				t.Errorf("runtime session %q was killed by a rejected %s suspend", sessName, tc.name)
			}
		})
	}
}

// The same teardown-failure contract as draining above: a failed teardown is
// the stop's error, never a refusal of the row, and the row reads as it did.
func TestConformance_SweepSuspendCreatingPropagatesTeardownFailure(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := newTestManager(t, store, sp)

	id := createTestSession(t, m, "worker")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}
	const seededStamp = "2026-10-30T12:00:01Z"
	for k, v := range map[string]string{
		"state":        string(StateCreating),
		"last_woke_at": seededStamp,
	} {
		if err := store.SetMetadata(id, k, v); err != nil {
			t.Fatalf("seeding %s: %v", k, err)
		}
	}
	sp.StopErrors = map[string]error{sessName: errors.New("provider refused the stop")}
	before, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}

	err = m.Suspend(context.Background(), testActor(m, ActorSweep), id, false)
	if err == nil {
		t.Fatal("sweep Suspend(creating) = nil despite a failed teardown; gc stop would report a live pane as stopped")
	}
	if !strings.Contains(err.Error(), "stopping runtime session") {
		t.Errorf("err = %v, want it to name the runtime teardown failure, not refuse the row", err)
	}
	if errors.Is(err, ErrIllegalTransition) {
		t.Errorf("err = %v, a teardown failure must not read as an illegal transition", err)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if got := after.Metadata["last_woke_at"]; got != seededStamp {
		t.Errorf("last_woke_at = %q, want the seeded %q", got, seededStamp)
	}
	if !maps.Equal(after.Metadata, before.Metadata) {
		t.Errorf("metadata = %v, want it unchanged %v after the failed stop", after.Metadata, before.Metadata)
	}
}

// The scope control for the pins above. The sweep's escape hatches stay
// state-specific (draining, creating) and sweep-only, never a blanket "any
// state suspends" that would hide a real illegal-transition bug elsewhere: an
// operator's suspend of creating and start-pending still returns the illegal
// transition here. (An operator's suspend of a drained row is legal and holds
// it; the sweep and the idle auto-suspend still refuse it, see
// TestOnlyTheOperatorSuspendsADrainedRow.)
func TestConformance_SuspendStillRejectsOtherIllegalStates(t *testing.T) {
	for _, from := range []State{StateStartPending, StateCreating} {
		t.Run(string(from), func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			m := newTestManager(t, store, sp)

			id := createTestSession(t, m, "worker")
			if err := store.SetMetadata(id, "state", string(from)); err != nil {
				t.Fatalf("set state %q: %v", from, err)
			}

			err := m.Suspend(context.Background(), testActor(m, ActorOperator), id, false)
			if err == nil {
				t.Fatalf("Suspend from %q should return ErrIllegalTransition", from)
			}
			if !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("err = %v, want wrapping ErrIllegalTransition", err)
			}
			var ite *IllegalTransitionError
			if !errors.As(err, &ite) {
				t.Fatalf("err should unwrap to *IllegalTransitionError; got %T", err)
			}
			if ite.From != from {
				t.Errorf("ite.From = %q, want %q", ite.From, from)
			}
			if ite.Command != CmdSuspend {
				t.Errorf("ite.Command = %q, want %q", ite.Command, CmdSuspend)
			}
		})
	}
}

// The other scope control: an ACTIVE seat's suspend is completely unchanged —
// runtime stopped, state rewritten to suspended, suspended_at stamped.
func TestConformance_SuspendActiveSessionUnchanged(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := newTestManager(t, store, sp)

	id := createTestSession(t, m, "worker")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}

	if err := m.Suspend(context.Background(), testActor(m, ActorOperator), id, false); err != nil {
		t.Fatalf("Suspend(active) = %v, want nil", err)
	}
	if got := getState(t, m, id); got != StateSuspended {
		t.Errorf("state = %q, want %q", got, StateSuspended)
	}
	if sp.IsRunning(sessName) {
		t.Errorf("runtime session %q still running after Suspend(active)", sessName)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if after.Metadata["suspended_at"] == "" {
		t.Error("suspended_at not stamped on an ordinary active suspend")
	}
}

// The state machine itself must NOT gain a draining edge: the fix lives in
// Manager.Suspend's pre-checks, and a table edge would release no pool name
// while silently converting a drain into a suspension.
func TestConformance_SuspendTableStillRejectsDraining(t *testing.T) {
	if _, err := Transition(StateDraining, CmdSuspend); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("Transition(draining, suspend) err = %v, want ErrIllegalTransition — "+
			"the escape hatch belongs in Manager.Suspend, not the transition table", err)
	}
}

func TestConformance_SuspendFailedCreateTearsDownRuntime(t *testing.T) {
	// #2597: `gc stop` issues suspend on every session bead, including
	// failed-create ones (it does not pre-filter by state). failed-create is a
	// create-rollback terminal state with no live turn to suspend, but it may
	// have leaked a runtime process. Under a backing-store outage the reconciler
	// cannot reap these (its close path requires a reachable store), so suspend
	// is the only thing that can tear the leaked process down. Suspend must
	// therefore succeed and stop the runtime rather than reject the command
	// with an illegal-transition error that blocks `gc stop` city-wide.
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := newTestManager(t, store, sp)

	id := createTestSession(t, m, "dog")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]

	// Seed a leaked runtime process and the failed-create landing state.
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}
	if err := store.SetMetadata(id, "state", string(StateFailedCreate)); err != nil {
		t.Fatalf("set failed-create state: %v", err)
	}

	// Suspend(failed-create) must succeed so `gc stop` is not blocked
	// city-wide. The pre-fix regression returned a wrapped ErrIllegalTransition;
	// either symptom (any non-nil) trips this assertion and pinpoints the
	// regression by quoting the returned error.
	if err := m.Suspend(context.Background(), testActor(m, ActorOperator), id, false); err != nil {
		t.Fatalf("Suspend(failed-create) = %v, want nil (must not block gc stop)", err)
	}
	if sp.CountCalls("Stop", sessName) == 0 {
		t.Errorf("Suspend(failed-create) did not tear down the leaked runtime session %q", sessName)
	}
	if sp.IsRunning(sessName) {
		t.Errorf("runtime session %q still running after Suspend(failed-create)", sessName)
	}
}

func TestCanonicalLifecycleState(t *testing.T) {
	cases := []struct {
		name string
		in   State
		want State
	}{
		{"empty legacy state normalizes to active", StateNone, StateActive},
		{"awake alias normalizes to active", StateAwake, StateActive},
		{"active is unchanged", StateActive, StateActive},
		{"asleep is unchanged", StateAsleep, StateAsleep},
		{"suspended is unchanged", StateSuspended, StateSuspended},
		{"failed-create is unchanged", StateFailedCreate, StateFailedCreate},
		{"drained is not remapped here", State("drained"), State("drained")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalLifecycleState(tc.in); got != tc.want {
				t.Errorf("canonicalLifecycleState(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
