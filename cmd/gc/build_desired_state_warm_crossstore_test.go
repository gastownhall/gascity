package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// warmCrossStoreCfg builds the shared fixture: a rig-scoped default-probe pool
// agent ("gascity/worker") on rig "gascity" under cityPath.
func warmCrossStoreCfg(t *testing.T, cityPath string) *config.City {
	t.Helper()
	rigPath := filepath.Join(cityPath, "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Rigs:      []config.Rig{{Name: "gascity", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "gascity",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(5),
		}},
	}
}

// warmWorkerTemplate is the single pool template every fixture in this file
// exercises.
const warmWorkerTemplate = "gascity/worker"

// createWarmSessionBead makes the pool WARM the way the reconciler sees it: an
// open, awake, pool-managed session bead. isCold is computed from session
// BEADS (no process probe), so this is exactly the input that flips the
// warm/cold gate.
func createWarmSessionBead(t *testing.T, store beads.Store) {
	t.Helper()
	if _, err := store.Create(beads.Bead{
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"session_name": "gc__worker-1",
			"template":     warmWorkerTemplate,
			"state":        "active",
			"pool_managed": "true",
		},
	}); err != nil {
		t.Fatalf("create warm session bead: %v", err)
	}
}

// requireWarm asserts the fixture actually registers as WARM using the same
// predicates the reconciler's isCold computation uses
// (collectAllOpenSessionInfos + isPoolManagedSessionInfo +
// poolSessionIsLiveInfo + template identity equivalence). Without this the
// tests could silently pin the cold path and pass for the wrong reason.
func requireWarm(t *testing.T, cfg *config.City, cityStore beads.Store, rigStores map[string]beads.Store) {
	t.Helper()
	infos, err := collectAllOpenSessionInfos(cfg, cityStore, rigStores, nil)
	if err != nil {
		t.Fatalf("collectAllOpenSessionInfos: %v", err)
	}
	running := 0
	for _, si := range infos {
		if isPoolManagedSessionInfo(si) && poolSessionIsLiveInfo(si) &&
			agentTemplateIdentitiesEquivalent(cfg, si.Template, warmWorkerTemplate) {
			running++
		}
	}
	if running == 0 {
		t.Fatalf("fixture is not WARM: no live pool-managed session info matched template %q (infos=%d) — the test would exercise the cold path instead", warmWorkerTemplate, len(infos))
	}
}

func createRoutedBead(t *testing.T, store beads.Store, title string) {
	t.Helper()
	if _, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": warmWorkerTemplate},
	}); err != nil {
		t.Fatalf("create routed bead %q: %v", title, err)
	}
}

// TestBuildDesiredState_WarmRigPoolSeesCityStoreRoutedDemand: cross-store
// delivery (vp-kvp) routes work for rig pools into the CITY store, so
// city-store routed demand is legitimate demand for a rig pool at all times —
// not only while the pool sleeps. The city-store probe used to be gated on
// isCold, leaving a WARM rig pool structurally blind to city-store routed
// work: demand pinned at the rig-store count while routed beads sat unclaimed
// in the city store, and pools at the warm/cold boundary oscillated
// pool_desired N↔0 (cold ticks glimpsed city demand, warm ticks went blind)
// and were mass orphan-drained every flip.
func TestBuildDesiredState_WarmRigPoolSeesCityStoreRoutedDemand(t *testing.T) {
	cityPath := t.TempDir()
	cfg := warmCrossStoreCfg(t, cityPath)
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"gascity": rigStore}

	createWarmSessionBead(t, cityStore)
	requireWarm(t, cfg, cityStore, rigStores)

	// Routed demand delivered cross-store into the CITY store; the rig store
	// stays empty. Before the fix the warm pool probed only the rig store and
	// read 0 here.
	for i := 0; i < 3; i++ {
		createRoutedBead(t, cityStore, "cross-store routed work")
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, rigStores, nil, nil, io.Discard,
	)

	if got := dsResult.ScaleCheckCounts["gascity/worker"]; got != 3 {
		t.Fatalf("ScaleCheckCounts[gascity/worker] = %d, want 3: a WARM rig pool must count "+
			"city-store routed demand (cross-store delivery), not just its own rig store — "+
			"gating the city probe on isCold leaves warm rig pools blind to routed work and "+
			"pins pool demand at the rig-store count (full ScaleCheckCounts=%v, partial=%v)",
			got, dsResult.ScaleCheckCounts, dsResult.PoolScaleCheckPartialTemplates)
	}
}

// TestBuildDesiredState_WarmRigPoolCityProbeDoesNotDoubleCountRigDemand: the
// warm city probe is a UNION with the rig-store probe, not a duplicate — a
// bead routed to the pool in the RIG store must be counted exactly once when
// both probes run.
func TestBuildDesiredState_WarmRigPoolCityProbeDoesNotDoubleCountRigDemand(t *testing.T) {
	cityPath := t.TempDir()
	cfg := warmCrossStoreCfg(t, cityPath)
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"gascity": rigStore}

	createWarmSessionBead(t, cityStore)
	requireWarm(t, cfg, cityStore, rigStores)

	// One routed bead in EACH store: expect a count of exactly 2 (1+1), not 3+.
	createRoutedBead(t, rigStore, "rig-store routed work")
	createRoutedBead(t, cityStore, "city-store routed work")

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, rigStores, nil, nil, io.Discard,
	)

	if got := dsResult.ScaleCheckCounts["gascity/worker"]; got != 2 {
		t.Fatalf("ScaleCheckCounts[gascity/worker] = %d, want 2 (1 rig + 1 city, no double count; full=%v)",
			got, dsResult.ScaleCheckCounts)
	}
}

