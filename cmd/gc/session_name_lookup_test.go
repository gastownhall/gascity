package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestCreatePoolSessionBead_SetsPendingCreateClaim(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 5, 1, 9, 15, 0, 0, time.UTC)

	bead, err := createPoolSessionBead(sessionFrontDoor(store), "gascity/claude", now, poolSessionCreateIdentity{})
	if err != nil {
		t.Fatalf("createPoolSessionBead: %v", err)
	}

	if got := bead.PendingCreateClaimMetadata; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
	if got, want := bead.PendingCreateStartedAt, pendingCreateStartedAtNow(now); got != want {
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

func TestCreatePoolSessionBead_UsesExplicitIDThroughCachingStore(t *testing.T) {
	var created beads.Bead
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		switch args[0] {
		case "create":
			id := testFlagValue(args, "--id")
			if id == "" {
				return nil, fmt.Errorf("bd create missing explicit --id: %v", args)
			}
			var metadata map[string]string
			if raw := testFlagValue(args, "--metadata"); raw != "" {
				if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
					return nil, err
				}
			}
			created = beads.Bead{
				ID:        id,
				Title:     args[2],
				Status:    "open",
				Type:      "task",
				CreatedAt: nowForBDJSONTest(),
				Metadata:  metadata,
			}
			return mustMarshalBDIssueJSON(t, created, false), nil
		case "show":
			return mustMarshalBDIssueJSON(t, created, true), nil
		case "update":
			return nil, fmt.Errorf("unexpected bd update after explicit create: %v", args)
		default:
			return nil, fmt.Errorf("unexpected bd args: %v", args)
		}
	}
	backing := beads.NewBdStoreWithPrefix(t.TempDir(), runner, "mc")
	store := beads.NewCachingStore(backing, nil)
	now := time.Date(2026, 5, 1, 9, 15, 0, 0, time.UTC)

	bead, err := createPoolSessionBead(sessionFrontDoor(store), "gascity/claude", now, poolSessionCreateIdentity{})
	if err != nil {
		t.Fatalf("createPoolSessionBead: %v", err)
	}
	if !strings.HasPrefix(bead.ID, "mc-session-") {
		t.Fatalf("bead.ID = %q, want explicit mc-session-* ID", bead.ID)
	}
	wantSessionName := PoolSessionName("gascity/claude", bead.ID)
	if got := bead.SessionNameMetadata; got != wantSessionName {
		t.Fatalf("session_name = %q, want %q", got, wantSessionName)
	}

	stored, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("backing.Get(%s): %v", bead.ID, err)
	}
	if got := stored.Metadata["session_name"]; got != wantSessionName {
		t.Fatalf("stored session_name = %q, want %q", got, wantSessionName)
	}
}

