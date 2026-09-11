package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orders"
)

// This file is the one-shot half of the graph-class birth invariant that
// order dispatch already holds (order_dispatch_graph_store_test.go, #5106).
//
// A one-shot command builds no CityRuntime, so it never sees the routes the
// controller opened at boot; it opens one store for the scope it is working in
// and, before this change, materialized the whole molecule through it. On a
// city whose graph class is served by its own binding, that mints every wisp
// root and every step in the WORK ledger under the WORK prefix, and the city's
// own convergence check reads them as graph-class beads stranded off their
// binding — which is fatal to boot. The measured rate on maintainer-city was
// ~42 stranded beads an hour.
//
// The tests come in pairs. The split subtest is the regression: it fails if the
// routing is reverted. The single-store subtest is the compatibility claim, and
// it is green before and after by design — resolveGraphStore returns the exact
// store value it was handed when the routes relocate nothing, so a compatibility
// guard that went red on revert would be testing the wrong thing. Its teeth come
// from mutation instead (documented on each such test).

// enableFormulaV2ForOneShotTest turns on the graph.v2 compiler capability the way
// a booting city does — through the sanctioned propagator, derived from a config
// that declares [daemon] formula_v2, rather than through the legacy global
// setters the freeze in internal/testenv is ratcheting down.
func enableFormulaV2ForOneShotTest(t *testing.T) {
	t.Helper()
	applyFeatureFlags(&config.City{Daemon: config.DaemonConfig{FormulaV2: boolPtr(true)}})
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
}

// oneShotGraphOrderCity writes a city an actual `gc order run` can load config
// for, plus a graph.v2 order whose single step routes to a city agent. It is the
// CLI twin of newGraphOrderFixture: the order runs through doOrderRunWithJSON,
// which loads the city config from disk rather than being handed one.
func oneShotGraphOrderCity(t *testing.T, trigger string) (string, orders.Order) {
	t.Helper()
	cityPath := t.TempDir()
	body := `[workspace]
name = "one-shot-city"

[daemon]
formula_v2 = true

[[agent]]
name = "worker"
max_active_sessions = 2
` + testControlDispatcherAgentTOML("")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	formulaDir := filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("mkdir formulas: %v", err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "reaper.toml"), []byte(`
formula = "reaper"
version = 2
contract = "graph.v2"

[[steps]]
id = "sweep"
title = "Reaper sweep"
metadata = { "gc.run_target" = "worker" }
`), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	enableFormulaV2ForOneShotTest(t)
	return cityPath, orders.Order{
		Name:         "reaper",
		Formula:      "reaper",
		Trigger:      trigger,
		Interval:     "15m",
		FormulaLayer: formulaDir,
	}
}

// oneShotPouredOrderCity is oneShotGraphOrderCity's v1 twin: a city whose order
// formula is a legacy POURED molecule. Its root is a molecule container and its
// steps are plain assigned tasks — no gc.kind, no gc.root_bead_id — so
// coordclass.Classify calls every one of them ClassWork, and work is the one
// class a split city does not relocate.
func oneShotPouredOrderCity(t *testing.T) (string, orders.Order) {
	t.Helper()
	cityPath := t.TempDir()
	body := `[workspace]
name = "one-shot-city"

[daemon]
formula_v2 = true

[[agent]]
name = "worker"
max_active_sessions = 2
` + testControlDispatcherAgentTOML("")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	formulaDir := filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("mkdir formulas: %v", err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "poured.toml"), []byte(`
formula = "poured"
version = 1
pour = true

[[steps]]
id = "sweep"
title = "Poured sweep"
assignee = "worker"
`), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	enableFormulaV2ForOneShotTest(t)
	return cityPath, orders.Order{
		Name:         "poured",
		Formula:      "poured",
		Trigger:      "cooldown",
		Interval:     "15m",
		FormulaLayer: formulaDir,
	}
}

// runOneShotOrder fires `gc order run` against the given scope store and returns
// the wisp id it reported.
func runOneShotOrder(t *testing.T, cityPath string, a orders.Order, scope beads.Store, ep events.Provider) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := doOrderRunWithJSON([]orders.Order{a}, a.Name, a.Rig, cityPath, beads.OrdersStore{Store: scope}, ep, true, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc order run = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var res orderRunJSON
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("parse order run json %q: %v", stdout.String(), err)
	}
	if res.WispID == "" {
		t.Fatalf("order run reported no wisp id: %s", stdout.String())
	}
	return res.WispID
}

