package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestDefaultScaleDemandReportsAnUnreadableClaimViewPartial: when the read of
// the claim view fails, the pass cannot say whether the seat would win the
// cross-store rows it asked about. One read answers all of a view's rows, so
// none of them is counted, the pool is reported partial (so its live seats
// are retained rather than drained), and each row's error names the template,
// the row, the view and the cause. The pool's own rows still count.
func TestDefaultScaleDemandReportsAnUnreadableClaimViewPartial(t *testing.T) {
	cfg := agreementConfig()
	rigMem := beads.NewMemStore()
	rigMem.HonorExplicitIDs = true
	cityMem := beads.NewMemStore()
	cityMem.HonorExplicitIDs = true
	route := map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate}
	mustCreateRow(t, rigMem, beads.Bead{ID: "rig-own", Title: "own", Type: "task", Metadata: route}, "open")
	mustCreateRow(t, cityMem, beads.Bead{ID: "city-e", Title: "e", Type: "task", Metadata: route}, "open")
	mustCreateRow(t, cityMem, beads.Bead{ID: "city-y", Title: "y", Type: "task", Metadata: route}, "open")
	cause := errors.New("bd show: connection refused")
	rig := newClaimViewProbeStore(rigMem)
	rig.fail["city-e"] = cause
	own := defaultScaleCheckTarget{template: agreementTemplate, store: rig, storeKey: "rig:rig"}
	targets := append([]defaultScaleCheckTarget{own}, crossStoreDemandLegs([]defaultScaleCheckTarget{{template: agreementTemplate, store: cityMem, storeKey: "city"}}, own)...)

	counts, demand, partial, errs := defaultScaleCheckCountsAndDemand(cfg, targets)
	if ids := demand[agreementTemplate].WorkBeadIDs; counts[agreementTemplate] != 1 || !slices.Equal(ids, []string{"rig-own"}) {
		t.Fatalf("demand[%s] = %d %v, want only rig-own: no city row is known claimable", agreementTemplate, counts[agreementTemplate], ids)
	}
	if !partial[agreementTemplate] {
		t.Fatalf("partial = %v, want %s partial", partial, agreementTemplate)
	}
	for _, id := range []string{"city-e", "city-y"} {
		var named error
		for _, err := range errs {
			if errors.Is(err, cause) && strings.Contains(err.Error(), "cross-store row "+id+":") {
				named = err
			}
		}
		if named == nil {
			t.Fatalf("errs = %v, want one for %s wrapping the claim view's error", errs, id)
		}
		for _, want := range []string{agreementTemplate, "rig:rig"} {
			if !strings.Contains(named.Error(), want) {
				t.Fatalf("error %q does not name %q", named, want)
			}
		}
	}
}
