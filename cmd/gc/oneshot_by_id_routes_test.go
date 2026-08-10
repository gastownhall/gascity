package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
)

// This file covers the one-shot commands that hold a BEAD ID and read or mutate
// that bead: `gc formula version-check` and the two `gc formula cook --attach`
// arms. They resolve their store through classRoutedStoreForID
// (by_id_store_route.go), which is the shared candidate-list-and-probe.
//
// Its siblings live in oneshot_class_routes_test.go, which covers the BIRTH
// half — where a newly minted bead lands. The distinction matters: a birth is
// routed by the CLASS of what is being created, a by-id operation by where the
// subject already IS.

// cookCityWithSplitGraph is the shared fixture: a cook city whose graph class is
// served from its own binding, plus its work store.
func cookCityWithSplitGraph(t *testing.T) (work, graph beads.Store) {
	t.Helper()
	cityDir := oneShotCookCity(t)
	graph = splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))
	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	return work, graph
}

// runVersionCheck drives the real `gc formula version-check <id>` command and
// returns its error, so a routing failure is read from the command rather than
// from a helper.
func runVersionCheck(t *testing.T, beadID string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newFormulaVersionCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{beadID})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return stdout.String() + stderr.String(), err
}

// TestFormulaVersionCheckReadsTheGraphResidentRoot is one of the two readers
// #5150's council named for this slice.
//
// version-check's subject is always a molecule/workflow bead — it needs
// gc.formula_hash, which only instantiation writes — and on a split city every
// such root is minted in the binding. Reading it through the scope store
// reported "not found" for a live root: an existence answer from a ledger that
// has never held the bead, which is the by-id defect this slice closes.
func TestFormulaVersionCheckReadsTheGraphResidentRoot(t *testing.T) {
	work, graph := cookCityWithSplitGraph(t)

	res := cookFormula(t, "graph-work")
	root, err := graph.Get(res.RootID)
	if err != nil {
		t.Fatalf("cooked root %s is not resident in the graph binding: %v", res.RootID, err)
	}
	if root.Metadata[beadmeta.FormulaHashMetadataKey] == "" {
		t.Fatalf("cooked root %s carries no %s; version-check has nothing to compare and this fixture proves nothing", res.RootID, beadmeta.FormulaHashMetadataKey)
	}
	if _, err := work.Get(res.RootID); err == nil {
		t.Fatalf("the work store also holds %s; the premise is that only the binding does", res.RootID)
	}

	out, err := runVersionCheck(t, res.RootID)
	if err != nil {
		t.Fatalf("gc formula version-check %s failed: %v\n%s", res.RootID, err, out)
	}
	if !strings.Contains(out, res.RootID) {
		t.Errorf("version-check output %q does not name %s", out, res.RootID)
	}
}

// TestFormulaVersionCheckLeavesWorkResidentRootsOnTheScopeStore is the other
// half: a root the work ledger holds is still read from the work ledger, so the
// probe cannot have become an unconditional route to the binding.
func TestFormulaVersionCheckLeavesWorkResidentRootsOnTheScopeStore(t *testing.T) {
	work, graph := cookCityWithSplitGraph(t)

	res := cookFormula(t, "legacy-work")
	if _, err := work.Get(res.RootID); err != nil {
		t.Fatalf("a v1 molecule root must stay in the work ledger: %v", err)
	}
	if _, err := graph.Get(res.RootID); err == nil {
		t.Fatalf("the binding holds the v1 root %s; the premise is that only the work store does", res.RootID)
	}

	if out, err := runVersionCheck(t, res.RootID); err != nil {
		t.Fatalf("gc formula version-check %s failed: %v\n%s", res.RootID, err, out)
	}
}

