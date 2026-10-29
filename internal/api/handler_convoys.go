package api

import "github.com/gastownhall/gascity/internal/beads"

// isGraphConvoyID checks if the bead is a formula-compiled graph convoy
// (workflow) by looking for the gc.kind=workflow marker. It consults the
// dedicated graph store first (graph-first) so a relocated graph convoy id is
// recognized — its bead lives only in the graph store, never on a rig store. On a
// default city the graph store equals the work store, so this leg is byte-identical.
func isGraphConvoyID(s *Server, id string) bool {
	if graphStore := s.state.GraphBeadStore().Store; graphStore != nil && graphStore != s.state.CityBeadStore() {
		// Graph-first leg: a plain (work) convoy id misses here and the rig
		// legs below answer it.
		if b, err := beads.FederatedGet(graphStore, id); err == nil {
			return isGraphConvoyBead(b)
		}
	}
	stores := s.state.BeadStores()
	for _, rigName := range sortedRigNames(stores) {
		// On a split city a rig's graph convoys live in the binding asked
		// above (the S4 placement rule). A rig store is asked only while it
		// may still hold pre-S4 graph rows; a relic-free rig holds only work,
		// whose convoys are never graph convoys.
		if rigGraphLegRetired(s.state, rigName) {
			continue
		}
		store := stores[rigName]
		b, err := store.Get(id)
		if err != nil {
			continue
		}
		return isGraphConvoyBead(b)
	}
	return false
}