// assertNoGraphBeads fails when store holds any bead a coordination-class split
// assigns to the graph class. gc.kind and gc.root_bead_id are the two markers
// coordclass.Classify reads for the workflow/wisp arms, so a molecule that
// landed here is caught whether it is the root or one of its steps.
func assertNoGraphBeads(t *testing.T, store beads.Store, storeName string) {
	t.Helper()
	for _, b := range allBeads(t, store) {
		kind := b.Metadata[beadmeta.KindMetadataKey]
		root := b.Metadata[beadmeta.RootBeadIDMetadataKey]
		if kind == "" && root == "" {
			continue
		}
		t.Errorf("%s store holds graph bead %s (%q, gc.kind=%q gc.root_bead_id=%q); on a split city that row is a stranded infrastructure bead and boot refuses on it", storeName, b.ID, b.Title, kind, root)
	}
}

// TestOrderRunWispRootLandsInGraphStoreOnSplitCity is the producer half for the
// manual run. `gc order run <formula-order>` materializes the same molecule the
// controller's dispatchWisp does, and coordclass classifies a wisp root and its
// steps as ClassGraph, so on a split city they belong in the graph binding. The
// scope store the command opened is a work ledger; creating them there is the
// stranded-bead shape #5106 fixed for the dispatcher and left open here.
func TestOrderRunWispRootLandsInGraphStoreOnSplitCity(t *testing.T) {
	cityPath, a := oneShotGraphOrderCity(t, "cooldown")
	scope := splittest.NewWorkStore(t, "mc")
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(graph))

	wispID := runOneShotOrder(t, cityPath, a, scope, nil)

	root, err := graph.Get(wispID)
	if err != nil {
		t.Fatalf("wisp root %s is not resident in the graph binding: %v", wispID, err)
	}
	if !strings.HasPrefix(root.ID, "gcg-") {
		t.Errorf("wisp root id = %q, want the graph binding's %q prefix", root.ID, "gcg-")
	}
	if !hasLabel(root.Labels, "order-run:reaper") {
		t.Errorf("wisp root labels = %v, want order-run:reaper stamped on the bead the run created", root.Labels)
	}
	if _, err := scope.Get(wispID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("wisp root %s resolves in the scope work store (err=%v); the run minted a graph bead in the work ledger", wispID, err)
	}
	assertNoGraphBeads(t, scope, "scope work")
}

// TestOrderRunWispStaysOnTheOneStoreOnSingleStoreCity is the compatibility half:
// a city that authors no [storage] relocates nothing, so every bead the run
// creates stays in the single store it always used and nothing else is opened.
//
// Green before and after by design — see the file header. Its teeth were proven
// by mutation: making resolveGraphStore's identity branch hand back a fresh
// store when the routes are nil fails this test with
//
//	wisp root gc-1 is not resident in the single store: bead not found
func TestOrderRunWispStaysOnTheOneStoreOnSingleStoreCity(t *testing.T) {
	cityPath, a := oneShotGraphOrderCity(t, "cooldown")
	store := splittest.NewWorkStore(t, "gc")
	seedCLIStorageRoutes(t, cityPath, nil)

	wispID := runOneShotOrder(t, cityPath, a, store, nil)

	root, err := store.Get(wispID)
	if err != nil {
		t.Fatalf("wisp root %s is not resident in the single store: %v", wispID, err)
	}
	if !hasLabel(root.Labels, "order-run:reaper") {
		t.Errorf("wisp root labels = %v, want order-run:reaper", root.Labels)
	}
	tracking, err := store.ListByLabel(labelOrderTracking, 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("listing order-tracking beads: %v", err)
	}
	if len(tracking) != 1 {
		t.Fatalf("order-tracking beads in the single store = %d, want 1", len(tracking))
	}
}