// TestFormulaCookAttachGraftsOntoAClassResidentBeadInOneStore is the by-id half
// of the two-store attach, and the case main cannot serve at all.
//
// A graft onto a bead the binding owns — a workflow step expanding itself
// mid-run, which is what late-bound DAG expansion IS — used to run `store.Get`
// against the scope store, which has never held that bead, and failed outright.
// Routing the whole arm by the attach bead's id both makes it work and keeps the
// edge CO-RESIDENT: the sub-DAG root, the steps and the `blocks` row all land in
// the store that holds the parent, so no dep has an endpoint the store cannot
// resolve.
func TestFormulaCookAttachGraftsOntoAClassResidentBeadInOneStore(t *testing.T) {
	work, graph := cookCityWithSplitGraph(t)

	source, err := graph.Create(beads.Bead{Title: "a running workflow step", Type: "task"})
	if err != nil {
		t.Fatalf("create the class-resident attach bead: %v", err)
	}
	if !bdIDIsClassReserved(source.ID) {
		t.Fatalf("the binding minted %q, which carries no reserved class prefix", source.ID)
	}

	res := cookFormula(t, "graph-work", "--attach", source.ID)

	if _, err := graph.Get(res.RootID); err != nil {
		t.Fatalf("sub-DAG root %s is not resident in the store that holds its attach bead: %v", res.RootID, err)
	}
	if _, err := work.Get(res.RootID); err == nil {
		t.Errorf("the work ledger holds sub-DAG root %s, which was grafted onto a bead it does not hold", res.RootID)
	}

	deps, err := graph.DepList(source.ID, "down")
	if err != nil {
		t.Fatalf("listing attach deps: %v", err)
	}
	if len(deps) == 0 {
		t.Fatalf("attach bead %s has no blocking dep after cook; the graft was never wired", source.ID)
	}
	for _, dep := range deps {
		if _, err := graph.Get(dep.DependsOnID); err != nil {
			t.Errorf("store holds dep %s -> %s (%s) whose target it cannot resolve: %v — a dangling cross-store blocking edge no backend rejects and no finalize path removes", dep.IssueID, dep.DependsOnID, dep.Type, err)
		}
	}
	if workDeps, err := work.DepList(source.ID, "down"); err == nil && len(workDeps) > 0 {
		t.Errorf("the work ledger recorded %d deps for a bead it does not hold: %+v", len(workDeps), workDeps)
	}

	// The graft gate itself: with the whole sub-DAG closed, the attach bead
	// comes back to Ready. A split edge leaves it wedged out of Ready forever.
	closeEveryBeadExcept(t, graph, source.ID)
	ready, err := graph.Ready()
	if err != nil {
		t.Fatalf("graph Ready(): %v", err)
	}
	if !slices.Contains(beadIDs(ready), source.ID) {
		t.Fatalf("attach bead %s is not Ready after the whole workflow closed (ready=%v, root=%s)", source.ID, beadIDs(ready), res.RootID)
	}
}

// TestFormulaCookLegacyAttachGraftsOntoAClassResidentBeadInOneStore is the same
// claim for the v1 arm, which runs molecule.Attach rather than the graph.v2
// pipeline. Attach reads the parent, materializes the sub-DAG and writes the
// blocking dep through ONE store, so the store it is handed has to be the one
// that holds the parent.
func TestFormulaCookLegacyAttachGraftsOntoAClassResidentBeadInOneStore(t *testing.T) {
	work, graph := cookCityWithSplitGraph(t)

	source, err := graph.Create(beads.Bead{Title: "a running workflow step", Type: "task"})
	if err != nil {
		t.Fatalf("create the class-resident attach bead: %v", err)
	}

	res := cookFormula(t, "legacy-work", "--attach", source.ID)

	if _, err := graph.Get(res.RootID); err != nil {
		t.Fatalf("v1 sub-DAG root %s is not resident in the store that holds its attach bead: %v", res.RootID, err)
	}
	if _, err := work.Get(res.RootID); err == nil {
		t.Errorf("the work ledger holds v1 sub-DAG root %s, grafted onto a bead it does not hold", res.RootID)
	}
	deps, err := graph.DepList(source.ID, "down")
	if err != nil {
		t.Fatalf("listing attach deps: %v", err)
	}
	if len(deps) == 0 {
		t.Fatalf("attach bead %s has no blocking dep after a v1 cook; the graft was never wired", source.ID)
	}
	for _, dep := range deps {
		if _, err := graph.Get(dep.DependsOnID); err != nil {
			t.Errorf("store holds dep %s -> %s (%s) whose target it cannot resolve: %v", dep.IssueID, dep.DependsOnID, dep.Type, err)
		}
	}
}

