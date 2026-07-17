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

// TestBuildDesiredState_ScaleFromZero_LegacyBoundTemplateSuppressesCold proves
// the cold detection counts an adopted session bead persisted under a removed
// binding ("rig-A/gc.planner") as a running session of the current unbound
// rig-A/planner agent. The identities are equivalent after bound→unbound
// migration, so the pool is NOT cold and the cold-wake probe must not fire —
// otherwise every tick over-probes and transiently over-wakes a pool that
// already has a live adopted session. The bare-name distinctness guarantee of
// TestBuildDesiredState_ScaleFromZero_UnqualifiedTemplateDoesNotSuppressCold
// must survive this widening.
func TestBuildDesiredState_ScaleFromZero_LegacyBoundTemplateSuppressesCold(t *testing.T) {
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

	// Adopted pool session bead persisted under the removed binding. Its
	// stored template is the legacy bound identity of the SAME agent, so it
	// must count toward rig-A/planner's running sessions.
	if _, err := rigAStore.Create(beads.Bead{
		ID:     "adopted-session",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "rig-A/gc.planner",
			"session_name": "planner-legacy-1",
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

	// The adopted legacy-bound session counts as running → pool is not cold →
	// the custom check's 0 stands and no cold-wake probe inflates demand.
	if demand := result.ScaleCheckCounts[qualifiedName]; demand != 0 {
		t.Errorf("expected demand 0 (adopted legacy-bound session suppresses cold probe), got %d", demand)
	}
}

// TestBuildDesiredState_ScaleFromZero_LegacyBoundUnassignedRoutedWorkWakesCanonicalPool
// proves the BF-1 review finding is closed: open, unassigned work routed to the
// legacy bound form of a now-unbound pool agent ("rig-A/gc.planner") must wake
// and be claimable by the canonical "rig-A/planner" pool. The canonical
// pool-demand probe matches gc.routed_to against the canonical target by raw
// string, so before the reconciler canonicalizes the route the cold pool never
// sees the demand and migration-era ready work stays stuck at zero. After the
// re-home the cold-wake probe counts it (clamped to 1) and the persisted route
// is canonical so the canonical worker's work_query/claim path can surface it.
func TestBuildDesiredState_ScaleFromZero_LegacyBoundUnassignedRoutedWorkWakesCanonicalPool(t *testing.T) {
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

	const legacyRoute = "rig-A/gc.planner"
	const canonical = "rig-A/planner"

	// Open, unassigned demand still routed to the removed bound identity. No live
	// session owns it (empty assignee, open status), so it is pure migration-era
	// ready work that the canonical pool cannot see until its route is rewritten.
	created, err := cityStore.Create(beads.Bead{
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			"gc.routed_to": legacyRoute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionBeads := &sessionBeadSnapshot{} // cold pool: no running sessions

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	// The legacy-routed demand is canonicalized to rig-A/planner, so the cold-wake
	// probe now sees it and wakes the pool from zero (clamped to 1).
	if demand := result.ScaleCheckCounts[canonical]; demand != 1 {
		t.Errorf("expected demand 1 (legacy-routed work canonicalized wakes cold pool), got %d", demand)
	}

	// The persisted route is canonical, so the canonical worker's work_query and
	// the claim predicate (raw-string gc.routed_to match) can surface and claim it.
	got, err := cityStore.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", created.ID, err)
	}
	if routed := got.Metadata["gc.routed_to"]; routed != canonical {
		t.Errorf("gc.routed_to = %q, want %q (re-homed to canonical)", routed, canonical)
	}
}

// TestBuildDesiredState_ScaleFromZero_LegacyBoundUnassignedRoutedWorkWakesCanonicalPoolCachingStore
// pins BC-1: within one reconcile pass, canonicalizeLegacyBoundUnassignedRoutedWork
// rewrites gc.routed_to on open ready work between the assigned-work ready probe
// and the later scale-check probe. On a production-style CachingStore (explicit
// cached/live handles) the scale-check must read the POST-rewrite route, not a
// live snapshot memoized before the write, or the canonical cold pool never
// wakes. The MemStore sibling test above cannot catch this: a plain store has no
// cached/live handle split, so its controller-demand read re-reads current state
// instead of returning the pre-write live memo.
func TestBuildDesiredState_ScaleFromZero_LegacyBoundUnassignedRoutedWorkWakesCanonicalPoolCachingStore(t *testing.T) {
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

	const legacyRoute = "rig-A/gc.planner"
	const canonical = "rig-A/planner"

	// City store is a production-style CachingStore with explicit cached/live
	// handles. Seed the open, unassigned, legacy-routed demand into the backing
	// store and prime the cache, mirroring a live city where the bead predates
	// this tick. No live session owns it, so it is pure migration-era ready work.
	backing := beads.NewMemStore()
	created, err := backing.Create(beads.Bead{
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			"gc.routed_to": legacyRoute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewCachingStoreForTest(backing, nil)
	if err := cityStore.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	rigStores := map[string]beads.Store{
		"rig-A": beads.NewMemStore(),
	}

	sessionBeads := &sessionBeadSnapshot{} // cold pool: no running sessions

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	// The scale-check probe runs after the same-pass canonicalization write, so
	// it must observe the canonical route through the CachingStore and wake the
	// cold pool (clamped to 1). A stale pre-write live snapshot would bucket the
	// demand under the legacy route and leave this at 0.
	if demand := result.ScaleCheckCounts[canonical]; demand != 1 {
		t.Errorf("expected demand 1 (canonicalized legacy route wakes cold pool via CachingStore), got %d", demand)
	}

	// The persisted route is canonical.
	got, err := cityStore.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", created.ID, err)
	}
	if routed := got.Metadata["gc.routed_to"]; routed != canonical {
		t.Errorf("gc.routed_to = %q, want %q (re-homed to canonical)", routed, canonical)
	}
}
