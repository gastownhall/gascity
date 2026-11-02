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
		ID:       id,
		Title:    title,
		Type:     "task",
		Status:   "in_progress",
		Metadata: workflowRootMetadata(supersedeSourceBeadID, supersedeSourceStoreRef),
	}
}

// workflowRootMetadata stamps a graph.v2 workflow root for a given source bead
// and the work store that bead lives in.
func workflowRootMetadata(sourceBeadID, sourceStoreRef string) map[string]string {
	return map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.SourceBeadIDMetadataKey:    sourceBeadID,
		beadmeta.SourceStoreRefMetadataKey:  sourceStoreRef,
	}
}

// newBindingRowUnderID writes a row into the binding under an id the work ledger
// already uses, letting a caller vary exactly one identity field at a time.
func newBindingRowUnderID(t *testing.T, store *beads.MemStore, rootID string, metadata map[string]string) {
	t.Helper()
	store.HonorExplicitIDs = true
	row, err := store.Create(beads.Bead{
		ID:       rootID,
		Title:    "binding row sharing an id with the work ledger",
		Type:     "task",
		Status:   "in_progress",
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("Create(binding row): %v", err)
	}
	if row.ID != rootID {
		t.Fatalf("binding row minted %s, want the work root's id %s", row.ID, rootID)
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

// TestListSourceWorkflowRootsKeepsARootTheBindingOnlyShadowsByID is the negative
// side of the supersede. A shared id does not make two rows the same bead: ids
// are unique only WITHIN a store, and store-prefixed ids collide across stores
// by construction, so two rigs can hold a source bead under one id string. Each
// case below varies exactly one identity field on the binding's side; the work
// ledger's root stays live every time, the sling is refused, and the one blocked
// workflow is named once.
//
// Without these the identity check in bindingHoldsRoot has no proof at all: a
// supersede that fired on the id alone would retire a live root belonging to a
// different source and admit the second workflow this guard exists to refuse.
func TestListSourceWorkflowRootsKeepsARootTheBindingOnlyShadowsByID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
	}{
		{
			// The binding's row is a workflow root, but for another source bead.
			name:     "different source bead",
			metadata: workflowRootMetadata("mc-other-source", supersedeSourceStoreRef),
		},
		{
			// The same source bead id string, resident in another rig's work
			// store. gc.source_store_ref is what tells those two apart.
			name:     "different source store ref",
			metadata: workflowRootMetadata(supersedeSourceBeadID, "rig:alpha"),
		},
		{
			// Not a workflow root at all — an ordinary bead the binding happens
			// to hold under the same id.
			name:     "not a workflow root",
			metadata: map[string]string{beadmeta.SourceBeadIDMetadataKey: supersedeSourceBeadID},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := beads.NewMemStore()
			work := beads.NewMemStore()
			root := newRelocatedWorkflowRoot(t, work)
			newBindingRowUnderID(t, binding, root.ID, tc.metadata)
			deps := convergedSplitCityDeps(binding, work)

			roots, err := listSourceWorkflowRoots(deps, supersedeSourceBeadID)
			if err != nil {
				t.Fatalf("listSourceWorkflowRoots: %v", err)
			}
			if len(roots) != 1 || roots[0].storeRef != supersedeSourceStoreRef {
				t.Fatalf("listSourceWorkflowRoots returned %d roots (%v), want the work ledger's live root kept",
					len(roots), blockingWorkflowIDs(roots))
			}
			err = checkLegacySourceWorkflowConflict(deps, supersedeSourceBeadID, "", false)
			var conflictErr *sourceworkflow.ConflictError
			if !errors.As(err, &conflictErr) {
				t.Fatalf("checkLegacySourceWorkflowConflict error = %v, want the live work-leg root to conflict", err)
			}
			if !slices.Equal(conflictErr.WorkflowIDs, []string{root.ID}) {
				t.Fatalf("conflicting workflow IDs = %v, want the single root [%s]", conflictErr.WorkflowIDs, root.ID)
			}
		})
	}
}

// TestListSourceWorkflowRootsSupersedesAnUnstampedLegacyBindingRow is the
// control for the source-store half above: a binding row written before
// gc.source_store_ref existed carries no ref to compare, and demanding one would
// exclude exactly the pre-migration roots this supersede was built for. Such a
// row still supersedes its retained twin, on the id plus source-bead rule.
func TestListSourceWorkflowRootsSupersedesAnUnstampedLegacyBindingRow(t *testing.T) {
	binding := beads.NewMemStore()
	work := beads.NewMemStore()
	root := newRelocatedWorkflowRoot(t, work)
	newBindingRowUnderID(t, binding, root.ID, map[string]string{
		beadmeta.KindMetadataKey:         beadmeta.KindWorkflow,
		beadmeta.SourceBeadIDMetadataKey: supersedeSourceBeadID,
	})
	closeWorkflowRoot(t, binding, root.ID)
	deps := convergedSplitCityDeps(binding, work)

	roots, err := listSourceWorkflowRoots(deps, supersedeSourceBeadID)
	if err != nil {
		t.Fatalf("listSourceWorkflowRoots: %v", err)
	}
	if len(roots) != 0 {
		t.Fatalf("listSourceWorkflowRoots = %v, want the unstamped binding row to supersede %s", blockingWorkflowIDs(roots), root.ID)
	}
	if err := checkLegacySourceWorkflowConflict(deps, supersedeSourceBeadID, "", false); err != nil {
		t.Fatalf("checkLegacySourceWorkflowConflict = %v, want the sling admitted", err)
	}
}

// TestListSourceWorkflowRootsKeepsALiveUnstampedLegacyBindingRootsTwin is the
// live half of the row above. The binding's own ListLiveRoots cannot report an
// unstamped row while a source store ref is in play — WorkflowMatchesSource
// falls back to the root's PHYSICAL ref ("graph:<city>"), which never equals the
// source's "city:" ref — so that row never reaches bindingIDs and nothing else
// names it. Retiring its twin would leave zero blockers and admit the second
// workflow this guard exists to refuse, so while the binding's row is LIVE the
// twin keeps blocking.
func TestListSourceWorkflowRootsKeepsALiveUnstampedLegacyBindingRootsTwin(t *testing.T) {
	binding := beads.NewMemStore()
	work := beads.NewMemStore()
	root := newRelocatedWorkflowRoot(t, work)
	newBindingRowUnderID(t, binding, root.ID, map[string]string{
		beadmeta.KindMetadataKey:         beadmeta.KindWorkflow,
		beadmeta.SourceBeadIDMetadataKey: supersedeSourceBeadID,
	})
	deps := convergedSplitCityDeps(binding, work)

	roots, err := listSourceWorkflowRoots(deps, supersedeSourceBeadID)
	if err != nil {
		t.Fatalf("listSourceWorkflowRoots: %v", err)
	}
	if len(roots) != 1 || roots[0].storeRef != supersedeSourceStoreRef {
		t.Fatalf("listSourceWorkflowRoots returned %d roots (%v), want the work ledger's twin kept while the binding's row is live",
			len(roots), blockingWorkflowIDs(roots))
	}
	err = checkLegacySourceWorkflowConflict(deps, supersedeSourceBeadID, "", false)
	var conflictErr *sourceworkflow.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("checkLegacySourceWorkflowConflict error = %v, want the live unstamped binding row's twin to conflict", err)
	}
	if !slices.Equal(conflictErr.WorkflowIDs, []string{root.ID}) {
		t.Fatalf("conflicting workflow IDs = %v, want the single root [%s]", conflictErr.WorkflowIDs, root.ID)
	}
}
