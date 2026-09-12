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

// The converged-city fixture below is one source bead resident in one work
// store, which is all the supersede rule turns on.
const (
	supersedeSourceBeadID   = "mc-source"
	supersedeSourceStoreRef = "city:test"
)

// newRelocatedWorkflowRoot writes a live graph.v2 workflow root into the store
// that holds it, stamped the way doStartGraphWorkflow stamps one.
func newRelocatedWorkflowRoot(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	root, err := store.Create(workflowRootBead("relocated workflow root", ""))
	if err != nil {
		t.Fatalf("Create(workflow root): %v", err)
	}
	return root
}

// newRetainedWorkflowTwin writes the frozen copy a storage migration leaves in
// the work ledger: same id, same source stamps, still open, because the
// migration copies rows into the binding and deletes nothing. The shared id is
// the whole point of the fixture, so it is pinned rather than minted.
func newRetainedWorkflowTwin(t *testing.T, store *beads.MemStore, rootID string) {
	t.Helper()
	store.HonorExplicitIDs = true
	twin, err := store.Create(workflowRootBead("retained copy of the relocated workflow root", rootID))
	if err != nil {
		t.Fatalf("Create(retained twin): %v", err)
	}
	if twin.ID != rootID {
		t.Fatalf("retained twin minted %s, want the relocated root's id %s", twin.ID, rootID)
	}
}

func workflowRootBead(title, id string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  title,
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.SourceBeadIDMetadataKey:    supersedeSourceBeadID,
			beadmeta.SourceStoreRefMetadataKey:  supersedeSourceStoreRef,
		},
	}
}

// closeWorkflowRoot closes a workflow root in place, the way a city closes a
// relocated root when the workflow it heads finishes.
func closeWorkflowRoot(t *testing.T, store beads.Store, rootID string) {
	t.Helper()
	if _, err := store.CloseAll([]string{rootID}, map[string]string{
		"close_reason": "the workflow this root headed finished",
	}); err != nil {
		t.Fatalf("CloseAll(%s): %v", rootID, err)
	}
}

// sourceWorkflowGetFailStore faults every Get while leaving List healthy — the
// shape of a binding whose supersede probe cannot be answered.
type sourceWorkflowGetFailStore struct {
	beads.Store
	err error
}

func (s sourceWorkflowGetFailStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, s.err
}

// convergedSplitCityDeps wires the two legs a converged split city hands the
// singleton guard: the relocated class binding first and strict, then the work
// ledger the migration left its retained copies in.
func convergedSplitCityDeps(binding, work beads.Store) SlingDeps {
	return SlingDeps{
		Store:    work,
		StoreRef: supersedeSourceStoreRef,
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{
				{Store: binding, StoreRef: sourceworkflow.GraphStoreRef("test"), Strict: true},
				{Store: work, StoreRef: supersedeSourceStoreRef},
			}, nil
		},
		SourceWorkflowStoreScanWarning: func(string, error) {},
	}
}

// TestListSourceWorkflowRootsLetsClosedBindingRowSupersedeRetainedTwin is the
// ga-x5lpj row. On a converged split city a workflow root relocated into the
// binding and CLOSED there still exists as an OPEN frozen copy in the retained
// work ledger. ListLiveRoots hides the closed binding row, so the work leg's
// copy was the only row the guard saw, and it refused a sling whose only live
// root is gone. The binding's row supersedes its retained twin whether that row
// is live or closed.
func TestListSourceWorkflowRootsLetsClosedBindingRowSupersedeRetainedTwin(t *testing.T) {
	binding := beads.NewMemStore()
	work := beads.NewMemStore()
	root := newRelocatedWorkflowRoot(t, binding)
	closeWorkflowRoot(t, binding, root.ID)
	newRetainedWorkflowTwin(t, work, root.ID)
	deps := convergedSplitCityDeps(binding, work)

	roots, err := listSourceWorkflowRoots(deps, supersedeSourceBeadID)
	if err != nil {
		t.Fatalf("listSourceWorkflowRoots: %v", err)
	}
	if len(roots) != 0 {
		t.Fatalf("listSourceWorkflowRoots = %v, want no live root: the binding closed %s", blockingWorkflowIDs(roots), root.ID)
	}
	if err := checkLegacySourceWorkflowConflict(deps, supersedeSourceBeadID, "", false); err != nil {
		t.Fatalf("checkLegacySourceWorkflowConflict = %v, want the sling admitted", err)
	}
}

