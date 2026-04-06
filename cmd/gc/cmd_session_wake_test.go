package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSessionWake_ClearsMetadata(t *testing.T) {
	// Test the wake logic directly: clear held_until, quarantined_until,
	// wake_attempts, and sleep_reason via SetMetadataBatch.
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":          "worker",
			"held_until":        "9999-12-31T23:59:59Z",
			"quarantined_until": "9999-12-31T23:59:59Z",
			"wait_hold":         "true",
			"sleep_intent":      "wait-hold",
			"wake_attempts":     "5",
			"sleep_reason":      "wait-hold",
		},
	})

	// Simulate what cmdSessionWake does.
	batch := map[string]string{
		"held_until":        "",
		"quarantined_until": "",
		"wait_hold":         "",
		"sleep_intent":      "",
		"wake_attempts":     "0",
	}
	sr := b.Metadata["sleep_reason"]
	if sr == "user-hold" || sr == "wait-hold" || sr == "quarantine" {
		batch["sleep_reason"] = ""
	}
	if err := store.SetMetadataBatch(b.ID, batch); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	updated, _ := store.Get(b.ID)
	if got := updated.Metadata["held_until"]; got != "" {
		t.Errorf("held_until should be cleared, got %q", got)
	}
	if got := updated.Metadata["quarantined_until"]; got != "" {
		t.Errorf("quarantined_until should be cleared, got %q", got)
	}
	if got := updated.Metadata["wait_hold"]; got != "" {
		t.Errorf("wait_hold should be cleared, got %q", got)
	}
	if got := updated.Metadata["sleep_intent"]; got != "" {
		t.Errorf("sleep_intent should be cleared, got %q", got)
	}
	if got := updated.Metadata["wake_attempts"]; got != "0" {
		t.Errorf("wake_attempts should be 0, got %q", got)
	}
	if got := updated.Metadata["sleep_reason"]; got != "" {
		t.Errorf("sleep_reason should be cleared, got %q", got)
	}
}

func TestSessionWake_ClearsChurnQuarantine(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":          "worker",
			"quarantined_until": "9999-12-31T23:59:59Z",
			"churn_count":       "3",
			"sleep_reason":      "context-churn",
			"wake_attempts":     "5",
		},
	})

	if _, err := session.WakeSession(store, b, time.Now()); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}

	updated, _ := store.Get(b.ID)
	if got := updated.Metadata["quarantined_until"]; got != "" {
		t.Errorf("quarantined_until should be cleared, got %q", got)
	}
	if got := updated.Metadata["churn_count"]; got != "0" {
		t.Errorf("churn_count should be 0, got %q", got)
	}
	if got := updated.Metadata["sleep_reason"]; got != "" {
		t.Errorf("sleep_reason should be cleared, got %q", got)
	}
	if got := updated.Metadata["wake_attempts"]; got != "0" {
		t.Errorf("wake_attempts should be 0, got %q", got)
	}
}

func TestSessionWake_PreservesNonHoldSleepReason(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":     "worker",
			"sleep_reason": "idle",
		},
	})

	// wake should NOT clear sleep_reason when it's "idle" (not hold/quarantine).
	batch := map[string]string{
		"held_until":        "",
		"quarantined_until": "",
		"wait_hold":         "",
		"sleep_intent":      "",
		"wake_attempts":     "0",
	}
	sr := b.Metadata["sleep_reason"]
	if sr == "user-hold" || sr == "wait-hold" || sr == "quarantine" {
		batch["sleep_reason"] = ""
	}
	if err := store.SetMetadataBatch(b.ID, batch); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	updated, _ := store.Get(b.ID)
	if got := updated.Metadata["sleep_reason"]; got != "idle" {
		t.Errorf("sleep_reason should be preserved as 'idle', got %q", got)
	}
}
