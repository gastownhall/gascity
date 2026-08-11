package main

// The class gate on `gc formula cook --attach`.
//
// by_id_store_route.go answers WHERE a graft is written: the store that holds
// the attach bead, so the two ends of the blocking edge stay co-resident. This
// file answers whether that store is allowed to hold it.
//
// A graft is always GRAPH class. Not "usually", and not "when the formula is
// v2": molecule.Attach stamps gc.root_bead_id on every step it materializes
// (resolved through the run chain, falling back to the attach bead's own id, so
// it is never empty) and a non-empty gc.root_bead_id is coordclass.Classify's
// workflow arm. A graph.v2 pour is graph class end to end for its own reasons.
// So on a city that serves the graph class from its own binding, a graft onto a
// WORK-resident bead writes graph-class rows into the work ledger — the
// stranded write the city's own containment check counts, `gc storage status`
// exits 1 on, and every later command carries as a permanent alarm.
//
// "The work ledger" here means the CITY's. A relocation moves one city-level
// store per class and leaves every rig store exactly where it was, so a
// rig-scoped graft is co-resident in the rig's own ledger and is not a stranded
// write at all — see the scope gate in attachGraftClassRefusal.
//
// The other placement is worse and is not re-shipped: #5150 moved the sub-DAG
// to the binding and left the work store holding a `blocks` row naming an id it
// could not resolve, which no backend rejects and every Ready implementation
// reads as a blocker that never clears.
//
// Neither end being expressible is what makes this a REFUSAL rather than a
// route. Nothing automated calls `cook --attach` — it has no caller anywhere in
// the tree, in any pack, formula, prompt or script — so the whole cost is borne
// by a human or an agent invoking it directly, and being told "not yet
// supported on a split city" is strictly better than holding a permanently
// alarming ledger. The mechanism that lifts it is a cross-class membership
// edge: ga-2orlf.

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// attachGraftClassRefusal returns the reason a graft onto attachBeadID cannot
// be expressed on this city, or nil when it can.
//
// scopeRoot is the directory resolveFormulaScope selected, scope is the store
// opened for it, and attachStore is the store classRoutedStoreForID found the
// attach bead in. Four gates, in the order that keeps a single-store city
// untouched:
//
//   - the city relocates nothing, so there is no second store and no class to
//     be on the wrong side of. Returns before reading anything, which is what
//     makes an unsplit `--attach` byte-identical to what it always did.
//   - the scope is a RIG, whose store the relocation never touched.
//     controlScopeTakesGraphClass is the dispatcher's own predicate for this and
//     the reason is the same one stated there: `gc storage migrate` copies only
//     the CITY work store (openInfraMigrationSource), resolveClassStore holds a
//     single city-level store per class with no per-scope binding to route a rig
//     to, and controlGraphStore hands a rig scope back the store it was given.
//     A rig graft is therefore co-resident in the rig's own ledger and cannot be
//     stranded — the city-level containment check never reads that store, and
//     the remedy this refusal names, which reads the city work store, would
//     repair nothing there. Asking the city question and applying it to a rig
//     store refused a shape that has always worked, and GC_RIG is ambient on
//     every controller-spawned agent, so the answer had to be scoped.
//   - the binding holds the attach bead, so the graft lands in the binding with
//     it: graph-class beads in the graph store, co-resident with the edge. That
//     is the shape the v1 arm serves today and it stays served.
//   - the work ledger does not hold it either, so this is a bad id rather than
//     a bad topology. The arms' own "attach bead <id>: bead not found" is a
//     better answer than a topology lecture, so it is left to them.
func attachGraftClassRefusal(cityPath, scopeRoot, attachBeadID string, scope, attachStore beads.Store) error {
	class, relocated := graphClassBinding(cliStorageRoutes(cityPath))
	if !relocated || class == nil || class == scope {
		return nil
	}
	if !controlScopeTakesGraphClass(cityPath, scopeRoot) {
		return nil
	}
	if attachStore != scope {
		return nil
	}
	source, err := scope.Get(attachBeadID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("reading attach bead %q from the work ledger: %w", attachBeadID, err)
	}
	return fmt.Errorf(
		"--attach %s: %s is %s class and lives in a WORK store rather than in the %s binding, and the sub-DAG a graft materializes is %s class whatever the formula's version — molecule.Attach stamps gc.root_bead_id on every step it creates, and a graph.v2 pour is graph class end to end. This city serves the %s class from its own binding, so neither end of the graft is expressible: written beside %s the sub-DAG is a graph-class bead born in the work ledger, which is the stranded write `%s` counts and every later command refuses on; written into the binding the work store keeps a `blocks` row naming an id it cannot resolve, which no backend rejects and every Ready implementation reads as a blocker that never clears (#5150). Carrying that block across the store boundary needs a cross-class membership edge: ga-2orlf. Until it lands, --attach onto a work-resident bead is not supported on a split city; attach to a bead the binding owns instead. If an earlier cook already stranded beads here, stop every writer and copy them into the binding with `%s`",
		attachBeadID,
		attachBeadID,
		coordclass.Classify(source),
		coordclass.ClassGraph,
		coordclass.ClassGraph,
		coordclass.ClassGraph,
		attachBeadID,
		storageStatusInstruction(),
		storageRecoveryInstruction(),
	)
}