// TestOrderRunGraphResidentRunIsVisibleToTheOrderReaders is the reader half, and
// it is the check that stops this change from being a moved write with a reader
// left behind.
//
// The run's evidence is a pair of beads carrying the same order-run:<scoped>
// label: the ORDERS-class tracking bead whose CreatedAt is the cooldown clock,
// and the GRAPH-class wisp root that also carries the order:<scoped> and seq:<n>
// event-cursor labels. `gc order check` reads both through the front doors
// cachedOrderStoresResolver builds, and that federation appends
// relocatedOrdersClassStore. The only split shape this build serves puts every
// infrastructure class on ONE binding, so that leg is also where the graph class
// lives — which is why the readers still see a run whose root moved. This test
// pins that agreement rather than asserting it in prose.
func TestOrderRunGraphResidentRunIsVisibleToTheOrderReaders(t *testing.T) {
	cityPath, a := oneShotGraphOrderCity(t, "event")
	scope := splittest.NewWorkStore(t, "mc")
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(graph))

	ep := events.NewFake()
	ep.Record(events.Event{Type: events.OrderCompleted, Actor: "test", Subject: "seed"})
	wispID := runOneShotOrder(t, cityPath, a, scope, ep)

	cfg, err := loadCityConfig(cityPath, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	ordersLeg := relocatedOrdersClassStore(cityPath, cfg)
	if ordersLeg == nil {
		t.Fatal("a split city resolved no orders-class leg for the one-shot order readers")
	}
	if _, err := ordersLeg.Get(wispID); err != nil {
		t.Fatalf("the order readers' class leg cannot see the wisp root %s: %v — `gc order check` would report an order that just fired as never run", wispID, err)
	}

	front := orders.NewStoreWithGraph(beads.OrdersStore{Store: ordersLeg}, beads.GraphStore{Store: ordersLeg})
	last, err := front.LastRun(a.ScopedName())
	if err != nil {
		t.Fatalf("LastRun(%s): %v", a.ScopedName(), err)
	}
	if last.IsZero() {
		t.Errorf("LastRun(%s) is zero after a manual run; the cooldown clock cannot see the run's evidence", a.ScopedName())
	}
	if cursor := front.Cursor(a.ScopedName()); cursor == 0 {
		t.Errorf("Cursor(%s) is zero after an event-triggered manual run; the seq: label rides the wisp root and the readers must reach it", a.ScopedName())
	}
}

// oneShotCookCity writes a city `gc formula cook` can resolve, holding one
// graph.v2 formula and one legacy (v1) formula, and puts the process in the
// hermetic env the cook command needs. The env/cwd setup routes through
// internal/formulatest so its t.Setenv/t.Chdir call sites stay off the cmd/gc
// resource-census ratchet.
func oneShotCookCity(t *testing.T) string {
	t.Helper()
	enableFormulaV2ForOneShotTest(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(withBuiltinProviderAliasesTOMLForTest(`
[workspace]
name = "cook-city"
provider = "claude"

[daemon]
formula_v2 = true
`, "claude")+testControlDispatcherAgentTOML("")), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	formulaDir := filepath.Join(cityDir, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("mkdir formulas: %v", err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatalf("write graph.v2 formula: %v", err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "legacy-work.formula.toml"), []byte(`
formula = "legacy-work"
version = 1

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatalf("write legacy formula: %v", err)
	}
	// A v1 ROOT-ONLY formula: the legacy compiler stamps gc.kind=wisp on the
	// root of any `phase = "vapor"` (or step-less) formula, which is
	// coordclass.Classify's first arm. examples/bd/dolt/formulas/mol-dog-stale-db.toml
	// is a shipped instance of this shape, and formula-spec-v1 documents it.
	if err := os.WriteFile(filepath.Join(formulaDir, "vapor-work.formula.toml"), []byte(`
formula = "vapor-work"
version = 1
phase = "vapor"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatalf("write vapor formula: %v", err)
	}
	formulatest.SetupHermeticCookEnv(t, cityDir, cityDir)
	return cityDir
}

// cookFormula runs `gc formula cook <name> --json` and returns the result.
// extraArgs are appended after the formula name, so a caller can drive the
// --attach and --meta arms through the same real cobra command.
func cookFormula(t *testing.T, name string, extraArgs ...string) formulaCookJSONResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newFormulaCookCmd(&stdout, &stderr)
	cmd.SetArgs(append([]string{name, "--json"}, extraArgs...))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc formula cook %s: %v\nstdout=%s\nstderr=%s", name, err, stdout.String(), stderr.String())
	}
	var res formulaCookJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("parse cook json %q: %v", stdout.String(), err)
	}
	return res
}

