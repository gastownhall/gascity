package main

import (
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// poolChurnIdentity is the pool instance identity used by the churn tests: a
// concrete slot on an expanding pool, exactly as poolDesiredRequestIdentity
// hands it to the create path.
func poolChurnIdentity() poolSessionCreateIdentity {
	return poolSessionCreateIdentity{AgentName: "gastown/bd.dog-1", Slot: 1}
}

const poolChurnTemplate = "gastown/bd.dog"

// TestPoolSessionCreate_FailedRetriesAddressOneRuntimeName is the ga-vcjr9
// regression pin. On cherry a pool whose start op failed every tick produced a
// NEW runtime session name per attempt (bd__dog-<beadID>), and because the
// runtime name is the sandbox pod name, 600+ pods for a pool whose desired
// count was 1.
//
// The loop modeled here is the measured one: create the pool session bead,
// fail the start, roll the bead back as failed_create (the reconciler's
// rollbackPendingCreateClears path), repeat. The invariant is that every
// attempt for the same pool identity addresses the SAME runtime box.
func TestPoolSessionCreate_FailedRetriesAddressOneRuntimeName(t *testing.T) {
	store := beads.NewMemStore()
	front := sessionFrontDoor(store)
	identity := poolChurnIdentity()

	seen := map[string]struct{}{}
	for attempt := 1; attempt <= 5; attempt++ {
		now := time.Date(2026, 8, 15, 0, 0, attempt, 0, time.UTC)
		// A fresh tick snapshot, loaded before the previous attempt's rollback
		// is visible to it — the production shape, and the one that catches a
		// fix whose retry stalls on its own dead predecessor.
		open, err := loadSessionBeads(store)
		if err != nil {
			t.Fatalf("attempt %d: loadSessionBeads: %v", attempt, err)
		}
		snapshot := newSessionBeadSnapshot(open)
		info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, snapshot, now, identity, "")
		if err != nil {
			t.Fatalf("attempt %d: createPoolSessionBeadWithAlias: %v", attempt, err)
		}
		name := strings.TrimSpace(info.SessionNameMetadata)
		if name == "" {
			t.Fatalf("attempt %d: empty session_name on %s", attempt, info.ID)
		}
		seen[name] = struct{}{}
		// The start op failed; the reconciler rolls the pending create back by
		// closing the bead as failed_create.
		if !closeFailedCreateBead(front, info.ID, now, io.Discard) {
			t.Fatalf("attempt %d: closeFailedCreateBead(%s) failed", attempt, info.ID)
		}
	}

	if len(seen) != 1 {
		t.Fatalf("5 failed create attempts minted %d distinct runtime session names %v; want 1 — every extra name is a leaked sandbox box (ga-vcjr9)", len(seen), sortedNameSet(seen))
	}
}

// TestPoolSessionCreate_NameIsIndependentOfBeadID is the direct statement of
// the structural invariant: the runtime name is a pure function of the
// configured pool identity, so no bead ID can appear in it.
func TestPoolSessionCreate_NameIsIndependentOfBeadID(t *testing.T) {
	store := beads.NewMemStore()
	identity := poolChurnIdentity()

	info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, nil, time.Now().UTC(), identity, "")
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if strings.Contains(name, info.ID) {
		t.Fatalf("session_name %q embeds bead ID %q; the runtime name must derive from the pool identity alone (ga-vcjr9)", name, info.ID)
	}
	if want := poolIdentitySessionName(identity.AgentName, poolChurnTemplate); name != want {
		t.Fatalf("session_name = %q, want %q (pool identity derivation)", name, want)
	}
}

// TestPoolSessionCreate_DistinctSlotsGetDistinctNames is the control for the
// test above: a fix that collapsed every pool session onto one name would pass
// the churn test and destroy the pool. Distinct slots must stay distinct.
func TestPoolSessionCreate_DistinctSlotsGetDistinctNames(t *testing.T) {
	store := beads.NewMemStore()

	names := map[string]struct{}{}
	for slot := 1; slot <= 3; slot++ {
		identity := poolSessionCreateIdentity{
			AgentName: "gastown/bd.dog-" + string(rune('0'+slot)),
			Slot:      slot,
		}
		info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, nil, time.Now().UTC(), identity, "")
		if err != nil {
			t.Fatalf("slot %d: createPoolSessionBeadWithAlias: %v", slot, err)
		}
		names[strings.TrimSpace(info.SessionNameMetadata)] = struct{}{}
	}
	if len(names) != 3 {
		t.Fatalf("3 distinct pool slots produced %d distinct session names %v; want 3", len(names), sortedNameSet(names))
	}
}

