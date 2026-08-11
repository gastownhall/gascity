package main

// The class half of `gc formula cook --attach` on a split city.
//
// oneshot_by_id_routes_test.go answers WHERE the graft is written (the store
// that holds the attach bead). This file answers whether it may be written at
// all: a graft materializes GRAPH-class beads whatever the formula's compiler
// version, so grafting onto a bead the WORK ledger holds mints graph-class rows
// in the work store — the stranded write a converged city's own containment
// check counts and every later command refuses on (ga-99xhy, live-proven on a
// throwaway split city as `stranded: 4` with `gc storage status` exiting 1).

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
)

// convergedSplitCookCity builds the one city that can answer both halves of
// this bead: it cooks formulas AND reports its own storage layout.
//
// It is the one-shot cook fixture cut over by the PRODUCTION migration onto a
// real SQLite binding, so `gc storage status` has a marker, a non-empty
// proven-copy manifest and a real destination to re-check containment against —
// the state in which a stranded write is detectable at all. The CLI's own
// routes are pointed at that same binding, so the cook command and the
// containment check are talking about one topology rather than two fixtures
// that happen to agree.
func convergedSplitCookCity(t *testing.T) (cityDir string, cfg *config.City, work beads.Store, target infraBindingTarget) {
	t.Helper()
	cityDir = oneShotCookCity(t)
	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return work, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	// One infrastructure bead so the proven-copy manifest is non-empty: an
	// empty manifest turns stranded-write detection off and the assertion below
	// would prove nothing.
	mustCreateInfraBead(t, work, beads.Bead{Title: "live session", Type: "session", Labels: []string{"gc:session"}})

	cfg = infraSplitConfig(filepath.Join(cityDir, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityDir, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	target = mustResolveInfraTarget(t, cityDir, cfg)
	seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(openMigratedDestination(t, target)))
	return cityDir, cfg, work, target
}

// storageStatusExit runs the read-only `gc storage status` body and returns its
// exit code with everything it said.
func storageStatusExit(t *testing.T, cityPath string, cfg *config.City) (int, string) {
	t.Helper()
	stubInfraControllerPing(t, 0)
	var stdout, stderr bytes.Buffer
	code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// TestFormulaCookAttachOnAWorkResidentBeadStrandsNothingOnAConvergedSplitCity
// is the live proof, reproduced in-process.
//
// Red-before, on main, it fails once per arm with the city's own containment
// check. graph-work:
//
//	gc formula cook --attach gc-2 left the city stranded: `gc storage status`
//	exited 1 and reports:
//	  ...
//	  converged: yes
//	    proven copy: 1 bead(s)
//	    stranded:    3
//	  stranded ids: gc-4, gc-5, gc-6
//	(the cook itself said err=<nil>: Attached: gc-2 -> gc-4 (root: gc-4))
//
// legacy-work:
//
//	    stranded:    2
//	  stranded ids: gc-3, gc-4
//	(the cook itself said err=<nil>: Attached: gc-2 -> gc-3 (root: gc-2))
//
// Same shape as the `stranded: 4` measured on the live throwaway city, and the
// same shape as the ~42/hr accrual that made maintainer-city boot-fatal —
// reached through a path `gc formula cook --help` documented as correct. The
// count is the fixture's step count, not a constant.
func TestFormulaCookAttachOnAWorkResidentBeadStrandsNothingOnAConvergedSplitCity(t *testing.T) {
	for _, formulaName := range []string{"graph-work", "legacy-work"} {
		t.Run(formulaName, func(t *testing.T) {
			cityDir, cfg, work, _ := convergedSplitCookCity(t)
			if code, said := storageStatusExit(t, cityDir, cfg); code != 0 {
				t.Fatalf("the fixture is not clean before the cook: `gc storage status` exited %d: %s", code, said)
			}
			source, err := work.Create(beads.Bead{Title: "attach target", Type: "task"})
			if err != nil {
				t.Fatalf("create attach bead: %v", err)
			}

			out, cookErr := cookFormulaErr(t, formulaName, "--attach", source.ID)

			code, said := storageStatusExit(t, cityDir, cfg)
			if code != 0 {
				t.Fatalf("gc formula cook --attach %s left the city stranded: `gc storage status` exited %d and reports:\n%s\n(the cook itself said err=%v: %s)", source.ID, code, said, cookErr, out)
			}
			if !strings.Contains(said, "stranded:    0") {
				t.Errorf("`gc storage status` does not report a clean ledger after the cook: %s", said)
			}
		})
	}
}

// TestFormulaCookAttachOnAWorkResidentBeadIsRefusedOnSplitCity is the refusal
// itself, for the graph.v2 arm.
//
// It replaces TestFormulaCookAttachOnAWorkResidentBeadIsUnchanged, which pinned
// this shape as SERVED and is the behavior ga-99xhy found minting strands. The
// deferral that test described has not been closed — the block still cannot be
// represented across the store boundary (ga-2orlf) — so the graft is refused
// rather than mis-homed in either direction.
//
// Red-before, on main:
//
//	gc formula cook graph-work --attach gc-2 exited 0 on a split city; a graft
//	that mints graph-class beads in the work ledger must refuse, not serve
func TestFormulaCookAttachOnAWorkResidentBeadIsRefusedOnSplitCity(t *testing.T) {
	work, graph := cookCityWithSplitGraph(t)

	source, err := work.Create(beads.Bead{Title: "attach target", Type: "task"})
	if err != nil {
		t.Fatalf("create attach bead: %v", err)
	}
	before := beadIDs(allBeads(t, work))

	out, err := cookFormulaErr(t, "graph-work", "--attach", source.ID)
	if err == nil {
		t.Fatalf("gc formula cook graph-work --attach %s exited 0 on a split city; a graft that mints graph-class beads in the work ledger must refuse, not serve\n%s", source.ID, out)
	}
	assertAttachRefusalNamesItsReason(t, out, source.ID)
	assertRefusedGraftWroteNothing(t, work, graph, source.ID, before)
}

// TestFormulaCookLegacyAttachOnAWorkResidentBeadIsRefusedOnSplitCity is the v1
// arm, and it is the half #5163's reasoning did NOT already cover.
//
// #5163 kept the v1 arm's capability because a v1 formula returns from
// PrepareInvocation before NormalizeInputConvoy and therefore mints no
// work-class input convoy. That is still true, and it is about the CONVOY, not
// about the sub-DAG: molecule.Attach stamps gc.root_bead_id on every step it
// materializes, which is coordclass.Classify's workflow arm, so a v1 graft's
// beads are graph class exactly like a v2 graft's. Grafted onto a work-resident
// bead they are stranded writes just the same, and the arm is refused for the
// same reason.
//
// Red-before, on main:
//
//	gc formula cook legacy-work --attach gc-2 exited 0 on a split city; a v1
//	graft stamps gc.root_bead_id on every step, so its sub-DAG is graph class
//	and mints strands in the work ledger
func TestFormulaCookLegacyAttachOnAWorkResidentBeadIsRefusedOnSplitCity(t *testing.T) {
	work, graph := cookCityWithSplitGraph(t)

	source, err := work.Create(beads.Bead{Title: "attach target", Type: "task"})
	if err != nil {
		t.Fatalf("create attach bead: %v", err)
	}
	before := beadIDs(allBeads(t, work))

	out, err := cookFormulaErr(t, "legacy-work", "--attach", source.ID)
	if err == nil {
		t.Fatalf("gc formula cook legacy-work --attach %s exited 0 on a split city; a v1 graft stamps gc.root_bead_id on every step, so its sub-DAG is graph class and mints strands in the work ledger\n%s", source.ID, out)
	}
	assertAttachRefusalNamesItsReason(t, out, source.ID)
	assertRefusedGraftWroteNothing(t, work, graph, source.ID, before)
}

// assertAttachRefusalNamesItsReason requires the refusal to carry everything an
// operator needs: which bead, which class each end is, why the graft is not
// expressible, the bead that will make it expressible, and — for the operator
// who already HAS strands from this path — the verb that repairs them.
func assertAttachRefusalNamesItsReason(t *testing.T, out, attachBeadID string) {
	t.Helper()
	for _, want := range []string{
		attachBeadID,
		"work",
		"graph",
		"ga-2orlf",
		storageRecoveryInstruction(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal %q does not mention %q; the reason and the remedy have to travel with the refusal", out, want)
		}
	}
}

// assertRefusedGraftWroteNothing requires a refusal to be a refusal: no bead in
// either ledger, and no dep on the attach bead. A half-graft is the state this
// change exists to prevent, so a refusal that wrote one would be worse than the
// bug.
func assertRefusedGraftWroteNothing(t *testing.T, work, graph beads.Store, attachBeadID string, before []string) {
	t.Helper()
	if got := beadIDs(allBeads(t, work)); len(got) != len(before) {
		t.Errorf("the work ledger holds %v after a refused graft, want the %v it held before; a refusal writes nothing", got, before)
	}
	if got := allBeads(t, graph); len(got) != 0 {
		t.Errorf("the binding holds %d bead(s) after a refused graft: %+v", len(got), got)
	}
	if deps, err := work.DepList(attachBeadID, "down"); err != nil {
		t.Fatalf("listing attach deps: %v", err)
	} else if len(deps) > 0 {
		t.Errorf("attach bead %s gained %d dep(s) from a refused graft: %+v", attachBeadID, len(deps), deps)
	}
}

// TestAttachedSubDAGIsGraphClassWhateverTheFormulaVersion pins the premise the
// refusal rests on, so it is measured rather than asserted in prose.
//
// The class of a graft is not the class of its recipe. molecule.Attach stamps
// gc.root_bead_id on EVERY step before instantiating — resolved through the run
// chain and falling back to the attach bead's own id, so it is never empty —
// and a non-empty gc.root_bead_id is coordclass.Classify's workflow arm. So a
// v1 POURED formula, which cooks standalone into the work ledger as ClassWork
// (TestFormulaCookLegacyMoleculeStaysOnTheWorkStore), materializes a GRAPH-class
// sub-DAG the moment it is grafted.
func TestAttachedSubDAGIsGraphClassWhateverTheFormulaVersion(t *testing.T) {
	store := beads.NewMemStore()
	parent, err := store.Create(beads.Bead{Title: "existing work", Type: "task"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	recipe := &formula.Recipe{
		Name: "poured",
		Steps: []formula.RecipeStep{
			{ID: "poured", Title: "Poured", Type: "molecule", IsRoot: true},
			{ID: "poured.sweep", Title: "Sweep", Type: "task", Assignee: "worker"},
		},
	}
	if got := recipeCoordClass(recipe); got != coordclass.ClassWork {
		t.Fatalf("the fixture recipe is %v, not %v; it has to be the shape a standalone cook leaves in the work ledger", got, coordclass.ClassWork)
	}

	result, err := molecule.Attach(context.Background(), store, recipe, parent.ID, molecule.AttachOptions{})
	if err != nil {
		t.Fatalf("molecule.Attach: %v", err)
	}

	graftedGraphBeads := 0
	for _, b := range allBeads(t, store) {
		if b.ID == parent.ID {
			continue
		}
		if coordclass.Classify(b) != coordclass.ClassGraph {
			t.Errorf("grafted bead %s (%q) classifies as %v, not %v (gc.root_bead_id=%q)", b.ID, b.Title, coordclass.Classify(b), coordclass.ClassGraph, b.Metadata[beadmeta.RootBeadIDMetadataKey])
			continue
		}
		graftedGraphBeads++
	}
	if graftedGraphBeads != result.Created {
		t.Fatalf("the graft created %d bead(s) and %d classify as graph; the refusal's premise is that ALL of them do", result.Created, graftedGraphBeads)
	}
}
