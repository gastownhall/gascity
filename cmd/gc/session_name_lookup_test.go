package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

func TestResolvedTemplateForIdentity_ResolvesUniqueInBoundsLegacyLocalPoolIdentity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(5)},
			{Name: "worker", Dir: "backend", MaxActiveSessions: intPtr(1)},
		},
	}

	if got := resolvedTemplateForIdentity("worker-5", cfg); got != "frontend/worker" {
		t.Fatalf("resolvedTemplateForIdentity(worker-5) = %q, want %q", got, "frontend/worker")
	}
}

func TestResolvedTemplateForIdentity_DoesNotResolveAmbiguousLegacyLocalPoolIdentity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(5)},
			{Name: "worker", Dir: "backend", MaxActiveSessions: intPtr(5)},
		},
	}

	if got := resolvedTemplateForIdentity("worker-7", cfg); got != "" {
		t.Fatalf("resolvedTemplateForIdentity(worker-7) = %q, want unresolved ambiguity", got)
	}
}

func TestResolvedTemplateForIdentity_DoesNotResolveZeroCapacityLocalIdentity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(0)},
		},
	}

	if got := resolvedTemplateForIdentity("worker-1", cfg); got != "" {
		t.Fatalf("resolvedTemplateForIdentity(worker-1) = %q, want zero-capacity template to stay unresolved", got)
	}
}

func TestResolvedTemplateForIdentity_DoesNotResolveOutOfBoundsQualifiedPoolIdentity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(5)},
		},
	}

	if got := resolvedTemplateForIdentity("frontend/worker-7", cfg); got != "" {
		t.Fatalf("resolvedTemplateForIdentity(frontend/worker-7) = %q, want unresolved out-of-bounds identity", got)
	}
}
