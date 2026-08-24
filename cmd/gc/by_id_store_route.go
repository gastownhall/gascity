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
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
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

// cityGraphClassBinding opens a city's graph class binding, nil when the class
// is not relocated. Sole derivation point: re-asking is the split-store bug.
func cityGraphClassBinding(cityPath string) beads.Store {
	if strings.TrimSpace(cityPath) == "" {
		return nil
	}
	store, relocated := graphClassBinding(cliStorageRoutes(cityPath))
	if !relocated {
		return nil
	}
	return store
}

// classBindingForID is the class leg of classRoutedStoreForID, for callers whose
// work answer is a leg list rather than one store. A nil work leg makes the
// residual answer nil, so ok=false means "no named leg answered".
func classBindingForID(cityPath, id string) (beads.Store, bool, error) {
	class := cityGraphClassBinding(cityPath)
	if class == nil {
		return nil, false, nil
	}
	store, err := classRoutedStoreForIDIn(class, id, nil)
	if err != nil {
		return nil, false, err
	}
	if store == nil {
		return nil, false, nil
	}
	return class, true, nil
}

// classRoutedStoreForIDIn is classRoutedStoreForID with the binding already in
// hand, for the callers that resolve it once per request and hold no cityPath to
// hand cliByIDOwner: gc graph's per-id resolver (graphStores.storeFor), which
// also needs the raw binding for convoycore.MemberClasses, and classBindingForID
// itself. Nil class means no relocation.
//
// It applies the resolver's rule directly against the opened binding. The class
// store leads because it MINTS the reserved namespace, but minting is not
// holding: a work store may legitimately hold an id inside the class namespace
// (ReservedPrefixWarnings warns, ValidateRigs does not reject), so the answer is
// a RESIDENCE PROBE and not an unconditional route on the prefix. A miss falls
// through to work — the caller's own store, both the residual answer and the
// last leg — so its own later read produces its own error message.
//
// An error is a read that FAILED, never absence: reading "the binding could not
// answer" as "the bead is not there" is the root-loss shape this lane exists to
// prevent. The one error that is not a fault is the one-shot funnel's standing
// refusal (storeref.IsStandingRefusal) — a verdict about the CITY's storage
// configuration that says nothing about a bead, and a refused city still serves
// WORK from its work ledger. So for a work-shaped id the refusal establishes
// nothing and work answers; for an id inside a reserved namespace the refusal IS
// the answer and surfaces.
func classRoutedStoreForIDIn(class beads.Store, id string, work beads.Store) (beads.Store, error) {
	if class == nil || class == work {
		return work, nil
	}
	if _, err := class.Get(id); err != nil {
		switch {
		case errors.Is(err, beads.ErrNotFound):
			return work, nil
		case storeref.IsStandingRefusal(err) && !bdIDIsClassReserved(id):
			return work, nil
		default:
			return nil, fmt.Errorf("reading %q from the relocated class binding: %w", id, err)
		}
	}
	return class, nil
}
