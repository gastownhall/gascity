package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/executionevent"
)

// nonResidentBeadStore hides one bead from Get while serving everything else,
// modeling a split-store city where a convoy's tracks edges are readable from
// the store that materialized the molecule while the launch bead they name is
// resident in another store.
type nonResidentBeadStore struct {
	beads.Store
	hidden string
}

func (s nonResidentBeadStore) Get(id string) (beads.Bead, error) {
	if id == s.hidden {
		return beads.Bead{}, beads.ErrNotFound
	}
	return s.Store.Get(id)
}

func TestExecutionEmitStoreAnchorsLaunchesResidentInAnotherStore(t *testing.T) {
	graph := beads.NewMemStore()
	work := beads.NewMemStore()
	rig := beads.NewMemStore()
	work.HonorExplicitIDs = true
	rig.HonorExplicitIDs = true

	convoy, err := work.Create(beads.Bead{ID: "mc-convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	launch, err := work.Create(beads.Bead{ID: "ga-launch"})
	if err != nil {
		t.Fatalf("create launch placeholder: %v", err)
	}
	if err := work.DepAdd(convoy.ID, launch.ID, "tracks"); err != nil {
		t.Fatalf("add tracks edge: %v", err)
	}
	if _, err := rig.Create(beads.Bead{
		ID:       launch.ID,
		Metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: "mc-source"},
	}); err != nil {
		t.Fatalf("create rig launch: %v", err)
	}
	root, err := graph.Create(beads.Bead{Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   convoy.ID,
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}

	primary := nonResidentBeadStore{Store: work, hidden: launch.ID}

	// Without routing, the launch read misses and the anchor is silently
	// dropped — the split-store regression this seam exists to close.
	bare, err := executionevent.ProjectCurrent(
		beads.GraphStore{Store: graph}, beads.WorkStore{Store: primary}, root.ID)
	if err != nil {
		t.Fatalf("bare projection: %v", err)
	}
	if len(bare.WorkAssociations) != 1 || len(bare.RunAnchors) != 0 {
		t.Fatalf("bare projection = %d associations, %d anchors; want the association without an anchor", len(bare.WorkAssociations), len(bare.RunAnchors))
	}

	routed := executionEmitWorkStore{Store: primary, resolveOwning: func(id string) (beads.Store, bool) {
		if id != launch.ID {
			return nil, false
		}
		return rig, true
	}}
	projection, err := executionevent.ProjectCurrent(
		beads.GraphStore{Store: graph}, beads.WorkStore{Store: routed}, root.ID)
	if err != nil {
		t.Fatalf("routed projection: %v", err)
	}
	if len(projection.RunAnchors) != 1 || projection.RunAnchors[0].SourceBeadID != "mc-source" {
		t.Fatalf("routed anchors = %#v, want one anchor on mc-source", projection.RunAnchors)
	}
}

func TestExecutionEmitStorePreservesPrimaryReadsAndMisses(t *testing.T) {
	work := beads.NewMemStore()
	resident, err := work.Create(beads.Bead{ID: "mc-resident"})
	if err != nil {
		t.Fatalf("create resident: %v", err)
	}
	resolved := 0
	routed := executionEmitWorkStore{Store: work, resolveOwning: func(string) (beads.Store, bool) {
		resolved++
		return nil, false
	}}
	if got, err := routed.Get(resident.ID); err != nil || got.ID != resident.ID {
		t.Fatalf("resident read = %v, %v", got, err)
	}
	if resolved != 0 {
		t.Fatalf("resident read consulted the owning-store resolver %d times, want none", resolved)
	}
	if _, err := routed.Get("mc-absent"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("absent read error = %v, want the primary store's not-found", err)
	}
	if resolved != 1 {
		t.Fatalf("absent read consulted the resolver %d times, want once", resolved)
	}
}
