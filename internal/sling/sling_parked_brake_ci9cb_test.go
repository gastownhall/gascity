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
		labels     []string
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
			// gk-bx4v 2026-11-02T00:53Z: a blocked human-quarantine bead
			// (merge_result=blocked, auto_push=false, NO halt stamp, NOT
			// deferred) fell through the idempotency gate — which only
			// proves "already routed here" — and got a pool-routed
			// workflow minted around it. routed_to=human vetoes at any
			// status.
			name:       "gk-bx4v shape: blocked + routed_to=human, no halt stamp",
			status:     "blocked",
			metadata:   map[string]string{"gc.routed_to": "human"},
			wantParked: true,
		},
		{
			name:       "open + routed_to=human: terminal marker, not a repair target",
			status:     "open",
			metadata:   map[string]string{"gc.routed_to": "human"},
			wantParked: true,
		},
		{
			// ci-soai 2026-11-02: the mayor lane is a terminal non-pool
			// route for the sling. The gk-f4gr family (held 07:05Z, routed
			// gastown.mayor) was re-wrapped by the pourer anyway —
			// gk-o687j/gk-xt9uc minted 06:34Z and burned a polecat session.
			name:       "gk-f4gr shape: open + routed_to=gastown.mayor",
			status:     "deferred",
			metadata:   map[string]string{"gc.routed_to": "gastown.mayor"},
			wantParked: true,
		},
		{
			// ci-soai: a dispatch hold (hold:mayor) names the required next
			// actor as someone other than the pool worker — the witness
			// hold pass on the f4gr family is the brake the attach path
			// must honor at any status.
			name:       "open + hold:mayor label, no halt stamp",
			status:     "open",
			labels:     []string{beadmeta.HoldMayorLabel, "verify"},
			metadata:   map[string]string{"gc.routed_to": "worker"},
			wantParked: true,
		},
		{
			name:       "open + hold:external label",
			status:     "open",
			labels:     []string{beadmeta.HoldExternalLabel},
			metadata:   map[string]string{},
			wantParked: true,
		},
		{
			// gk-3n3jk (intake re-fires gk-6wed9→gk-h8sdl, gk-y6t8r→gk-ojstx):
			// a parked branch_ready+auto_push=false bead holds its deliverable
			// local-only; re-pouring duplicates it. No halt stamp needed —
			// the metadata pair is the brake.
			name:       "h8sdl shape: branch_ready=true + auto_push=false, no halt",
			status:     "open",
			metadata:   map[string]string{"branch_ready": "true", "auto_push": "false"},
			wantParked: true,
		},
		{
			name:       "case/whitespace variant of the parked-deliverable stamps",
			status:     "open",
			metadata:   map[string]string{"branch_ready": "TRUE ", "auto_push": " False"},
			wantParked: true,
		},
		{
			// Discriminators: only the pair parks. A pushed deliverable
			// (auto_push=true) or a not-yet-ready branch (branch_ready
			// absent) is ordinary pool work.
			name:       "branch_ready=true + auto_push=true: pushed, not parked",
			status:     "open",
			metadata:   map[string]string{"branch_ready": "true", "auto_push": "true"},
			wantParked: false,
		},
		{
			name:       "auto_push=false alone: branch not ready, pour proceeds",
			status:     "open",
			metadata:   map[string]string{"auto_push": "false"},
			wantParked: false,
		},
		{
			// Precision discriminator: an ordinary label is not a brake —
			// the routed-raw repair must keep attaching for plain rows.
			name:       "plain label, no brake: the footgun stays repaired",
			status:     "open",
			labels:     []string{"verify", "qa-retest"},
			metadata:   map[string]string{"gc.routed_to": "worker"},
			wantParked: false,
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
				{ID: "PK-1", Type: "task", Status: tc.status, Labels: tc.labels, Metadata: tc.metadata},
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
