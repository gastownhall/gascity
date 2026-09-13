package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestConvoyList_CharacterizationGolden freezes the current per-lane behavior of
// `gc convoy list` across the three routing lanes (remote / local-controller-
// alive / serverless). It is the pilot proving the three-lane harness end to
// end; later unification moves must reproduce each lane's golden byte-for-byte
// (human text) after canonicalization. Regenerate with -chartest-update.
//
// FINDING (surfaced by this pilot): with >1 convoy, `gc convoy list --json`
// emits the convoys array in NON-DETERMINISTIC order (the human table sorts by
// id; the --json renderer preserves the store/API iteration order, which is not
// stable). The pilot therefore seeds a single convoy — enough to prove the
// harness (cross-surface identity, A==B, lane telemetry, boundary counts);
// distinct-token numbering is unit-tested in internal/chartest. Multi-element
// JSON list-order is a shape-comparison concern for the differ (chartest.
// JSONShapeDiff) and must be pinned when convoy list actually migrates.
func TestConvoyList_CharacterizationGolden(t *testing.T) {
	h := newCharCity(t, charCityBasic, func(t *testing.T, store beads.Store) {
		if _, err := store.Create(beads.Bead{Title: "Alpha convoy", Type: "convoy"}); err != nil {
			t.Fatalf("seed convoy: %v", err)
		}
	})
	h.runCharGolden(t, charCommand{
		name:     "convoy-list",
		route:    routeConvoyList,
		readback: convoyReadback,
	})
}
