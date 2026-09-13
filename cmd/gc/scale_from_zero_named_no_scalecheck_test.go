package main

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// newNoScaleCheckNamedBackingCity builds a city with a single rig pool agent
// that has min=0 and NO custom scale_check, backed by one on_demand named
// session. This shape mirrors Voxist's coordinator/planner pools: the session
// identity is the rig-scoped name (e.g. "rig-A/planner") and routed demand
// lives in the city store.
func newNoScaleCheckNamedBackingCity(t *testing.T) (cfg *config.City, cityStore beads.Store, rigStores map[string]beads.Store, identity string) {
	t.Helper()
	rigPath := t.TempDir() + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	maxSess := 5
	minSess := 0
	cfg = &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				// No ScaleCheck: default-probe pool.
				Dir:      "rig-A",
				Provider: "mock",
			},
		},
		NamedSessions: []config.NamedSession{{
			Template: "planner",
			Dir:      "rig-A",
			Mode:     "on_demand",
		}},
		Rigs:      []config.Rig{{Name: "rig-A", Path: rigPath}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}
	cityStore = beads.NewMemStore()
	rigStores = map[string]beads.Store{"rig-A": beads.NewMemStore()}
	return cfg, cityStore, rigStores, "rig-A/planner"
}

// TestBuildDesiredState_ScaleFromZero_NoScaleCheck_CrossStore_NamedPath is the
// regression guard for vp-cl4 — the named-session-backed path symmetry gap.
// A cold rig pool that backs a named session and has no custom scale_check must
// cold-wake from routed demand in the CITY store (the vp-kvp cross-store
// delivery model) just as a generic no-scale_check pool does after vp-s37.
// The routed work still wakes the backing pool, not the named-session alias.
// Before the fix the named-path branch did not add the city-store probe, so a
// sleeping named-backing rig pool never woke on cross-store demand.
func TestBuildDesiredState_ScaleFromZero_NoScaleCheck_CrossStore_NamedPath(t *testing.T) {
	cfg, cityStore, rigStores, identity := newNoScaleCheckNamedBackingCity(t)

	if _, err := cityStore.Create(beads.Bead{
		ID:       "bead-city-1",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": identity},
	}); err != nil {
		t.Fatal(err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if result.NamedSessionDemand[identity] {
		t.Errorf("cross-store cold-wake: NamedSessionDemand[%q] = true, want false for pool-routed work", identity)
	}
	if got := result.ScaleCheckCounts[identity]; got != 1 {
		t.Errorf("cross-store cold-wake demand = %d, want 1 (city-store routed bead must wake cold named-backing pool)", got)
	}
	if len(result.State) < 1 {
		t.Errorf("desired sessions = %d, want >= 1 (backing pool must be materialized)", len(result.State))
	}
}

// TestBuildDesiredState_ScaleFromZero_NoScaleCheck_NamedPath_OwnRigStillWakes
// guards that the existing own-rig-store wake path is preserved for named-backing
// pools after the cross-store fix is applied.
func TestBuildDesiredState_ScaleFromZero_NoScaleCheck_NamedPath_OwnRigStillWakes(t *testing.T) {
	cfg, cityStore, rigStores, identity := newNoScaleCheckNamedBackingCity(t)

	if _, err := rigStores["rig-A"].Create(beads.Bead{
		ID:       "bead-rig-1",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": identity},
	}); err != nil {
		t.Fatal(err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if result.NamedSessionDemand[identity] {
		t.Errorf("own-rig cold-wake: NamedSessionDemand[%q] = true, want false for pool-routed work", identity)
	}
	if got := result.ScaleCheckCounts[identity]; got != 1 {
		t.Errorf("own-rig cold-wake demand = %d, want 1", got)
	}
}

// TestBuildDesiredState_ScaleFromZero_NoScaleCheck_NamedPath_NoDemandNoWake
// guards that the cross-store probe does not spuriously wake a cold named-backing
// pool when there is no routed demand anywhere.
func TestBuildDesiredState_ScaleFromZero_NoScaleCheck_NamedPath_NoDemandNoWake(t *testing.T) {
	cfg, cityStore, rigStores, identity := newNoScaleCheckNamedBackingCity(t)

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if result.NamedSessionDemand[identity] {
		t.Errorf("no-demand: NamedSessionDemand[%q] = true, want false (must not spuriously wake)", identity)
	}
	if len(result.State) != 0 {
		t.Errorf("desired sessions = %d, want 0", len(result.State))
	}
}

// TestBuildDesiredState_ScaleFromZero_NoScaleCheck_NamedPath_MissingRigStoreNoCrossWake
// guards the missing-rig-store contract: when a cold named-backing pool's own
// rig store is unreachable, cross-store (city) demand must NOT wake it.
func TestBuildDesiredState_ScaleFromZero_NoScaleCheck_NamedPath_MissingRigStoreNoCrossWake(t *testing.T) {
	cfg, cityStore, _, identity := newNoScaleCheckNamedBackingCity(t)

	if _, err := cityStore.Create(beads.Bead{
		ID:       "bead-city-1",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": identity},
	}); err != nil {
		t.Fatal(err)
	}

	// Rig store absent (nil map): the own-rig target is unavailable.
	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, nil, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if result.NamedSessionDemand[identity] {
		t.Errorf("missing rig store: NamedSessionDemand[%q] = true, want false (must not cross-store-wake without rig store)", identity)
	}
}

// TestBuildDesiredState_ScaleFromZero_NoScaleCheck_NamedPath_AliasedRigStoreNoDoubleWake
// guards the alias defense: if the rig store aliases the city store (same
// object), the cross-store city probe must be skipped so one city-store bead
// does not produce duplicate demand signals.
func TestBuildDesiredState_ScaleFromZero_NoScaleCheck_NamedPath_AliasedRigStoreNoDoubleWake(t *testing.T) {
	cfg, cityStore, _, identity := newNoScaleCheckNamedBackingCity(t)

	if _, err := cityStore.Create(beads.Bead{
		ID:       "shared-1",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": identity},
	}); err != nil {
		t.Fatal(err)
	}

	// Rig store IS the city store (aliased).
	aliased := map[string]beads.Store{"rig-A": cityStore}
	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, aliased, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	// With an aliased store, demand should still be detected (it's a real bead)
	// but must not be double-counted.
	if result.NamedSessionDemand[identity] {
		t.Errorf("aliased-store: NamedSessionDemand[%q] = true, want false for pool-routed work", identity)
	}
	if got := result.ScaleCheckCounts[identity]; got != 1 {
		t.Errorf("aliased-store demand = %d, want 1 (aliased store bead must still be detected once)", got)
	}
}
