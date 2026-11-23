package sling

import (
	"context"
	"errors"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"

	"github.com/gastownhall/gascity/internal/beads"
)

// heldEdgeStorage is a native storage backing that answers the edge read the
// way a real backend does: an anchor it holds an issue for gets its stored
// edges, and any other anchor is reported Missing on the anchor rather than
// failing the read. Nothing else is implemented, so a walk that strays onto
// another storage method panics on the nil embedded interface.
type heldEdgeStorage struct {
	beadslib.Storage
	edges map[string][]*issueops.Dependency
}

func (s *heldEdgeStorage) EdgeReader() (issueops.EdgeReader, error) { return s, nil }

func (s *heldEdgeStorage) ReadEdges(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
	var result issueops.EdgeReadResult
	for _, id := range req.IDs {
		edges, held := s.edges[id]
		result.Anchors = append(result.Anchors, issueops.AnchorEdges{ID: id, Edges: edges, Missing: !held})
	}
	return result, nil
}

// A blocking edge can target a bead the store holds no issue for — an
// "external:" reference, or an id in another repository's ledger — and the
// native store keeps such edges in DepList's DOWN answer, so the cycle walk
// asks about the target too. It is a leaf, not a failure: refusing it would
// make every bead that blocks on an external reference unslingable.
func TestDetectCycleTreatsATargetTheNativeStoreDoesNotHoldAsALeaf(t *testing.T) {
	storage := &heldEdgeStorage{edges: map[string][]*issueops.Dependency{
		"gc-1": {
			{IssueID: "gc-1", DependsOnID: "external:github/1", Type: beadslib.DepBlocks},
			{IssueID: "gc-1", DependsOnID: "gc-2", Type: beadslib.DepBlocks},
		},
		"gc-2": {{IssueID: "gc-2", DependsOnID: "gcg-9999", Type: beadslib.DepBlocks}},
	}}
	store := beads.NewNativeDoltStoreOverStorageForTest(storage)

	if err := DetectCycle("gc-1", store); err != nil {
		t.Fatalf("DetectCycle across targets the store does not hold = %v, want nil", err)
	}

	// The control: the same walk over the same store still sees a cycle among
	// the beads it does hold. Without it, the nil above could mean only that
	// the walk read no edges at all.
	storage.edges["gc-2"] = append(storage.edges["gc-2"], &issueops.Dependency{IssueID: "gc-2", DependsOnID: "gc-1", Type: beadslib.DepBlocks})
	var cycle *CycleError
	if err := DetectCycle("gc-1", store); !errors.As(err, &cycle) {
		t.Fatalf("DetectCycle over gc-1 -> gc-2 -> gc-1 = %v, want a *CycleError", err)
	}
}