// TestListSourceWorkflowRootsRefusesWhenBindingRootIsLiveBesideRetainedTwin is
// the control: the same converged fixture with the binding row still LIVE
// refuses, and the one blocked workflow is reported once, from the binding.
func TestListSourceWorkflowRootsRefusesWhenBindingRootIsLiveBesideRetainedTwin(t *testing.T) {
	binding := beads.NewMemStore()
	work := beads.NewMemStore()
	root := newRelocatedWorkflowRoot(t, binding)
	newRetainedWorkflowTwin(t, work, root.ID)
	deps := convergedSplitCityDeps(binding, work)

	roots, err := listSourceWorkflowRoots(deps, supersedeSourceBeadID)
	if err != nil {
		t.Fatalf("listSourceWorkflowRoots: %v", err)
	}
	if len(roots) != 1 || roots[0].storeRef != sourceworkflow.GraphStoreRef("test") {
		t.Fatalf("listSourceWorkflowRoots returned %d roots (%v), want only the binding's row", len(roots), blockingWorkflowIDs(roots))
	}
	err = checkLegacySourceWorkflowConflict(deps, supersedeSourceBeadID, "", false)
	var conflictErr *sourceworkflow.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("checkLegacySourceWorkflowConflict error = %v, want the live binding root to conflict", err)
	}
	if !slices.Equal(conflictErr.WorkflowIDs, []string{root.ID}) {
		t.Fatalf("conflicting workflow IDs = %v, want the single root [%s]", conflictErr.WorkflowIDs, root.ID)
	}
}

// TestListSourceWorkflowRootsRefusesARootTheBindingNeverHeld is the second
// control: a root the migration never relocated has no binding row to supersede
// it, so the guard refuses exactly as it does today. Without this row a
// supersede that dropped every work-leg root would pass the row above.
func TestListSourceWorkflowRootsRefusesARootTheBindingNeverHeld(t *testing.T) {
	binding := beads.NewMemStore()
	work := beads.NewMemStore()
	root := newRelocatedWorkflowRoot(t, work)
	deps := convergedSplitCityDeps(binding, work)

	err := checkLegacySourceWorkflowConflict(deps, supersedeSourceBeadID, "", false)
	var conflictErr *sourceworkflow.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("checkLegacySourceWorkflowConflict error = %v, want the never-migrated root to conflict", err)
	}
	if !slices.Equal(conflictErr.WorkflowIDs, []string{root.ID}) {
		t.Fatalf("conflicting workflow IDs = %v, want [%s]", conflictErr.WorkflowIDs, root.ID)
	}
}

// TestListSourceWorkflowRootsFailsWhenBindingSupersedeProbeFaults keeps the
// residency rule the supersede is built on: a binding fault is an error, never
// absence. Reading a failed probe as "the binding holds nothing" would serve
// every frozen twin as live data; reading it as "the binding holds everything"
// would drop live roots and admit a second workflow. Neither: it refuses.
func TestListSourceWorkflowRootsFailsWhenBindingSupersedeProbeFaults(t *testing.T) {
	probeErr := errors.New("graph binding unreachable")
	work := beads.NewMemStore()
	root := newRelocatedWorkflowRoot(t, work)
	deps := convergedSplitCityDeps(sourceWorkflowGetFailStore{Store: beads.NewMemStore(), err: probeErr}, work)

	_, err := listSourceWorkflowRoots(deps, supersedeSourceBeadID)
	if !errors.Is(err, probeErr) {
		t.Fatalf("listSourceWorkflowRoots error = %v, want the binding probe fault %v", err, probeErr)
	}
	if !strings.Contains(err.Error(), root.ID) || !strings.Contains(err.Error(), sourceworkflow.GraphStoreRef("test")) {
		t.Fatalf("error = %v, want it to name the probed root and the binding leg", err)
	}
}

// TestListSourceWorkflowRootsLeavesSingleStoreCityEnumerationUnchanged pins the
// blast radius. A city that relocates nothing enumerates no binding leg, so
// there is nothing to supersede with: ids are unique only WITHIN a store, and a
// city and a rig that each mint the same id hold two different beads that both
// belong in the answer. Both legs fault every Get, so a probe run here at all
// fails the row.
func TestListSourceWorkflowRootsLeavesSingleStoreCityEnumerationUnchanged(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	root := newRelocatedWorkflowRoot(t, cityStore)
	newRetainedWorkflowTwin(t, rigStore, root.ID)
	noProbe := errors.New("no probe belongs on a city that relocates nothing")

	deps := SlingDeps{
		Store:    cityStore,
		StoreRef: supersedeSourceStoreRef,
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{
				{Store: sourceWorkflowGetFailStore{Store: cityStore, err: noProbe}, StoreRef: supersedeSourceStoreRef},
				{Store: sourceWorkflowGetFailStore{Store: rigStore, err: noProbe}, StoreRef: "rig:alpha"},
			}, nil
		},
		SourceWorkflowStoreScanWarning: func(string, error) {},
	}

	roots, err := listSourceWorkflowRoots(deps, supersedeSourceBeadID)
	if err != nil {
		t.Fatalf("listSourceWorkflowRoots: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("listSourceWorkflowRoots returned %d roots, want both same-id rows a single-store city holds", len(roots))
	}
	if roots[0].storeRef != supersedeSourceStoreRef || roots[1].storeRef != "rig:alpha" {
		t.Fatalf("roots came from %q and %q, want %s then rig:alpha", roots[0].storeRef, roots[1].storeRef, supersedeSourceStoreRef)
	}
	if !slices.Equal(blockingWorkflowIDs(roots), []string{root.ID}) {
		t.Fatalf("blocking ids = %v, want the id named once [%s]", blockingWorkflowIDs(roots), root.ID)
	}
}
