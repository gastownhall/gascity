package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
		fireRootCook: func(_ beads.Store, storeRef, rigName string, fireRoot beads.Bead, formulaName string, _ map[string]string) error {
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

// TestCityRuntimePromoteReadyFireRootsScopeErrorIsolation pins the other half
// of the sibling-independence promise TestCityRuntimePromoteReadyFireRoots
// checks: a cook failure in one scope must not stop any OTHER scope from being
// attempted. This specifically guards against a future refactor that resolves
// scopes via storeref.Walk instead of storeref.EachLeg plus a caller-owned
// loop — Walk aborts a Union plan on its first Fatal-policy leg error, and the
// work (city) leg is exactly the leg storeref marks Fatal, so a Walk-based
// implementation would silently stop at the city scope and never reach the
// rig below it.
func TestCityRuntimePromoteReadyFireRootsScopeErrorIsolation(t *testing.T) {
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

	var cooked []string
	cr := &CityRuntime{
		cityName:            "city",
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{"dip": rigStore},
		stderr:              io.Discard,
		fireRootCook: func(_ beads.Store, storeRef, _ string, fireRoot beads.Bead, _ string, _ map[string]string) error {
			if storeRef == "city:city" {
				return fmt.Errorf("simulated cook failure for %s", fireRoot.ID)
			}
			cooked = append(cooked, fireRoot.ID)
			return nil
		},
	}

	cr.promoteReadyFireRoots()

	if len(cooked) != 1 || cooked[0] != "RFR-1" {
		t.Fatalf("cooked=%v, want [RFR-1] (the city scope's cook error must not stop the rig scope from being attempted)", cooked)
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

// linkStampFailingStore fails linkFiredRoot's forward/back-pointer SetMetadata
// writes while fail is set, simulating a best-effort stamp that does not land
// after a successful molecule.Cook. molecule.Cook stamps beads (including the
// root's idempotency_key) at CREATE time, not through these keys, so the cook
// itself is unaffected — only the post-hoc linkage fails, exactly the caveat the
// hardening closes.
type linkStampFailingStore struct {
	*beads.MemStore
	fail bool
}

func (s *linkStampFailingStore) SetMetadata(id, key, value string) error {
	if s.fail {
		switch key {
		case "workflow_id", beadmeta.MoleculeIDMetadataKey, beadmeta.SourceBeadIDMetadataKey, beadmeta.SourceStoreRefMetadataKey:
			return fmt.Errorf("simulated stamp failure setting %s on %s", key, id)
		}
	}
	return s.MemStore.SetMetadata(id, key, value)
}

// firedRootsByKey returns every bead carrying the given cook idempotency key,
// across both tiers and including closed, so the test counts the real convoy
// population regardless of the cooked root's tier or lifecycle.
func firedRootsByKey(t *testing.T, store beads.Store, key string) []beads.Bead {
	t.Helper()
	matches, err := store.ListByMetadata(map[string]string{"idempotency_key": key}, 0, beads.WithBothTiers, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByMetadata(%q): %v", key, err)
	}
	var out []beads.Bead
	for _, b := range matches {
		if b.Metadata["idempotency_key"] == key {
			out = append(out, b)
		}
	}
	return out
}

// TestPromoteReadyFireRootsKeyedIdempotencySurvivesLinkStampFailure is the
// Fix-1 hardening regression. Because promoteReadyFireRoots re-runs on EVERY
// patrol tick, a linkFiredRoot forward-pointer stamp that fails right after a
// successful molecule.Cook would blind the HasMoleculeChildren probe and re-cook
// the fire-root every subsequent tick — an every-tick duplicate-convoy
// amplification unique to Fix-1 (the one-shot manual sling it replaces could not
// re-fire). The keyed idempotency molecule.Cook stamps on the cooked root
// (atomically with creation, independent of the post-hoc stamp) stops it: the
// next tick re-detects the already-cooked live convoy by that key, re-asserts the
// missing linkage, and does NOT cook a duplicate.
func TestPromoteReadyFireRootsKeyedIdempotencySurvivesLinkStampFailure(t *testing.T) {
	// A minimal graph.v2 workflow formula: its root is a gc.kind=workflow bead
	// molecule.Cook does NOT parent on the fire-root, so the ONLY idempotency link
	// is the workflow_id forward pointer linkFiredRoot stamps afterward — the
	// best-effort stamp this test fails. This is the graph.v2-specific shape the
	// caveat calls out.
	const fireFormula = "graph-demo"
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-demo.toml"), []byte(`
formula = "graph-demo"
version = 2
contract = "graph.v2"

[[steps]]
id = "setup"
title = "Setup"

[[steps]]
id = "work"
title = "Work"
needs = ["setup"]
`), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	base := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "FR-1", Title: "build fire-root", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.FireFormulaMetadataKey: fireFormula,
		}},
	}, nil)
	store := &linkStampFailingStore{MemStore: base, fail: true}

	cr := &CityRuntime{
		cityName:            "city",
		standaloneCityStore: store,
		stderr:              io.Discard,
		cfg:                 &config.City{FormulaLayers: config.FormulaLayers{City: []string{formulaDir}}},
	}
	// Enable formula v2 through the sanctioned propagator: daemon.formula_v2
	// defaults to true when unset, so this leaves the flag at its production
	// default and the graph.v2 cook below resolves.
	applyFeatureFlags(cr.cfg)
	key := firedRootIdempotencyKey("FR-1")

	// Tick 1: the fire-root cooks, but the forward-pointer stamp fails (transient).
	cr.promoteReadyFireRoots()
	roots := firedRootsByKey(t, base, key)
	if len(roots) != 1 {
		t.Fatalf("after tick 1: %d cooked roots with key %q, want 1 (molecule.Cook must stamp the key it was passed)", len(roots), key)
	}
	wfRootID := roots[0].ID
	if fr, _ := base.Get("FR-1"); fr.Metadata["workflow_id"] != "" {
		t.Fatalf("after tick 1: FR-1 workflow_id = %q, want empty (the stamp failed)", fr.Metadata["workflow_id"])
	}
	// Bug precondition: with the forward pointer missing and the graph.v2 root
	// unparented, HasMoleculeChildren is blind, so the next tick re-enters the cook
	// — this is exactly where the old code amplified.
	if has, err := sling.HasMoleculeChildren(base, "FR-1", base); err != nil {
		t.Fatalf("HasMoleculeChildren: %v", err)
	} else if has {
		t.Fatal("HasMoleculeChildren(FR-1) = true after failed stamp, want false (the test would not exercise the keyed guard otherwise)")
	}

	// The stamp failure clears (it was transient). Tick 2: the probe is still
	// blind, but keyed idempotency finds the live convoy and re-links instead of
	// cooking a duplicate.
	store.fail = false
	cr.promoteReadyFireRoots()
	if roots := firedRootsByKey(t, base, key); len(roots) != 1 {
		t.Fatalf("after tick 2: %d cooked roots with key %q, want 1 (every-tick amplification!)", len(roots), key)
	}
	if fr, _ := base.Get("FR-1"); fr.Metadata["workflow_id"] != wfRootID {
		t.Fatalf("after tick 2: FR-1 workflow_id = %q, want %q (re-link must heal the pointer)", fr.Metadata["workflow_id"], wfRootID)
	}

	// Tick 3: the healed forward pointer lets HasMoleculeChildren short-circuit at
	// the cheaper probe before cookFireRoot — still exactly one convoy.
	cr.promoteReadyFireRoots()
	if roots := firedRootsByKey(t, base, key); len(roots) != 1 {
		t.Fatalf("after tick 3: %d cooked roots with key %q, want 1", len(roots), key)
	}
}
