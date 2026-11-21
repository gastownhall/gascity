package api

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// runCancelBeads builds the beads of a run-cancel test, each in the store it
// is handed.
type runCancelBeads struct {
	t *testing.T
}

func (b runCancelBeads) create(store beads.Store, bead beads.Bead) beads.Bead {
	b.t.Helper()
	created, err := store.Create(bead)
	if err != nil {
		b.t.Fatal(err)
	}
	return created
}

func (b runCancelBeads) get(store beads.Store, id string) beads.Bead {
	b.t.Helper()
	got, err := store.Get(id)
	if err != nil {
		b.t.Fatal(err)
	}
	return got
}

// workflowRoot creates a graph.v2 workflow root, a drain item workflow's when
// drainControlID names its drain.
func (b runCancelBeads) workflowRoot(store beads.Store, title, drainControlID string) beads.Bead {
	b.t.Helper()
	meta := map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, "gc.formula_contract": "graph.v2"}
	if drainControlID != "" {
		meta[beadmeta.DrainControlIDMetadataKey] = drainControlID
	}
	return b.create(store, beads.Bead{Title: title, Type: "molecule", Metadata: meta})
}

// drain creates a drain control in the workflow rootID, with extra metadata.
func (b runCancelBeads) drain(store beads.Store, title, rootID string, extra map[string]string) beads.Bead {
	b.t.Helper()
	meta := map[string]string{beadmeta.RootBeadIDMetadataKey: rootID, beadmeta.KindMetadataKey: beadmeta.KindDrain}
	for k, v := range extra {
		meta[k] = v
	}
	return b.create(store, beads.Bead{Title: title, Type: "task", Metadata: meta})
}

// unit creates an open unit convoy of the drain drainID, keyed unitKey.
func (b runCancelBeads) unit(store beads.Store, drainID, unitKey string) beads.Bead {
	b.t.Helper()
	return b.create(store, beads.Bead{Title: "drain unit " + unitKey, Type: "convoy", Metadata: map[string]string{
		beadmeta.SyntheticMetadataKey:      "true",
		beadmeta.SyntheticKindMetadataKey:  beadmeta.SyntheticKindDrainUnitConvoy,
		beadmeta.DrainControlIDMetadataKey: drainID,
		beadmeta.DrainUnitKeyMetadataKey:   unitKey,
	}})
}

// assertUnitRetired fails unless unitID is closed as outcome with the drain
// unit retirement reason.
func (b runCancelBeads) assertUnitRetired(store beads.Store, unitID, outcome string) {
	b.t.Helper()
	unit := b.get(store, unitID)
	if unit.Status != "closed" || unit.Metadata[beadmeta.OutcomeMetadataKey] != outcome || unit.Metadata["close_reason"] != sourceworkflow.DrainUnitRetiredCloseReason {
		b.t.Errorf("unit %s = status %q outcome %q reason %q, want closed %s %q", unitID, unit.Status, unit.Metadata[beadmeta.OutcomeMetadataKey], unit.Metadata["close_reason"], outcome, sourceworkflow.DrainUnitRetiredCloseReason)
	}
}

// cancelRunByID cancels runID through the run-cancel handler over state.
func cancelRunByID(state State, runID string) (*RunCancelOutput, error) {
	s := &Server{state: state}
	return s.humaHandleRunCancel(context.Background(), &RunCancelInput{
		CityScope: CityScope{CityName: "test-city"},
		RunID:     runID,
	})
}

// runCancelDrainUnitsFixture is a run on a split city whose root drain minted
// one open and one closed unit convoy and whose nested drain, under a drain
// item workflow, minted one open unit, beside the drain's member, input convoy
// and a pending decomposed item, which the cancel must leave alone. The
// workflow beads live in the graph binding and the work beads, the units among
// them, in the rig's work store, where a drain mints them.
type runCancelDrainUnitsFixture struct {
	runCancelBeads
	state      relicReportingState
	binding    beads.Store
	work       beads.Store
	runID      string
	openUnits  []string
	closedUnit beads.Bead
	untouched  []beads.Bead
}

