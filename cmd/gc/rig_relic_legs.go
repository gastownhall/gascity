package main

// The S4 relic legs, retired once a rig is proven relic-free (S4b).
//
// Before S4a a rig's convergence loops, `cook --attach` grafts, control trees
// and pre-split orders and sessions were written to the rig's own store, so a
// split city's readers kept every rig store as a relic leg behind the binding.
// Once `gc storage migrate --from-rigs` imports a rig, or records it clean, its
// store holds only work, and a reader of graph, orders or sessions rows stops
// reading it. The predicate is a file read (storageRoutes.rigGraphRelicFree);
// a single-store city never retires a leg, because there the rig store IS
// where the rig's rows live.

// rigViewRelicFree reports whether a convoy store view is a rig's own store
// proven relic-free on a split city. The binding view and the city view never
// are.
func rigViewRelicFree(routes *storageRoutes, cityPath string, view convoyStoreView) bool {
	if view.store == nil || view.role == convoyViewClassBinding {
		return false
	}
	dir := view.scopePath(cityPath)
	if scopeIsCity(cityPath, dir) {
		return false
	}
	return splitRigGraphRelicFree(routes, routes.rigGraphScopeForDir(cityPath, dir).rig)
}

// cutRigLegLabel returns the rig a plan leg labeled "rig <name>"
// (planeLegLabel) names.
func cutRigLegLabel(label string) (string, bool) {
	const prefix = "rig "
	if len(label) <= len(prefix) || label[:len(prefix)] != prefix {
		return "", false
	}
	return label[len(prefix):], true
}
