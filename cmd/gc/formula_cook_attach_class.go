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
// "The work ledger" means a city's AND a rig's. Under the S4 placement rule a
// split city keeps graph rows out of every work store, rig stores included:
// rig stores hold only work. So a graft onto a rig WORK bead is refused exactly
// as one onto a city work bead is. The one rig-store graft that stays served is
// onto a graph bead of a tree the rig minted before S4 (a relic, while the rig
// is not yet imported): the graft extends that tree in the store that holds it
// (R-S4.2), so its edge stays co-resident.
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
// attach bead in. The gates, in the order that keeps a single-store city
// untouched:
//
//   - the city relocates nothing, so there is no second store and no class to
//     be on the wrong side of. Returns before reading anything, which is what
//     makes an unsplit `--attach` byte-identical to what it always did.
//   - the binding holds the attach bead, so the graft lands in the binding with
//     it: graph-class beads in the graph store, co-resident with the edge.
//   - the scope store does not hold it either, so this is a bad id rather than
//     a bad topology. The arms' own "attach bead <id>: bead not found" is a
//     better answer than a topology lecture, so it is left to them.
//   - a RIG scope whose store still holds the attach bead as a GRAPH bead —
//     a node of a tree the rig minted before S4, which stays in the rig until
//     the rig is imported. The graft follows residence and stays co-resident
//     (R-S4.2), so it is served.
//   - everything else is a graft onto a WORK bead in a work store, the city's
//     or a rig's, and is refused: neither end of its edge is expressible
//     without writing graph rows into a work store or a cross-store edge.
func attachGraftClassRefusal(cityPath, scopeRoot, attachBeadID string, scope, attachStore beads.Store) error {
	routes := cliStorageRoutes(cityPath)
	class, relocated := graphClassBinding(routes)
	if !relocated || class == nil || class == scope {
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
	if !scopeIsCity(cityPath, scopeRoot) {
		rigScope := routes.rigGraphScopeForDir(cityPath, scopeRoot)
		if coordclass.Classify(source) == coordclass.ClassGraph && graphRelicLegForScope(routes, rigScope) {
			return nil
		}
		return rigAttachGraftRefusal(attachBeadID, source)
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

// rigAttachGraftRefusal is the rig-scope refusal (S4 decision D6): on a split
// city a rig's store holds only work, and every graph row of the rig lives in
// the city binding, so a graft onto a rig work bead has no expressible
// placement. It names the supported alternative.
func rigAttachGraftRefusal(attachBeadID string, source beads.Bead) error {
	return fmt.Errorf(
		"--attach %s: %s is a %s-class bead in this rig's own store, and on a split city a rig store holds only work — every graph bead of every rig lives in the city's graph binding, with the rig recorded as gc.root_store_ref. A graft is graph class whatever the formula's version, so written beside %s it would put graph rows in the rig's work store, and written into the binding it would leave a `blocks` edge crossing stores, which is never written. --attach onto a rig work bead is therefore not supported on a split city. Launch the formula against the bead instead: `gc sling <target> %s --on <formula>` attaches it as a workflow in the binding that tracks %s without a cross-store edge, and `gc formula cook <formula>` without --attach starts a new root there",
		attachBeadID,
		attachBeadID,
		coordclass.Classify(source),
		attachBeadID,
		attachBeadID,
		attachBeadID,
	)
}

// rigAttachGraphV2Refusal refuses a graph.v2 graft onto a rig-resident bead on
// a split city. The only rig-resident bead attachGraftClassRefusal lets through
// is a graph node of a pre-S4 tree; a graph.v2 pour is a NEW root, which the
// placement rule puts in the binding, while its attach edge would stay with the
// rig-resident bead — a cross-store edge. The v1 arm extends the tree in place
// and is the one that serves it.
func rigAttachGraphV2Refusal(attachBeadID string) error {
	return fmt.Errorf(
		"--attach %s: %s lives in this rig's own store (a tree created before the rig's graph beads moved to the city binding), and a graph.v2 formula starts a NEW workflow root, which a split city always creates in the binding; its attach edge to %s would cross stores, which is never written. Attach a v1 formula to %s instead, which extends the existing tree where it lives, or launch the formula with `gc sling <target> %s --on <formula>`",
		attachBeadID, attachBeadID, attachBeadID, attachBeadID, attachBeadID)
}