func newRunCancelDrainUnitsFixture(t *testing.T) runCancelDrainUnitsFixture {
	t.Helper()
	b := runCancelBeads{t: t}
	state, binding, work := newS4APIState(t, true)

	input := b.create(work, beads.Bead{Title: "implement-source", Type: "convoy"})
	member := b.create(work, beads.Bead{Title: "drain member", Type: "task"})
	run := b.workflowRoot(binding, "implement run", "")
	drain := b.drain(binding, "drain work", run.ID, nil)
	pending := b.create(work, beads.Bead{Title: "decomposed item", Type: "task", Metadata: map[string]string{
		"gc.publish_state":                    "pending",
		beadmeta.DrainControlIDMetadataKey:    drain.ID,
		beadmeta.ParentConvoyIDMetadataKey:    input.ID,
		beadmeta.DrainMemberAccessMetadataKey: "exclusive",
	}})
	if err := work.DepAdd(pending.ID, input.ID, "discovered-from"); err != nil {
		t.Fatal(err)
	}
	item := b.workflowRoot(binding, "do-work item", drain.ID)
	nestedDrain := b.drain(binding, "nested drain", item.ID, nil)
	b.workflowRoot(binding, "nested item", nestedDrain.ID)

	f := runCancelDrainUnitsFixture{runCancelBeads: b, state: state, binding: binding, work: work, runID: run.ID}
	f.openUnits = []string{b.unit(work, drain.ID, member.ID).ID, b.unit(work, nestedDrain.ID, pending.ID).ID}
	closedUnit := b.unit(work, drain.ID, "already-done")
	if _, err := work.CloseAll([]string{closedUnit.ID}, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, "close_reason": sourceworkflow.DrainUnitRetiredCloseReason}); err != nil {
		t.Fatal(err)
	}
	f.closedUnit = b.get(work, closedUnit.ID)
	f.untouched = []beads.Bead{b.get(work, input.ID), b.get(work, member.ID), b.get(work, pending.ID)}
	return f
}

func (f runCancelDrainUnitsFixture) cancel() (*RunCancelOutput, error) {
	return cancelRunByID(f.state, f.runID)
}

