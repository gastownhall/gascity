package dispatch

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
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

// The drains here expand graph.v2 item formulas with no formula_v2 gate set:
// the compiler's v2 flag defaults to enabled (the formula package's init and
// rollout.ForTest's defaults agree), and no test in this package turns it off.
func newExternallyClosedDrain(t *testing.T) externallyClosedDrain {
	t.Helper()
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

// unitCloseFailingStore fails every CloseAll naming one of its units with err
// while armed.
type unitCloseFailingStore struct {
	beads.Store
	units []string
	err   error
	armed bool
}

func (s *unitCloseFailingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if s.armed && slices.ContainsFunc(ids, func(id string) bool { return slices.Contains(s.units, id) }) {
		return 0, s.err
	}
	return s.Store.CloseAll(ids, metadata)
}

// TestProcessWorkflowFinalizeRetiresUnitConvoysOfExternallyClosedDrainKeepsFinalizerOpenOnCloseFailure
// pins the transient half of the finalizer's retirement policy: a unit close
// the store refuses for now keeps the finalizer open with its error recorded,
// which the control dispatcher retries, and the retry closes the rest.
func TestProcessWorkflowFinalizeRetiresUnitConvoysOfExternallyClosedDrainKeepsFinalizerOpenOnCloseFailure(t *testing.T) {
	f := newExternallyClosedDrain(t)
	failing := &unitCloseFailingStore{Store: f.store, units: f.units, err: errors.New("database is locked"), armed: true}
	_, err := f.finalize(t, failing)
	if err == nil {
		t.Fatal("ProcessControl(workflow-finalize) over a store refusing the unit close returned nil")
	}
	if !IsTransientControllerError(err) {
		t.Fatalf("ProcessControl(workflow-finalize) = %v, want the transient unit close error the dispatcher retries", err)
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

// TestProcessWorkflowFinalizeDefersANonTransientUnitRetireError pins the other
// half: a unit convoy is bookkeeping, as in closeDrainControl, so a retirement
// error no retry clears is traced and left to `gc convoy check`, and the
// workflow still settles. Returned, the control dispatcher would quarantine the
// finalizer of a passed workflow before its source chain closed.
func TestProcessWorkflowFinalizeDefersANonTransientUnitRetireError(t *testing.T) {
	f := newExternallyClosedDrain(t)
	failing := &unitCloseFailingStore{Store: f.store, units: f.units, err: errors.New("unit close refused"), armed: true}
	var traces []string
	result, err := ProcessControl(beadstest.Graph(failing), mustGetBead(t, f.store, f.finalizer.ID), ProcessOptions{
		Work:   beadstest.Work(failing),
		Tracef: func(format string, args ...any) { traces = append(traces, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize) = %v, want the non-transient unit close error deferred", err)
	}
	if result.Action != "workflow-pass" {
		t.Fatalf("workflow result = %+v, want workflow-pass", result)
	}
	if got := mustGetBead(t, f.store, f.finalizer.ID).Status; got != "closed" {
		t.Fatalf("finalizer status = %q, want closed", got)
	}
	for _, id := range f.units {
		if got := mustGetBead(t, f.store, id).Status; got != "open" {
			t.Fatalf("unit %s status = %q after a refused close, want it left open for gc convoy check", id, got)
		}
	}
	if !slices.ContainsFunc(traces, func(line string) bool {
		return strings.Contains(line, "retire-units-deferred") && strings.Contains(line, "unit close refused")
	}) {
		t.Fatalf("traces = %q, want the deferred unit close error traced", traces)
	}
}

// TestProcessWorkflowFinalizeRetiresUnitConvoysOfASplitCityDrain runs the
// finalizer with the options the control dispatcher gives it on a city whose
// graph class relocated: Work is the scope store and there is no member tail,
// which only the kinds that read beads they did not create take
// (controlMemberTail). The drain minted its units in the work store, through
// its tail, so the finalizer must read them there.
func TestProcessWorkflowFinalizeRetiresUnitConvoysOfASplitCityDrain(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	work, graph, drain, _ := seedSplitClassDrainWorkflow(t, false)
	if _, err := ProcessControl(beadstest.Graph(graph), drain, ProcessOptions{Work: beadstest.Work(work), MemberTail: true, FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	units := drainUnitConvoysFor(t, work, drain.ID)
	if len(units) != 2 {
		t.Fatalf("work store holds %d unit convoys of drain %s after expansion, want 2", len(units), drain.ID)
	}
	if _, err := graph.CloseAll([]string{drain.ID}, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}); err != nil {
		t.Fatalf("closing drain %s: %v", drain.ID, err)
	}
	finalizer := mustCreateWorkflowBead(t, graph, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       "workflow-finalize",
			beadmeta.RootBeadIDMetadataKey: drain.Metadata[beadmeta.RootBeadIDMetadataKey],
		},
	})
	var traces []string
	result, err := ProcessControl(beadstest.Graph(graph), finalizer, ProcessOptions{
		Work:   beadstest.Work(work),
		Tracef: func(format string, args ...any) { traces = append(traces, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if result.Action != "workflow-pass" {
		t.Fatalf("workflow result = %+v, want workflow-pass", result)
	}
	for _, unit := range units {
		assertDrainUnitRetired(t, work, unit.ID, beadmeta.OutcomeSkipped)
	}
	if !slices.ContainsFunc(traces, func(line string) bool { return strings.Contains(line, "retired-units=2") }) {
		t.Fatalf("traces = %q, want the finalizer to trace the two units it retired", traces)
	}
}

// TestProcessWorkflowFinalizeRetiresUnitConvoysWithTheirRowsVerdicts: a shared
// drain records each row's verdict while it is still open, so a drain closed
// some other way can leave open a unit whose row already succeeded or failed.
// The finalizer retires that unit with its row's verdict, as the drain's own
// close and `gc convoy check` do, and only a unit whose row reached no verdict
// with the sweep's outcome.
func TestProcessWorkflowFinalizeRetiresUnitConvoysWithTheirRowsVerdicts(t *testing.T) {
	for _, itemOutcome := range []string{beadmeta.OutcomePass, beadmeta.OutcomeFail} {
		t.Run(itemOutcome, func(t *testing.T) {
			dir := t.TempDir()
			writeDrainItemFormula(t, dir)
			store, drain := seedDrainWorkflow(t)
			if err := store.SetMetadataBatch(drain.ID, map[string]string{
				beadmeta.DrainContextMetadataKey:       beadmeta.DrainContextShared,
				beadmeta.DrainOnItemFailureMetadataKey: beadmeta.DrainOnItemFailureContinue,
			}); err != nil {
				t.Fatalf("SetMetadataBatch(shared drain): %v", err)
			}
			opts := ProcessOptions{Work: beadstest.Work(store), FormulaSearchPaths: []string{dir}}
			if _, err := ProcessControl(beadstest.Graph(store), mustGetBead(t, store, drain.ID), opts); err != nil {
				t.Fatalf("ProcessControl(shared first): %v", err)
			}
			first := mustDrainManifest(t, mustGetBead(t, store, drain.ID)).Rows[0]
			if err := updateMetadataAndClose(beadstest.Graph(store), first.ItemRootID, map[string]string{beadmeta.OutcomeMetadataKey: itemOutcome}); err != nil {
				t.Fatalf("closing the first item root %s: %v", first.ItemRootID, err)
			}
			if _, err := ProcessControl(beadstest.Graph(store), mustGetBead(t, store, drain.ID), opts); err != nil {
				t.Fatalf("ProcessControl(shared second): %v", err)
			}
			manifest := mustDrainManifest(t, mustGetBead(t, store, drain.ID))
			if row := manifest.Rows[0]; row.Status != "succeeded" && row.Status != "failed" {
				t.Fatalf("row 0 = %+v, want its verdict recorded on the open drain", row)
			}
			if manifest.Rows[1].UnitConvoyID == "" {
				t.Fatalf("row 1 = %+v, want its unit minted", manifest.Rows[1])
			}
			for _, row := range manifest.Rows {
				if got := mustGetBead(t, store, row.UnitConvoyID).Status; got != "open" {
					t.Fatalf("unit %s of row %d status = %q before the drain closed, want open", row.UnitConvoyID, row.Index, got)
				}
			}
			if _, err := store.CloseAll([]string{drain.ID}, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}); err != nil {
				t.Fatalf("closing drain %s: %v", drain.ID, err)
			}
			finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
				Title: "Finalize workflow",
				Type:  "task",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:       "workflow-finalize",
					beadmeta.RootBeadIDMetadataKey: drain.Metadata[beadmeta.RootBeadIDMetadataKey],
				},
			})
			if _, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)}); err != nil {
				t.Fatalf("ProcessControl(workflow-finalize): %v", err)
			}
			assertDrainUnitRetired(t, store, manifest.Rows[0].UnitConvoyID, itemOutcome)
			assertDrainUnitRetired(t, store, manifest.Rows[1].UnitConvoyID, beadmeta.OutcomeSkipped)
		})
	}
}

// TestProcessWorkflowFinalizeLeavesTheUnitConvoysOfATeardownDrainOpen: the
// teardown tail stays executable once its root is terminal, so the terminal
// sweep spares it. A teardown drain still running keeps its units open too,
// for its own close to retire with their rows' verdicts.
func TestProcessWorkflowFinalizeLeavesTheUnitConvoysOfATeardownDrainOpen(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadataBatch(drain.ID, map[string]string{
		beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
		beadmeta.StepIDMetadataKey:    "release",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(teardown drain): %v", err)
	}
	if _, err := ProcessControl(beadstest.Graph(store), mustGetBead(t, store, drain.ID), ProcessOptions{Work: beadstest.Work(store), FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	units := drainUnitConvoysFor(t, store, drain.ID)
	if len(units) != 2 {
		t.Fatalf("store holds %d unit convoys of drain %s after expansion, want 2", len(units), drain.ID)
	}
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       "workflow-finalize",
			beadmeta.RootBeadIDMetadataKey: drain.Metadata[beadmeta.RootBeadIDMetadataKey],
		},
	})
	if _, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)}); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if got := mustGetBead(t, store, finalizer.ID).Status; got != "closed" {
		t.Fatalf("finalizer status = %q, want closed", got)
	}
	if got := mustGetBead(t, store, drain.ID).Status; got == "closed" {
		t.Fatalf("teardown drain %s closed by the terminal sweep, want it spared", drain.ID)
	}
	for _, unit := range units {
		if got := mustGetBead(t, store, unit.ID); got.Status != "open" {
			t.Errorf("unit %s of the running teardown drain = status %q outcome %q, want open", unit.ID, got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
}
