package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCreatePoolSessionBead_SetsPendingCreateClaim(t *testing.T) {
	store := beads.NewMemStore()

	bead, err := createPoolSessionBead(store, "gascity/claude", nil)
	if err != nil {
		t.Fatalf("createPoolSessionBead: %v", err)
	}

	if got := bead.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}

	stored, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got := stored.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("stored pending_create_claim = %q, want true", got)
	}
}