func (f runCancelDrainUnitsFixture) assertRetired(t *testing.T) {
	t.Helper()
	for _, id := range f.openUnits {
		f.assertUnitRetired(f.work, id, beadmeta.OutcomeCanceled)
	}
	for _, want := range append([]beads.Bead{f.closedUnit}, f.untouched...) {
		got := f.get(f.work, want.ID)
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
	refusing := &unitCloseRefusingStore{Store: f.work, units: f.openUnits, armed: true}
	f.state.stores["myrig"] = refusing

	if _, err := f.cancel(); err == nil {
		t.Fatal("cancel over a store refusing the unit close returned nil")
	}
	if root := f.get(f.binding, f.runID); root.Status == "closed" {
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
	if root := f.get(f.binding, f.runID); root.Status != "closed" {
		t.Errorf("run root status = %q after the retry, want closed", root.Status)
	}
	f.assertRetired(t)
}

// TestRunCancelRetiresDrainUnitConvoysInARelicFreeRig: on a relocated city the
// run's workflows live in the binding, and a rig proven relic-free is no
// workflow leg, but its store is still the work store its drains minted units
// in. The cancel retires them there.
func TestRunCancelRetiresDrainUnitConvoysInARelicFreeRig(t *testing.T) {
	b := runCancelBeads{t: t}
	state, binding, rig := newS4APIState(t, true)
	state.relicFree["myrig"] = true
	run := b.workflowRoot(binding, "implement run", "")
	drain := b.drain(binding, "drain work", run.ID, nil)
	unit := b.unit(rig, drain.ID, "rig-member")

	out, err := cancelRunByID(state, run.ID)
	if err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	if out.Body.Status != RunStatusCanceled {
		t.Errorf("status = %q, want canceled", out.Body.Status)
	}
	b.assertUnitRetired(rig, unit.ID, beadmeta.OutcomeCanceled)
	// run + drain, plus the rig's unit.
	if out.Body.Closed != 3 {
		t.Errorf("closed = %d, want 3 (the run, its drain and the rig's unit)", out.Body.Closed)
	}
}

// TestRunCancelRetiresDrainUnitConvoysInTheCityBesideARigNamedLikeIt: the work
// ledger's listing (storeref.WorkList) lists a rig named like the city in the
// city's place, but a city-scoped run's drains mint their units in the city's
// own work store, on a split city and an unsplit one alike. The cancel reads
// the city's work store whatever its rigs are named, and the rig's too, and
// retires the units in both before the run root closes: while the city refuses
// the unit close, the cancel fails and the run stays open for the retry.
func TestRunCancelRetiresDrainUnitConvoysInTheCityBesideARigNamedLikeIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		split bool
	}{
		{name: "split city", split: true},
		{name: "unsplit city", split: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.split {
				// An unsplit city's workflow rows sit in its work store, which
				// the split-strict gate's work view hides.
				t7dGateOff(t)
			}
			b := runCancelBeads{t: t}
			state, binding, _ := newS4APIState(t, tc.split)
			city := state.cityBeadStore
			named := &beads.MemStore{IDPrefix: "tx"}
			state.stores[state.cityName] = named
			state.cfg.Rigs = append(state.cfg.Rigs, config.Rig{Name: state.cityName, Path: "/nonexistent/named"})
			home := binding
			if !tc.split {
				home = city
			}
			run := b.workflowRoot(home, "implement run", "")
			drain := b.drain(home, "drain work", run.ID, nil)
			cityUnit := b.unit(city, drain.ID, "city-member")
			rigUnit := b.unit(named, drain.ID, "rig-member")

			refusing := &unitCloseRefusingStore{Store: city, units: []string{cityUnit.ID}, armed: true}
			state.cityBeadStore = refusing
			if _, err := cancelRunByID(state, run.ID); err == nil {
				t.Fatal("cancel over a city store refusing the unit close returned nil: the city's work store was not read for the run's drain units")
			}
			if root := b.get(home, run.ID); root.Status == "closed" {
				t.Fatal("run root closed although its drain unit convoy in the city's work store could not be")
			}

			refusing.armed = false
			out, err := cancelRunByID(state, run.ID)
			if err != nil {
				t.Fatalf("retried cancel error: %v", err)
			}
			if out.Body.Status != RunStatusCanceled {
				t.Errorf("retried status = %q, want canceled", out.Body.Status)
			}
			if root := b.get(home, run.ID); root.Status != "closed" {
				t.Errorf("run root status = %q after the retry, want closed", root.Status)
			}
			b.assertUnitRetired(city, cityUnit.ID, beadmeta.OutcomeCanceled)
			b.assertUnitRetired(named, rigUnit.ID, beadmeta.OutcomeCanceled)
		})
	}
}

// unitListCountingStore counts the drain unit convoy listings it serves.
type unitListCountingStore struct {
	beads.Store
	lists int
}

func (s *unitListCountingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if filters[beadmeta.SyntheticKindMetadataKey] == beadmeta.SyntheticKindDrainUnitConvoy {
		s.lists++
	}
	return s.Store.ListByMetadata(filters, limit, opts...)
}

// TestRunCancelListsADrainsUnitsInTheCityWorkStoreOnce: the cancel reads the
// city's work store for a drain's units once, both where the work ledger's
// listing reads that store and where a rig named like the city takes its
// place there, so that the store is read on its own.
func TestRunCancelListsADrainsUnitsInTheCityWorkStoreOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		named bool
	}{
		{name: "no rig named like the city"},
		{name: "a rig named like the city", named: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := runCancelBeads{t: t}
			state, binding, _ := newS4APIState(t, true)
			city := &unitListCountingStore{Store: state.cityBeadStore}
			state.cityBeadStore = city
			if tc.named {
				state.stores[state.cityName] = &beads.MemStore{IDPrefix: "tx"}
			}
			run := b.workflowRoot(binding, "implement run", "")
			b.drain(binding, "drain work", run.ID, nil)

			if _, err := cancelRunByID(state, run.ID); err != nil {
				t.Fatalf("cancel error: %v", err)
			}
			if city.lists != 1 {
				t.Errorf("the cancel listed the drain's units in the city's work store %d times, want once", city.lists)
			}
		})
	}
}

