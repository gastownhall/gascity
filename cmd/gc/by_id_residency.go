package main

// The one-shot CLI's by-id residency seam.
//
// Every one-shot command that holds a bead id and a work store asks the same
// question — "which store actually holds this bead?" — and before the residency
// resolver each asked it in its own words. That is the split-store bug class
// (#5125, #5127): the ordering, the identity gate, the namespace rule and the
// failure classification are four clauses, and every restatement is a chance to
// get one of them wrong.
//
// # The plane keeps its work axis; the resolver owns the binding legs
//
// The CLI's structural difference from internal/api is that its work axis is
// often not a beads.Store at all: `gc bd`'s fall-through is a bd subprocess,
// and the convoy surface's is a directory scan with a uniqueness contract of
// its own. So this seam does not try to own the work half. It hands the plan a
// work leg and reads back storeref's residual contract: the LAST
// RoleWorkFallback leg is returned UNPROBED, which means "no binding answered —
// run your own work axis". A caller whose work axis IS a store passes that
// store and uses the answer directly; a caller whose work axis is a subprocess
// or a scan passes a sentinel and treats it as the signal to fall through.
//
// That residual is also what keeps a single-store city byte-identical: its plan
// has one leg, nothing is probed, and the caller gets back the exact store value
// it passed in — so every optional-capability type assertion it already made
// keeps holding, and it never pays for the funnel.

import (
	"errors"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// cliByIDOwner resolves id over the city's relocated class bindings and the
// caller's own work leg, returning the row a winning binding probe already read.
//
// The plan is Plan(ByID) over a topology of {bindings: cliResidencyBindings,
// work: the caller's leg} with NO rig legs. That absence is deliberate and is
// the pre-resolver behavior preserved: none of these call sites ever read the
// rig stores, and a caller that starts reading them here would silently change
// which beads its command is about. storeref carries rigs as Shadows for the
// surfaces that already hold them open (internal/api's by-id resolver); this one
// passes none, and an empty Shadows list is what keeps the probe order the
// [binding, work] the pre-seam list used.
//
// cfg is nil for the same reason the legacy route never loaded one: the only
// thing a City would contribute here is the work and rig id prefixes, which
// decide SHADOWING — and with no rig legs there is nothing to shadow.
//
// An error is a read that FAILED, never absence. Reading "the binding could not
// answer" as "the bead is not there" is the root-loss shape this lane exists to
// prevent. The one error that is not a fault is the one-shot funnel's standing
// refusal: it is a verdict about a CITY's storage configuration and says nothing
// about a bead, and a refused city still serves WORK from its work ledger. The
// resolver applies that distinction from the leg's own role — a residence probe
// for an id no relocated class could own tolerates the refusal, while the
// authority leg for an id inside a reserved namespace surfaces it, because there
// the refusal IS the answer.
func cliByIDOwner(cityPath, id string, work beads.Store) (storeref.Owner, error) {
	plan, err := cliByIDPlan(cityPath, id, work)
	if err != nil {
		return storeref.Owner{}, err
	}
	owner, err := storeref.ResolveOwnerRow(plan, id)
	switch {
	case err == nil:
		return owner, nil
	case errors.Is(err, beads.ErrNotFound):
		// The second miss shape: every leg cleanly missed and the plan had no
		// work residual to hand back. With no rig legs and no routed work axis
		// this topology reaches it exactly one way — a city whose binding
		// resolved back to the caller's own work store, which dedupeLegs folds
		// into a single probed leg. Then "not found in the binding" and "not
		// found in work" are the same sentence about the same store, and the
		// work leg is the honest answer: the caller reads through it and its own
		// Get produces its own error message, which is the pre-seam behavior.
		return storeref.Owner{Store: work, Ref: storeref.WorkRef}, nil
	default:
		return storeref.Owner{}, err
	}
}

// cliByIDPlan builds the leg list cliByIDOwner probes.
//
// Split out so the before/after order pin
// (TestClassRoutedStoreForIDKeepsThePreSeamCandidateOrder) reads the plan this
// seam actually executes rather than one assembled the same way beside it. A
// pin over a parallel construction proves the topology constructor is right and
// says nothing about whether the seam still uses it.
func cliByIDPlan(cityPath, id string, work beads.Store) (storeref.ResolvedPlan, error) {
	return storeref.Plan(storeref.ByID{ID: id}, cliResidencyTopology(cityPath, nil, work, nil))
}
