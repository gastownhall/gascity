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
	"fmt"

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

// cliByIDBindingOwner answers the binding half of the by-id question for a
// surface whose work axis is not a beads.Store.
//
// `gc convoy`'s resolution is a directory scan with a uniqueness contract — it
// probes every candidate and REFUSES an id present in more than one — and `gc
// beads show`'s is the same scan taking the first hit. Neither is expressible
// as a leg, and neither should be: the scan is what those commands are. What
// they were missing is the leg in FRONT of it. A relocated class binding is not
// one of the city's directories, so a directory scan cannot reach it at all,
// and the id it cannot reach is answered instead by the retained pre-migration
// copy sitting in the city store — successfully, with no error to notice.
//
// So the plan is resolved with a SENTINEL where the work leg goes and the
// answer is read as a yes/no. ok=true is a binding that owns the id, with the
// row it already read; ok=false is "no binding answered, run your own scan",
// and the caller then does exactly what it did before.
func cliByIDBindingOwner(cityPath, id string) (storeref.Owner, bool, error) {
	owner, err := cliByIDOwner(cityPath, id, unprobedWorkResidual{})
	if err != nil {
		return storeref.Owner{}, false, err
	}
	if _, isResidual := owner.Store.(unprobedWorkResidual); isResidual {
		return storeref.Owner{}, false, nil
	}
	return owner, true, nil
}

// beadForOwner returns the row the owner names, reading it only when the
// resolver has not already. A probed leg's read IS the caller's read, and doing
// it again doubles every by-id operation against a relocated city's binding.
func beadForOwner(owner storeref.Owner, id string) (beads.Bead, error) {
	if owner.Read {
		return owner.Bead, nil
	}
	return owner.Store.Get(id)
}

// unprobedWorkResidual stands in for a work axis the resolver must not run.
//
// Plan(ByID) ends every plan in a work leg and hands the LAST one back
// UNPROBED, which is the contract cliByIDBindingOwner rests on: this value is
// returned, never read. Its Get therefore reports a bug rather than a miss — a
// resolver that started probing the residual would otherwise turn "the caller
// runs its own scan" into "the bead is absent", silently, on every convoy
// command of every relocated city.
type unprobedWorkResidual struct{ beads.Store }

// Get reports the contract violation described on the type.
func (unprobedWorkResidual) Get(id string) (beads.Bead, error) {
	return beads.Bead{}, fmt.Errorf("internal: the by-id work residual was probed for %s; it is a placeholder for a work axis this surface runs itself", id)
}
