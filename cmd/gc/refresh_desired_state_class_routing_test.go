package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

// TestFullBuildWritesDependencyFloorToSessionsClass preserves the placement
// proof independently of refresh. A full build performs the complete Session-
// leg census and may therefore mint the missing floor, but the create must land
// only in the relocated sessions binding—not the city or rig work stores.
func TestFullBuildWritesDependencyFloorToSessionsClass(t *testing.T) {
	cityPath := t.TempDir()
	workStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	sessionsStore := beads.NewMemStore()
	seedPoolRootSessionBead(t, sessionsStore)
	cfg := dependencyFloorCity()

	cr := &CityRuntime{
		cs: &controllerState{
			cfg:           cfg,
			cityBeadStore: workStore,
			beadStores:    map[string]beads.Store{"fixture": rigStore},
			cityName:      "demo",
			cityPath:      cityPath,
		},
		cfg:           cfg,
		sp:            runtime.NewFake(),
		cityName:      "demo",
		cityPath:      cityPath,
		stderr:        io.Discard,
		storageRoutes: relocatedSessionRoutes(sessionsStore),
	}
	cr.buildFnWithSessionBeads = supervisorBuildAgentsFnWithSessionBeads(cityPath, "demo", io.Discard)

	result := cr.buildDesiredState(cr.loadSessionBeadSnapshot(), nil)
	floor := false
	for _, params := range result.State {
		if params.TemplateName == "gascity/db" && params.DependencyOnly {
			floor = true
		}
	}
	if !floor {
		t.Fatalf("full build did not realize the dependency floor; keys=%v", mapKeys(result.State))
	}

	sessions, err := sessionsStore.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list sessions store: %v", err)
	}
	var floorRows int
	for _, row := range sessions {
		if row.Metadata["template"] == "gascity/db" && row.Metadata["pool_managed"] == "true" {
			floorRows++
		}
	}
	if floorRows != 1 {
		t.Fatalf("sessions binding dependency-floor rows = %d, want exactly one; rows=%#v", floorRows, sessions)
	}
	for name, store := range map[string]beads.Store{"city work": workStore, "rig work": rigStore} {
		rows, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
		if err != nil {
			t.Fatalf("list %s store: %v", name, err)
		}
		if len(rows) != 0 {
			t.Fatalf("%s store holds session-class rows after full build: %#v", name, rows)
		}
	}
}

// TestRefreshDesiredStateDefersFreshSessionCreationOnRelocatedCity pins the
// post-build safety boundary. Refresh reloads only the primary sessions store,
// not the complete Session residency union, so it may reuse the seeded root but
// must defer a missing dependency floor to the next full build/census. Neither
// the work store nor the sessions binding may receive a speculative new row.
func TestRefreshDesiredStateDefersFreshSessionCreationOnRelocatedCity(t *testing.T) {
	cityPath := t.TempDir()
	workStore := beads.NewMemStore()
	sessionsStore := beads.NewMemStore()
	seedPoolRootSessionBead(t, sessionsStore)
	var stderr bytes.Buffer

	cr := &CityRuntime{
		cs:            &controllerState{cityBeadStore: workStore, cityName: "demo", cityPath: cityPath},
		cfg:           dependencyFloorCity(),
		sp:            runtime.NewFake(),
		cityName:      "demo",
		cityPath:      cityPath,
		stderr:        &stderr,
		storageRoutes: relocatedSessionRoutes(sessionsStore),
	}

	sessionBeads := cr.loadSessionBeadSnapshot()
	if sessionBeads == nil {
		t.Fatal("session-bead snapshot is nil; the sessions store never loaded")
	}
	refreshed := cr.refreshDesiredState(DesiredStateResult{
		BeaconTime:              time.Now().UTC(),
		SessionSnapshotComplete: true,
		SessionOccupancyInfos:   sessionBeads.OpenInfos(),
	}, sessionBeads)

	floor := false
	for _, params := range refreshed.State {
		if params.TemplateName == "gascity/db" && params.DependencyOnly {
			floor = true
		}
	}
	if floor {
		t.Fatal("refresh realized a dependency floor without re-censusing every Session leg")
	}
	if got := stderr.String(); !strings.Contains(got, "dependency floor \"gascity/db\"") || !strings.Contains(got, errPoolSessionCreatePartial.Error()) {
		t.Fatalf("refresh did not exercise and fail closed at dependency creation: stderr=%q", got)
	}

	work, err := workStore.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list work store: %v", err)
	}
	if len(work) != 0 {
		t.Fatalf("work store holds %d bead(s) after freshness-incomplete refresh; want zero", len(work))
	}

	sessions, err := sessionsStore.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list sessions store: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions store holds %d bead(s), want only the seeded root until the next full census", len(sessions))
	}
}

// TestRefreshDesiredStateDefersFreshSessionCreationOnSingleStoreCity keeps the
// same fail-closed rule on today's single-store topology. That topology can
// evolve through class bindings and graph-run foreign writers, so refresh does
// not infer a permanent one-leg invariant from the current accessor identity.
func TestRefreshDesiredStateDefersFreshSessionCreationOnSingleStoreCity(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	seedPoolRootSessionBead(t, cityStore)
	var stderr bytes.Buffer

	cr := &CityRuntime{
		cs:       &controllerState{cityBeadStore: cityStore, cityName: "demo", cityPath: cityPath},
		cfg:      dependencyFloorCity(),
		sp:       runtime.NewFake(),
		cityName: "demo",
		cityPath: cityPath,
		stderr:   &stderr,
	}
	if got := cr.sessionsBeadStore().Store; got != cr.cityBeadStore() {
		t.Fatal("default backend: the refresh path's sessions store must be the identical value cr.cityBeadStore() returns")
	}

	sessionBeads := cr.loadSessionBeadSnapshot()
	refreshed := cr.refreshDesiredState(DesiredStateResult{
		BeaconTime:              time.Now().UTC(),
		SessionSnapshotComplete: true,
		SessionOccupancyInfos:   sessionBeads.OpenInfos(),
	}, sessionBeads)
	floor := false
	for _, params := range refreshed.State {
		if params.TemplateName == "gascity/db" && params.DependencyOnly {
			floor = true
		}
	}
	if floor {
		t.Fatal("single-store refresh realized a dependency floor without a new full Session-leg census")
	}
	if got := stderr.String(); !strings.Contains(got, "dependency floor \"gascity/db\"") || !strings.Contains(got, errPoolSessionCreatePartial.Error()) {
		t.Fatalf("single-store refresh did not exercise and fail closed at dependency creation: stderr=%q", got)
	}

	all, err := cityStore.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list city store: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("city store holds %d bead(s), want only the seeded root until the next full census", len(all))
	}
}
