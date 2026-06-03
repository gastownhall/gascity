package main

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type localMockProvider struct {
	runtime.Provider
}

func (m *localMockProvider) IsRunning(_ string) bool { return false }

func TestBuildDesiredState_ScaleFromZero_CrossRig(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Setup config: one pool agent on a rig, min=0.
	maxSess := 5
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				ScaleCheck:        "printf 0", // custom check returns 0
				Dir:               "rig-A",
				Provider:          "mock",
			},
		},
		Rigs: []config.Rig{
			{Name: "rig-A", Path: rigPath},
		},
		Providers: map[string]config.ProviderSpec{
			"mock": {
				Command: "true",
			},
		},
	}

	cityStore := beads.NewMemStore()
	rigAStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{
		"rig-A": rigAStore,
	}

	qualifiedName := "rig-A/planner"

	// Route a bead to the planner in the CITY store.
	// Native check for rig-A would miss this if not aggregated.
	_, err := cityStore.Create(beads.Bead{
		ID:     "bead-1",
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			"gc.routed_to": qualifiedName,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionBeads := &sessionBeadSnapshot{} // Empty city store snapshot

	// Call buildDesiredStateWithSessionBeads.
	// It should:
	// 1. Detect that 'planner' is cold (no sessions in city or rig stores).
	// 2. Run a native probe across ALL stores (city + rig-A).
	// 3. Find bead-1 in the city store.
	// 4. Set demand to 1 (max of custom 0 and native 1).
	// 5. Materialize a new session bead.
	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	demand := result.ScaleCheckCounts[qualifiedName]
	if demand != 1 {
		t.Errorf("expected demand 1, got %d", demand)
	}

	if len(result.State) != 1 {
		t.Errorf("expected 1 desired session, got %d", len(result.State))
	}
}

// TestBuildDesiredState_ScaleFromZero_ClampsWakeDemandToOne proves the cold-pool
// wake probe only wakes the pool from zero (contributes at most 1) and never
// scales to the full routed-bead count. With the clamp removed, the cross-store
// probe would report demand 3 (one per routed bead) instead of 1.
func TestBuildDesiredState_ScaleFromZero_ClampsWakeDemandToOne(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	maxSess := 5
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				ScaleCheck:        "printf 0", // custom check returns 0
				Dir:               "rig-A",
				Provider:          "mock",
			},
		},
		Rigs: []config.Rig{
			{Name: "rig-A", Path: rigPath},
		},
		Providers: map[string]config.ProviderSpec{
			"mock": {
				Command: "true",
			},
		},
	}

	cityStore := beads.NewMemStore()
	rigAStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{
		"rig-A": rigAStore,
	}

	qualifiedName := "rig-A/planner"

	// Route THREE beads to the planner in the CITY store. The cross-store cold
	// probe sees all three; the clamp must reduce the wake contribution to 1.
	for _, id := range []string{"bead-0", "bead-1", "bead-2"} {
		if _, err := cityStore.Create(beads.Bead{
			ID:     id,
			Status: "open",
			Type:   "task",
			Metadata: map[string]string{
				"gc.routed_to": qualifiedName,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	sessionBeads := &sessionBeadSnapshot{} // Empty city store snapshot

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	// Wake-from-zero: demand is clamped to 1 (max of custom 0 and clamped 1),
	// NOT the routed-bead count of 3.
	if demand := result.ScaleCheckCounts[qualifiedName]; demand != 1 {
		t.Errorf("expected wake demand clamped to 1, got %d", demand)
	}
	if len(result.State) != 1 {
		t.Errorf("expected 1 desired session, got %d", len(result.State))
	}
}

func TestBuildDesiredState_ScaleFromZero_IncludesRigSessions(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Setup config: one pool agent on rig-A, min=0.
	maxSess := 5
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				ScaleCheck:        "printf 0",
				Dir:               "rig-A",
				Provider:          "mock",
			},
		},
		Rigs: []config.Rig{
			{Name: "rig-A", Path: rigPath},
		},
		Providers: map[string]config.ProviderSpec{
			"mock": {
				Command: "true",
			},
		},
	}

	cityStore := beads.NewMemStore()
	rigAStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{
		"rig-A": rigAStore,
	}

	qualifiedName := "rig-A/planner"

	// Create a running session bead in the RIG store.
	// City store snapshot will miss this.
	_, err := rigAStore.Create(beads.Bead{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     qualifiedName,
			"session_name": "planner-1",
			"state":        "active",
			"pool_slot":    "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Route demand to the city store.
	_, err = cityStore.Create(beads.Bead{
		ID:     "bead-1",
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			"gc.routed_to": qualifiedName,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionBeads := &sessionBeadSnapshot{} // Empty city store snapshot

	// Call buildDesiredStateWithSessionBeads.
	// It should:
	// 1. Correctly detect that 'planner' has 1 running session (in rig-A store).
	// 2. NOT treat it as "cold" (isCold = false because runningSessions = 1).
	// 3. Skip the native probe because ScaleCheck is not empty and it's not cold.
	// 4. Use custom check (printf 0) -> demand 0.
	// 5. Resulting demand should be 0.
	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	demand := result.ScaleCheckCounts[qualifiedName]
	if demand != 0 {
		t.Errorf("expected demand 0 (custom check only), got %d", demand)
	}
}

// TestBuildDesiredState_ScaleFromZero_UnqualifiedTemplateDoesNotSuppressCold
// proves the cold detection counts only the agent's qualified template. A stray
// pool session bead carrying the unqualified base name ("planner", e.g. a
// same-base-name pool in another rig or a legacy bead) must NOT count toward
// rig-A/planner's running sessions, so rig-A/planner stays cold and its
// cold-wake probe still fires. With the bare-name match present, the stray bead
// would suppress the probe and demand would be 0.
func TestBuildDesiredState_ScaleFromZero_UnqualifiedTemplateDoesNotSuppressCold(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	maxSess := 5
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				ScaleCheck:        "printf 0", // custom check returns 0
				Dir:               "rig-A",
				Provider:          "mock",
			},
		},
		Rigs: []config.Rig{
			{Name: "rig-A", Path: rigPath},
		},
		Providers: map[string]config.ProviderSpec{
			"mock": {
				Command: "true",
			},
		},
	}

	cityStore := beads.NewMemStore()
	rigAStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{
		"rig-A": rigAStore,
	}

	qualifiedName := "rig-A/planner"

	// Stray pool session bead carrying the UNQUALIFIED base name "planner"
	// (not "rig-A/planner"). It must not be attributed to rig-A/planner.
	if _, err := rigAStore.Create(beads.Bead{
		ID:     "stray-session",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "planner",
			"session_name": "planner-1",
			"state":        "active",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Route demand to rig-A/planner in the city store.
	if _, err := cityStore.Create(beads.Bead{
		ID:     "bead-1",
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			"gc.routed_to": qualifiedName,
		},
	}); err != nil {
		t.Fatal(err)
	}

	sessionBeads := &sessionBeadSnapshot{} // Empty city store snapshot

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	// rig-A/planner is genuinely cold (the bare "planner" bead is not its
	// session), so the cold-wake probe fires on the city-routed demand.
	if demand := result.ScaleCheckCounts[qualifiedName]; demand != 1 {
		t.Errorf("expected demand 1 (stray unqualified session must not suppress cold), got %d", demand)
	}
}
