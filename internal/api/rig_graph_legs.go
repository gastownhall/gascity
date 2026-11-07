package api

import (
	"github.com/gastownhall/gascity/internal/beadmeta"
)

// rigGraphRelicReporter is implemented by states that know the S4 relic-free
// predicate: whether a rig's own store is proven to hold no graph row any more
// because its graph rows were imported into the city binding or it never held
// any. A state that does not implement it keeps every rig leg, which is the
// pessimistic default: retiring a leg is the claim that needs evidence.
type rigGraphRelicReporter interface {
	RigGraphRelicFree(rig string) bool
}

// rigGraphLegRetired reports whether graph readers may skip rig's own store.
// It is true only on a split city whose state proves the rig relic-free; a
// single-store city never retires a rig leg, because there the rig store is
// where the rig's graph rows live.
func rigGraphLegRetired(state State, rig string) bool {
	reporter, ok := state.(rigGraphRelicReporter)
	if !ok || rig == "" {
		return false
	}
	return reporter.RigGraphRelicFree(rig)
}

// workflowRigLegRetired is rigGraphLegRetired for a workflow store entry: only
// a rig entry can be retired.
func workflowRigLegRetired(state State, info workflowStoreInfo) bool {
	if info.scopeKind != beadmeta.ScopeKindRig {
		return false
	}
	return rigGraphLegRetired(state, info.scopeRef)
}

// workflowGraphStoreInfo returns the graph binding's workflow store entry on a
// city that relocates the graph class, and false on every other city (where
// the graph store IS the city store and no separate probe is needed).
func workflowGraphStoreInfo(state State) (workflowStoreInfo, bool) {
	graphStore := state.GraphBeadStore().Store
	if graphStore == nil || graphStore == state.CityBeadStore() {
		return workflowStoreInfo{}, false
	}
	cityName := workflowCityScopeRef(state.CityName())
	return workflowStoreInfo{
		ref:       workflowGraphStoreRefPrefix + ":" + cityName,
		scopeKind: beadmeta.ScopeKindCity,
		scopeRef:  cityName,
		store:     graphStore,
	}, true
}
