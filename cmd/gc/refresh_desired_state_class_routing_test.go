package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/runtime"
)

// dependencyFloorCity is the smallest config that makes the session-bead
// overlay realize a dependency floor: an always-on agent so the refresh has a
// non-empty base to rebuild from, plus a pooled agent whose depends_on names a
// second agent with no live session. Reconciling the pooled root makes the
// controller mint the floor session for the dependency.
func dependencyFloorCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents: []config.Agent{
			{
				Name:         "always-on",
				Dir:          "gascity",
				StartCommand: "true",
			},
			{
				Name:              "db",
				Dir:               "gascity",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "printf 0",
			},
			{
				Name:              "api",
				Dir:               "gascity",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "printf 0",
				DependsOn:         []string{"gascity/db"},
			},
		},
	}
}

// seedPoolRootSessionBead seeds the live pool session whose dependency floor the
// overlay has to realize.
func seedPoolRootSessionBead(t *testing.T, store beads.Store) {
	t.Helper()
	if _, err := store.Create(beads.Bead{
		Title:  "api",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:gascity/api"},
		Metadata: map[string]string{
			"template":     "gascity/api",
			"agent_name":   "gascity/api",
			"session_name": "s-api-root",
			"state":        "active",
			"pool_managed": "true",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatalf("seed pool root session bead: %v", err)
	}
}

// TestRefreshDesiredStateWritesSessionBeadsToSessionsClass is the stranded-write
// guard for the controller reconcile loop.
//
// cr.refreshDesiredState's store becomes agentBuildParams.beadStore, and the
// session-bead overlay MINTS through it: realizeDependencyFloors ->
// ensureDependencyOnlyTemplate -> selectOrCreateDependencyPoolSessionBead ->
// CreateSessionInfo. Routed at the work store on a converged split city that
// create is a stranded `type=session` bead — invisible to the sessions binding
// and named by the per-boot containment re-check. It also recurs, because the
// reuse check reads the sessions snapshot while the create wrote the work store,
// so the floor can never satisfy itself.
func TestRefreshDesiredStateWritesSessionBeadsToSessionsClass(t *testing.T) {
	cityPath := t.TempDir()
	workStore := beads.NewMemStore()
	sessionsStore := beads.NewMemStore()
	seedPoolRootSessionBead(t, sessionsStore)

	cr := &CityRuntime{
		cs:            &controllerState{cityBeadStore: workStore, cityName: "demo", cityPath: cityPath},
		cfg:           dependencyFloorCity(),
		sp:            runtime.NewFake(),
		cityName:      "demo",
		cityPath:      cityPath,
		stderr:        io.Discard,
		storageRoutes: relocatedSessionRoutes(sessionsStore),
	}

	sessionBeads := cr.loadSessionBeadSnapshot()
	if sessionBeads == nil {
		t.Fatal("session-bead snapshot is nil; the sessions store never loaded")
	}
	refreshed := cr.refreshDesiredState(DesiredStateResult{BeaconTime: time.Now().UTC()}, sessionBeads)

	// The overlay must actually have realized the floor — otherwise the
	// zero-work-store assertion below would hold for the trivial reason that
	// nothing was created at all.
	floor := false
	for _, params := range refreshed.State {
		if params.TemplateName == "gascity/db" && params.DependencyOnly {
			floor = true
		}
	}
	if !floor {
		t.Fatal("fixture no longer realizes a dependency floor; the create path this guards is not being exercised")
	}

	work, err := workStore.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list work store: %v", err)
	}
	if len(work) != 0 {
		t.Fatalf("STRANDED WRITE: work store holds %d bead(s) after a desired-state refresh (first: id=%s type=%s class=%v); session-class creates must land in the sessions binding",
			len(work), work[0].ID, work[0].Type, coordclass.Classify(work[0]))
	}

	sessions, err := sessionsStore.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list sessions store: %v", err)
	}
	if len(sessions) < 2 {
		t.Fatalf("sessions store holds %d bead(s); the realized dependency floor must be minted there alongside the seeded root", len(sessions))
	}
}

// TestRefreshDesiredStateIsUnchangedOnSingleStoreCity is the byte-identity proof
// for the refresh path on a city that relocates nothing: the store it threads is
// the exact value cr.cityBeadStore() returns, and the realized floor is minted
// into that one city store — the same place, and the same bead, as before the
// accessor substitution.
func TestRefreshDesiredStateIsUnchangedOnSingleStoreCity(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	seedPoolRootSessionBead(t, cityStore)

	cr := &CityRuntime{
		cs:       &controllerState{cityBeadStore: cityStore, cityName: "demo", cityPath: cityPath},
		cfg:      dependencyFloorCity(),
		sp:       runtime.NewFake(),
		cityName: "demo",
		cityPath: cityPath,
		stderr:   io.Discard,
	}
	if got := cr.sessionsBeadStore().Store; got != cr.cityBeadStore() {
		t.Fatal("default backend: the refresh path's sessions store must be the identical value cr.cityBeadStore() returns")
	}

	refreshed := cr.refreshDesiredState(DesiredStateResult{BeaconTime: time.Now().UTC()}, cr.loadSessionBeadSnapshot())
	floor := false
	for _, params := range refreshed.State {
		if params.TemplateName == "gascity/db" && params.DependencyOnly {
			floor = true
		}
	}
	if !floor {
		t.Fatal("single-store city: the dependency floor must still be realized")
	}

	all, err := cityStore.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list city store: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("city store holds %d bead(s), want 2 (the seeded root plus the realized floor); the single-store path must be unchanged", len(all))
	}
}
