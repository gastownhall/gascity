package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
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
		case "list":
			return []byte(`[]`), nil
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
	wantSessionName := poolIdentitySessionName("gascity/claude", "gascity/claude")
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

func TestCreatePoolSessionBeadWithAlias_WritesResolvedAliasInTheExplicitIDCreate(t *testing.T) {
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
		case "list":
			return []byte(`[]`), nil
		case "update":
			return nil, fmt.Errorf("unexpected bd update: the resolved session_name must land in the create write: %v", args)
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
	if got := created.Metadata["session_name"]; got != "crew--gastown" {
		t.Fatalf("created metadata session_name = %q, want crew--gastown", got)
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

func TestCreatePoolSessionBeadWithAlias_FallsBackToPoolIdentityNameWhenAliasEmpty(t *testing.T) {
	store := beads.NewMemStore()

	bead, err := createPoolSessionBeadWithAlias(store, "claude", nil, nil, time.Now().UTC(), poolSessionCreateIdentity{}, "")
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	want := poolIdentitySessionName("", "claude")
	if got := bead.SessionNameMetadata; got != want {
		t.Fatalf("session_name = %q, want %q (pool identity fallback)", got, want)
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

// The four tests below used to assert that a name collision minted
// "<name>-<beadID>". That fallback is the ga-vcjr9 leak: the suffixed name is a
// fresh runtime identity, so a pool whose start keeps failing provisions a new
// sandbox box per attempt and abandons the previous one. Each now asserts the
// fail-closed contract instead — the create is refused with a typed error and
// leaves no bead behind, so the slot retries next tick against the same name.

func TestCreatePoolSessionBeadWithAlias_FailsClosedOnSnapshotCollision(t *testing.T) {
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
	if err == nil {
		t.Fatalf("second createPoolSessionBeadWithAlias returned %q; want errPoolSessionNameUnavailable", second.SessionNameMetadata)
	}
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("second createPoolSessionBeadWithAlias error = %v, want errPoolSessionNameUnavailable", err)
	}
	assertOnlySessionBead(t, store, first.ID)
}

func TestCreatePoolSessionBeadWithAlias_FailsClosedOnOutOfSnapshotLiveCollision(t *testing.T) {
	store := beads.NewMemStore()
	existing, err := store.Create(beads.Bead{
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

	_, err = createPoolSessionBeadWithAlias(store, "crew-gastown", nil, newSessionBeadSnapshot(nil), time.Now().UTC(), poolSessionCreateIdentity{}, "crew--gastown")
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("createPoolSessionBeadWithAlias error = %v, want errPoolSessionNameUnavailable for a live out-of-snapshot collision", err)
	}
	assertOnlySessionBead(t, store, existing.ID)
}

func TestCreatePoolSessionBeadWithAlias_FailsClosedOnClosedSessionNameCollision(t *testing.T) {
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

	_, err = createPoolSessionBeadWithAlias(store, "crew-gastown", nil, newSessionBeadSnapshot(nil), time.Now().UTC(), poolSessionCreateIdentity{}, "crew--gastown")
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("createPoolSessionBeadWithAlias error = %v, want errPoolSessionNameUnavailable for a closed session-name collision", err)
	}
	if _, getErr := store.Get(existing.ID); getErr != nil {
		t.Fatalf("store.Get(%s): %v", existing.ID, getErr)
	}
	assertOnlySessionBead(t, store)
}

func TestCreatePoolSessionBeadWithAlias_FailsClosedOnConfiguredNamedSessionReservation(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		NamedSessions: []config.NamedSession{{
			Name:     "crew",
			Template: "worker",
		}},
	}
	reserved := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "crew")

	_, err := createPoolSessionBeadWithAlias(store, "worker", cfg, newSessionBeadSnapshot(nil), time.Now().UTC(), poolSessionCreateIdentity{}, reserved)
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("createPoolSessionBeadWithAlias error = %v, want errPoolSessionNameUnavailable for a foreign configured named-session reservation", err)
	}
	assertOnlySessionBead(t, store)
}

