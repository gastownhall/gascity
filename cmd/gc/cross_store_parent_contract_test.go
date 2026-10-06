package main

// The cross-store parent, end to end.
//
// The conformance suite pins what ONE store does with a ParentID naming a row
// it does not have. This pins the shape that makes that case matter: a molecule
// in the graph binding whose parent is a work bead in a rig ledger, which is
// what defaultAttachGitHubPRRepairWorkflow produces on every converged city
// (see its store-choice comment). The link spans two ledgers by design, and the
// design only holds if neither store needs the other's rows.
//
// It is written against a genuinely disjoint pair rather than one store used
// twice, because a single store makes both halves of the claim vacuous: the
// parent resolves, so nothing is being tolerated, and the child is co-resident,
// so nothing is being kept apart.

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// TestCrossStoreParentSurvivesTheSplit is the falsifying test for rule (b): a
// store persists and filters ParentID verbatim and never resolves it.
//
// Each assertion names the production failure it stands in for, because none of
// them is loud on its own — a store that started validating would refuse repair
// molecules with a not-found naming a bead that exists, and a store that
// started filtering on resolvability would report an empty step list for a
// molecule that is running.
func TestCrossStoreParentSurvivesTheSplit(t *testing.T) {
	work, graph := splittest.NewSplitStores(t)

	parent, err := work.Create(beads.Bead{Title: "pr repair", Type: "task"})
	if err != nil {
		t.Fatalf("creating the work-class parent: %v", err)
	}

	root, err := graph.Create(beads.Bead{Title: "repair workflow", Type: "molecule", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("the graph binding refused a parent it cannot see (%q): %v — every repair molecule on a converged city fails this way", parent.ID, err)
	}
	if root.ParentID != parent.ID {
		t.Errorf("Create echoed ParentID %q, want %q verbatim", root.ParentID, parent.ID)
	}

	got, err := graph.Get(root.ID)
	if err != nil {
		t.Fatalf("reading the molecule back: %v", err)
	}
	if got.ParentID != parent.ID {
		t.Errorf("the stored molecule names parent %q, want %q — the link is the only thing tying the two ledgers together", got.ParentID, parent.ID)
	}

	children, err := graph.Children(parent.ID)
	if err != nil {
		t.Fatalf("Children against the binding for a work-ledger parent: %v", err)
	}
	if len(children) != 1 || children[0].ID != root.ID {
		t.Errorf("Children(%q) on the binding returned %d beads, want the molecule; a step list that reads empty looks exactly like a molecule that never cooked", parent.ID, len(children))
	}

	// The other half of the contract: no store co-locates to satisfy the link.
	// A create that quietly moved the child to reach its parent would mint it
	// under the work ledger's prefix, and an id cannot be changed afterwards.
	if _, err := work.Get(root.ID); err == nil {
		t.Errorf("the molecule %q is also in the work ledger; placement followed ParentID instead of class", root.ID)
	}
	if _, err := graph.Get(parent.ID); err == nil {
		t.Errorf("the parent %q is also in the graph binding; the two ledgers are not disjoint and this test proves nothing", parent.ID)
	}
	orphans, err := work.Children(parent.ID)
	if err != nil {
		t.Fatalf("Children against the work ledger: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("the work ledger reports %d children for %q; the molecule was placed with its parent, not with its class", len(orphans), parent.ID)
	}
}
