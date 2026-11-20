package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// relicLegRoutes is a split city's routes over a real SQLite binding, with
// the per-rig import records at bindingRoot: rig "packs" recorded clean,
// rig "busy" awaiting import.
func relicLegRoutes(t *testing.T, split bool) (*storageRoutes, string, map[string]string) {
	t.Helper()
	cityPath := t.TempDir()
	rigDirs := map[string]string{"packs": t.TempDir(), "busy": t.TempDir()}
	cfg := &config.City{Rigs: []config.Rig{{Name: "packs", Path: rigDirs["packs"]}, {Name: "busy", Path: rigDirs["busy"]}}}
	bindingRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(bindingRoot, rigGraphCleanMarkerName("packs")), rigTestRecord(cityPath, rigDirs["packs"]), 0o644); err != nil {
		t.Fatal(err)
	}
	markers := &rigGraphMarkers{bindingRoot: bindingRoot, cityPath: cityPath, rigs: cfg.Rigs}
	if !split {
		return &storageRoutes{rigMarkers: markers}, cityPath, rigDirs
	}
	binding, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = binding.(interface{ CloseStore() error }).CloseStore() })
	routes := splitEnvRoutes(binding)
	routes.rigMarkers = markers
	return routes, cityPath, rigDirs
}

// TestOrderTrackingSweepDropsRelicFreeRigTargets: the stale-close and
// retention sweeps read a rig store only while it may hold pre-split
// tracking beads.
func TestOrderTrackingSweepDropsRelicFreeRigTargets(t *testing.T) {
	targets := []orderTrackingSweepTarget{
		{target: execStoreTarget{ScopeKind: "city"}, label: "city"},
		{target: execStoreTarget{ScopeKind: "rig", RigName: "packs"}, label: "rig packs"},
		{target: execStoreTarget{ScopeKind: "rig", RigName: "busy"}, label: "rig busy"},
	}
	routes, _, _ := relicLegRoutes(t, true)
	got := withoutRelicFreeRigSweepTargets(routes, append([]orderTrackingSweepTarget(nil), targets...))
	if len(got) != 2 || got[0].label != "city" || got[1].label != "rig busy" {
		t.Errorf("split sweep targets = %+v, want city and rig busy", got)
	}
	single, _, _ := relicLegRoutes(t, false)
	if got := withoutRelicFreeRigSweepTargets(single, append([]orderTrackingSweepTarget(nil), targets...)); len(got) != 3 {
		t.Errorf("a single-store city dropped a sweep target: %+v", got)
	}
}

// TestRigViewRelicFreeRetiresOnlyAProvenRig covers the workflow delete, the
// source-workflow root scan and the control-scope lookup.
func TestRigViewRelicFreeRetiresOnlyAProvenRig(t *testing.T) {
	routes, cityPath, rigDirs := relicLegRoutes(t, true)
	store := beads.NewMemStore()
	if !rigViewRelicFree(routes, cityPath, convoyStoreView{path: rigDirs["packs"], store: store}) {
		t.Error("a rig recorded clean is still read as a graph leg")
	}
	if rigViewRelicFree(routes, cityPath, convoyStoreView{path: rigDirs["busy"], store: store}) {
		t.Error("a rig awaiting import lost its relic leg")
	}
	if rigViewRelicFree(routes, cityPath, convoyStoreView{path: cityPath, store: store}) {
		t.Error("the city view was retired")
	}
	if rigViewRelicFree(routes, cityPath, convoyStoreView{path: convoyBindingViewPath, store: store, role: convoyViewClassBinding}) {
		t.Error("the binding view was retired")
	}
	single, cityPath2, rigDirs2 := relicLegRoutes(t, false)
	if rigViewRelicFree(single, cityPath2, convoyStoreView{path: rigDirs2["packs"], store: store}) {
		t.Error("a single-store city retired a rig leg")
	}
}

// refusingListStore fails any List: a store the gate must not read.
type refusingListStore struct{ beads.Store }

func (refusingListStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("a relic-free rig's own sessions were listed")
}

// TestDetachedOrphanLaneSkipsARelicFreeRigsOwnSessions: the session index
// for a relic-free rig leg is built from the session store alone.
func TestDetachedOrphanLaneSkipsARelicFreeRigsOwnSessions(t *testing.T) {
	lane := newDetachedOrphanLane()
	lane.retiredRig = func(rig string) bool { return rig == "packs" }
	if !lane.rigRetired("rig packs") || lane.rigRetired("rig busy") || lane.rigRetired("city") {
		t.Fatal("rigRetired does not read the leg label")
	}
	sessions := beads.NewMemStore()
	if _, _, err := detachedOrphanRoutesForLeg(refusingListStore{beads.NewMemStore()}, sessions, false); err != nil {
		t.Errorf("a sessionless leg listed its own store: %v", err)
	}
	if _, _, err := detachedOrphanRoutesForLeg(refusingListStore{beads.NewMemStore()}, sessions, true); err == nil {
		t.Error("control: a leg that holds sessions did not list its own store")
	}
}
