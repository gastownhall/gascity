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
// or a scan asks only the binding half, through storeref.ResolveBindingOwner,
// and reads its ok=false as the signal to fall through.
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
// prevent. The one error that is not always a fault is the one-shot funnel's
// standing refusal: it is a verdict about a CITY's storage configuration and
// says nothing about a bead, and a refused city still serves WORK from its work
// ledger. The resolver applies that distinction from the leg's own role — the
// authority leg for an id inside a reserved namespace surfaces the refusal,
// because there the refusal IS the answer, while a residence probe for an id no
// relocated class could own tolerates it.
//
// That toleration is conditional, and the condition is a relic census proving
// the binding holds ids the migration preserved. Then the probe's miss is not
// harmless — the work store's frozen pre-migration copy is what the caller
// falls through to — so the refusal is surfaced there too, wrapped by
// withProvenRelicRemedy with the move that clears it.
func cliByIDOwner(cityPath, id string, work beads.Store) (storeref.Owner, error) {
	return byIDOwnerForTopology(byIDResidencyTopology(cityPath, nil, work, nil), id, work)
}

// byIDOwnerForTopology is cliByIDOwner over a topology the caller already holds.
//
// The split is at the topology line because that is the only thing the one-shot
// funnel contributes: everything below it — the ByID plan, the residual
// contract, the two miss shapes — is the same question asked of whatever legs
// are in play. A caller that captured its topology at construction gets the
// resolver's rules without re-entering the funnel per bead, which is what makes
// its probe handle and its write handle the same store by construction rather
// than by coincidence.
//
// work is passed again rather than read back off the topology: it is the value
// the not-found fallback below must return, and reaching into topo.Work.Store
// for it would be this file picking a store out of a leg — the residency
// boundary's one forbidden move, and pointless when the caller already has it.
func byIDOwnerForTopology(topo storeref.Topology, id string, work beads.Store) (storeref.Owner, error) {
	plan, err := byIDPlanForTopology(topo, id)
	if err != nil {
		return storeref.Owner{}, err
	}
	owner, err := withProvenRelicRemedy(storeref.ResolveOwnerRow(plan, id))
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
// The bindings come from residencyTopologyForCity, so a plan built INSIDE a
// running controller prefers the routes that controller registered at boot and
// only a genuine one-shot command falls through to the funnel. That preference
// is not a tidiness: this seam is reachable in-process — order dispatch reaches
// it through the autoclose owner resolution — and resolving a controller's
// bindings through the one-shot funnel would open a SECOND handle on the same
// binding root, a duplicate managed-Dolt server or a second sqlite writer,
// which is the reason residencyTopologyForCity exists and documents.
//
// Split out so the before/after order pin
// (TestClassRoutedStoreForIDKeepsThePreSeamCandidateOrder) reads the plan this
// seam actually executes rather than one assembled the same way beside it. A
// pin over a parallel construction proves the topology constructor is right and
// says nothing about whether the seam still uses it.
//
// It stays a composition of the two calls cliByIDOwner makes, not a third
// spelling of them: the same residencyTopologyForCity and the same
// byIDPlanForTopology, so the pin cannot drift from the executed path without
// one of those two changing under both.
func cliByIDPlan(cityPath, id string, work beads.Store) (storeref.ResolvedPlan, error) {
	return byIDPlanForTopology(byIDResidencyTopology(cityPath, nil, work, nil), id)
}

// byIDPlanForTopology is the ByID plan itself, the one line both the cityPath
// and the captured-topology forms go through.
func byIDPlanForTopology(topo storeref.Topology, id string) (storeref.ResolvedPlan, error) {
	return storeref.Plan(storeref.ByID{ID: id}, topo)
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
// So the plan is executed by storeref.ResolveBindingOwner, the executor that
// walks the binding legs and stops at the work axis. ok=true is a binding that
// owns the id, with the row it already read; ok=false is "no binding answered,
// run your own scan", and the caller then does exactly what it did before.
func cliByIDBindingOwner(cityPath, id string) (storeref.Owner, bool, error) {
	return byIDBindingOwnerForTopology(byIDResidencyTopology(cityPath, nil, newUnprobedWorkResidual(), nil), id)
}

// byIDBindingOwnerForTopology is cliByIDBindingOwner over a topology the caller
// already holds.
//
// The work leg is still supplied — Plan(ByID) ends every plan in one, and a
// topology without it is not a topology — but the executor never reads it, so
// what goes there only has to be a beads.Store. Both call sites pass the
// unprobedWorkResidual sentinel, which turns a resolver that started probing
// the work leg into a loud error here rather than a silent "the work ledger
// owns it" on every convoy command of every relocated city.
func byIDBindingOwnerForTopology(topo storeref.Topology, id string) (storeref.Owner, bool, error) {
	plan, err := byIDPlanForTopology(topo, id)
	if err != nil {
		return storeref.Owner{}, false, err
	}
	owner, ok, err := storeref.ResolveBindingOwner(plan, id)
	owner, err = withProvenRelicRemedy(owner, err)
	return owner, ok, err
}

// withProvenRelicRemedy names the operator's next move on the one denial that
// is otherwise unactionable.
//
// storeref decides that a refused binding proven to hold migration-preserved
// ids may not be skipped, and says why — but it does not know what clears the
// condition, because that is a fact about this plane's boot gate rather than
// about a plan. The refusal's own text is the boot gate's, identical to the one
// a city sees for an infrastructure-class id it simply cannot serve, so an
// operator reading it looks for a missing bead. What they need is that
// converging the split is what makes the id resolve from the binding again.
//
// The remedy is a SECOND SENTENCE on one line, not a second line. Every caller
// prints this value as `gc <cmd>: %v`, so an embedded newline drops the command
// prefix off everything after it and the operator cannot tell which surface
// refused.
func withProvenRelicRemedy(owner storeref.Owner, err error) (storeref.Owner, error) {
	if err == nil || !errors.Is(err, storeref.ErrProvenRelicRefusal) {
		return owner, err
	}
	return owner, fmt.Errorf("%w. Converge the configured [storage] split and this id resolves from the binding again; `gc doctor` reports what is outstanding", err)
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
// Plan(ByID) ends every plan in a work leg, so a surface that owns its own work
// axis still has to put something there. storeref.ResolveBindingOwner
// guarantees the leg is never read; this value makes a broken guarantee visible
// instead of plausible. Its Get reports a bug rather than a miss, because a
// resolver that started probing here would otherwise turn "the caller runs its
// own scan" into "the work ledger owns it" — silently, on every convoy command
// of every relocated city.
//
// It refuses through a WHOLE beads.Store rather than an embedded nil one, for
// the reason refusedClassStore already carries: a nil embedded interface makes
// every method this file does not override a nil-pointer panic naming a line of
// runtime.go, and the one method that must never be reached is exactly the one
// least likely to be the first a future leg role calls. Get is overridden only
// to name the id in the message; every other operation reports the same
// violation.
type unprobedWorkResidual struct{ refusedClassStore }

// errWorkResidualProbed is what every residual operation but Get reports.
var errWorkResidualProbed = errors.New("internal: the by-id work residual was operated on; it is a placeholder for a work axis this surface runs itself")

// newUnprobedWorkResidual builds the sentinel with its refusal armed.
//
// The zero value would embed a refusedClassStore with a nil error, and a
// refusing store that returns nil is worse than one that panics: List would
// answer "no beads" and the caller would read the residual as absence, which is
// the exact confusion this type exists to prevent. Both construction sites go
// through here so no zero value is ever in play.
func newUnprobedWorkResidual() unprobedWorkResidual {
	return unprobedWorkResidual{refusedClassStore{err: errWorkResidualProbed}}
}

// Get reports the contract violation described on the type, naming the id the
// resolver reached for.
func (unprobedWorkResidual) Get(id string) (beads.Bead, error) {
	return beads.Bead{}, fmt.Errorf("%w: probed for %s", errWorkResidualProbed, id)
}