func TestCreatePoolSessionBeadWithAlias_UpdatesExplicitIDBeadToResolvedAlias(t *testing.T) {
	var created beads.Bead
	var updatedMetadata string
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		switch args[0] {
		case "create":
			id := testFlagValue(args, "--id")
			if id == "" {
				return nil, fmt.Errorf("bd create missing explicit --id: %v", args)
			}
			var metadata map[string]string
			if raw := testFlagValue(args, "--metadata"); raw != "" {
				if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
					return nil, err
				}
			}
			created = beads.Bead{
				ID:        id,
				Title:     args[2],
				Status:    "open",
				Type:      "task",
				CreatedAt: nowForBDJSONTest(),
				Metadata:  metadata,
			}
			return mustMarshalBDIssueJSON(t, created, false), nil
		case "show":
			return mustMarshalBDIssueJSON(t, created, true), nil
		case "list":
			return []byte(`[]`), nil
		case "update":
			if got := args[2]; got != created.ID {
				return nil, fmt.Errorf("bd update id = %q, want %q", got, created.ID)
			}
			updatedMetadata = testFlagValue(args, "--set-metadata")
			if updatedMetadata == "" {
				return nil, fmt.Errorf("bd update missing --set-metadata: %v", args)
			}
			key, value, ok := strings.Cut(updatedMetadata, "=")
			if !ok || key != "session_name" {
				return nil, fmt.Errorf("bd update metadata = %q, want session_name=<alias>", updatedMetadata)
			}
			created.Metadata[key] = value
			return []byte(`{"id":"` + created.ID + `"}`), nil
		default:
			return nil, fmt.Errorf("unexpected bd args: %v", args)
		}
	}
	backing := beads.NewBdStoreWithPrefix(t.TempDir(), runner, "mc")
	store := beads.NewCachingStore(backing, nil)
	now := time.Date(2026, 5, 1, 9, 15, 0, 0, time.UTC)

	bead, err := createPoolSessionBeadWithAlias(store, "crew-gastown", nil, nil, now, poolSessionCreateIdentity{}, "crew--gastown")
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	if !strings.HasPrefix(bead.ID, "mc-session-") {
		t.Fatalf("bead.ID = %q, want explicit mc-session-* ID", bead.ID)
	}
	if updatedMetadata != "session_name=crew--gastown" {
		t.Fatalf("updated metadata = %q, want session_name=crew--gastown", updatedMetadata)
	}
	if got := bead.SessionNameMetadata; got != "crew--gastown" {
		t.Fatalf("session_name = %q, want resolved alias", got)
	}

	stored, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("backing.Get(%s): %v", bead.ID, err)
	}
	if got := stored.Metadata["session_name"]; got != "crew--gastown" {
		t.Fatalf("stored session_name = %q, want resolved alias", got)
	}
}

func testFlagValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func nowForBDJSONTest() time.Time {
	return time.Date(2026, 5, 1, 9, 15, 0, 0, time.UTC)
}

func mustMarshalBDIssueJSON(t *testing.T, bead beads.Bead, list bool) []byte {
	t.Helper()
	item := map[string]any{
		"id":         bead.ID,
		"title":      bead.Title,
		"status":     bead.Status,
		"issue_type": bead.Type,
		"created_at": bead.CreatedAt.Format(time.RFC3339),
		"metadata":   bead.Metadata,
	}
	var v any = item
	if list {
		v = []any{item}
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
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

	bead, err := createPoolSessionBeadWithAlias(store, "claude", nil, nil, time.Now().UTC(), poolSessionCreateIdentity{}, "")
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	want := PoolSessionName("claude", bead.ID)
	if got := bead.SessionNameMetadata; got != want {
		t.Fatalf("session_name = %q, want %q (universal fallback)", got, want)
	}
}

func TestCreatePoolSessionBeadWithAlias_UsesResolvedAlias(t *testing.T) {
	store := beads.NewMemStore()

	bead, err := createPoolSessionBeadWithAlias(store, "crew-gastown", nil, nil, time.Now().UTC(), poolSessionCreateIdentity{}, "crew--gastown")
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	if got := bead.SessionNameMetadata; got != "crew--gastown" {
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

	first, err := createPoolSessionBeadWithAlias(store, "crew-gastown", nil, snapshot, time.Now().UTC(), poolSessionCreateIdentity{}, "crew--gastown")
	if err != nil {
		t.Fatalf("first createPoolSessionBeadWithAlias: %v", err)
	}
	if got := first.SessionNameMetadata; got != "crew--gastown" {
		t.Fatalf("first session_name = %q, want %q", got, "crew--gastown")
	}

	second, err := createPoolSessionBeadWithAlias(store, "crew-gastown", nil, snapshot, time.Now().UTC(), poolSessionCreateIdentity{}, "crew--gastown")
	if err != nil {
		t.Fatalf("second createPoolSessionBeadWithAlias: %v", err)
	}
	want := "crew--gastown-" + second.ID
	if got := second.SessionNameMetadata; got != want {
		t.Fatalf("second session_name = %q, want %q (collision suffix)", got, want)
	}
}

func TestCreatePoolSessionBeadWithAlias_AppendsBeadIDForOutOfSnapshotLiveCollision(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Title:  "manual session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "crew--gastown",
		},
	})
	if err != nil {
		t.Fatalf("create existing session bead: %v", err)
	}

	bead, err := createPoolSessionBeadWithAlias(store, "crew-gastown", nil, newSessionBeadSnapshot(nil), time.Now().UTC(), poolSessionCreateIdentity{}, "crew--gastown")
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	want := "crew--gastown-" + bead.ID
	if got := bead.SessionNameMetadata; got != want {
		t.Fatalf("session_name = %q, want %q for live out-of-snapshot collision", got, want)
	}
}

