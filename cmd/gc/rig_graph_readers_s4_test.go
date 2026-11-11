package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// TestCompletionBackstopSweepsOnlyRigsThatMayHoldRoots is
// CompletionBackstop.ensureStoreRoots' store list: on a split city a rig's
// workflow roots live in the binding, so a rig store is swept only while it may
// still hold pre-S4 roots. A single-store city sweeps every rig as before.
func TestCompletionBackstopSweepsOnlyRigsThatMayHoldRoots(t *testing.T) {
	f := newS4RigFixture(t, true)
	f.markImported(t, "rb", true)
	cs := &controllerState{
		cfg:           f.cfg,
		cityPath:      f.cityPath,
		cityBeadStore: f.work,
		beadStores:    map[string]beads.Store{"ra": f.rigs["ra"], "rb": f.rigs["rb"]},
		eventProv:     events.NewFake(),
		storageRoutes: f.routes,
	}
	_, fan := cs.completionReconcileInputs(reconcilePlane)
	var sawRA, sawRB bool
	for _, leg := range fan {
		sawRA = sawRA || sameStorePtr(leg.Store, f.rigs["ra"])
		sawRB = sawRB || sameStorePtr(leg.Store, f.rigs["rb"])
	}
	if !sawRA {
		t.Error("the backstop dropped rig ra, which awaits import and may still hold pre-S4 roots")
	}
	if sawRB {
		t.Error("the backstop still sweeps rig rb, which is proven relic-free; its store holds only work")
	}
	if !cs.RigGraphRelicFree("rb") || cs.RigGraphRelicFree("ra") {
		t.Errorf("RigGraphRelicFree(ra, rb) = (%v, %v), want (false, true)", cs.RigGraphRelicFree("ra"), cs.RigGraphRelicFree("rb"))
	}

	single := newS4RigFixture(t, false)
	scs := &controllerState{
		cfg:           single.cfg,
		cityPath:      single.cityPath,
		cityBeadStore: single.work,
		beadStores:    map[string]beads.Store{"ra": single.rigs["ra"], "rb": single.rigs["rb"]},
		eventProv:     events.NewFake(),
	}
	if _, fan := scs.completionReconcileInputs(reconcilePlane); len(fan) != 3 {
		t.Errorf("single-store backstop fans out to %d store(s), want 3 (city + both rigs)", len(fan))
	}
	if scs.RigGraphRelicFree("ra") {
		t.Error("a single-store city reports a rig relic-free to the API; there the rig store holds the rig's graph rows")
	}
}