// TestRunCancelRetiresDrainUnitConvoysWithTheirRowsVerdicts: a shared drain
// records each row's verdict while it is still open, and the cancel retires
// before the drain closes. A unit whose row already succeeded or failed keeps
// that verdict, as the drain's own close and `gc convoy check` give it; only a
// unit whose row reached none closes canceled.
func TestRunCancelRetiresDrainUnitConvoysWithTheirRowsVerdicts(t *testing.T) {
	b := runCancelBeads{t: t}
	state, binding, work := newS4APIState(t, true)
	run := b.workflowRoot(binding, "implement run", "")
	drain := b.drain(binding, "drain work", run.ID, map[string]string{
		beadmeta.DrainManifestMetadataKey: `{"version":1,"context":"shared","rows":[` +
			`{"index":0,"member_id":"m-0","unit_key":"k-0","status":"succeeded"},` +
			`{"index":1,"member_id":"m-1","unit_key":"k-1","status":"failed"},` +
			`{"index":2,"member_id":"m-2","unit_key":"k-2","status":"unit-created"}]}`,
	})
	succeeded := b.unit(work, drain.ID, "k-0")
	failed := b.unit(work, drain.ID, "k-1")
	pending := b.unit(work, drain.ID, "k-2")

	if _, err := cancelRunByID(state, run.ID); err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	b.assertUnitRetired(work, succeeded.ID, beadmeta.OutcomePass)
	b.assertUnitRetired(work, failed.ID, beadmeta.OutcomeFail)
	b.assertUnitRetired(work, pending.ID, beadmeta.OutcomeCanceled)
}

// TestRunCancelLeavesTheUnitConvoysOfTeardownDrainsOpen: the cancel spares each
// workflow's teardown tail, which runs after its root is terminal, and a nested
// item workflow already closed keeps running its own. A teardown drain still
// running keeps its units open too, for its own close to retire with their
// rows' verdicts.
func TestRunCancelLeavesTheUnitConvoysOfTeardownDrainsOpen(t *testing.T) {
	b := runCancelBeads{t: t}
	state, binding, work := newS4APIState(t, true)
	run := b.workflowRoot(binding, "implement run", "")
	runTeardown := b.drain(binding, "release run", run.ID, map[string]string{
		beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
		beadmeta.StepIDMetadataKey:    "release-run",
	})
	drain := b.drain(binding, "drain work", run.ID, nil)
	item := b.workflowRoot(binding, "do-work item", drain.ID)
	itemTeardown := b.drain(binding, "release item", item.ID, map[string]string{
		beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
		beadmeta.StepIDMetadataKey:    "release-item",
	})
	if _, err := binding.CloseAll([]string{item.ID}, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}); err != nil {
		t.Fatal(err)
	}
	runTeardownUnit := b.unit(work, runTeardown.ID, "release-run-0")
	itemTeardownUnit := b.unit(work, itemTeardown.ID, "release-item-0")
	drainUnit := b.unit(work, drain.ID, "work-0")

	if _, err := cancelRunByID(state, run.ID); err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	b.assertUnitRetired(work, drainUnit.ID, beadmeta.OutcomeCanceled)
	for _, id := range []string{runTeardown.ID, itemTeardown.ID} {
		if got := b.get(binding, id); got.Status != "open" {
			t.Errorf("teardown drain %s (%q) = status %q after the cancel, want open", id, got.Title, got.Status)
		}
	}
	for _, id := range []string{runTeardownUnit.ID, itemTeardownUnit.ID} {
		if got := b.get(work, id); got.Status != "open" {
			t.Errorf("%s (%q) = status %q outcome %q after the cancel, want open", id, got.Title, got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
}
