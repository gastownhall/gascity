package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// executionEmitWorkStore is the work-store leg an execution-fact projection
// reads through. A convoy's tracks edges live in the store that materialized
// the molecule, but the launch beads those edges name may be resident in a
// per-rig work store on a split-store city. Reads that the primary store
// answers are returned untouched; only a primary miss consults the
// owning-store resolver, so a single-store city is byte-identical.
type executionEmitWorkStore struct {
	beads.Store
	resolveOwning func(id string) (beads.Store, bool)
}

func (s executionEmitWorkStore) Get(id string) (beads.Bead, error) {
	bead, err := s.Store.Get(id)
	if err == nil {
		return bead, nil
	}
	if s.resolveOwning == nil {
		return bead, err
	}
	owning, ok := s.resolveOwning(id)
	if !ok || owning == nil {
		return bead, err
	}
	return owning.Get(id)
}

// executionEmitStore wraps store for executionevent projection so run anchors
// resolve launch beads across the city's convoy stores (city + per-rig). The
// resolver probes every candidate and refuses ambiguous ids, so a bead id
// present in more than one store never anchors to a guessed row.
func executionEmitStore(store beads.Store, cityPath string) beads.Store {
	if store == nil || strings.TrimSpace(cityPath) == "" {
		return store
	}
	return executionEmitWorkStore{Store: store, resolveOwning: func(id string) (beads.Store, bool) {
		owning, _, ok := autocloseOwningStore(id, cityPath)
		return owning, ok
	}}
}