// TestFormulaCookAttachOnAWorkResidentBeadIsUnchanged pins the DEFERRED half,
// so it cannot close or widen without a test moving.
//
// When the attach bead lives in the work ledger, the graft stays there with it —
// including the sub-DAG, whose beads are graph class. Relocating them would put
// the two ends of the `blocks` edge in different stores, which no backend
// rejects and every Ready implementation reads as a blocker that never clears
// (#5150 reproduced it as "attach bead gc-1 is not Ready after the whole
// workflow closed"). Closing this gap needs the block REPRESENTED across the
// store boundary, which no mechanism provides today.
//
// If a cross-boundary representation ever lands, this test flips: the root moves
// to the binding, the work store keeps only a resolvable edge, and the deferral
// paragraph on attachStore in cmd_formula.go goes with it.
func TestFormulaCookAttachOnAWorkResidentBeadIsUnchanged(t *testing.T) {
	work, graph := cookCityWithSplitGraph(t)

	source, err := work.Create(beads.Bead{Title: "attach target", Type: "task"})
	if err != nil {
		t.Fatalf("create attach bead: %v", err)
	}

	res := cookFormula(t, "graph-work", "--attach", source.ID)

	if _, err := work.Get(res.RootID); err != nil {
		t.Fatalf("sub-DAG root %s left the work ledger its attach bead lives in: %v — that is the split edge, not the fix", res.RootID, err)
	}
	if _, err := graph.Get(res.RootID); err == nil {
		t.Errorf("sub-DAG root %s landed in the binding while its attach bead stayed in the work ledger; the `blocks` row then names an id the work store cannot resolve", res.RootID)
	}
	deps, err := work.DepList(source.ID, "down")
	if err != nil {
		t.Fatalf("listing attach deps: %v", err)
	}
	if len(deps) == 0 {
		t.Fatalf("attach bead %s has no blocking dep after cook", source.ID)
	}
	for _, dep := range deps {
		if _, err := work.Get(dep.DependsOnID); err != nil {
			t.Errorf("work store holds dep %s -> %s (%s) whose target it cannot resolve: %v", dep.IssueID, dep.DependsOnID, dep.Type, err)
		}
	}
}

// TestFormulaCookAttachStaysOnTheOneStoreOnASingleStoreCity is the single-store
// compatibility row for the whole attach change: a city that relocates nothing
// gets the exact store its scope resolved, so both arms behave as they always
// did.
func TestFormulaCookAttachStaysOnTheOneStoreOnASingleStoreCity(t *testing.T) {
	cityDir := oneShotCookCity(t)
	resetCLIStorageRoutes(t)
	seedCLIStorageRoutes(t, cityDir, nil)
	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	source, err := work.Create(beads.Bead{Title: "attach target", Type: "task"})
	if err != nil {
		t.Fatalf("create attach bead: %v", err)
	}

	for _, formulaName := range []string{"graph-work", "legacy-work"} {
		res := cookFormula(t, formulaName, "--attach", source.ID)
		if _, err := work.Get(res.RootID); err != nil {
			t.Fatalf("%s: sub-DAG root %s is not in the one store: %v", formulaName, res.RootID, err)
		}
		deps, err := work.DepList(source.ID, "down")
		if err != nil {
			t.Fatalf("%s: listing attach deps: %v", formulaName, err)
		}
		for _, dep := range deps {
			if _, err := work.Get(dep.DependsOnID); err != nil {
				t.Errorf("%s: dep %s -> %s has an unresolvable target on a single-store city: %v", formulaName, dep.IssueID, dep.DependsOnID, err)
			}
		}
	}
}
