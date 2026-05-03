package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCreatePoolSessionBead_SetsPendingCreateClaim(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 5, 1, 9, 15, 0, 0, time.UTC)

	bead, err := createPoolSessionBead(store, "gascity/claude", nil, now)
	if err != nil {
		t.Fatalf("createPoolSessionBead: %v", err)
	}

	if got := bead.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
	if got, want := bead.Metadata["pending_create_started_at"], pendingCreateStartedAtNow(now); got != want {
		t.Fatalf("pending_create_started_at = %q, want %q", got, want)
	}

	stored, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got := stored.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("stored pending_create_claim = %q, want true", got)
	}
	if got, want := stored.Metadata["pending_create_started_at"], pendingCreateStartedAtNow(now); got != want {
		t.Fatalf("stored pending_create_started_at = %q, want %q", got, want)
	}
}