// TestFormulaCookGraphV2RootLandsInGraphStoreOnSplitCity is the producer half
// for `gc formula cook`. A graph.v2 pour is graph class end to end —
// molecule.Instantiate stamps gc.root_bead_id on every member — so on a split
// city the root and its steps belong in the binding, not in the scope store the
// command opened for the formula's scope.
func TestFormulaCookGraphV2RootLandsInGraphStoreOnSplitCity(t *testing.T) {
	cityDir := oneShotCookCity(t)
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))

	res := cookFormula(t, "graph-work")

	root, err := graph.Get(res.RootID)
	if err != nil {
		t.Fatalf("cooked root %s is not resident in the graph binding: %v", res.RootID, err)
	}
	if !strings.HasPrefix(root.ID, "gcg-") {
		t.Errorf("cooked root id = %q, want the graph binding's %q prefix", root.ID, "gcg-")
	}
	if got := root.Metadata[beadmeta.RootStoreRefMetadataKey]; got != "city:cook-city" {
		t.Errorf("cooked root %s: gc.root_store_ref = %q, want %q — the recipe must be decorated through the store that will own it", res.RootID, got, "city:cook-city")
	}
	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	if _, err := work.Get(res.RootID); err == nil {
		t.Errorf("cooked root %s resolves in the WORK store; the pour minted graph beads in the work ledger", res.RootID)
	}
	assertNoGraphBeads(t, work, "work")
}

// TestFormulaCookLegacyMoleculeStaysOnTheWorkStore is the boundary this change
// deliberately does not cross. A v1 molecule is NOT graph class: Instantiate
// stamps gc.root_bead_id only on a graph pour and on gc.kind steps, so
// coordclass.Classify leaves a legacy molecule on ClassWork — and work stays on
// the work ledger even in a split city. Routing it at the graph binding would
// relocate a class the split does not relocate.
func TestFormulaCookLegacyMoleculeStaysOnTheWorkStore(t *testing.T) {
	cityDir := oneShotCookCity(t)
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))

	res := cookFormula(t, "legacy-work")

	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	if _, err := work.Get(res.RootID); err != nil {
		t.Fatalf("legacy molecule root %s is not resident in the work store: %v", res.RootID, err)
	}
	if got := allBeads(t, graph); len(got) != 0 {
		t.Errorf("graph binding holds %d beads after a legacy cook, want 0 — a work-class molecule was relocated: %+v", len(got), got)
	}
	// The negative has to be two-sided. Asserting only that the binding is
	// empty stays true while the work store quietly holds a graph-class bead,
	// which is the stranded row boot refuses on.
	assertNoGraphBeads(t, work, "work")
}

// TestFormulaCookGraphV2StaysOnTheOneStoreOnSingleStoreCity is the
// compatibility half for the cook path: a city that relocates nothing cooks
// into the one store it always did.
//
// Green before and after by design — see the file header. Its teeth were proven
// by mutation: making resolveGraphStore's identity branch hand back a fresh
// store when the routes are nil fails this test with
//
//	cooked root gc-1 is not resident in the single store: no issue found: gc-1
func TestFormulaCookGraphV2StaysOnTheOneStoreOnSingleStoreCity(t *testing.T) {
	cityDir := oneShotCookCity(t)
	seedCLIStorageRoutes(t, cityDir, nil)

	res := cookFormula(t, "graph-work")

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	root, err := store.Get(res.RootID)
	if err != nil {
		t.Fatalf("cooked root %s is not resident in the single store: %v", res.RootID, err)
	}
	if got := root.Metadata[beadmeta.RootStoreRefMetadataKey]; got != "city:cook-city" {
		t.Errorf("cooked root %s: gc.root_store_ref = %q, want %q", res.RootID, got, "city:cook-city")
	}
	recorded, err := events.ReadAll(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("read execution events: %v", err)
	}
	if len(recorded) == 0 || recorded[0].RunID != res.RootID {
		t.Fatalf("execution events = %#v, want the initial step-definition snapshot for %s", recorded, res.RootID)
	}
}

// closeEveryBeadExcept closes every bead the store holds apart from keep. It
// models "the whole workflow ran to completion", which is the state the
// --attach contract promises will unblock the source bead.
func closeEveryBeadExcept(t *testing.T, store beads.Store, keep string) {
	t.Helper()
	closed := "closed"
	for _, b := range allBeads(t, store) {
		if b.ID == keep || b.Status == "closed" {
			continue
		}
		if err := store.Update(b.ID, beads.UpdateOpts{Status: &closed}); err != nil {
			t.Fatalf("closing bead %s: %v", b.ID, err)
		}
	}
}

