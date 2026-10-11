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
//
// "The work ledger" is the CITY's. The relocation never moves a rig store, so a
// rig-scoped graft is co-resident in the rig's own ledger and stays served —
// TestFormulaCookAttachInRigScopeStaysServedOnASplitCity is that row.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
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

// rigScopedSplitCookCity is the fixture for the OTHER scope: the same split
// city, plus a bound rig with its own file-backed ledger, entered the way every
// controller-spawned agent enters one — the ambient GC_RIG the controller sets,
// which resolveFormulaScope honors even when cwd is elsewhere.
//
// It returns the rig's store, the city work store and the binding, so a test
// can tell all three apart. The rig gets its own control-dispatcher agent
// because a graph.v2 cook in rig scope decorates its recipe against one; that
// requirement predates this bead and is not what is under test.
func rigScopedSplitCookCity(t *testing.T) (rig, cityWork, graph beads.Store) {
	t.Helper()
	cityDir := oneShotCookCity(t)
	rigDir := filepath.Join(cityDir, "wf")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	cityTOML := filepath.Join(cityDir, "city.toml")
	declared, err := os.ReadFile(cityTOML)
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	declared = append(declared, []byte(testControlDispatcherAgentTOML("wf"))...)
	declared = append(declared, []byte("\n[[rigs]]\nname = \"wf\"\n")...)
	if err := os.WriteFile(cityTOML, declared, 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeCatalogFile(t, cityDir, ".gc/site.toml", fmt.Sprintf("[[rig]]\nname = \"wf\"\npath = %q\n", rigDir))

	// Scoped file-store roots, so the rig has a ledger of its own rather than
	// aliasing the city's — without the marker every scope shares one file and
	// the fixture would prove nothing.
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	for _, root := range []string{cityDir, rigDir} {
		if err := ensurePersistedScopeLocalFileStore(root); err != nil {
			t.Fatalf("ensurePersistedScopeLocalFileStore(%s): %v", root, err)
		}
	}

	graph = splittest.NewClassStore(t, config.BeadClassGraph)
	seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))

	// GC_RIG with no --rig is the shape under test: the persistent flag is the
	// higher-priority tier, so a value another test left on the package global
	// would resolve a different scope and the ambient path would go unexercised.
	prevRigFlag := rigFlag
	t.Cleanup(func() { rigFlag = prevRigFlag })
	rigFlag = ""
	t.Setenv("GC_RIG", "wf")

	if rig, err = openStoreAtForCity(rigDir, cityDir); err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	if cityWork, err = openStoreAtForCity(cityDir, cityDir); err != nil {
		t.Fatalf("open city work store: %v", err)
	}
	if got := beadIDs(allBeads(t, cityWork)); len(got) != 0 {
		t.Fatalf("the city work store already holds %v; the fixture's premise is that only the rig ledger does", got)
	}
	return rig, cityWork, graph
}

// TestFormulaCookAttachInRigScopeStaysServedOnASplitCity is the scope half of
// the class gate, and the reason the gate asks controlScopeTakesGraphClass
// before it asks anything else.
//
// A relocation is a CITY-scope event. `gc storage migrate` copies only the city
// work store (openInfraMigrationSource -> openStoreAtForCity(cityPath,
// cityPath)), resolveClassStore holds one city-level store per class with no
// per-rig binding to route to, and controlGraphStore hands a rig scope back the
// very store it was given — the three facts controlScopeTakesGraphClass's own
// doc comment states. So a rig's ledger holds BOTH ends of a graft, the city's
// containment check never reads it, and nothing a rig-scoped cook writes can be
// stranded. Refusing it would also hand the operator an inoperative remedy:
// `gc storage recover-stranded --from-work` reads the city work store.
//
// GC_RIG is ambient on every controller-spawned agent, so a gate that asked the
// city question and applied it to the rig store took `--attach` away from
// essentially every agent on a split city with rigs.
//
// Red-before, with the scope gate removed — both rows:
//
//	gc formula cook legacy-work --attach gc-1 was refused in RIG scope on a
//	split city: exit
//	gc formula cook: --attach gc-1: gc-1 is work class and lives in a WORK
//	store rather than in the graph binding, and the sub-DAG a graft
//	materializes is graph class whatever the formula's version ...
//	a rig store is never relocated, so both ends of the graft are co-resident
//	in it and nothing it writes can be stranded
func TestFormulaCookAttachInRigScopeStaysServedOnASplitCity(t *testing.T) {
	for _, formulaName := range []string{"legacy-work", "graph-work"} {
		t.Run(formulaName, func(t *testing.T) {
			rig, cityWork, graph := rigScopedSplitCookCity(t)

			source, err := rig.Create(beads.Bead{Title: "attach target", Type: "task"})
			if err != nil {
				t.Fatalf("create attach bead: %v", err)
			}

			out, err := cookFormulaErr(t, formulaName, "--attach", source.ID)
			if err != nil {
				t.Fatalf("gc formula cook %s --attach %s was refused in RIG scope on a split city: %v\n%s\na rig store is never relocated, so both ends of the graft are co-resident in it and nothing it writes can be stranded", formulaName, source.ID, err, out)
			}
			assertGraftIsCoResidentIn(t, rig, source.ID)
			if got := beadIDs(allBeads(t, cityWork)); len(got) != 0 {
				t.Errorf("the city work store holds %v after a rig-scoped graft; a rig scope writes to its own ledger", got)
			}
			if got := allBeads(t, graph); len(got) != 0 {
				t.Errorf("the binding holds %d bead(s) after a rig-scoped graft: %+v", len(got), got)
			}
		})
	}
}

// assertGraftIsCoResidentIn requires the graft to be a real graft in ONE store:
// the attach bead gained a blocking dep, the store resolves its target, and the
// store holds GRAPH-class beads the graft minted. The last part is what makes
// this the same shape the city-scope gate refuses — a rig graft is served
// because of WHERE it lands, not because its beads are somehow work class.
//
// Not every grafted bead is graph class: a graph.v2 invocation also mints a
// synthetic input convoy, which coordclass classifies as WORK by design. In a
// rig scope both classes land in the one ledger, which is exactly why nothing
// here is cross-store.
func assertGraftIsCoResidentIn(t *testing.T, store beads.Store, attachBeadID string) {
	t.Helper()
	deps, err := store.DepList(attachBeadID, "down")
	if err != nil {
		t.Fatalf("listing attach deps: %v", err)
	}
	if len(deps) == 0 {
		t.Fatalf("attach bead %s has no blocking dep; the graft was never wired", attachBeadID)
	}
	for _, dep := range deps {
		if _, err := store.Get(dep.DependsOnID); err != nil {
			t.Errorf("the rig store holds dep %s -> %s (%s) whose target it cannot resolve: %v", dep.IssueID, dep.DependsOnID, dep.Type, err)
		}
	}
	graphClass := 0
	for _, b := range allBeads(t, store) {
		if b.ID == attachBeadID {
			continue
		}
		if coordclass.Classify(b) == coordclass.ClassGraph {
			graphClass++
		}
	}
	if graphClass == 0 {
		t.Fatalf("the rig store holds no %v-class bead from the graft (%v); the premise is that a rig graft mints the same class the city-scope gate refuses", coordclass.ClassGraph, beadIDs(allBeads(t, store)))
	}
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
