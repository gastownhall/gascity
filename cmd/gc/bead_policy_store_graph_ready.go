package main

import "github.com/gastownhall/gascity/internal/beads"

// ReadyGraphOnlyHandle returns a graph-only-ready handle when the wrapped store
// exposes GraphOnlyReadyStore. The handle delegates without applying
// expandPolicyReadyQuery: this surface is wisp-only by contract and the lower
// layer forces TierWisps regardless of caller TierMode, so tier promotion is
// meaningless here.
func (s *beadPolicyStore) ReadyGraphOnlyHandle() (beads.GraphOnlyReadyStore, bool) {
	g, ok := beads.GraphOnlyReadyFor(s.Store)
	if !ok {
		return nil, false
	}
	return beadPolicyGraphOnlyStore{g: g}, true
}

type beadPolicyGraphOnlyStore struct {
	g beads.GraphOnlyReadyStore
}

func (s beadPolicyGraphOnlyStore) ReadyGraphOnly(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return s.g.ReadyGraphOnly(query...)
}
