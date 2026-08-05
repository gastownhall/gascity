package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sling"
)

// These are the golden tests for FIX-1 (promoteReadyFireRoots), mirroring
// route_recovery_test.go's ZFC tests. They assert the five acceptance criteria
// from FIX1-SPEC-auto-fire-ready-fire-roots.md:
//
//  1. Blocked→ready fires — a fire-root gated by an open bead does not fire;
//     closing the gate makes it bd ready and the next tick cooks it.
//  2. Idempotent — a fire-root that already has a live convoy is not re-cooked.
//  3. Declared-only (ZFC) — a ready bead with no gc.fire_formula, or a
//     control/topology bead, is left untouched.
//  4. Vars pass-through — gc.fire_vars reach the cook 1:1 (reserved vars dropped).
//  5. No mail in the loop — the whole path is bead-state → cook; no message bead.
//
// The controller's real cook (CityRuntime.cookFireRoot) is a thin wrapper over
// the well-tested molecule.Cook layer; these tests inject a recorder cooker
// through the same seam so the judgment-free selection/idempotency logic is
// exercised hermetically, exactly as route_recovery's tests drive
// restoreCarriedWorkRoutes without a live pool.

const testFireFormula = "build-from-plan"

type recordedFire struct {
	fireRootID  string
	formulaName string
	vars        map[string]string
}

type fireRecorder struct {
	calls []recordedFire
}

func (r *fireRecorder) cook(fireRoot beads.Bead, formulaName string, vars map[string]string) error {
	r.calls = append(r.calls, recordedFire{fireRootID: fireRoot.ID, formulaName: formulaName, vars: vars})
	return nil
}

// TestPromoteReadyFireRootsBlockedToReadyFires covers criterion 1: the
// blocked→ready transition IS the GO. A fire-root blocked-by an open gate bead
// is absent from store.Ready() and must not fire; closing the gate — a pure
// bead-state change — makes it ready and the next call cooks it exactly once
// with the declared formula.
func TestPromoteReadyFireRootsBlockedToReadyFires(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "G-1", Title: "auth gate", Type: "task", Status: "open"},
		{ID: "FR-1", Title: "build fire-root", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: testFireFormula,
			beadmeta.FireVarsMetadataKey:    `{"base_ref":"dev","requirements_path":"r.md"}`,
		}},
	}, []beads.Dep{{IssueID: "FR-1", DependsOnID: "G-1", Type: "blocks"}})

	rec := &fireRecorder{}
	fired, err := promoteReadyFireRoots(store, rec.cook)
	if err != nil {
		t.Fatalf("promoteReadyFireRoots (gate open): %v", err)
	}
	if fired != 0 || len(rec.calls) != 0 {
		t.Fatalf("fired=%d calls=%d while gate open, want 0/0 (a blocked fire-root must not fire)", fired, len(rec.calls))
	}

	if err := store.Close("G-1"); err != nil {
		t.Fatalf("closing gate: %v", err)
	}
	fired, err = promoteReadyFireRoots(store, rec.cook)
	if err != nil {
		t.Fatalf("promoteReadyFireRoots (gate closed): %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired=%d after gate closed, want 1", fired)
	}
	if len(rec.calls) != 1 || rec.calls[0].fireRootID != "FR-1" || rec.calls[0].formulaName != testFireFormula {
		t.Fatalf("cook calls=%+v, want one cook of FR-1 with %q", rec.calls, testFireFormula)
	}
}