func TestCreatePoolSessionBeadWithAlias_AppendsBeadIDForClosedSessionNameCollision(t *testing.T) {
	store := beads.NewMemStore()
	existing, err := store.Create(beads.Bead{
		Title:  "closed session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "crew--gastown",
		},
	})
	if err != nil {
		t.Fatalf("create existing session bead: %v", err)
	}
	if err := store.Close(existing.ID); err != nil {
		t.Fatalf("close existing session bead: %v", err)
	}

	bead, err := createPoolSessionBeadWithAlias(store, "crew-gastown", nil, newSessionBeadSnapshot(nil), time.Now().UTC(), poolSessionCreateIdentity{}, "crew--gastown")
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	want := "crew--gastown-" + bead.ID
	if got := bead.SessionNameMetadata; got != want {
		t.Fatalf("session_name = %q, want %q for closed session-name collision", got, want)
	}
}

func TestCreatePoolSessionBeadWithAlias_AppendsBeadIDForConfiguredNamedSessionReservation(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		NamedSessions: []config.NamedSession{{
			Name:     "crew",
			Template: "worker",
		}},
	}
	reserved := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "crew")

	bead, err := createPoolSessionBeadWithAlias(store, "worker", cfg, newSessionBeadSnapshot(nil), time.Now().UTC(), poolSessionCreateIdentity{}, reserved)
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	want := reserved + "-" + bead.ID
	if got := bead.SessionNameMetadata; got != want {
		t.Fatalf("session_name = %q, want %q for configured named-session reservation", got, want)
	}
}

func TestCreatePoolSessionBeadWithAliasRejectsInvalidResolvedAlias(t *testing.T) {
	tests := []struct {
		name  string
		alias string
	}{
		{name: "reserved prefix", alias: "s-crew"},
		{name: "invalid syntax", alias: "crew demo"},
		{name: "too long", alias: strings.Repeat("a", 65)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			_, err := createPoolSessionBeadWithAlias(store, "crew", nil, nil, time.Now().UTC(), poolSessionCreateIdentity{}, tt.alias)
			if err == nil {
				t.Fatalf("createPoolSessionBeadWithAlias alias %q: want error", tt.alias)
			}
			stored, listErr := store.ListByLabel(sessionBeadLabel, 0)
			if listErr != nil {
				t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, listErr)
			}
			if len(stored) != 0 {
				t.Fatalf("stored session beads = %d, want none after rejected alias: %#v", len(stored), stored)
			}
		})
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
			got, err := derivePoolSessionName(nil, nil, tt.template, tt.beadID, tt.alias, tt.snapshot)
			if err != nil {
				t.Fatalf("derivePoolSessionName: %v", err)
			}
			if got != tt.want {
				t.Fatalf("derivePoolSessionName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDerivePoolSessionNameRejectsInvalidCollisionSuffix(t *testing.T) {
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		ID:     "existing",
		Status: "open",
		Metadata: map[string]string{
			"session_name": strings.Repeat("a", 64),
		},
	}})

	_, err := derivePoolSessionName(nil, nil, "crew", "gc-1", strings.Repeat("a", 64), snapshot)
	if err == nil {
		t.Fatal("derivePoolSessionName: want error when collision suffix would exceed explicit session name length")
	}
}
