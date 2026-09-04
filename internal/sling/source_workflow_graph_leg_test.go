package sling

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// newLiveWorkflowRoot writes a live graph.v2 workflow root for sourceBeadID,
// stamped with the work store its source bead lives in — the shape
// doStartGraphWorkflow leaves behind.
func newLiveWorkflowRoot(t *testing.T, store beads.Store, sourceBeadID, sourceStoreRef string) beads.Bead {
	t.Helper()
	root, err := store.Create(beads.Bead{
		Title:  "live workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.SourceBeadIDMetadataKey:    sourceBeadID,
			beadmeta.SourceStoreRefMetadataKey:  sourceStoreRef,
		},
	})
	if err != nil {
		t.Fatalf("Create(workflow root): %v", err)
	}
	return root
}

// TestListSourceWorkflowRootsKeepsStrictStoreFailureFatal pins decision (3) of
// ga-nqdff at the collector: a Strict leg's scan failure aborts even though a
// warning sink is wired and the leg is not the selected source store. The graph
// binding is where a split city's live roots are, so tolerating a fault there
// would let the guard answer "no conflict" from the outage itself.
func TestListSourceWorkflowRootsKeepsStrictStoreFailureFatal(t *testing.T) {
	scanErr := errors.New("graph binding unreachable")
	var warnings []string
	deps := SlingDeps{
		Store:    beads.NewMemStore(),
		StoreRef: "city:test",
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{
				{
					Store:    sourceWorkflowListFailStore{Store: beads.NewMemStore(), err: scanErr},
					StoreRef: sourceworkflow.GraphStoreRef("test"),
					Strict:   true,
				},
				{Store: beads.NewMemStore(), StoreRef: "city:test"},
			}, nil
		},
		SourceWorkflowStoreScanWarning: func(storeRef string, _ error) {
			warnings = append(warnings, storeRef)
		},
	}

	_, err := listSourceWorkflowRoots(deps, "mc-source")
	if !errors.Is(err, scanErr) {
		t.Fatalf("listSourceWorkflowRoots error = %v, want the strict graph-leg failure %v", err, scanErr)
	}
	if !strings.Contains(err.Error(), sourceworkflow.GraphStoreRef("test")) {
		t.Fatalf("error = %v, want it to name the graph leg", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("strict leg failure was downgraded to warnings %v", warnings)
	}
}

// TestListSourceWorkflowRootsStillToleratesNonStrictLegFailure is the control
// for the row above: the Strict flag narrows tolerance to the legs that carry
// it and leaves the degraded-rig behavior the API relies on untouched.
func TestListSourceWorkflowRootsStillToleratesNonStrictLegFailure(t *testing.T) {
	sourceStore := beads.NewMemStore()
	graphStore := beads.NewMemStore()
	root := newLiveWorkflowRoot(t, graphStore, "mc-source", "city:test")

	var warnings []string
	deps := SlingDeps{
		Store:    sourceStore,
		StoreRef: "city:test",
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{
				{Store: graphStore, StoreRef: sourceworkflow.GraphStoreRef("test"), Strict: true},
				{Store: sourceStore, StoreRef: "city:test"},
				{
					Store:    sourceWorkflowListFailStore{Store: beads.NewMemStore(), err: errors.New("schema v54 has no revision")},
					StoreRef: "rig:stale",
				},
			}, nil
		},
		SourceWorkflowStoreScanWarning: func(storeRef string, _ error) {
			warnings = append(warnings, storeRef)
		},
	}

	err := checkLegacySourceWorkflowConflict(deps, "mc-source")
	var conflictErr *sourceworkflow.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("checkLegacySourceWorkflowConflict error = %v, want the binding-resident root to conflict", err)
	}
	if !slices.Equal(conflictErr.WorkflowIDs, []string{root.ID}) {
		t.Fatalf("conflicting workflow IDs = %v, want [%s]", conflictErr.WorkflowIDs, root.ID)
	}
	if !slices.Equal(warnings, []string{"rig:stale"}) {
		t.Fatalf("scan warnings = %v, want only the skipped non-strict rig store", warnings)
	}
}

// TestListSourceWorkflowRootsNamesOneRootOnceAcrossOverlappingLegs is decision
// (4). Two legs can reach one physical root — a converged city's work ledger
// still holds the frozen copy the storage migration retained under the id the
// binding now owns, and a caller that hands the same store in twice reaches it
// twice as well. One blocked workflow must be named once.
func TestListSourceWorkflowRootsNamesOneRootOnceAcrossOverlappingLegs(t *testing.T) {
	store := beads.NewMemStore()
	root := newLiveWorkflowRoot(t, store, "mc-source", "city:test")

	deps := SlingDeps{
		Store:    store,
		StoreRef: "city:test",
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{
				{Store: store, StoreRef: sourceworkflow.GraphStoreRef("test"), Strict: true},
				{Store: store, StoreRef: "city:test"},
			}, nil
		},
		SourceWorkflowStoreScanWarning: func(string, error) {},
	}

	err := checkLegacySourceWorkflowConflict(deps, "mc-source")
	var conflictErr *sourceworkflow.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("checkLegacySourceWorkflowConflict error = %v, want ConflictError", err)
	}
	if !slices.Equal(conflictErr.WorkflowIDs, []string{root.ID}) {
		t.Fatalf("conflicting workflow IDs = %v, want the single root [%s] named once", conflictErr.WorkflowIDs, root.ID)
	}
}