// assertOnlySessionBead fails unless the store's OPEN session beads are exactly
// the given ones. A refused create must not leave a bead — and therefore a
// runtime name — behind.
func assertOnlySessionBead(t *testing.T, store beads.Store, want ...string) {
	t.Helper()
	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, err)
	}
	got := make([]string, 0, len(all))
	for _, b := range all {
		got = append(got, b.ID)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("session beads = %v, want %v", got, want)
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
		identity string
		slot     int
		alias    string
		snapshot *sessionBeadSnapshot
		want     string
	}{
		{
			name:     "empty alias falls back to the pool identity",
			template: "claude",
			identity: "claude-1",
			alias:    "",
			snapshot: nil,
			want:     "claude-1",
		},
		{
			name:     "whitespace-only alias falls back to the pool identity",
			template: "claude",
			identity: "claude-1",
			alias:    "   ",
			snapshot: nil,
			want:     "claude-1",
		},
		{
			name:     "identity is sanitized for tmux",
			template: "gastown/bd.dog",
			identity: "gastown/bd.dog-1",
			alias:    "",
			snapshot: nil,
			want:     "gastown--bd__dog-1",
		},
		{
			name:     "empty identity falls back to the template basename",
			template: "gastown/bd.dog",
			identity: "",
			alias:    "",
			snapshot: nil,
			want:     "bd__dog",
		},
		{
			name:     "resolved alias wins over the identity",
			template: "crew-gastown",
			identity: "crew-gastown-1",
			alias:    "crew--gastown",
			snapshot: nil,
			want:     "crew--gastown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := derivePoolSessionName(tt.template, poolSessionCreateIdentity{AgentName: tt.identity, Slot: tt.slot}, tt.alias, tt.snapshot)
			if err != nil {
				t.Fatalf("derivePoolSessionName: %v", err)
			}
			if got != tt.want {
				t.Fatalf("derivePoolSessionName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDerivePoolSessionNameFailsClosedOnSnapshotCollision(t *testing.T) {
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		ID:     "existing",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "claude-1",
		},
	}})

	_, err := derivePoolSessionName("claude", poolSessionCreateIdentity{AgentName: "claude-1", Slot: 1}, "", snapshot)
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("derivePoolSessionName error = %v, want errPoolSessionNameUnavailable", err)
	}
}

// TestPoolIdentitySessionNameShortensDeterministically pins the length bound:
// an identity longer than an explicit session name may be must still map to one
// stable name, and two long identities must not collapse onto the same one.
func TestPoolIdentitySessionNameShortensDeterministically(t *testing.T) {
	long := strings.Repeat("a", 80)
	first := poolIdentitySessionName(long+"-1", "claude")
	if first != poolIdentitySessionName(long+"-1", "claude") {
		t.Fatal("poolIdentitySessionName is not deterministic for a shortened identity")
	}
	if len(first) > session.MaxExplicitSessionNameLen {
		t.Fatalf("shortened name %q is %d chars, max %d", first, len(first), session.MaxExplicitSessionNameLen)
	}
	if _, err := session.ValidateExplicitName(first); err != nil {
		t.Fatalf("shortened name %q is not a valid explicit session name: %v", first, err)
	}
	if second := poolIdentitySessionName(long+"-2", "claude"); second == first {
		t.Fatalf("two distinct long identities shortened to the same name %q", first)
	}
}

// TestDerivePoolSessionNameBoundsAliasedSlotSuffix pins the length-limit
// boundary on the aliased multi-slot lane: a tmux_alias valid at exactly the
// explicit-name limit must not be locked out of creation once
// derivePoolSessionName appends "-<slot>" for a higher slot. Before the suffix
// was folded into the deterministic shortening, alias+"-2" overflowed
// MaxExplicitSessionNameLen and failed ValidateExplicitName, so the slot could
// never create. The final name must shorten to a valid explicit name, and
// distinct slots must still map to distinct boxes.
func TestDerivePoolSessionNameBoundsAliasedSlotSuffix(t *testing.T) {
	alias := strings.Repeat("a", session.MaxExplicitSessionNameLen) // valid exactly at the limit
	if _, err := session.ValidateExplicitName(alias); err != nil {
		t.Fatalf("precondition: %d-char alias should be a valid explicit name: %v", len(alias), err)
	}

	slot2, err := derivePoolSessionName("claude", poolSessionCreateIdentity{AgentName: "claude-2", Slot: 2}, alias, nil)
	if err != nil {
		t.Fatalf("derivePoolSessionName(slot 2): %v", err)
	}
	if len(slot2) > session.MaxExplicitSessionNameLen {
		t.Fatalf("slot-2 name %q is %d chars, max %d", slot2, len(slot2), session.MaxExplicitSessionNameLen)
	}
	if _, err := session.ValidateExplicitName(slot2); err != nil {
		t.Fatalf("slot-2 name %q is not a valid explicit session name: %v", slot2, err)
	}

	again, err := derivePoolSessionName("claude", poolSessionCreateIdentity{AgentName: "claude-2", Slot: 2}, alias, nil)
	if err != nil {
		t.Fatalf("derivePoolSessionName(slot 2 again): %v", err)
	}
	if again != slot2 {
		t.Fatalf("derivePoolSessionName is not deterministic for a shortened aliased slot: %q vs %q", slot2, again)
	}

	slot3, err := derivePoolSessionName("claude", poolSessionCreateIdentity{AgentName: "claude-3", Slot: 3}, alias, nil)
	if err != nil {
		t.Fatalf("derivePoolSessionName(slot 3): %v", err)
	}
	if slot3 == slot2 {
		t.Fatalf("boundary-length slots 2 and 3 collapsed onto the same name %q", slot2)
	}
}

