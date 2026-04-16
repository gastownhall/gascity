package api

// isGraphConvoyID checks if the bead is a formula-compiled graph convoy
// (workflow) by looking for the gc.kind=workflow marker.
func isGraphConvoyID(s *Server, id string) bool {
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
