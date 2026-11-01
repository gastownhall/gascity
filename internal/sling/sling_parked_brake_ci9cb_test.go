package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// ci-9cb: the re-pour loop this file's veto closes. On city gk, six
// mol-polecat-work pours fired back-to-back over workflow roots a witness had
// parked (defer + halt_reason=not_polecat_routable). Each pour minted a
// synthetic "input convoy for <parked root>" and a fresh graph.v2 root keyed to
// the new convoy, and the halted root's halt_reason was cleared inside the
// mint window (14:47:38-14:48:06Z on gk-hueo7) while gc.routed_to was retained.
// The routed-raw override treated a deferred+unassigned+halted root as a
// footgun to repair, forever. These cases replay the observed bead shapes
// through the decision function: NO override may fire on a parked brake, and
// the brake stamp must come back byte-identical.
func TestOnFormulaNeedsAttachmentParkedBrakeVeto(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		metadata   map[string]string
		wantParked bool
	}{
		{
			name:       "gk-hueo7 shape: deferred + halt + pool route",
			status:     "deferred",
			metadata:   map[string]string{"gc.routed_to": "worker", beadmeta.HaltReasonMetadataKey: "not_polecat_routable"},
			wantParked: true,
		},
		{
			name:       "post-mint shape: halt cleared, deferred retained",
			status:     "deferred",
			metadata:   map[string]string{"gc.routed_to": "worker"},
			wantParked: true,
		},
		{
			name:       "STOP-arm shape: open + halt, no deferral yet",
			status:     "open",
			metadata:   map[string]string{"gc.routed_to": "worker", beadmeta.HaltReasonMetadataKey: "content_gate_error"},
			wantParked: true,
		},
		{
			name:       "unassigned routed-raw, no brake: the footgun stays repaired",
			status:     "open",
			metadata:   map[string]string{"gc.routed_to": "worker"},
			wantParked: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// NewMemStoreFrom seeds status verbatim; MemStore.Create folds
			// every new bead to "open" and cannot express the parked shape.
			store := beads.NewMemStoreFrom(1, []beads.Bead{
				{ID: "PK-1", Type: "task", Status: tc.status, Metadata: tc.metadata},
			}, nil)
			deps := SlingDeps{Store: store}

			dec, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "mol-polecat-work", BeadOrFormula: "PK-1"}, store, deps)
			if err != nil {
				t.Fatalf("decision: %v", err)
			}
			if !tc.wantParked {
				if !dec.NeedsAttach || dec.Parked != "" {
					t.Fatalf("plain routed-raw --on must still attach: %+v", dec)
				}
				return
			}
			if dec.NeedsAttach || dec.Conflict != nil || dec.Parked == "" {
				t.Fatalf("parked brake not honored: %+v (want NeedsAttach=false, Conflict=nil, Parked set)", dec)
			}
			// req-2 in decision form: reading the parked bead must not
			// have mutated its brake.
			after, err := store.Get("PK-1")
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			if after.Metadata[beadmeta.HaltReasonMetadataKey] != tc.metadata[beadmeta.HaltReasonMetadataKey] {
				t.Fatalf("halt_reason mutated by the decision path: %q -> %q",
					tc.metadata[beadmeta.HaltReasonMetadataKey], after.Metadata[beadmeta.HaltReasonMetadataKey])
			}
			if after.Status != tc.status {
				t.Fatalf("status mutated by the decision path: %q -> %q", tc.status, after.Status)
			}
		})
	}
}

// The burn-and-attach conflict path must not burn a parked root's live
// attachment either: the veto runs before the attachment probe.
func TestOnFormulaNeedsAttachmentParkedWithLiveMoleculeNotBurned(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BK-1", Type: "task", Status: "deferred", Metadata: map[string]string{
			"gc.routed_to":                 "worker",
			beadmeta.HaltReasonMetadataKey: "not_polecat_routable",
		}},
		{ID: "ROOT-1", Type: "molecule", Status: "open", ParentID: "BK-1",
			Metadata: map[string]string{beadmeta.FormulaNameMetadataKey: "other-formula"}},
	}, nil)
	dec, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "mol-polecat-work", BeadOrFormula: "BK-1"}, store, SlingDeps{Store: store})
	if err != nil {
		t.Fatalf("decision: %v", err)
	}
	if dec.Parked == "" || dec.Conflict != nil || dec.NeedsAttach {
		t.Fatalf("parked bead with a conflicting live attachment must stay idempotent, not conflict/burn: %+v", dec)
	}
	root, err := store.Get("ROOT-1")
	if err != nil || root.Status != "open" {
		t.Fatalf("live attachment of a parked bead was touched: %+v err=%v", root, err)
	}
}
