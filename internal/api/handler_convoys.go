package api

// isGraphConvoyID checks if the bead is a formula-compiled graph convoy
// (workflow) by looking for the gc.kind=workflow marker. It consults the
// dedicated graph store first (graph-first) so a relocated graph convoy id is
// recognized — its bead lives only in the graph store, never on a rig store. On a
// default city the graph store equals the work store, so this leg is byte-identical.
func isGraphConvoyID(s *Server, id string) bool {
	if graphStore := s.state.GraphBeadStore().Store; graphStore != nil && graphStore != s.state.CityBeadStore() {
		if b, err := graphStore.Get(id); err == nil {
			return isGraphConvoyBead(b)
		}
	}
	stores := s.state.BeadStores()
	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
		b, err := store.Get(id)
		if err != nil {
			continue
		}
		return isGraphConvoyBead(b)
	}
	return false
}