// aliasStore wraps a beads.Store in a distinct interface value so the
// pointer-inequality alias guard (ownTarget.store != store) cannot detect
// that both "stores" share the same backing — modeling a legacy unscoped
// file-store layout or a rig dir whose missing .beads resolves a walk-up to
// the city DB.
type aliasStore struct{ beads.Store }

// TestBuildDesiredState_WarmAliasedRigStoreDoesNotDoubleCountDemand: when the
// rig "store" is a distinct object over the SAME backing as the city store,
// the rig-group and city-group probes both see the same beads. The
// cross-group per-template bead-ID dedup must keep the count at real demand
// (1), not 2 — with the city probe no longer cold-gated, a double count here
// would be a persistent warm 2x-demand condition, not a one-tick overshoot.
func TestBuildDesiredState_WarmAliasedRigStoreDoesNotDoubleCountDemand(t *testing.T) {
	cityPath := t.TempDir()
	cfg := warmCrossStoreCfg(t, cityPath)
	cityStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"gascity": aliasStore{cityStore}}

	createWarmSessionBead(t, cityStore)
	requireWarm(t, cfg, cityStore, rigStores)

	createRoutedBead(t, cityStore, "shared-backing routed work")

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, rigStores, nil, nil, io.Discard,
	)

	if got := dsResult.ScaleCheckCounts["gascity/worker"]; got != 1 {
		t.Fatalf("ScaleCheckCounts[gascity/worker] = %d, want 1: rig store aliasing the city "+
			"backing behind a distinct Store value must not double-count the same bead "+
			"across the rig and city probe groups (full=%v)", got, dsResult.ScaleCheckCounts)
	}
}

// TestBuildDesiredState_WarmRigPoolMissingRigStoreStaysPartialNoCityProbe:
// the unhealthy-own-store contract is unchanged by the warm city probe. When
// the rig store is missing/errored, the pool must stay PARTIAL (retaining
// sessions, suppressing drains) and must NOT be woken/scaled by a spurious
// city-store probe — a rig executor cannot work while its rig store is
// unreachable.
func TestBuildDesiredState_WarmRigPoolMissingRigStoreStaysPartialNoCityProbe(t *testing.T) {
	cityPath := t.TempDir()
	cfg := warmCrossStoreCfg(t, cityPath)
	cityStore := beads.NewMemStore()
	// No rig store entry: the pool's own target is errored/unavailable.
	rigStores := map[string]beads.Store{}

	createWarmSessionBead(t, cityStore)
	requireWarm(t, cfg, cityStore, rigStores)

	createRoutedBead(t, cityStore, "city routed work while rig store down")

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, rigStores, nil, nil, io.Discard,
	)

	if got, ok := dsResult.ScaleCheckCounts["gascity/worker"]; ok && got > 0 {
		t.Fatalf("ScaleCheckCounts[gascity/worker] = %d, want absent/0: an unhealthy rig store must not "+
			"gain a city probe (spurious wake while the executor cannot reach its own store)", got)
	}
	if !dsResult.ScaleCheckPartialTemplates["gascity/worker"] {
		t.Fatalf("ScaleCheckPartialTemplates missing gascity/worker: unhealthy own store must keep the "+
			"template partial so session retention still suppresses drains (got %v)",
			dsResult.ScaleCheckPartialTemplates)
	}
}

// TestBuildDesiredState_WarmNamedBackingRigPoolSeesCityStoreRoutedDemand: the
// named-session-backing branch has its own copy of the city probe with the
// same warm-blindness: an on_demand named-backing rig pool that was WARM
// stopped counting city-store routed demand, dropping to zero between city
// beads and getting orphan-drained (amplitude clamped to 1 by the
// namedOnDemandTemplates clamp, but the same spawn/drain treadmill). The
// clamp keeps the counted demand at 1 regardless of queue depth; the point
// pinned here is nonzero-ness while WARM.
func TestBuildDesiredState_WarmNamedBackingRigPoolSeesCityStoreRoutedDemand(t *testing.T) {
	cityPath := t.TempDir()
	cfg := warmCrossStoreCfg(t, cityPath)
	cfg.NamedSessions = []config.NamedSession{{
		Template: "worker",
		Dir:      "gascity",
		Mode:     "on_demand",
	}}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"gascity": rigStore}

	createWarmSessionBead(t, cityStore)
	requireWarm(t, cfg, cityStore, rigStores)

	for i := 0; i < 3; i++ {
		createRoutedBead(t, cityStore, "cross-store routed work")
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, rigStores, nil, nil, io.Discard,
	)

	if got := dsResult.ScaleCheckCounts["gascity/worker"]; got != 1 {
		t.Fatalf("ScaleCheckCounts[gascity/worker] = %d, want 1 (namedOnDemandTemplates clamp): a WARM "+
			"named-backing rig pool must still count city-store routed demand or it churns at "+
			"the warm/cold boundary (full=%v)", got, dsResult.ScaleCheckCounts)
	}
}
