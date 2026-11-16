package coordclass_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/orders"
)

// TestCoordinationOwnedReadsTheOrdersRunLabel pins coordclass's order-run
// prefix against the orders store's, behaviorally (orders imports
// coordclass through sourceworkflow, so the internal test cannot): a label
// the orders store reads as a run of x is coordination-owned.
func TestCoordinationOwnedReadsTheOrdersRunLabel(t *testing.T) {
	b := beads.Bead{Type: "task", Labels: []string{"order-run:x"}}
	if name, ok := orders.NameFromOrderRunLabel(b); !ok || name != "x" {
		t.Fatalf("orders reads %v as %q %t, want x", b.Labels, name, ok)
	}
	if !coordclass.CoordinationOwned(b) {
		t.Fatalf("CoordinationOwned(%v) = false: coordclass's order-run prefix drifted from the orders store's", b.Labels)
	}
}
