package session

import (
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
	m := NewManager(store, sp)

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
	m := NewManager(store, sp)

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
	m := NewManager(store, sp)

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
	m := NewManager(store, sp)

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

func TestConformance_QuarantineReactivation(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManager(store, sp)

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