// TestFormulaCookAttachLeavesTheSourceBeadUnblockableOnSplitCity is the
// assertion the --attach arm never had, and the one that catches a half-split
// attach.
//
// `gc formula cook --attach` is documented as a graft: "the bead gains a
// blocking dependency on the sub-DAG root, so it won't close until the sub-DAG
// completes". A dep is a two-ended edge. If the sub-DAG root is created in the
// graph binding while the blocking dep is written through the work store, the
// work store holds a `blocks` row naming an id it can never resolve — and
// MemStore/FileStore/SQLite all treat an unresolvable blocker as open
// (readyLocked: statusByID[dep.DependsOnID] != "closed"), so the source bead
// leaves Ready permanently. Neither production backend rejects that write
// (internal/beads/splittest/strict_store.go's backend table), so the only thing
// that can catch it is this test.
//
// It is asserted on the one graft a split city still serves: a v1 formula onto
// a bead the BINDING owns. The work-resident residence it used to use refuses
// now, because the sub-DAG it would mint beside the attach bead is graph class
// and therefore a stranded write (ga-99xhy,
// TestFormulaCookAttachOnAWorkResidentBeadIsRefusedOnSplitCity).
func TestFormulaCookAttachLeavesTheSourceBeadUnblockableOnSplitCity(t *testing.T) {
	cityDir := oneShotCookCity(t)
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))

	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	source, err := graph.Create(beads.Bead{Title: "a running workflow step", Type: "task"})
	if err != nil {
		t.Fatalf("create the class-resident attach bead: %v", err)
	}

	res := cookFormula(t, "legacy-work", "--attach", source.ID)

	deps, err := graph.DepList(source.ID, "down")
	if err != nil {
		t.Fatalf("listing attach deps: %v", err)
	}
	if len(deps) == 0 {
		t.Fatalf("attach bead %s has no blocking dep after cook; the graft was never wired", source.ID)
	}
	for _, dep := range deps {
		if _, err := graph.Get(dep.DependsOnID); err != nil {
			t.Errorf("the binding holds dep %s -> %s (%s) whose target it cannot resolve: %v — a dangling cross-store blocking edge no backend rejects and no finalize path removes", dep.IssueID, dep.DependsOnID, dep.Type, err)
		}
	}

	closeEveryBeadExcept(t, graph, source.ID)
	closeEveryBeadExcept(t, work, source.ID)

	ready, err := graph.Ready()
	if err != nil {
		t.Fatalf("binding Ready(): %v", err)
	}
	if !slices.Contains(beadIDs(ready), source.ID) {
		t.Fatalf("attach bead %s is not Ready after the whole workflow closed (ready=%v, root=%s); the --attach contract says the graft unblocks it, so it is wedged out of Ready forever", source.ID, beadIDs(ready), res.RootID)
	}
}

// TestFormulaCookRootOnlyLegacyWispLandsInGraphStoreOnSplitCity is the class
// verdict re-derived from the classifier rather than from the compiler version.
//
// "v1 means work class" is false for a root-only formula. internal/formula
// compiles `phase = "vapor"` (and any step-less formula) to `RootOnly` with the
// root stamped gc.kind=wisp, and gc.kind=wisp is coordclass.Classify's FIRST
// arm — reached long before gc.root_bead_id is consulted. Cooked into the work
// ledger on a split city, that single bead is exactly the row readInfraSnapshot
// selects as a graph-class bead residing outside its binding, and boot refuses
// on it.
func TestFormulaCookRootOnlyLegacyWispLandsInGraphStoreOnSplitCity(t *testing.T) {
	cityDir := oneShotCookCity(t)
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))

	res := cookFormula(t, "vapor-work")

	root, err := graph.Get(res.RootID)
	if err != nil {
		t.Fatalf("root-only wisp %s is not resident in the graph binding: %v", res.RootID, err)
	}
	if got := root.Metadata[beadmeta.KindMetadataKey]; got != beadmeta.KindWisp {
		t.Fatalf("cooked root %s: gc.kind = %q, want %q — the fixture must actually be the root-only shape", res.RootID, got, beadmeta.KindWisp)
	}
	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	assertNoGraphBeads(t, work, "work")
}

// TestFormulaCookRootOnlyLegacyWispStaysOnTheOneStoreOnSingleStoreCity is the
// compatibility half: a city that relocates nothing cooks the same wisp into
// the one store it always used.
//
// Green before and after by design — see the file header.
func TestFormulaCookRootOnlyLegacyWispStaysOnTheOneStoreOnSingleStoreCity(t *testing.T) {
	cityDir := oneShotCookCity(t)
	seedCLIStorageRoutes(t, cityDir, nil)

	res := cookFormula(t, "vapor-work")

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Get(res.RootID); err != nil {
		t.Fatalf("root-only wisp %s is not resident in the single store: %v", res.RootID, err)
	}
}

