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

	bead, err := createPoolSessionBead(store, "gascity/claude", nil, now, poolSessionCreateIdentity{})
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

func TestExistingPoolSlotWithConfig_PrefersConcreteAgentIdentityOverStaleSlot(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(10)},
			{Name: "worker", Dir: "backend", MaxActiveSessions: intPtr(10)},
		},
	}
	cfgAgent := &cfg.Agents[0]
	bead := beads.Bead{
		Metadata: map[string]string{
			"template":   "frontend/worker",
			"agent_name": "frontend/worker-3",
			"alias":      "backend/worker-4",
			"pool_slot":  "4",
		},
	}

	if got := existingPoolSlotWithConfig(cfg, cfgAgent, bead); got != 3 {
		t.Fatalf("existingPoolSlotWithConfig = %d, want concrete agent slot 3 over stale slot/foreign alias", got)
	}
}

func TestExistingPoolSlot_CanonicalSingletonReturnsZero(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "refinery",
			Dir:               "cashmaster",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "printf 1",
		}},
	}
	cfgAgent := &cfg.Agents[0]
	bead := beads.Bead{
		Metadata: map[string]string{
			"template":   "cashmaster/refinery",
			"agent_name": "cashmaster/refinery-1",
			"alias":      "cashmaster/refinery-1",
			"pool_slot":  "1",
		},
	}

	if got := existingPoolSlot(cfgAgent, bead); got != 0 {
		t.Fatalf("existingPoolSlot(canonical singleton) = %d, want 0", got)
	}
	if got := existingPoolSlotWithConfig(cfg, cfgAgent, bead); got != 0 {
		t.Fatalf("existingPoolSlotWithConfig(canonical singleton) = %d, want 0", got)
	}
}

func TestCreatePoolSessionBeadWithAlias_FallsBackToPoolNameWhenAliasEmpty(t *testing.T) {
	store := beads.NewMemStore()

	bead, err := createPoolSessionBeadWithAlias(store, "claude", nil, time.Now().UTC(), poolSessionCreateIdentity{}, "")
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	want := PoolSessionName("claude", bead.ID)
	if got := bead.Metadata["session_name"]; got != want {
		t.Fatalf("session_name = %q, want %q (universal fallback)", got, want)
	}
}

func TestCreatePoolSessionBeadWithAlias_UsesResolvedAlias(t *testing.T) {
	store := beads.NewMemStore()

	bead, err := createPoolSessionBeadWithAlias(store, "crew-gastown", nil, time.Now().UTC(), poolSessionCreateIdentity{}, "crew--gastown")
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	if got := bead.Metadata["session_name"]; got != "crew--gastown" {
		t.Fatalf("session_name = %q, want %q (resolved alias wins)", got, "crew--gastown")
	}
	stored, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got := stored.Metadata["session_name"]; got != "crew--gastown" {
		t.Fatalf("stored session_name = %q, want %q", got, "crew--gastown")
	}
}

func TestCreatePoolSessionBeadWithAlias_AppendsBeadIDOnCollision(t *testing.T) {
	store := beads.NewMemStore()
	snapshot := newSessionBeadSnapshot(nil)

	first, err := createPoolSessionBeadWithAlias(store, "crew-gastown", snapshot, time.Now().UTC(), poolSessionCreateIdentity{}, "crew--gastown")
	if err != nil {
		t.Fatalf("first createPoolSessionBeadWithAlias: %v", err)
	}
	if got := first.Metadata["session_name"]; got != "crew--gastown" {
		t.Fatalf("first session_name = %q, want %q", got, "crew--gastown")
	}

	second, err := createPoolSessionBeadWithAlias(store, "crew-gastown", snapshot, time.Now().UTC(), poolSessionCreateIdentity{}, "crew--gastown")
	if err != nil {
		t.Fatalf("second createPoolSessionBeadWithAlias: %v", err)
	}
	want := "crew--gastown-" + second.ID
	if got := second.Metadata["session_name"]; got != want {
		t.Fatalf("second session_name = %q, want %q (collision suffix)", got, want)
	}
}

func TestDerivePoolSessionName(t *testing.T) {
	tests := []struct {
		name     string
		template string
		beadID   string
		alias    string
		snapshot *sessionBeadSnapshot
		want     string
	}{
		{
			name:     "empty alias falls back to PoolSessionName",
			template: "claude",
			beadID:   "gc-1",
			alias:    "",
			snapshot: nil,
			want:     "claude-gc-1",
		},
		{
			name:     "whitespace-only alias falls back to PoolSessionName",
			template: "claude",
			beadID:   "gc-1",
			alias:    "   ",
			snapshot: nil,
			want:     "claude-gc-1",
		},
		{
			name:     "resolved alias wins when no collision",
			template: "crew-gastown",
			beadID:   "gc-2",
			alias:    "crew--gastown",
			snapshot: nil,
			want:     "crew--gastown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := derivePoolSessionName(tt.template, tt.beadID, tt.alias, tt.snapshot)
			if got != tt.want {
				t.Fatalf("derivePoolSessionName = %q, want %q", got, tt.want)
			}
		})
	}
}
