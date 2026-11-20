package sourceworkflow

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// drainUnitFixture is one drain with two open unit convoys and one closed
// one, beside every bead a retire of that drain must leave alone.
type drainUnitFixture struct {
	store       *beads.MemStore
	drainID     string
	openUnits   []beads.Bead
	closedUnit  beads.Bead
	untouchable []beads.Bead
}

func newDrainUnitFixture(t *testing.T) drainUnitFixture {
	t.Helper()
	store := beads.NewMemStore()
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("Create(%s): %v", b.Title, err)
		}
		return created
	}
	unit := func(drainID, key string) beads.Bead {
		return create(beads.Bead{Title: "drain unit " + key, Type: "convoy", Metadata: map[string]string{
			beadmeta.SyntheticMetadataKey:      "true",
			beadmeta.SyntheticKindMetadataKey:  beadmeta.SyntheticKindDrainUnitConvoy,
			beadmeta.DrainControlIDMetadataKey: drainID,
			beadmeta.DrainUnitKeyMetadataKey:   key,
		}})
	}
	drain := create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindDrain}})
	f := drainUnitFixture{store: store, drainID: drain.ID}

	input := create(beads.Bead{Title: "implement-source", Type: "convoy"})
	pending := create(beads.Bead{Title: "decomposed item", Type: "task", Metadata: map[string]string{
		"gc.publish_state":                    "pending",
		beadmeta.DrainControlIDMetadataKey:    drain.ID,
		beadmeta.DrainMemberIDMetadataKey:     "self",
		beadmeta.ParentConvoyIDMetadataKey:    input.ID,
		beadmeta.DrainMemberAccessMetadataKey: "exclusive",
	}})
	if err := store.DepAdd(pending.ID, input.ID, "discovered-from"); err != nil {
		t.Fatalf("DepAdd(discovered-from): %v", err)
	}
	itemRoot := create(beads.Bead{Title: "item workflow", Type: "molecule", Metadata: map[string]string{
		beadmeta.KindMetadataKey:           beadmeta.KindWorkflow,
		beadmeta.DrainControlIDMetadataKey: drain.ID,
	}})
	forged := create(beads.Bead{Title: "forged unit", Type: "convoy", Metadata: map[string]string{
		beadmeta.SyntheticKindMetadataKey:  beadmeta.SyntheticKindDrainUnitConvoy,
		beadmeta.DrainControlIDMetadataKey: drain.ID,
	}})
	otherDrain := create(beads.Bead{Title: "other drain", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindDrain}})
	otherUnit := unit(otherDrain.ID, "other-0")

	f.openUnits = []beads.Bead{unit(drain.ID, "k-0"), unit(drain.ID, "k-1")}
	closedUnit := unit(drain.ID, "k-2")
	if _, err := store.CloseAll([]string{closedUnit.ID}, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail, "close_reason": "closed before the retire ran"}); err != nil {
		t.Fatalf("closing unit: %v", err)
	}
	f.closedUnit = mustStoreGet(t, store, closedUnit.ID)
	f.untouchable = []beads.Bead{
		mustStoreGet(t, store, input.ID),
		mustStoreGet(t, store, pending.ID),
		mustStoreGet(t, store, itemRoot.ID),
		mustStoreGet(t, store, forged.ID),
		mustStoreGet(t, store, otherDrain.ID),
		mustStoreGet(t, store, otherUnit.ID),
		mustStoreGet(t, store, drain.ID),
	}
	return f
}

