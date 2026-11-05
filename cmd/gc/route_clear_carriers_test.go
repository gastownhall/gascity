package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// These tests cover the cmd/gc carriers of a gc.routed_to compare-and-set
// that are not the route-recovery lane (route_recovery_lane_route_clear_test.go):
// the one-shot `gc beads metadata-cas` command and the emitting class store's
// own CompareAndSetMetadataKey. Each has to reach the engine through the
// route-clearing capability seam (ga-l69b9m); a type assertion against the
// decorator, or a resolver that peels it, writes the route and leaves the
// previous executor's stamps behind.

const (
	carrierOldExecutor = "gascity/old-executor"
	carrierNewExecutor = "gascity/new-executor"
)

func seedCarrierBead(t *testing.T, engine beads.Store) beads.Bead {
	t.Helper()
	created, err := engine.Create(beads.Bead{Title: "stamped", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey:      carrierOldExecutor,
		beadmeta.SessionNameMetadataKey:   "gascity--old-executor",
		beadmeta.WorkDirMetadataKey:       "worktrees/old-executor",
		beadmeta.LegacyWorkDirMetadataKey: "worktrees/old-executor-legacy",
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

func requireCarrierStampsCleared(t *testing.T, engine beads.Store, id string) {
	t.Helper()
	got, err := engine.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != carrierNewExecutor {
		t.Fatalf("gc.routed_to = %q, want %q", got.Metadata[beadmeta.RoutedToMetadataKey], carrierNewExecutor)
	}
	for _, key := range laneStampKeys() {
		if got.Metadata[key] != "" {
			t.Errorf("%s = %q after a re-route through the carrier, want it cleared", key, got.Metadata[key])
		}
	}
}

// updatedRowCount counts the city journal's bead.updated rows for id.
func updatedRowCount(t *testing.T, cityPath, id string) int {
	t.Helper()
	n := 0
	for _, evt := range beadEvents(readCityJournal(t, cityPath)) {
		if evt.Subject == id && evt.Type == events.BeadUpdated {
			n++
		}
	}
	return n
}

// `gc beads metadata-cas` writes through beads.ApplyMetadataCAS.
func TestBeadsMetadataCASRerouteClearsStampsThroughTheDecorator(t *testing.T) {
	for _, shape := range laneClearShapes() {
		t.Run(shape.name, func(t *testing.T) {
			engine := beads.NewMemStore()
			bead := seedCarrierBead(t, engine)
			store := shape.open(t, engine)

			result, err := applyBeadsMetadataCAS(store, beadsMetadataCASRequest{
				beadID:   bead.ID,
				key:      beadmeta.RoutedToMetadataKey,
				expected: carrierOldExecutor,
				next:     carrierNewExecutor,
			})
			if err != nil {
				t.Fatalf("applyBeadsMetadataCAS: %v", err)
			}
			if result.Outcome != beads.MetadataCASSwapped {
				t.Fatalf("outcome = %q, want %q", result.Outcome, beads.MetadataCASSwapped)
			}
			requireCarrierStampsCleared(t, engine, bead.ID)
		})
	}
}

// The one-shot CLI funnel wraps the decorated engine in an emittingClassStore
// whose own CompareAndSetMetadataKey must reach the engine through the seam,
// and must still emit exactly one bead.updated row for the swap that landed.
func TestEmittingClassStoreCASRerouteClearsStampsAndEmitsOnce(t *testing.T) {
	cityPath := t.TempDir()
	engine := beads.NewMemStore()
	bead := seedCarrierBead(t, engine)
	emitter := &emittingClassStore{
		Store:    beads.WithRouteChangeClearing(engine, routeClearIdentity),
		cityPath: cityPath,
	}

	swapped, err := emitter.CompareAndSetMetadataKey(bead.ID, beadmeta.RoutedToMetadataKey, carrierOldExecutor, carrierNewExecutor)
	if err != nil || !swapped {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want a landed swap", swapped, err)
	}
	requireCarrierStampsCleared(t, engine, bead.ID)
	if got := updatedRowCount(t, cityPath, bead.ID); got != 1 {
		t.Errorf("the swap appended %d bead.updated row(s), want exactly 1", got)
	}

	// A lost compare changes nothing: no clear and no row.
	other := seedCarrierBead(t, engine)
	swapped, err = emitter.CompareAndSetMetadataKey(other.ID, beadmeta.RoutedToMetadataKey, "gascity/not-current", carrierNewExecutor)
	if err != nil || swapped {
		t.Fatalf("stale CompareAndSetMetadataKey = (%v, %v), want a refused compare", swapped, err)
	}
	got, err := engine.Get(other.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, key := range laneStampKeys() {
		if got.Metadata[key] == "" {
			t.Errorf("%s was cleared by a compare that lost", key)
		}
	}
	if n := updatedRowCount(t, cityPath, other.ID); n != 0 {
		t.Errorf("a refused compare appended %d bead.updated row(s), want 0", n)
	}
}
