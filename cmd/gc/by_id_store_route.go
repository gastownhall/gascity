package main

// By-id store routing for the one-shot commands that hold a bead id and a work
// store, and have to read or mutate THAT bead.
//
// `gc bd` answers the same question through its own closed front door
// (cmd_bd_by_id.go), because its fall-through leg is a `bd` subprocess rather
// than a beads.Store. Every other one-shot by-id call site holds two ordinary
// stores and needs the ordering, the identity gate and the failure
// classification to come from one place — answering "which store owns this
// bead?" a second time is how this repo's split-store bug class reproduces
// (#5125, #5127).
//
// # Candidate list and probe, never an unconditional route
//
// The list comes from storeref.ClassCandidates, the shared resolver: the class
// store leads because it is the sole MINTER of the reserved namespace, but
// minting is not holding. A reserved prefix is only an ADVISORY on work stores
// (config.ReservedPrefixWarnings warns; config.ValidateRigs does not reject), so
// a work store can legitimately hold an id inside the class namespace and a
// resolver that routed unconditionally on the prefix would report those beads as
// absent.
//
// # Why the residence fallback exists
//
// storeref.ClassCandidates gates on the class NAMESPACE (IDInNamespace) BEFORE
// it builds a list, and `gc storage migrate` preserves ids
// (infra_class_migrate.go). A bead the migration relocated therefore keeps its
// HQ/rig-era prefix, sits outside the class namespace, and gets nil candidates
// back — for exactly the ids that moved. The shared resolver's own doc says so.
//
// So a nil answer from the resolver is not "the work store owns this". It is
// "the namespace rule cannot decide", and what decides is a RESIDENCE probe:
// ask the class store about the id itself. That is the same probe
// bdByIDClassDoor.resolve performs, for the same reason, and the fallback list
// is the same [class, work] head the resolver would have returned — so the two
// paths differ in what makes them fire, never in what they answer.

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// classRoutedStoreForID returns the store that actually holds id: the relocated
// class binding when it answers for the bead, and work otherwise.
//
// work is the caller's own resolved scope store and is BOTH the residual answer
// and the last candidate, so a city that relocates nothing — and an id no
// relocated class holds — gets back the exact store value the caller passed in.
// A single-store city therefore never changes behavior, and never pays for the
// funnel: the identity gate returns before any probe.
//
// An error is a read that FAILED, never absence. Reading "the binding could not
// answer" as "the bead is not there" is the root-loss shape this whole lane
// exists to prevent. The one error that is not a fault is the one-shot funnel's
// standing refusal (isStandingStorageRefusal): it says this city's storage
// configuration cannot be served, which is a fact about the city and none about
// a particular bead — and a refused city still serves WORK from its work
// ledger. So for a work-shaped id the refusal establishes nothing and the work
// store still answers; for an id only the class binding could own it is the
// answer, and surfaces.
//
// NOT consulted: the rig work stores. They are a different SCOPE, not a
// different class — a caller that never read them cannot start reading them
// here without changing which beads its command is about. storeref.ClassRouting
// carries them as Shadows for the call sites that already hold them open
// (internal/api's by-id resolver); this one does not, and passing an empty
// Shadows list is what keeps the candidate order identical to the pre-seam
// [class, work].
func classRoutedStoreForID(cityPath, id string, work beads.Store) (beads.Store, error) {
	class, relocated := graphClassBinding(cliStorageRoutes(cityPath))
	if !relocated || class == nil || class == work {
		return work, nil
	}
	for _, candidate := range classRouteCandidates(id, class, work) {
		if candidate == nil || candidate == work {
			// The work leg is the caller's own store and the residual answer.
			// Stop here rather than probing it, so the caller's own read
			// produces its own error message byte-identically.
			return work, nil
		}
		_, err := candidate.Get(id)
		switch {
		case err == nil:
			return candidate, nil
		case errors.Is(err, beads.ErrNotFound):
		case isStandingStorageRefusal(err) && !bdIDIsClassReserved(id):
		default:
			return nil, fmt.Errorf("reading %q from the relocated class binding: %w", id, err)
		}
	}
	return work, nil
}

// classRouteCandidates is the by-id probe list: the shared resolver's answer,
// or the residence-probe list when the resolver declines because id sits
// outside the class namespace.
//
// Both lists lead with the class store and fall back to work, which is the
// order the pre-seam resolver used and the order storeref.ClassCandidates
// documents. The fallback is not a second opinion about ownership — it is the
// same list, reached for the ids the namespace gate cannot speak about.
//
// One binding answers for every reserved prefix, which is sound only because
// the served split shape is storageSplitWhole (storageSplitShapeOf admits a
// split only when all five infrastructure classes name the same binding). The
// graph prefix is therefore representative rather than special: a sessions- or
// orders-prefixed id takes the fallback list and is probed against the same
// store the resolver would have named. If per-class fan-out is ever served,
// this has to resolve per class.
func classRouteCandidates(id string, class, work beads.Store) []beads.Store {
	if prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph); ok {
		if candidates := storeref.ClassCandidates(id, storeref.ClassRouting{
			Prefix: prefix,
			Class:  class,
			Work:   work,
		}); candidates != nil {
			return candidates
		}
	}
	return []beads.Store{class, work}
}
