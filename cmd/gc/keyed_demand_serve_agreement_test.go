package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The keyed readers owe the same agreement the fleet demand loop owes: a row
// counted as capacity demand for template T must be a row a T-worker's own
// Tier-3 query would serve (#5250, demand_serve_predicate.go).
//
// Breaking it costs more here than it does on the fleet arm. The fleet loop
// over-counts and the overshoot is one warm seat. The keyed path enqueues an
// exact (workID, poolTarget, sourceStore) admission, mints a seat for that one
// row, and re-validates the SAME row at every later boundary with the SAME
// predicate — so a predicate that disagrees with the worker agrees with itself
// all the way to a started session, whose hook query does carry the exclusions,
// reads empty, idles and drains. The row is still ready and unassigned, so the
// next sweep enqueues it again. That is the spawn/read-empty/drain treadmill,
// and it is unbounded because nothing in the loop ever changes the row.
//
// Three shapes carry the property. Two are excluded by
// config.PoolDemandServeRulesForQuery and must NOT route demand; the third is
// the control, and it must — an over-broad refusal would starve the pool, which
// is the same defect pointing the other way.
type keyedDemandAgreementRow struct {
	name     string
	beadType string
	labels   []string
	// wantDemand is the shared verdict: counted by the keyed reader AND
	// servable to a worker. There is deliberately one field — that IS the
	// property.
	wantDemand bool
}

func keyedDemandAgreementRows() []keyedDemandAgreementRow {
	return []keyedDemandAgreementRow{
		{name: "servable task", beadType: "task", wantDemand: true},
		{
			// An unassigned parent has no executable spec. The query excludes
			// it deliberately (--exclude-type=epic), so no seat can claim it.
			name: "ready unassigned epic", beadType: "epic", wantDemand: false,
		},
		{
			// Parked precisely because the next actor is not this worker. The
			// hook re-applies the hold filter in Go even when the reader serves
			// the row, so the worker never sees it either way.
			name: "parked on hold:mayor", beadType: "task",
			labels: []string{beadmeta.HoldMayorLabel}, wantDemand: false,
		},
		{
			name: "parked on hold:external", beadType: "task",
			labels: []string{beadmeta.HoldExternalLabel}, wantDemand: false,
		},
	}
}

// TestReadyRoutedWorkViewRoutesOnlyServableDemand covers the treadmill's ENTRY
// point. The sweep's declared routed-work view resolves each ready row's pool
// target, and unallocated() hands every row carrying one to detectPoolFill as a
// start candidate. A target resolved for a row no worker would be served is a
// seat minted for nothing.
func TestReadyRoutedWorkViewRoutesOnlyServableDemand(t *testing.T) {
	for _, test := range keyedDemandAgreementRows() {
		t.Run(test.name, func(t *testing.T) {
			city := &readyStaticStore{Store: beads.NewMemStore(), ready: []beads.Bead{{
				ID: "w-1", Status: "open", Type: test.beadType, Labels: test.labels,
				Metadata: map[string]string{"gc.routed_to": "worker"},
			}}}
			cr := readyRoutedWorkViewRuntime(t, city, nil)

			view := cr.readReadyRoutedWorkView()
			if len(view.Entries) != 1 {
				t.Fatalf("view entries = %+v, want the one ready row (every ready row stays in the view)", view.Entries)
			}
			gotDemand := view.Entries[0].PoolTarget != ""
			if gotDemand != test.wantDemand {
				t.Fatalf("view pool target = %q (demand=%t), want demand=%t",
					view.Entries[0].PoolTarget, gotDemand, test.wantDemand)
			}
			wantUnallocated := 0
			if test.wantDemand {
				wantUnallocated = 1
			}
			if got := len(view.unallocated()); got != wantUnallocated {
				t.Fatalf("unallocated rows = %d, want %d: this is what detectPoolFill mints a seat for",
					got, wantUnallocated)
			}
		})
	}
}

// TestAuthorizeRoutedWorkPoolStartRequiresServableDemand covers the
// re-validation boundary. The seat is already minted here; this is the last
// gate before it is authorized to start. It has to answer the same way the
// enqueue did, or the treadmill just moves one step later — and because it is
// the same predicate on both sides, a disagreeing one never self-corrects.
func TestAuthorizeRoutedWorkPoolStartRequiresServableDemand(t *testing.T) {
	for _, test := range keyedDemandAgreementRows() {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRoutedWorkPoolAuthorizationFixture(t)
			beadType := test.beadType
			if err := fixture.workStore.Update(fixture.work.ID, beads.UpdateOpts{
				Type:   &beadType,
				Labels: test.labels,
			}); err != nil {
				t.Fatalf("restamp routed work: %v", err)
			}

			authorized, err := fixture.cr.authorizeRoutedWorkPoolStart(
				t.Context(), fixture.snapshot, fixture.info, fixture.lease)
			if err != nil {
				t.Fatalf("authorize pool start: %v", err)
			}
			if authorized != test.wantDemand {
				t.Fatalf("pool start authorized = %t, want %t", authorized, test.wantDemand)
			}
		})
	}
}