// TestPromoteReadyFireRootsIdempotent covers criterion 2: a fire-root that
// already has a LIVE molecule/workflow child has been fired and must not be
// re-cooked (no double-fire), whether the link is the fire-root's workflow_id
// forward pointer or a parented graph.v2 workflow root. A fire-root whose only
// child is CLOSED is not live and remains eligible — the idempotency is
// live-aware.
func TestPromoteReadyFireRootsIdempotent(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		// Already fired — forward workflow_id pointer to a live workflow root.
		{ID: "FR-forward", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: testFireFormula,
			"workflow_id":                   "WF-live",
		}},
		{ID: "WF-live", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
		}},
		// Already fired — a live graph.v2 workflow root parented on the fire-root.
		{ID: "FR-parented", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: testFireFormula,
		}},
		{ID: "WF-child", Type: "task", Status: "open", ParentID: "FR-parented", Metadata: map[string]string{
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}},
		// Stale — its only workflow child is CLOSED, so the fire-root is NOT
		// already live and is eligible to fire.
		{ID: "FR-stale", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: testFireFormula,
			"workflow_id":                   "WF-closed",
		}},
		{ID: "WF-closed", Type: "task", Status: "closed", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
		}},
	}, nil)

	rec := &fireRecorder{}
	fired, err := promoteReadyFireRoots(store, rec.cook)
	if err != nil {
		t.Fatalf("promoteReadyFireRoots: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired=%d, want 1 (only the stale-child fire-root re-fires; the two live ones do not)", fired)
	}
	if len(rec.calls) != 1 || rec.calls[0].fireRootID != "FR-stale" {
		t.Fatalf("cook calls=%+v, want only FR-stale", rec.calls)
	}
}

// TestPromoteReadyFireRootsDeclaredOnlyZFC covers criterion 3: only a plain,
// unassigned, ready bead that DECLARES gc.fire_formula is a fire-root. A bead
// with no fire_formula, a control-dispatcher/topology bead (gc.kind set), and an
// already-claimed fire-root are all left untouched — the promotion invents
// nothing.
func TestPromoteReadyFireRootsDeclaredOnlyZFC(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		// Plain ready work with NO fire_formula — untouched (its owner slings it).
		{ID: "PLAIN", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.RunTargetMetadataKey: "rig/polecat",
		}},
		// Control-dispatcher (gc.kind set) that also declares a fire_formula —
		// skipped; a formula-cook is never a control bead's intent.
		{ID: "CTRL", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:        "retry",
			beadmeta.FireFormulaMetadataKey: testFireFormula,
		}},
		// Assigned fire-root — already being handled by a claimant, not re-fired.
		{ID: "ASSIGNED", Type: "task", Status: "open", Assignee: "rig/polecat/th-1", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: testFireFormula,
		}},
		// A genuine ready fire-root — the only one that fires.
		{ID: "FR-ok", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: testFireFormula,
		}},
	}, nil)

	rec := &fireRecorder{}
	fired, err := promoteReadyFireRoots(store, rec.cook)
	if err != nil {
		t.Fatalf("promoteReadyFireRoots: %v", err)
	}
	if fired != 1 || len(rec.calls) != 1 || rec.calls[0].fireRootID != "FR-ok" {
		t.Fatalf("fired=%d calls=%+v, want exactly FR-ok (ZFC: only a declared, plain, unassigned fire-root)", fired, rec.calls)
	}
}

// TestPromoteReadyFireRootsVarsPassThrough covers criterion 4: the declared
// gc.fire_vars reach the cook 1:1, and reserved graph.v2 runtime vars
// (convoy_id/issue/bead_id) are dropped by the canonical decoder so they can
// never be caller-supplied.
func TestPromoteReadyFireRootsVarsPassThrough(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "FR-1", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: testFireFormula,
			beadmeta.FireVarsMetadataKey:    `{"base_ref":"release/di-core","requirements_path":"docs/req.md","artifact_root":"/art","convoy_id":"SHOULD-BE-DROPPED"}`,
		}},
	}, nil)

	rec := &fireRecorder{}
	fired, err := promoteReadyFireRoots(store, rec.cook)
	if err != nil {
		t.Fatalf("promoteReadyFireRoots: %v", err)
	}
	if fired != 1 || len(rec.calls) != 1 {
		t.Fatalf("fired=%d calls=%d, want 1/1", fired, len(rec.calls))
	}
	got := rec.calls[0].vars
	want := map[string]string{"base_ref": "release/di-core", "requirements_path": "docs/req.md", "artifact_root": "/art"}
	if len(got) != len(want) {
		t.Fatalf("cook vars=%v, want %v (reserved convoy_id must be dropped)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("cook var %q = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["convoy_id"]; ok {
		t.Errorf("reserved var convoy_id leaked into cook vars: %v", got)
	}
}

// TestPromoteReadyFireRootsIsBeadStateDriven covers criterion 5: the promotion's
// only inputs are a bead store and a cooker — no mail store, no message bead. The
// fire is driven entirely by the blocked→ready dependency transition with an
// empty mail landscape (sc-jrp84z: mail is notification-only; beads propel work).
func TestPromoteReadyFireRootsIsBeadStateDriven(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "GATE", Type: "task", Status: "open"},
		{ID: "FR-1", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: testFireFormula,
		}},
	}, []beads.Dep{{IssueID: "FR-1", DependsOnID: "GATE", Type: "blocks"}})

	rec := &fireRecorder{}
	if fired, err := promoteReadyFireRoots(store, rec.cook); err != nil || fired != 0 {
		t.Fatalf("before gate closed: fired=%d err=%v, want 0/nil", fired, err)
	}
	// Closing the gate bead — a pure bead-state change, not a mail — is the GO.
	if err := store.Close("GATE"); err != nil {
		t.Fatalf("close gate: %v", err)
	}
	if fired, err := promoteReadyFireRoots(store, rec.cook); err != nil || fired != 1 {
		t.Fatalf("after gate closed: fired=%d err=%v, want 1/nil (bead-state alone propels the fire)", fired, err)
	}
}

