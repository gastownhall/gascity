package main

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// newNoScaleCheckRigPoolCity builds a city with a single rig pool agent that
// has min=0 and NO custom scale_check (the default-probe path). Voxist's
// executor and every specialist pool are shaped exactly this way.
func newNoScaleCheckRigPoolCity(t *testing.T) (cfg *config.City, cityStore beads.Store, rigStores map[string]beads.Store, qualified string) {
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
				Name:              "executor",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				// No ScaleCheck: default-probe pool.
				Dir:      "rig-A",
				Provider: "mock",
			},
		},
		Rigs:      []config.Rig{{Name: "rig-A", Path: rigPath}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}
	cityStore = beads.NewMemStore()
	rigStores = map[string]beads.Store{"rig-A": beads.NewMemStore()}
	return cfg, cityStore, rigStores, "rig-A/executor"
}

// TestBuildDesiredState_ScaleFromZero_NoScaleCheck_CrossStore is the regression
// guard for vp-s37 / the fleet-wide min=0 cold-spawn P1. A cold rig pool with
// no custom scale_check must still cold-wake from routed demand that lives in
// the CITY store (the vp-kvp cross-store delivery model) — not only from demand
// in its own rig store. Before the fix the default probe read only the rig
// store and this returned demand 0, so the pool never woke.
func TestBuildDesiredState_ScaleFromZero_NoScaleCheck_CrossStore(t *testing.T) {
	cfg, cityStore, rigStores, qualified := newNoScaleCheckRigPoolCity(t)

	if _, err := cityStore.Create(beads.Bead{
		ID:       "bead-city-1",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": qualified},
	}); err != nil {
		t.Fatal(err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 1 {
		t.Errorf("cross-store cold-wake demand = %d, want 1 (city-store routed bead must wake the cold no-scale_check pool)", got)
	}
	if len(result.State) != 1 {
		t.Errorf("desired sessions = %d, want 1", len(result.State))
	}
}

// TestBuildDesiredState_ScaleFromZero_NoScaleCheck_OwnRigStillWakes guards that
// the existing own-rig-store wake path is preserved by the change.
func TestBuildDesiredState_ScaleFromZero_NoScaleCheck_OwnRigStillWakes(t *testing.T) {
	cfg, cityStore, rigStores, qualified := newNoScaleCheckRigPoolCity(t)

	if _, err := rigStores["rig-A"].Create(beads.Bead{
		ID:       "bead-rig-1",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": qualified},
	}); err != nil {
		t.Fatal(err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 1 {
		t.Errorf("own-rig cold-wake demand = %d, want 1", got)
	}
}

// TestBuildDesiredState_ScaleFromZero_NoScaleCheck_NoDemandNoWake guards that
// the cross-store probe does not spuriously wake a cold pool when there is no
// routed demand anywhere — a min=0 pool with no ready work must stay at zero.
func TestBuildDesiredState_ScaleFromZero_NoScaleCheck_NoDemandNoWake(t *testing.T) {
	cfg, cityStore, rigStores, qualified := newNoScaleCheckRigPoolCity(t)

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 0 {
		t.Errorf("no-demand cold pool demand = %d, want 0 (must not spuriously wake)", got)
	}
	if len(result.State) != 0 {
		t.Errorf("desired sessions = %d, want 0", len(result.State))
	}
}

// TestBuildDesiredState_ScaleFromZero_NoScaleCheck_ScalesToCrossStoreWant guards
// that a no-scale_check pool scales to the full routed-bead count (bounded by
// max_active), not just 1. Unlike a custom-scale_check pool — where the probe
// is clamped so it cannot override the custom count — the default probe IS the
// authoritative count, so it scales to total routed demand across own-rig +
// city, matching the retired cold-pool-spawner's scale-to-want behavior.
func TestBuildDesiredState_ScaleFromZero_NoScaleCheck_ScalesToCrossStoreWant(t *testing.T) {
	cfg, cityStore, rigStores, qualified := newNoScaleCheckRigPoolCity(t)

	for _, id := range []string{"c1", "c2", "c3"} {
		if _, err := cityStore.Create(beads.Bead{
			ID:       id,
			Status:   "open",
			Type:     "task",
			Metadata: map[string]string{"gc.routed_to": qualified},
		}); err != nil {
			t.Fatal(err)
		}
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 3 {
		t.Errorf("cross-store demand = %d, want 3 (scale-to-want, bounded by max_active=5)", got)
	}
}

// TestBuildDesiredState_ScaleFromZero_NoScaleCheck_MissingRigStoreNoCrossWake
// guards the reconciliation with the missing-rig-store contract: when a cold
// rig pool's own rig store is unreachable, cross-store (city) demand must NOT
// wake it — a rig executor cannot do its work without its rig store, and the
// partial flag must keep suppressing drain rather than be overridden by a
// spurious city-store wake.
func TestBuildDesiredState_ScaleFromZero_NoScaleCheck_MissingRigStoreNoCrossWake(t *testing.T) {
	cfg, cityStore, _, qualified := newNoScaleCheckRigPoolCity(t)

	if _, err := cityStore.Create(beads.Bead{
		ID:       "bead-city-1",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": qualified},
	}); err != nil {
		t.Fatal(err)
	}

	// Rig store absent (nil map): the own-rig target is unavailable.
	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, nil, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 0 {
		t.Errorf("demand = %d, want 0 (missing rig store must not cross-store-wake)", got)
	}
	if !result.ScaleCheckPartialTemplates[qualified] {
		t.Errorf("template should be marked partial when its rig store is missing")
	}
}

// TestBuildDesiredState_ScaleFromZero_NoScaleCheck_AliasedRigStoreNoDoubleCount
// guards the alias defense: if a rig store aliases the city store (the same
// store object), the cross-store city probe must be skipped so the one routed
// bead is counted once, not twice (defaultScaleCheckCounts dedups per group,
// not across groups).
func TestBuildDesiredState_ScaleFromZero_NoScaleCheck_AliasedRigStoreNoDoubleCount(t *testing.T) {
	cfg, cityStore, _, qualified := newNoScaleCheckRigPoolCity(t)

	if _, err := cityStore.Create(beads.Bead{
		ID:       "shared-1",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": qualified},
	}); err != nil {
		t.Fatal(err)
	}

	// Rig store IS the city store (aliased).
	aliased := map[string]beads.Store{"rig-A": cityStore}
	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		cityStore, aliased, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 1 {
		t.Errorf("aliased-store demand = %d, want 1 (must not double-count the same bead)", got)
	}
}