// TestPoolRuntimeSessionNameStepsAsideForTransientSlot pins the #5241 identity
// invariant at the derivation boundary. A transient pool slot ("pooled-1") is a
// rebinding chair, not an occupant, so it must never become the runtime session
// name: clearPoolTemplateRuntimeIdentity puts GC_AGENT on the session name, and
// if that name is the bare slot the transient slot leaks straight into the
// identity channel — the sibling of the ga-vcjr9 pod churn, guarded end-to-end
// by TestE2E_MultiAgent_PoolAndFixed. The transient step-aside reuses the
// existing "-pool" suffix: stable per slot, distinct from the slot, distinct per
// slot, non-empty. A non-transient slot keeps the bare identity-derived name, so
// namepool and canonical-singleton pools are unaffected.
func TestPoolRuntimeSessionNameStepsAsideForTransientSlot(t *testing.T) {
	const slot1 = "pooled-1"
	got := poolRuntimeSessionName(nil, slot1, "pooled", true)
	if got == slot1 {
		t.Fatalf("transient slot runtime name %q must differ from the bare slot %q", got, slot1)
	}
	if got == "" {
		t.Fatal("transient slot runtime name must be non-empty")
	}
	if want := slot1 + poolRuntimeNameSuffix; got != want {
		t.Fatalf("poolRuntimeSessionName(transient) = %q, want %q", got, want)
	}
	if again := poolRuntimeSessionName(nil, slot1, "pooled", true); again != got {
		t.Fatalf("poolRuntimeSessionName(transient) is not deterministic: %q vs %q", got, again)
	}
	if slot2 := poolRuntimeSessionName(nil, "pooled-2", "pooled", true); slot2 == got {
		t.Fatalf("distinct transient slots collapsed onto the same runtime name %q", got)
	}
	if plain := poolRuntimeSessionName(nil, slot1, "pooled", false); plain != slot1 {
		t.Fatalf("non-transient slot runtime name = %q, want bare identity %q", plain, slot1)
	}
}

// TestDerivePoolSessionNameStepsAsideForTransientSlot pins the create-path end of
// the same invariant: derivePoolSessionName must carry the identity's
// TransientSlot flag into poolRuntimeSessionName so the persisted session_name
// (and therefore GC_AGENT) is never the bare slot.
func TestDerivePoolSessionNameStepsAsideForTransientSlot(t *testing.T) {
	got, err := derivePoolSessionName("pooled", poolSessionCreateIdentity{AgentName: "pooled-1", Slot: 1, TransientSlot: true}, "", nil)
	if err != nil {
		t.Fatalf("derivePoolSessionName: %v", err)
	}
	if got == "pooled-1" {
		t.Fatalf("transient slot session_name %q must differ from the bare slot", got)
	}
	if want := "pooled-1" + poolRuntimeNameSuffix; got != want {
		t.Fatalf("derivePoolSessionName(transient) = %q, want %q", got, want)
	}
}

// TestPoolRuntimeSessionNameBoundsPoolSuffix pins the length-limit boundary on
// the named-session step-aside lane: when a pool instance's identity name is
// valid at exactly the explicit-name limit AND collides with a configured named
// session's reserved runtime name, poolRuntimeSessionName appends "-pool" to
// step aside. Before the suffix was folded into the deterministic shortening,
// name+"-pool" overflowed MaxExplicitSessionNameLen and the unaliased pool
// create then failed ValidateExplicitName. The stepped-aside name must shorten
// to a valid explicit name and must not land back on the reserved name.
func TestPoolRuntimeSessionNameBoundsPoolSuffix(t *testing.T) {
	identity := strings.Repeat("a", session.MaxExplicitSessionNameLen) // valid exactly at the limit
	cfg := &config.City{
		NamedSessions: []config.NamedSession{{Name: identity, Template: "claude"}},
	}
	base := poolIdentitySessionName(identity, "claude")
	if !configuredNamedSessionReservesRuntimeName(cfg, base) {
		t.Fatalf("precondition: expected named session to reserve pool identity name %q", base)
	}

	got := poolRuntimeSessionName(cfg, identity, "claude", false)
	if len(got) > session.MaxExplicitSessionNameLen {
		t.Fatalf("stepped-aside name %q is %d chars, max %d", got, len(got), session.MaxExplicitSessionNameLen)
	}
	if _, err := session.ValidateExplicitName(got); err != nil {
		t.Fatalf("stepped-aside name %q is not a valid explicit session name: %v", got, err)
	}
	if got == base {
		t.Fatalf("poolRuntimeSessionName did not step aside from reserved name %q", base)
	}
	if again := poolRuntimeSessionName(cfg, identity, "claude", false); again != got {
		t.Fatalf("poolRuntimeSessionName is not deterministic: %q vs %q", got, again)
	}
}
