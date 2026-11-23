package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestDefaultScaleDemandReportsAnUnreadableClaimViewPartial: when the claim
// view cannot answer for a cross-store row, the pass cannot say whether the
// seat would win it. The row is not counted, the pool is reported partial (so
// its live seats are retained rather than drained), and the error names the
// template, the row and the cause.
func TestDefaultScaleDemandReportsAnUnreadableClaimViewPartial(t *testing.T) {
	cfg := agreementConfig()
	rigMem := beads.NewMemStore()
	rigMem.HonorExplicitIDs = true
	cityMem := beads.NewMemStore()
	cityMem.HonorExplicitIDs = true
	route := map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate}
	mustCreateRow(t, cityMem, beads.Bead{ID: "city-e", Title: "e", Type: "task", Metadata: route}, "open")
	mustCreateRow(t, cityMem, beads.Bead{ID: "city-y", Title: "y", Type: "task", Metadata: route}, "open")
	cause := errors.New("bd show: connection refused")
	rig := newClaimViewProbeStore(rigMem)
	rig.fail["city-e"] = cause
	own := defaultScaleCheckTarget{template: agreementTemplate, store: rig, storeKey: "rig:rig"}
	targets := append([]defaultScaleCheckTarget{own}, crossStoreDemandLegs([]defaultScaleCheckTarget{{template: agreementTemplate, store: cityMem, storeKey: "city"}}, own)...)

	counts, demand, partial, errs := defaultScaleCheckCountsAndDemand(cfg, targets)
	if got := counts[agreementTemplate]; got != 1 {
		t.Fatalf("counts[%s] = %d (named %v), want 1: only city-y is known claimable", agreementTemplate, got, demand[agreementTemplate].WorkBeadIDs)
	}
	if _, ok := demand[agreementTemplate].StoreRefs["city-e"]; ok {
		t.Fatal("city-e is named as demand though its claim view could not be read")
	}
	if !partial[agreementTemplate] {
		t.Fatalf("partial = %v, want %s partial", partial, agreementTemplate)
	}
	var named error
	for _, err := range errs {
		if errors.Is(err, cause) {
			named = err
		}
	}
	if named == nil {
		t.Fatalf("errs = %v, want one wrapping the claim view's error", errs)
	}
	for _, want := range []string{agreementTemplate, "city-e", "rig:rig"} {
		if !strings.Contains(named.Error(), want) {
			t.Fatalf("error %q does not name %q", named, want)
		}
	}
}
