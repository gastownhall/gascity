package dispatch

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/formulatest"
)

// externallyClosedDrain is an expanded separate drain whose control was closed
// without closeDrainControl, so the unit convoys it minted are still open, and
// the finalizer of the workflow that holds it.
type externallyClosedDrain struct {
	store     *beads.MemStore
	drainID   string
	rootID    string
	finalizer beads.Bead
	units     []string
	// untouched is the drain's members and input convoy as they were before
	// the finalizer ran.
	untouched []beads.Bead
}

func newExternallyClosedDrain(t *testing.T) externallyClosedDrain {
	t.Helper()
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if _, err := ProcessControl(beadstest.Graph(store), drain, ProcessOptions{Work: beadstest.Work(store), FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
	f := externallyClosedDrain{store: store, drainID: drain.ID, rootID: drain.Metadata[beadmeta.RootBeadIDMetadataKey]}
	for _, row := range manifest.Rows {
		if row.UnitConvoyID == "" {
			t.Fatalf("row %d has no unit convoy after expansion", row.Index)
		}
		f.units = append(f.units, row.UnitConvoyID)
		f.untouched = append(f.untouched, mustGetBead(t, store, row.MemberID))
	}
	f.untouched = append(f.untouched, mustGetBead(t, store, manifest.ParentConvoyID))
	// An operator (or the root-state gate, or a scope abort) closes the
	// drain; closeDrainControl never runs, so its units stay open.
	if _, err := store.CloseAll([]string{drain.ID}, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}); err != nil {
		t.Fatalf("closing drain %s: %v", drain.ID, err)
	}
	for _, id := range f.units {
		if got := mustGetBead(t, store, id).Status; got != "open" {
			t.Fatalf("unit %s status = %q before the finalizer, want open", id, got)
		}
	}
	f.finalizer = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       "workflow-finalize",
			beadmeta.RootBeadIDMetadataKey: f.rootID,
		},
	})
	return f
}

// finalize runs the workflow finalizer through store, which wraps f.store.
func (f externallyClosedDrain) finalize(t *testing.T, store beads.Store) (ControlResult, error) {
	t.Helper()
	return ProcessControl(beadstest.Graph(store), mustGetBead(t, f.store, f.finalizer.ID), ProcessOptions{Work: beadstest.Work(store)})
}

func (f externallyClosedDrain) assertUntouched(t *testing.T) {
	t.Helper()
	for _, want := range f.untouched {
		got := mustGetBead(t, f.store, want.ID)
		if got.Status != want.Status || !reflect.DeepEqual(got.Metadata, want.Metadata) {
			t.Errorf("%s (%q) changed by the finalizer: status %q -> %q, metadata %v -> %v", want.ID, want.Title, want.Status, got.Status, want.Metadata, got.Metadata)
		}
	}
}

func TestProcessWorkflowFinalizeRetiresUnitConvoysOfExternallyClosedDrain(t *testing.T) {
	f := newExternallyClosedDrain(t)
	result, err := f.finalize(t, f.store)
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed {
		t.Fatalf("workflow result = %+v, want processed", result)
	}
	for _, id := range f.units {
		assertDrainUnitRetired(t, f.store, id, beadmeta.OutcomeSkipped)
	}
	if got := mustGetBead(t, f.store, f.finalizer.ID).Status; got != "closed" {
		t.Fatalf("finalizer status = %q, want closed", got)
	}
	f.assertUntouched(t)
}

func TestProcessWorkflowFinalizeRetiresUnitConvoysOfExternallyClosedDrainAsCanceled(t *testing.T) {
	f := newExternallyClosedDrain(t)
	if err := f.store.SetMetadata(f.rootID, beadmeta.CancelRequestedMetadataKey, "2026-11-21T00:00:00Z"); err != nil {
		t.Fatalf("marking root canceled: %v", err)
	}
	if _, err := f.finalize(t, f.store); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	for _, id := range f.units {
		assertDrainUnitRetired(t, f.store, id, beadmeta.OutcomeCanceled)
	}
	f.assertUntouched(t)
}

// unitCloseFailingStore fails every CloseAll naming one of its units while
// armed.
type unitCloseFailingStore struct {
	beads.Store
	units []string
	armed bool
}

func (s *unitCloseFailingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if s.armed && slices.ContainsFunc(ids, func(id string) bool { return slices.Contains(s.units, id) }) {
		return 0, errors.New("unit close refused")
	}
	return s.Store.CloseAll(ids, metadata)
}

func TestProcessWorkflowFinalizeRetiresUnitConvoysOfExternallyClosedDrainKeepsFinalizerOpenOnCloseFailure(t *testing.T) {
	f := newExternallyClosedDrain(t)
	failing := &unitCloseFailingStore{Store: f.store, units: f.units, armed: true}
	if _, err := f.finalize(t, failing); err == nil {
		t.Fatal("ProcessControl(workflow-finalize) over a store refusing the unit close returned nil")
	}
	finalizer := mustGetBead(t, f.store, f.finalizer.ID)
	if finalizer.Status != "open" {
		t.Fatalf("finalizer status = %q after a failed unit close, want open", finalizer.Status)
	}
	if finalizer.Metadata[workflowFinalizeErrorMetadataKey] == "" {
		t.Fatalf("finalizer has no %s after a failed unit close", workflowFinalizeErrorMetadataKey)
	}
	for _, id := range f.units {
		if got := mustGetBead(t, f.store, id).Status; got != "open" {
			t.Fatalf("unit %s status = %q after a refused close, want open", id, got)
		}
	}

	failing.armed = false
	if _, err := f.finalize(t, failing); err != nil {
		t.Fatalf("retried ProcessControl(workflow-finalize): %v", err)
	}
	for _, id := range f.units {
		assertDrainUnitRetired(t, f.store, id, beadmeta.OutcomeSkipped)
	}
	if got := mustGetBead(t, f.store, f.finalizer.ID).Status; got != "closed" {
		t.Fatalf("finalizer status = %q after the retry, want closed", got)
	}
	f.assertUntouched(t)
}

func TestProcessWorkflowFinalizeRetiresUnitConvoysOfExternallyClosedDrainWritesNoRetiredUnit(t *testing.T) {
	f := newExternallyClosedDrain(t)
	if _, err := f.store.CloseAll(f.units, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, "close_reason": DrainUnitRetiredCloseReason}); err != nil {
		t.Fatalf("retiring units: %v", err)
	}
	recording := &writeRecordingStore{Store: f.store}
	if _, err := f.finalize(t, recording); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	for _, id := range f.units {
		if slices.Contains(recording.written, id) {
			t.Errorf("finalizer wrote already-retired unit %s", id)
		}
		assertDrainUnitRetired(t, f.store, id, beadmeta.OutcomePass)
	}
}