// TestFormulaCookMetaStampLandsOnTheGraphResidentRoot pins the --meta arm's
// store. SetMetadataBatch is a by-id write; addressed at the scope store while
// the root lives in the binding, it fails the cook after the workflow is
// already materialized.
func TestFormulaCookMetaStampLandsOnTheGraphResidentRoot(t *testing.T) {
	cityDir := oneShotCookCity(t)
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))

	res := cookFormula(t, "graph-work", "--meta", "gc.trace=abc123")

	root, err := graph.Get(res.RootID)
	if err != nil {
		t.Fatalf("cooked root %s is not resident in the graph binding: %v", res.RootID, err)
	}
	if got := root.Metadata["gc.trace"]; got != "abc123" {
		t.Errorf("cooked root %s: gc.trace = %q, want %q — the --meta stamp went to a store that never held the root", res.RootID, got, "abc123")
	}
}

// TestFormulaCookAttachIsIdempotent drives the `existing != nil` early return,
// the one attach path that returns without instantiating. A convoy target gives
// the invocation a stable input-convoy id and therefore a stable graph root key,
// so a repeat cook must adopt the live workflow instead of minting a second one
// — and must not add a second blocking edge.
//
// Two rows, because a split city answers this differently now. The graph.v2
// --attach arm is refused on BOTH residences there (the binding's, for the
// convoy that has nowhere to live — #5163; the work ledger's, for the sub-DAG
// that would be a stranded write — ga-99xhy), so the property a split city has
// to hold is that a repeated refusal accumulates nothing. The idempotent adopt
// itself is asserted where it still runs.
func TestFormulaCookAttachIsIdempotent(t *testing.T) {
	t.Run("single-store adopts the live workflow", func(t *testing.T) {
		cityDir := oneShotCookCity(t)
		seedCLIStorageRoutes(t, cityDir, nil)
		work, err := openStoreAtForCity(cityDir, cityDir)
		if err != nil {
			t.Fatalf("open work store: %v", err)
		}
		source, err := work.Create(beads.Bead{Title: "attach target", Type: "convoy"})
		if err != nil {
			t.Fatalf("create attach bead: %v", err)
		}

		first := cookFormula(t, "graph-work", "--attach", source.ID)
		second := cookFormula(t, "graph-work", "--attach", source.ID)

		if first.RootID != second.RootID {
			t.Fatalf("re-cook minted a second workflow root (%s then %s); the idempotent lookup read a store that does not hold the root", first.RootID, second.RootID)
		}
		if got := blockingDepCount(t, work, source.ID); got != 1 {
			t.Errorf("attach bead %s carries %d blocking deps after two cooks, want 1", source.ID, got)
		}
	})

	t.Run("split refuses repeatedly and accumulates nothing", func(t *testing.T) {
		cityDir := oneShotCookCity(t)
		graph := splittest.NewClassStore(t, config.BeadClassGraph)
		seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))
		work, err := openStoreAtForCity(cityDir, cityDir)
		if err != nil {
			t.Fatalf("open work store: %v", err)
		}
		source, err := work.Create(beads.Bead{Title: "attach target", Type: "convoy"})
		if err != nil {
			t.Fatalf("create attach bead: %v", err)
		}

		for attempt := 1; attempt <= 2; attempt++ {
			if out, err := cookFormulaErr(t, "graph-work", "--attach", source.ID); err == nil {
				t.Fatalf("attempt %d served a graft onto a work-resident bead on a split city: %s", attempt, out)
			}
		}
		if got := beadIDs(allBeads(t, work)); len(got) != 1 || got[0] != source.ID {
			t.Errorf("the work ledger holds %v after two refused grafts, want only %s", got, source.ID)
		}
		if got := allBeads(t, graph); len(got) != 0 {
			t.Errorf("the binding holds %d bead(s) after two refused grafts: %+v", len(got), got)
		}
		if got := blockingDepCount(t, work, source.ID); got != 0 {
			t.Errorf("attach bead %s carries %d blocking deps after two refused grafts, want 0", source.ID, got)
		}
	})
}

// blockingDepCount returns how many `blocks` edges hang off a bead.
func blockingDepCount(t *testing.T, store beads.Store, beadID string) int {
	t.Helper()
	deps, err := store.DepList(beadID, "down")
	if err != nil {
		t.Fatalf("listing deps of %s: %v", beadID, err)
	}
	blocking := 0
	for _, dep := range deps {
		if dep.Type == "blocks" {
			blocking++
		}
	}
	return blocking
}

