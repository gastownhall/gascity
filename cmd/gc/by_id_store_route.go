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
// That one place is now cliByIDOwner (by_id_residency.go), the CLI's plan over
// the residency resolver. What used to live here — a candidate list, a probe
// loop and a hand-written classification of which read errors are absence — is
// the resolver's ByID plan, its RoleResidenceProbe/PolicyRefusalTolerated leg
// and its work residual. This file is what remains: the callers that want a
// STORE rather than a row.

import (
	"github.com/gastownhall/gascity/internal/beads"
)

// classRoutedStoreForID returns the store that actually holds id: the relocated
// class binding when it answers for the bead, and work otherwise.
//
// work is the caller's own resolved scope store and is BOTH the residual answer
// and the last leg of the plan, so a city that relocates nothing — and an id no
// relocated class holds — gets back the exact store value the caller passed in.
// A single-store city therefore never changes behavior, and never pays for the
// funnel: its plan has one leg and the resolver returns it unprobed.
//
// An error is a read that FAILED, never absence. The resolver's own contract
// carries that rule and the one exception to it — a refused city still serves
// WORK, so the standing storage refusal is tolerated for an id no relocated
// class could own and surfaces for one inside a reserved namespace. See
// cliByIDOwner.
//
// The row a winning probe read is DISCARDED here. A caller that is about to
// read the bead anyway should call cliByIDOwner directly and use Owner.Bead
// rather than paying for the same read twice; this wrapper exists for the
// callers that hand the store to something else (a formula cook, an attach)
// instead of reading it themselves.
func classRoutedStoreForID(cityPath, id string, work beads.Store) (beads.Store, error) {
	owner, err := cliByIDOwner(cityPath, id, work)
	if err != nil {
		return nil, err
	}
	return owner.Store, nil
}
