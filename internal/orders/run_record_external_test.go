package orders_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// TestIsRunRecordTakesEveryWorkflowRoot pins IsRunRecord's graph-root arm to
// sourceworkflow.IsWorkflowRoot's marks: every root it names is a run record.
func TestIsRunRecordTakesEveryWorkflowRoot(t *testing.T) {
	for _, meta := range []map[string]string{
		{"gc.kind": "workflow"},
		{"gc.formula_contract": "graph.v2"},
		{"gc.kind": " Workflow "},
	} {
		b := beads.Bead{Type: "task", Metadata: meta}
		if sourceworkflow.IsWorkflowRoot(b) && !orders.IsRunRecord(b) {
			t.Errorf("%v: a workflow root is no run record", meta)
		}
	}
	if orders.IsRunRecord(beads.Bead{Type: "task", Labels: []string{"order-run:x"}}) {
		t.Error("a plain task is a run record")
	}
	for _, typ := range []string{"molecule", "wisp", " Wisp "} { // beads.IsMoleculeType's roots (M16)
		if !orders.IsRunRecord(beads.Bead{Type: typ, Labels: []string{"order-run:x"}}) {
			t.Errorf("a %q root is no run record", typ)
		}
	}
}
