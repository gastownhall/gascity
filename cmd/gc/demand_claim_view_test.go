package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestDefaultScaleDemandCountsACityOnlyRowOnce: a city row routed to a rig
// pool whose rig store has no copy of it is genuine city-delivered work
// (vp-s37). The claim view answers not-found, and the row is counted once,
// under the city leg.
func TestDefaultScaleDemandCountsACityOnlyRowOnce(t *testing.T) {
	cfg := agreementConfig()
	rigMem := beads.NewMemStore()
	rigMem.HonorExplicitIDs = true
	cityMem := beads.NewMemStore()
	cityMem.HonorExplicitIDs = true
	const id = "city-y"
	mustCreateRow(t, cityMem, beads.Bead{ID: id, Title: "y", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate}}, "open")
	if _, err := rigMem.Get(id); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("rig Get(%s) = %v, want not found", id, err)
	}
	rig := newClaimViewProbeStore(rigMem)
	own := defaultScaleCheckTarget{template: agreementTemplate, store: rig, storeKey: "rig:rig"}
	targets := append([]defaultScaleCheckTarget{own}, crossStoreDemandLegs([]defaultScaleCheckTarget{{template: agreementTemplate, store: cityMem, storeKey: "city"}}, own)...)

	counts, demand, partial, errs := defaultScaleCheckCountsAndDemand(cfg, targets)
	if len(errs) != 0 || partial[agreementTemplate] {
		t.Fatalf("errs = %v partial = %v, want a complete pass", errs, partial)
	}
	if got := counts[agreementTemplate]; got != 1 {
		t.Fatalf("counts[%s] = %d, want 1", agreementTemplate, got)
	}
	entry := demand[agreementTemplate]
	if len(entry.WorkBeadIDs) != 1 || entry.WorkBeadIDs[0] != id {
		t.Fatalf("WorkBeadIDs = %v, want [%s]", entry.WorkBeadIDs, id)
	}
	if ref := entry.StoreRefs[id]; ref != "city" {
		t.Fatalf("StoreRefs[%s] = %q, want city", id, ref)
	}
	if got := rig.probes(id); got != 1 {
		t.Fatalf("claim view probed %s %d times, want 1: the count must come from the claim's own answer", id, got)
	}
}