// TestOrderRunPouredV1MoleculeStaysOnTheWorkStoreOnSplitCity is the mirror of
// the wisp test, and the other half of the class verdict.
//
// `gc order run` must route on what the classifier says about the compiled
// recipe, not on "an order molecule is graph class". A v1 poured order formula
// compiles to a molecule container and plain assigned task steps: every bead is
// ClassWork, and the routes never relocate work. Minting them in the graph
// binding puts the step assigned to `worker` somewhere no work-scope reader
// scans — `gc hook` resolves GC_STORE_SCOPE=city against the city WORK root — so
// the run silently produces work nobody executes.
func TestOrderRunPouredV1MoleculeStaysOnTheWorkStoreOnSplitCity(t *testing.T) {
	cityPath, a := oneShotPouredOrderCity(t)
	scope := splittest.NewWorkStore(t, "mc")
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(graph))

	rootID := runOneShotOrder(t, cityPath, a, scope, nil)

	root, err := scope.Get(rootID)
	if err != nil {
		t.Fatalf("poured molecule root %s is not resident in the scope work store: %v", rootID, err)
	}
	if !hasLabel(root.Labels, "order-run:poured") {
		t.Errorf("root labels = %v, want order-run:poured stamped on the bead the run created", root.Labels)
	}
	var assigned []string
	for _, b := range allBeads(t, scope) {
		if b.Assignee != "" {
			assigned = append(assigned, b.ID)
		}
	}
	if len(assigned) == 0 {
		t.Errorf("no assigned step landed in the work store; `gc hook` reads the work scope and would find nothing to run")
	}
	// The binding legitimately holds the ORDERS-class tracking bead (this build
	// serves every infrastructure class from one binding), so the assertion is
	// about the molecule: nothing the cook materialized may be in there.
	for _, b := range allBeads(t, graph) {
		if hasLabel(b.Labels, labelOrderTracking) {
			continue
		}
		t.Errorf("graph binding holds molecule bead %s (%q) after a v1 poured order run — a work-class molecule was relocated into a binding no work-scope reader scans", b.ID, b.Title)
	}
	assertNoGraphBeads(t, scope, "scope work")
}