func mustStoreGet(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

// assertUntouched fails for any bead whose status or metadata changed.
func assertUntouched(t *testing.T, store beads.Store, before []beads.Bead) {
	t.Helper()
	for _, want := range before {
		got := mustStoreGet(t, store, want.ID)
		if got.Status != want.Status || !reflect.DeepEqual(got.Metadata, want.Metadata) {
			t.Errorf("%s (%q) changed: status %q -> %q, metadata %v -> %v", want.ID, want.Title, want.Status, got.Status, want.Metadata, got.Metadata)
		}
	}
}

func TestRetireDrainUnitConvoysClosesOnlyTheDrainsOpenUnits(t *testing.T) {
	f := newDrainUnitFixture(t)
	n, err := RetireDrainUnitConvoys(beadstest.Resident(f.store), f.drainID, func(beads.Bead) string { return beadmeta.OutcomeSkipped })
	if err != nil {
		t.Fatalf("RetireDrainUnitConvoys: %v", err)
	}
	if n != len(f.openUnits) {
		t.Errorf("closed = %d, want %d", n, len(f.openUnits))
	}
	for _, unit := range f.openUnits {
		got := mustStoreGet(t, f.store, unit.ID)
		if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped || got.Metadata["close_reason"] != DrainUnitRetiredCloseReason {
			t.Errorf("unit %s = status %q outcome %q reason %q, want closed skipped %q", unit.ID, got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], got.Metadata["close_reason"], DrainUnitRetiredCloseReason)
		}
	}
	assertUntouched(t, f.store, append([]beads.Bead{f.closedUnit}, f.untouchable...))
}

func TestRetireDrainUnitConvoysIsANoOpOnceEveryUnitClosed(t *testing.T) {
	f := newDrainUnitFixture(t)
	resident := beadstest.Resident(f.store)
	if _, err := RetireDrainUnitConvoys(resident, f.drainID, func(beads.Bead) string { return beadmeta.OutcomePass }); err != nil {
		t.Fatalf("first RetireDrainUnitConvoys: %v", err)
	}
	var retired []beads.Bead
	for _, unit := range f.openUnits {
		retired = append(retired, mustStoreGet(t, f.store, unit.ID))
	}
	n, err := RetireDrainUnitConvoys(resident, f.drainID, func(beads.Bead) string { return beadmeta.OutcomeCanceled })
	if err != nil {
		t.Fatalf("second RetireDrainUnitConvoys: %v", err)
	}
	if n != 0 {
		t.Errorf("second call closed %d, want 0", n)
	}
	assertUntouched(t, f.store, append(retired, f.closedUnit))
}

func TestRetireDrainUnitConvoysGroupsUnitsByOutcome(t *testing.T) {
	f := newDrainUnitFixture(t)
	outcomes := map[string]string{
		f.openUnits[0].ID: beadmeta.OutcomePass,
		f.openUnits[1].ID: beadmeta.OutcomeFail,
	}
	n, err := RetireDrainUnitConvoys(beadstest.Resident(f.store), f.drainID, func(unit beads.Bead) string { return outcomes[unit.ID] })
	if err != nil {
		t.Fatalf("RetireDrainUnitConvoys: %v", err)
	}
	if n != 2 {
		t.Errorf("closed = %d, want 2", n)
	}
	for id, want := range outcomes {
		if got := mustStoreGet(t, f.store, id).Metadata[beadmeta.OutcomeMetadataKey]; got != want {
			t.Errorf("unit %s outcome = %q, want %q", id, got, want)
		}
	}
}

// failingCloseStore fails every CloseAll.
type failingCloseStore struct {
	*beads.MemStore
}

func (failingCloseStore) CloseAll([]string, map[string]string) (int, error) {
	return 0, errors.New("close refused")
}

func TestRetireDrainUnitConvoysReturnsACloseError(t *testing.T) {
	f := newDrainUnitFixture(t)
	_, err := RetireDrainUnitConvoys(beadstest.Resident(failingCloseStore{f.store}), f.drainID, func(beads.Bead) string { return beadmeta.OutcomeSkipped })
	if err == nil {
		t.Fatal("RetireDrainUnitConvoys over a store that refuses the close returned nil")
	}
	for _, unit := range f.openUnits {
		if got := mustStoreGet(t, f.store, unit.ID).Status; got != "open" {
			t.Errorf("unit %s status = %q, want open", unit.ID, got)
		}
	}
}

func TestDrainControlIDs(t *testing.T) {
	members := []beads.Bead{
		{ID: "d-1", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindDrain}},
		{ID: "s-1", Metadata: map[string]string{beadmeta.KindMetadataKey: "task"}},
		{ID: "d-2", Status: "closed", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindDrain}},
		{ID: "d-1", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindDrain}},
		{ID: "u-1", Metadata: map[string]string{beadmeta.DrainControlIDMetadataKey: "d-1"}},
	}
	if got, want := DrainControlIDs(members), []string{"d-1", "d-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DrainControlIDs = %v, want %v", got, want)
	}
}
