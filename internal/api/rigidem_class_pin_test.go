package api

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/session"
)

// idemCreateRecorder wraps a store and records every bead handed to Create
// inside a transaction. Classifying the recorded value pins the write site
// itself rather than a hand-copied duplicate of its literal, so a change to the
// shape createIdemRecord writes is carried into the assertion instead of
// drifting away from it.
type idemCreateRecorder struct {
	beads.Store
	created []beads.Bead
}

// Tx runs the wrapped store's transaction with a Tx that records creates.
func (r *idemCreateRecorder) Tx(commitMsg string, fn func(beads.Tx) error) error {
	return r.Store.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&idemCreateRecorderTx{Tx: tx, rec: r})
	})
}

// idemCreateRecorderTx is the recording half of idemCreateRecorder.
type idemCreateRecorderTx struct {
	beads.Tx
	rec *idemCreateRecorder
}

// Create records the bead as the caller wrote it, then performs the real
// create. The pre-store value is the one that matters: it is what a class
// router would classify to choose a destination, before the store fills in ID,
// status and timestamps.
func (t *idemCreateRecorderTx) Create(b beads.Bead) (beads.Bead, error) {
	t.rec.created = append(t.rec.created, b)
	return t.Tx.Create(b)
}

// TestRigIdemRecordsStayWorkClass pins the durable rig-create idempotency
// record to the work class.
//
// createIdemRecord writes straight to the store it is handed — there is no
// placement hop on the path — and that is only correct while the record
// classifies as work, which today it does by falling through every arm of
// coordclass.Classify: "gc-idem" is not a class marker. The day coordclass
// grows an arm that claims it, the write becomes a stranded one: the record
// lands in the work ledger while the class's own readers look at the relocated
// binding, and the rig-create idempotency axis silently stops converging on a
// split city. Nothing else in the tree would notice, which is why the hazard
// gets a pin instead of a seam.
func TestRigIdemRecordsStayWorkClass(t *testing.T) {
	rec := &idemCreateRecorder{Store: beads.NewMemStore()}
	if _, err := createIdemRecord(rec, "c1", "req-classpin1", "digestval", "7", "web", idemStateInFlight); err != nil {
		t.Fatalf("createIdemRecord: %v", err)
	}

	// Non-vacuity: a pin that classifies nothing, or classifies the wrong
	// bead, passes for free. Both guards fail loudly instead.
	if len(rec.created) != 1 {
		t.Fatalf("createIdemRecord created %d beads, want exactly 1: there is nothing for this pin to classify", len(rec.created))
	}
	got := rec.created[0]
	if !slices.Contains(got.Labels, idemLabel) {
		t.Fatalf("recorded bead labels = %v, want the %q marker: this is not the idem record, so the class assertion below would prove nothing", got.Labels, idemLabel)
	}

	if class := coordclass.Classify(got); class != coordclass.ClassWork {
		t.Fatalf("coordclass.Classify(idem record) = %s, want %s.\n"+
			"Remedy: if coordclass grows a %q arm, rigidem's writes must take placement "+
			"through Server.createStoreForBead (internal/api/residency_create.go), which plans "+
			"and then resolves, in the same change — otherwise createIdemRecord "+
			"keeps writing the record into the work ledger that no longer serves its class.",
			class, coordclass.ClassWork, idemLabel)
	}

	// Control: the classifier must still be able to answer something other
	// than work for this bead. Without it, a Classify gutted to always-work —
	// the mutation that would make the pin above meaningless — would read as a
	// pass.
	infra := got
	infra.Labels = append(slices.Clone(got.Labels), session.LabelSession)
	if class := coordclass.Classify(infra); class == coordclass.ClassWork {
		t.Fatalf("control: coordclass.Classify(idem record + %q) = %s, want any infrastructure class: "+
			"the classifier reports work for everything, so the pin above is vacuous",
			session.LabelSession, class)
	}
}