// TestEmitFormulaCookExecutionFactsReadsTheConvoyFromTheWorkLeg pins the two
// legs of the cook's execution-fact projection.
//
// executionevent.ProjectCurrent reads the root and its steps from the GRAPH
// leg and the input convoy's `tracks` edges from the WORK leg, and the convoy
// is work class (coordclass routes synthetic convoys to ClassWork explicitly).
// Wrapping one store as both legs therefore reads the convoy out of the ledger
// it does not live in, and the run's work associations vanish from the event
// stream while the step facts still look right.
//
// It is asserted on the helper because the helper is where the leg ASSIGNMENT
// lives, and a collapse of the two legs into one store value is only visible
// there. It is not a substitute for driving the command, and the earlier
// version of this paragraph — "no one-shot cook can reach the two-leg state
// today … the --attach arms … run wholly on the scope store" — was a claim
// about production standing in for coverage of it, which is exactly how a leg
// mismatch ships green: by-id routing moved PrepareInvocation off the scope
// store, the convoy went with it, the work leg did not, and this file did not
// move.
//
// So the command is asserted too, on the FACT rather than on the arguments:
// TestFormulaCookAttachEmitsTheWorkAssociation drives the real cobra
// `gc formula cook --attach` and requires an execution.work_associated naming
// the attach bead. It fails when the convoy leg names a store that does not
// hold the convoy, which the residence assertions around it cannot see —
// DepList on a convoy a store never held answers EMPTY, not an error.
//
// What keeps the two legs equal in production is a pair of REFUSALS, not an
// accident. The only cook that mints an input convoy is a graph.v2 --attach,
// and on a split city both of its residences refuse: a binding-owned attach
// bead, whose work-class convoy can live in neither ledger (ga-2orlf,
// TestFormulaCookGraphV2AttachOnAClassResidentBeadIsRefused), and a
// work-resident one, whose graph-class sub-DAG would be a stranded write
// (ga-99xhy, TestFormulaCookAttachOnAWorkResidentBeadIsRefusedOnSplitCity). So
// this unit test is the ONLY place the two legs can be handed different stores
// today. If either refusal is lifted, the command-level test moves back onto
// the split fixture and both are the ones that have to answer for it.
func TestEmitFormulaCookExecutionFactsReadsTheConvoyFromTheWorkLeg(t *testing.T) {
	cityPath := t.TempDir()
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	work := splittest.NewWorkStore(t, "mc")

	convoy, err := work.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	item, err := work.Create(beads.Bead{Title: "tracked work", Type: "task"})
	if err != nil {
		t.Fatalf("create tracked work: %v", err)
	}
	if err := work.DepAdd(convoy.ID, item.ID, convoycore.TrackingDepType); err != nil {
		t.Fatalf("track %s in %s: %v", item.ID, convoy.ID, err)
	}
	root, err := graph.Create(beads.Bead{
		Title: "workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.InputConvoyIDMetadataKey:   convoy.ID,
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	if _, err := graph.Create(beads.Bead{
		Title: "step",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.StepRefMetadataKey:    "graph-work.step",
		},
	}); err != nil {
		t.Fatalf("create step: %v", err)
	}

	var stderr bytes.Buffer
	emitFormulaCookExecutionFacts(graph, work, cityPath, &molecule.Result{RootID: root.ID, GraphWorkflow: true}, &stderr)

	recorded, err := events.ReadAll(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("read execution events: %v", err)
	}
	associated := false
	for _, e := range recorded {
		if e.Type == events.ExecutionWorkAssociated && e.RunID == root.ID && e.Subject == item.ID {
			associated = true
		}
	}
	if !associated {
		t.Fatalf("no execution.work_associated fact for %s in run %s (events=%+v, stderr=%s); the convoy leg read a ledger the convoy does not live in", item.ID, root.ID, recorded, stderr.String())
	}
}

// TestRecipeCoordClassRederivesTheVerdictFromTheClassifier is the unit-level
// statement of the rule both one-shot paths now route on: the store follows
// coordclass.Classify's verdict on the compiled recipe, never the formula's
// compiler version. The three rows are the three shapes the compiler emits, and
// the middle one is the shape a version-based gate gets wrong.
func TestRecipeCoordClassRederivesTheVerdictFromTheClassifier(t *testing.T) {
	rootOnly := func(kind string) *formula.Recipe {
		return &formula.Recipe{
			RootOnly: true,
			Steps: []formula.RecipeStep{
				{ID: "root", Type: "task", IsRoot: true, Metadata: map[string]string{beadmeta.KindMetadataKey: kind}},
				// A step RootOnly never creates. It must not vote.
				{ID: "root.step", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
			},
		}
	}
	for _, tc := range []struct {
		name   string
		recipe *formula.Recipe
		want   coordclass.Class
	}{
		{
			name: "graph.v2 pour is graph class",
			recipe: &formula.Recipe{Steps: []formula.RecipeStep{
				{ID: "root", Type: "task", IsRoot: true, Metadata: map[string]string{
					beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
					beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				}},
				{ID: "root.step", Type: "task"},
			}},
			want: coordclass.ClassGraph,
		},
		{
			name:   "v1 root-only wisp is graph class",
			recipe: rootOnly(beadmeta.KindWisp),
			want:   coordclass.ClassGraph,
		},
		{
			name: "v1 poured molecule is work class",
			recipe: &formula.Recipe{Steps: []formula.RecipeStep{
				{ID: "root", Type: "molecule", IsRoot: true, Metadata: map[string]string{beadmeta.FormulaNameMetadataKey: "poured"}},
				{ID: "root.step", Type: "task", Assignee: "worker", Metadata: map[string]string{beadmeta.StepRefMetadataKey: "poured.sweep"}},
			}},
			want: coordclass.ClassWork,
		},
		{
			name:   "an unmarked root-only recipe is work class and its skipped steps do not vote",
			recipe: rootOnly(""),
			want:   coordclass.ClassWork,
		},
		{name: "nil recipe is work class", recipe: nil, want: coordclass.ClassWork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recipeCoordClass(tc.recipe); got != tc.want {
				t.Fatalf("recipeCoordClass = %v, want %v", got, tc.want)
			}
			work, graph := beads.NewMemStore(), beads.NewMemStore()
			wantStore := beads.Store(work)
			if tc.want == coordclass.ClassGraph {
				wantStore = graph
			}
			if got := moleculeClassStore(tc.recipe, work, graph); got != wantStore {
				t.Fatalf("moleculeClassStore picked the wrong store for a %v recipe", tc.want)
			}
		})
	}
}
