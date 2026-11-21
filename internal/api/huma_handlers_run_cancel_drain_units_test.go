package api

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// runCancelDrainUnitsFixture is a run whose root drain minted one open and
// one closed unit convoy and whose nested drain, under a drain item workflow,
// minted one open unit, beside the drain's member, input convoy and a pending
// decomposed item, which the cancel must leave alone.
type runCancelDrainUnitsFixture struct {
	fs         *fakeState
	store      beads.Store
	runID      string
	openUnits  []string
	closedUnit beads.Bead
	untouched  []beads.Bead
}

func newRunCancelDrainUnitsFixture(t *testing.T) runCancelDrainUnitsFixture {
	t.Helper()
	fs := newFakeState(t)
	store := fs.stores["myrig"]
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	get := func(id string) beads.Bead {
		t.Helper()
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	workflowRoot := func(title, drainControlID string) beads.Bead {
		meta := map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}
		if drainControlID != "" {
			meta[beadmeta.DrainControlIDMetadataKey] = drainControlID
		}
		return create(beads.Bead{Title: title, Type: "molecule", Metadata: meta})
	}
	drainIn := func(title, rootID string) beads.Bead {
		return create(beads.Bead{Title: title, Type: "task", Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: rootID,
			beadmeta.KindMetadataKey:       beadmeta.KindDrain,
		}})
	}
	unit := func(drainID, memberID string) beads.Bead {
		return create(beads.Bead{Title: "drain unit " + memberID, Type: "convoy", Metadata: map[string]string{
			beadmeta.SyntheticMetadataKey:      "true",
			beadmeta.SyntheticKindMetadataKey:  beadmeta.SyntheticKindDrainUnitConvoy,
			beadmeta.DrainControlIDMetadataKey: drainID,
			beadmeta.DrainUnitKeyMetadataKey:   memberID,
			beadmeta.DrainMemberIDMetadataKey:  memberID,
		}})
	}

	input := create(beads.Bead{Title: "implement-source", Type: "convoy"})
	member := create(beads.Bead{Title: "drain member", Type: "task"})
	run := workflowRoot("implement run", "")
	drain := drainIn("drain work", run.ID)
	pending := create(beads.Bead{Title: "decomposed item", Type: "task", Metadata: map[string]string{
		"gc.publish_state":                    "pending",
		beadmeta.DrainControlIDMetadataKey:    drain.ID,
		beadmeta.ParentConvoyIDMetadataKey:    input.ID,
		beadmeta.DrainMemberAccessMetadataKey: "exclusive",
	}})
	if err := store.DepAdd(pending.ID, input.ID, "discovered-from"); err != nil {
		t.Fatal(err)
	}
	item := workflowRoot("do-work item", drain.ID)
	nestedDrain := drainIn("nested drain", item.ID)
	workflowRoot("nested item", nestedDrain.ID)

	f := runCancelDrainUnitsFixture{fs: fs, store: store, runID: run.ID}
	f.openUnits = []string{unit(drain.ID, member.ID).ID, unit(nestedDrain.ID, pending.ID).ID}
	closedUnit := unit(drain.ID, "already-done")
	if _, err := store.CloseAll([]string{closedUnit.ID}, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, "close_reason": sourceworkflow.DrainUnitRetiredCloseReason}); err != nil {
		t.Fatal(err)
	}
	f.closedUnit = get(closedUnit.ID)
	f.untouched = []beads.Bead{get(input.ID), get(member.ID), get(pending.ID)}
	return f
}

func (f runCancelDrainUnitsFixture) cancel() (*RunCancelOutput, error) {
	s := &Server{state: f.fs}
	return s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     f.runID,
	})
}

func (f runCancelDrainUnitsFixture) assertRetired(t *testing.T) {
	t.Helper()
	for _, id := range f.openUnits {
		unit, err := f.store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if unit.Status != "closed" || unit.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeCanceled || unit.Metadata["close_reason"] != sourceworkflow.DrainUnitRetiredCloseReason {
			t.Errorf("unit %s = status %q outcome %q reason %q, want closed canceled %q", id, unit.Status, unit.Metadata[beadmeta.OutcomeMetadataKey], unit.Metadata["close_reason"], sourceworkflow.DrainUnitRetiredCloseReason)
		}
	}
	for _, want := range append([]beads.Bead{f.closedUnit}, f.untouched...) {
		got, err := f.store.Get(want.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want.Status || !reflect.DeepEqual(got.Metadata, want.Metadata) {
			t.Errorf("%s (%q) changed by the cancel: status %q -> %q, metadata %v -> %v", want.ID, want.Title, want.Status, got.Status, want.Metadata, got.Metadata)
		}
	}
}

func TestRunCancelRetiresDrainUnitConvoys(t *testing.T) {
	f := newRunCancelDrainUnitsFixture(t)
	out, err := f.cancel()
	if err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("status = %q, want canceled", out.Body.Status)
	}
	f.assertRetired(t)
	// run + drain + item + nested drain + nested item, plus the two open units.
	if out.Body.Closed != 7 {
		t.Errorf("closed = %d, want 7 (five workflow beads and the two open units)", out.Body.Closed)
	}
}

// unitCloseRefusingStore refuses every CloseAll naming one of its units while
// armed.
type unitCloseRefusingStore struct {
	beads.Store
	units []string
	armed bool
}

func (s *unitCloseRefusingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if s.armed && slices.ContainsFunc(ids, func(id string) bool { return slices.Contains(s.units, id) }) {
		return 0, errors.New("unit close refused")
	}
	return s.Store.CloseAll(ids, metadata)
}

func TestRunCancelRetiresDrainUnitConvoysBeforeTheRunRootCloses(t *testing.T) {
	f := newRunCancelDrainUnitsFixture(t)
	refusing := &unitCloseRefusingStore{Store: f.store, units: f.openUnits, armed: true}
	f.fs.stores["myrig"] = refusing

	if _, err := f.cancel(); err == nil {
		t.Fatal("cancel over a store refusing the unit close returned nil")
	}
	root, err := f.store.Get(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status == "closed" {
		t.Fatal("run root closed although its drain unit convoys could not be")
	}

	refusing.armed = false
	out, err := f.cancel()
	if err != nil {
		t.Fatalf("retried cancel error: %v", err)
	}
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("retried status = %q, want canceled", out.Body.Status)
	}
	if root, err = f.store.Get(f.runID); err != nil {
		t.Fatal(err)
	}
	if root.Status != "closed" {
		t.Errorf("run root status = %q after the retry, want closed", root.Status)
	}
	f.assertRetired(t)
}
