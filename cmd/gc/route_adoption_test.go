package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const adoptionTestCityPool = "ccc-researcher"

// adoptionTestConfig is a city whose HQ store (prefix "ct") adopts unrouted
// work into the city-scoped pool adoptionTestCityPool.
func adoptionTestConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "city", Prefix: "ct", UnroutedWorkTarget: adoptionTestCityPool},
		Agents: []config.Agent{
			{Name: adoptionTestCityPool, MinActiveSessions: ptr(0), MaxActiveSessions: ptr(3)},
		},
	}
}

// plainCreatedBead is what a plain `bd create` leaves behind: open,
// unassigned, a task, and no metadata at all.
func plainCreatedBead(id string) beads.Bead {
	return beads.Bead{ID: id, Title: "hand-made work", Type: "task", Status: "open"}
}

// adoptionRuntime builds a runtime over one counting city store, configured
// with cfg.
func adoptionRuntime(t *testing.T, cfg *config.City, seed ...beads.Bead) (*CityRuntime, *countingRouteStore) {
	t.Helper()
	store := &countingRouteStore{Store: beads.NewMemStoreFrom(0, seed, nil)}
	cr := &CityRuntime{
		cityName:            "city",
		cityPath:            t.TempDir(),
		cfg:                 cfg,
		standaloneCityStore: store,
		stderr:              io.Discard,
	}
	return cr, store
}

// TestRouteAdoptionDeltaPassAdoptsPlainCreatedCityBead is AC-1: a bead made
// with plain `bd create` in a city whose workspace names an
// unrouted_work_target is routed by the delta pass to exactly the value
// built-in `gc sling <target>` writes, and it is reported as adopted, not
// restored.
func TestRouteAdoptionDeltaPassAdoptsPlainCreatedCityBead(t *testing.T) {
	cfg := adoptionTestConfig()
	bead := plainCreatedBead("ct-1")
	cr, store := adoptionRuntime(t, cfg, bead)
	lane := cr.routeRecoveryLaneOf()

	lane.observe(beadCreatedEvent(t, bead))
	report := cr.recoverUnroutedWorkRoutesDelta()

	want := agentutil.NormalizePoolRouteTarget(cfg, adoptionTestCityPool)
	if got := mustRoutedTo(t, store, "ct-1"); got != want {
		t.Fatalf("ct-1 gc.routed_to = %q, want %q", got, want)
	}
	if report.adopted != 1 {
		t.Fatalf("delta pass adopted %d, want 1", report.adopted)
	}
	if report.restored != 0 {
		t.Fatalf("delta pass restored %d, want 0 (adoption is not a restore)", report.restored)
	}
}

// TestRouteAdoptionCarriedRouteWins pins that a carried gc.run_target is
// restored by the existing recovery path and never overwritten by the target.
func TestRouteAdoptionCarriedRouteWins(t *testing.T) {
	cfg := adoptionTestConfig()
	bead := plainCreatedBead("ct-2")
	bead.Metadata = map[string]string{beadmeta.RunTargetMetadataKey: "other-pool"}
	cr, store := adoptionRuntime(t, cfg, bead)

	report := cr.runRouteRecoveryBackstop(backstopReasonCadence)

	if got := mustRoutedTo(t, store, "ct-2"); got != "other-pool" {
		t.Fatalf("ct-2 gc.routed_to = %q, want the carried route %q", got, "other-pool")
	}
	if report.restored != 1 || report.adopted != 0 {
		t.Fatalf("backstop restored=%d adopted=%d, want restored=1 adopted=0", report.restored, report.adopted)
	}
}
