package session

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type rejectLegacyWaitTypeQueryStore struct {
	*beads.MemStore
}

func (s rejectLegacyWaitTypeQueryStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == LegacyWaitBeadType {
		return nil, errors.New("legacy wait type query should not be used")
	}
	return s.MemStore.List(query)
}

func TestWaitNudgeIDs_AcceptsLegacyWaitBeadsWithoutLegacyTypeQuery(t *testing.T) {
	store := rejectLegacyWaitTypeQueryStore{MemStore: beads.NewMemStore()}
	if _, err := store.Create(beads.Bead{
		Type:   LegacyWaitBeadType,
		Labels: []string{WaitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{
			"session_id": "gc-session",
			"state":      "pending",
			"nudge_id":   "wait-nudge",
		},
	}); err != nil {
		t.Fatalf("create legacy wait: %v", err)
	}

	got, err := WaitNudgeIDs(store, "gc-session")
	if err != nil {
		t.Fatalf("WaitNudgeIDs: %v", err)
	}
	if len(got) != 1 || got[0] != "wait-nudge" {
		t.Fatalf("WaitNudgeIDs = %#v, want [wait-nudge]", got)
	}
}

func TestCancelWaits_CancelsLegacyWaitBeadsWithoutLegacyTypeQuery(t *testing.T) {
	store := rejectLegacyWaitTypeQueryStore{MemStore: beads.NewMemStore()}
	wait, err := store.Create(beads.Bead{
		Type:   LegacyWaitBeadType,
		Labels: []string{WaitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{
			"session_id": "gc-session",
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create legacy wait: %v", err)
	}

	if err := CancelWaits(store, "gc-session", time.Now().UTC()); err != nil {
		t.Fatalf("CancelWaits: %v", err)
	}
	updated, err := store.Get(wait.ID)
	if err != nil {
		t.Fatalf("Get(wait): %v", err)
	}
	if updated.Metadata["state"] != waitStateCanceled {
		t.Fatalf("state = %q, want %q", updated.Metadata["state"], waitStateCanceled)
	}
	if updated.Status != "closed" {
		t.Fatalf("status = %q, want closed", updated.Status)
	}
}

func TestWakeSessionRequestsStartForSuspendedBead(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 5, 3, 8, 30, 0, 0, time.UTC)
	sessionBead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":        string(StateSuspended),
			"state_reason": "user-hold",
			"held_until":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"wait_hold":    "true",
			"sleep_reason": "user-hold",
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := WakeSession(store, sessionBead, now); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := updated.Metadata["state"]; got != string(StateCreating) {
		t.Fatalf("state = %q, want creating", got)
	}
	if got := updated.Metadata["state_reason"]; got != string(WakeCauseExplicit) {
		t.Fatalf("state_reason = %q, want explicit", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
	if got, want := updated.Metadata["pending_create_started_at"], now.UTC().Format(time.RFC3339); got != want {
		t.Fatalf("pending_create_started_at = %q, want %q", got, want)
	}
	for _, key := range []string{"held_until", "wait_hold", "sleep_reason"} {
		if got := updated.Metadata[key]; got != "" {
			t.Fatalf("%s = %q, want cleared", key, got)
		}
	}
}

func TestWakeSessionRejectsArchivedHistoricalBead(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":               "archived",
			"continuity_eligible": "false",
			"held_until":          time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	wait, err := store.Create(beads.Bead{
		Type:   WaitBeadType,
		Labels: []string{WaitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id": sessionBead.ID,
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}

	if _, err := WakeSession(store, sessionBead, time.Now().UTC()); err == nil {
		t.Fatal("WakeSession returned nil error, want archived-session rejection")
	}

	updatedSession, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := updatedSession.Metadata["held_until"]; got == "" {
		t.Fatal("held_until was cleared on rejected archived wake")
	}
	updatedWait, err := store.Get(wait.ID)
	if err != nil {
		t.Fatalf("Get(wait): %v", err)
	}
	if updatedWait.Status == "closed" || updatedWait.Metadata["state"] == waitStateCanceled {
		t.Fatalf("wait was canceled on rejected archived wake: status=%q state=%q", updatedWait.Status, updatedWait.Metadata["state"])
	}
}

func TestWakeSessionRequestsStartForContinuityEligibleArchivedBead(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 5, 3, 8, 45, 0, 0, time.UTC)
	sessionBead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":                "archived",
			"state_reason":         "quarantine-recovery",
			"continuity_eligible":  "true",
			"archived_at":          time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			"held_until":           time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"quarantined_until":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"wait_hold":            "true",
			"sleep_intent":         "wait-hold",
			"sleep_reason":         "quarantine",
			"pending_create_claim": "",
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	wait, err := store.Create(beads.Bead{
		Type:   WaitBeadType,
		Labels: []string{WaitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id": sessionBead.ID,
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}

	if _, err := WakeSession(store, sessionBead, now); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := updated.Metadata["state"]; got != string(StateCreating) {
		t.Fatalf("state = %q, want creating", got)
	}
	if got := updated.Metadata["state_reason"]; got != "explicit" {
		t.Fatalf("state_reason = %q, want explicit", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
	if got, want := updated.Metadata["pending_create_started_at"], now.UTC().Format(time.RFC3339); got != want {
		t.Fatalf("pending_create_started_at = %q, want %q", got, want)
	}
	for _, key := range []string{"held_until", "quarantined_until", "wait_hold", "sleep_intent", "sleep_reason", "archived_at"} {
		if got := updated.Metadata[key]; got != "" {
			t.Fatalf("%s = %q, want cleared", key, got)
		}
	}
	updatedWait, err := store.Get(wait.ID)
	if err != nil {
		t.Fatalf("Get(wait): %v", err)
	}
	if updatedWait.Status != "closed" || updatedWait.Metadata["state"] != waitStateCanceled {
		t.Fatalf("wait status/state = %q/%q, want closed/canceled", updatedWait.Status, updatedWait.Metadata["state"])
	}
}