// TestPoolSessionCreate_LiveHolderKeepsItsNameAndBlocksTheSlot is the MANDATORY
// reverse control. Reusing a name is only safe when the previous holder is
// dead. While a live session bead holds the pool identity's runtime name, a
// fresh create must NOT hand that name out — and must NOT fall back to minting
// a bead-ID-suffixed sibling either, because that fallback is the leak.
//
// A fix that reuses the name over a living session is strictly worse than the
// leak it replaces: it points a second agent at a box that already has one.
func TestPoolSessionCreate_LiveHolderKeepsItsNameAndBlocksTheSlot(t *testing.T) {
	store := beads.NewMemStore()
	identity := poolChurnIdentity()

	live, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, nil, time.Now().UTC(), identity, "")
	if err != nil {
		t.Fatalf("live createPoolSessionBeadWithAlias: %v", err)
	}
	liveName := strings.TrimSpace(live.SessionNameMetadata)

	second, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, nil, time.Now().UTC(), identity, "")
	if err == nil {
		t.Fatalf("second create returned session_name %q while %s is live; want a typed unavailable error", second.SessionNameMetadata, live.ID)
	}
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("second create error = %v, want errPoolSessionNameUnavailable", err)
	}

	stored, getErr := store.Get(live.ID)
	if getErr != nil {
		t.Fatalf("store.Get(%s): %v", live.ID, getErr)
	}
	if got := strings.TrimSpace(stored.Metadata["session_name"]); got != liveName {
		t.Fatalf("live session_name = %q, want %q (a blocked create must not disturb the live holder)", got, liveName)
	}
	if stored.Status == "closed" {
		t.Fatalf("live session bead %s was closed by a blocked create", live.ID)
	}

	all, listErr := store.ListByLabel(sessionBeadLabel, 0)
	if listErr != nil {
		t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, listErr)
	}
	for _, b := range all {
		if b.ID == live.ID {
			continue
		}
		t.Fatalf("blocked create left session bead %s (session_name %q) behind; no suffixed sibling may be minted", b.ID, b.Metadata["session_name"])
	}
}

// TestPoolSessionCreate_ConfiguredNamedSessionOwnerReusesItsOwnName closes the
// selfOwner=="" gap. A pool that materializes a configured named session's
// identity must be allowed to claim that identity's reserved runtime name;
// otherwise the config reservation rejects it on every tick and — before the
// fix — the rejection was what minted a fresh suffixed name each time.
func TestPoolSessionCreate_ConfiguredNamedSessionOwnerReusesItsOwnName(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		NamedSessions: []config.NamedSession{{Name: "crew", Template: "worker"}},
	}
	reserved := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "crew")

	store := beads.NewMemStore()
	info, err := createPoolSessionBeadWithAlias(store, "worker", cfg, newSessionBeadSnapshot(nil), time.Now().UTC(),
		poolSessionCreateIdentity{AgentName: "crew"}, reserved)
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	if got := strings.TrimSpace(info.SessionNameMetadata); got != reserved {
		t.Fatalf("session_name = %q, want the reserved runtime name %q for its own configured owner", got, reserved)
	}
}

// TestPoolSessionCreate_ForeignConfiguredReservationStillBlocks is the control
// for the test above: passing the owner through must not turn the config
// reservation off for a pool that does NOT own the reserved name.
func TestPoolSessionCreate_ForeignConfiguredReservationStillBlocks(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		NamedSessions: []config.NamedSession{{Name: "crew", Template: "worker"}},
	}
	reserved := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "crew")

	store := beads.NewMemStore()
	_, err := createPoolSessionBeadWithAlias(store, "worker", cfg, newSessionBeadSnapshot(nil), time.Now().UTC(),
		poolSessionCreateIdentity{AgentName: "squatter"}, reserved)
	if err == nil {
		t.Fatal("createPoolSessionBeadWithAlias claimed a name reserved for a different configured named session")
	}
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("error = %v, want errPoolSessionNameUnavailable", err)
	}
}

func sortedNameSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