// TestPromoteReadyFireRootsNilInputs guards the nil-store / nil-cook paths the
// controller can hit when a scope's store is unavailable, mirroring
// TestRestoreCarriedWorkRoutesNilStore.
func TestPromoteReadyFireRootsNilInputs(t *testing.T) {
	noop := func(beads.Bead, string, map[string]string) error { return nil }
	if fired, err := promoteReadyFireRoots(nil, noop); fired != 0 || err != nil {
		t.Fatalf("nil store: fired=%d err=%v, want 0/nil", fired, err)
	}
	store := beads.NewMemStoreFrom(0, nil, nil)
	if fired, err := promoteReadyFireRoots(store, nil); fired != 0 || err != nil {
		t.Fatalf("nil cook: fired=%d err=%v, want 0/nil", fired, err)
	}
}

// TestCityRuntimePromoteReadyFireRoots confirms the controller method sweeps
// both the city store and every rig store and cooks each scope's fire-root with
// the storeRef/rigName of its resident store — mirroring
// TestCityRuntimeRecoverUnroutedWorkRoutes. It injects the cook seam so the
// sweep runs without a live formula layer.
func TestCityRuntimePromoteReadyFireRoots(t *testing.T) {
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CFR-1", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: "city-formula",
		}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RFR-1", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: testFireFormula,
		}},
	}, nil)

	type sweptFire struct {
		storeRef, rigName, fireRootID, formula string
	}
	var swept []sweptFire
	cr := &CityRuntime{
		cityName:            "city",
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{"dip": rigStore},
		stderr:              io.Discard,
		fireRootCook: func(store beads.Store, storeRef, rigName string, fireRoot beads.Bead, formulaName string, vars map[string]string) error {
			swept = append(swept, sweptFire{storeRef: storeRef, rigName: rigName, fireRootID: fireRoot.ID, formula: formulaName})
			return nil
		},
	}

	cr.promoteReadyFireRoots()

	if len(swept) != 2 {
		t.Fatalf("swept=%+v, want 2 (city + rig fire-roots)", swept)
	}
	var sawCity, sawRig bool
	for _, s := range swept {
		switch s.fireRootID {
		case "CFR-1":
			sawCity = true
			if s.storeRef != "city:city" || s.rigName != "" {
				t.Errorf(`city fire cooked with storeRef=%q rigName=%q, want "city:city"/""`, s.storeRef, s.rigName)
			}
		case "RFR-1":
			sawRig = true
			if s.storeRef != "rig:dip" || s.rigName != "dip" || s.formula != testFireFormula {
				t.Errorf("rig fire cooked with storeRef=%q rigName=%q formula=%q, want rig:dip/dip/%s", s.storeRef, s.rigName, s.formula, testFireFormula)
			}
		default:
			t.Errorf("unexpected cooked fire-root %q", s.fireRootID)
		}
	}
	if !sawCity || !sawRig {
		t.Errorf("sawCity=%v sawRig=%v, want both stores swept", sawCity, sawRig)
	}
}

