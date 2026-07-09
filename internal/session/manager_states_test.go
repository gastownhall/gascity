package session

import (
	"context"
	"errors"
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
	m := NewManagerWithOptions(store, sp)

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

func TestConformance_DrainState(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "worker")

	// Begin drain.
	if err := m.BeginDrain(id, "config-drift"); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateDraining {
		t.Errorf("state = %q, want %q", s, StateDraining)
	}
	b, _ := store.Get(id)
	if b.Metadata["state_reason"] != "config-drift" {
		t.Errorf("state_reason = %q, want config-drift", b.Metadata["state_reason"])
	}
	if b.Metadata["drain_at"] == "" {
		t.Error("drain_at should be set")
	}

	// Archive after drain.
	if err := m.Archive(id, "drain_complete"); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateArchived {
		t.Errorf("state = %q, want %q", s, StateArchived)
	}
	b, _ = store.Get(id)
	if b.Metadata["archived_at"] == "" {
		t.Error("archived_at should be set")
	}
	if b.Metadata["pending_create_claim"] != "" {
		t.Errorf("pending_create_claim = %q, want cleared", b.Metadata["pending_create_claim"])
	}
	if b.Metadata["continuity_eligible"] != "false" {
		t.Errorf("continuity_eligible = %q, want false", b.Metadata["continuity_eligible"])
	}
}

func TestConformance_QuarantineState(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "worker")
	if err := store.SetMetadata(id, "last_woke_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	until := time.Now().Add(5 * time.Minute)
	if err := m.Quarantine(id, until, 3); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateQuarantined {
		t.Errorf("state = %q, want %q", s, StateQuarantined)
	}
	b, _ := store.Get(id)
	if b.Metadata["quarantine_cycle"] != "3" {
		t.Errorf("quarantine_cycle = %q, want 3", b.Metadata["quarantine_cycle"])
	}
	if b.Metadata["quarantined_until"] == "" {
		t.Error("quarantined_until should be set")
	}
	if b.Metadata["last_woke_at"] != "" {
		t.Errorf("last_woke_at = %q, want cleared", b.Metadata["last_woke_at"])
	}
}

func TestConformance_ArchivedReactivation(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "worker")

	// Archive first.
	if err := m.Archive(id, "scale-down"); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateArchived {
		t.Fatalf("state = %q, want %q", s, StateArchived)
	}

	if err := store.SetMetadata(id, "pending_create_claim", "true"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(id, "continuity_eligible", "false"); err != nil {
		t.Fatal(err)
	}

	// Reactivate.
	if err := m.Reactivate(id); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateAsleep {
		t.Errorf("state = %q, want %q after reactivation", s, StateAsleep)
	}
	b, _ := store.Get(id)
	if b.Metadata["state_reason"] != "reactivated" {
		t.Errorf("state_reason = %q, want reactivated", b.Metadata["state_reason"])
	}
	if b.Metadata["pending_create_claim"] != "" {
		t.Errorf("pending_create_claim = %q, want cleared", b.Metadata["pending_create_claim"])
	}
	if b.Metadata["continuity_eligible"] != "false" {
		t.Errorf("continuity_eligible = %q, want preserved false", b.Metadata["continuity_eligible"])
	}
	if b.Metadata["archived_at"] != "" {
		t.Error("archived_at should be cleared on reactivation")
	}
}

func TestConformance_IllegalTransitionDraining(t *testing.T) {
	// Fix 3j: manager mutations now validate against the state machine.
	// Drain puts a session in Draining; Suspend from Draining is illegal.
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "worker")

	if err := m.BeginDrain(id, "shutdown"); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}

	err := m.Suspend(id)
	if err == nil {
		t.Fatal("Suspend from Draining should return ErrIllegalTransition")
	}
	if !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("err = %v, want wrapping ErrIllegalTransition", err)
	}
	var ite *IllegalTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("err should unwrap to *IllegalTransitionError; got %T", err)
	}
	if ite.From != StateDraining {
		t.Errorf("ite.From = %q, want %q", ite.From, StateDraining)
	}
	if ite.Command != CmdSuspend {
		t.Errorf("ite.Command = %q, want %q", ite.Command, CmdSuspend)
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
	m := NewManagerWithOptions(store, sp)

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
	if err := m.Suspend(id); err != nil {
		t.Fatalf("Suspend(failed-create) = %v, want nil (must not block gc stop)", err)
	}
	if sp.CountCalls("Stop", sessName) == 0 {
		t.Errorf("Suspend(failed-create) did not tear down the leaked runtime session %q", sessName)
	}
	if sp.IsRunning(sessName) {
		t.Errorf("runtime session %q still running after Suspend(failed-create)", sessName)
	}
}

func TestConformance_QuarantineReactivation(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "crasher")

	// Quarantine the session.
	until := time.Now().Add(5 * time.Minute)
	if err := m.Quarantine(id, until, 3); err != nil {
		t.Fatal(err)
	}

	// Reactivate.
	if err := m.Reactivate(id); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateAsleep {
		t.Errorf("state = %q, want %q after quarantine reactivation", s, StateAsleep)
	}
	b, _ := store.Get(id)

	// quarantine_cycle should be preserved (for eviction tracking).
	if b.Metadata["quarantine_cycle"] != "3" {
		t.Errorf("quarantine_cycle = %q, want 3 (should be preserved)", b.Metadata["quarantine_cycle"])
	}
	// crash_count should be reset.
	if b.Metadata["crash_count"] != "0" {
		t.Errorf("crash_count = %q, want 0", b.Metadata["crash_count"])
	}
	// quarantined_until should be cleared.
	if b.Metadata["quarantined_until"] != "" {
		t.Error("quarantined_until should be cleared on reactivation")
	}
	// Quarantined non-terminal sessions remain continuity eligible by default.
	if b.Metadata["continuity_eligible"] != "true" {
		t.Errorf("continuity_eligible = %q, want true", b.Metadata["continuity_eligible"])
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