// TestLinkFiredRootGraphWorkflow proves the linkage cookFireRoot applies after a
// graph.v2 cook closes the idempotency loop that molecule.Cook alone cannot:
// molecule.Cook does not set Options.ParentID on a gc.kind=workflow root and
// never stamps a source pointer, so linkFiredRoot must establish the fire-root
// ⇄ root link itself. After it runs, HasMoleculeChildren sees the fire-root's
// live root (so the very next promotion tick is a no-op), and the root carries
// the source back-pointers the source-workflow machinery reads.
func TestLinkFiredRootGraphWorkflow(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "FR-1", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: testFireFormula,
		}},
		{ID: "WF-1", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
		}},
	}, nil)

	if err := linkFiredRoot(store, "rig:dip", "FR-1", &molecule.Result{RootID: "WF-1", GraphWorkflow: true}); err != nil {
		t.Fatalf("linkFiredRoot: %v", err)
	}

	fr, err := store.Get("FR-1")
	if err != nil {
		t.Fatalf("get FR-1: %v", err)
	}
	if got := fr.Metadata["workflow_id"]; got != "WF-1" {
		t.Errorf("FR-1 workflow_id = %q, want WF-1 (forward pointer HasMoleculeChildren reads)", got)
	}
	wf, err := store.Get("WF-1")
	if err != nil {
		t.Fatalf("get WF-1: %v", err)
	}
	if got := wf.Metadata[beadmeta.SourceBeadIDMetadataKey]; got != "FR-1" {
		t.Errorf("WF-1 gc.source_bead_id = %q, want FR-1 (source back-pointer)", got)
	}
	if got := wf.Metadata[beadmeta.SourceStoreRefMetadataKey]; got != "rig:dip" {
		t.Errorf("WF-1 gc.source_store_ref = %q, want rig:dip", got)
	}

	// The idempotency loop closes: the fire-root now reads as already-fired.
	has, err := sling.HasMoleculeChildren(store, "FR-1", store)
	if err != nil {
		t.Fatalf("HasMoleculeChildren: %v", err)
	}
	if !has {
		t.Fatal("HasMoleculeChildren(FR-1) = false after linkFiredRoot, want true (next tick would double-fire)")
	}

	// And a subsequent promotion is a no-op — no re-fire.
	rec := &fireRecorder{}
	fired, err := promoteReadyFireRoots(store, rec.cook)
	if err != nil {
		t.Fatalf("promoteReadyFireRoots (post-link): %v", err)
	}
	if fired != 0 || len(rec.calls) != 0 {
		t.Fatalf("post-link fired=%d calls=%d, want 0/0 (idempotent after firing)", fired, len(rec.calls))
	}
}

// TestLinkFiredRootLegacyMolecule covers the non-graph path: a legacy molecule
// root is linked by the molecule_id forward pointer, which HasMoleculeChildren
// also reads.
func TestLinkFiredRootLegacyMolecule(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "FR-1", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: testFireFormula,
		}},
		{ID: "MOL-1", Type: "molecule", Status: "open"},
	}, nil)

	if err := linkFiredRoot(store, "rig:dip", "FR-1", &molecule.Result{RootID: "MOL-1", GraphWorkflow: false}); err != nil {
		t.Fatalf("linkFiredRoot: %v", err)
	}
	fr, err := store.Get("FR-1")
	if err != nil {
		t.Fatalf("get FR-1: %v", err)
	}
	if got := fr.Metadata[beadmeta.MoleculeIDMetadataKey]; got != "MOL-1" {
		t.Errorf("FR-1 molecule_id = %q, want MOL-1", got)
	}
	has, err := sling.HasMoleculeChildren(store, "FR-1", store)
	if err != nil {
		t.Fatalf("HasMoleculeChildren: %v", err)
	}
	if !has {
		t.Fatal("HasMoleculeChildren(FR-1) = false after legacy link, want true")
	}
}
