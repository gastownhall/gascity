package api

import (
	"sort"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/storeref"
)

// isGraphConvoyID checks if the bead is a formula-compiled graph convoy
// (workflow) by looking for the gc.kind=workflow marker. It reads the
// workflow legs (workflowLegs): the relocated graph binding first, so a
// relocated graph convoy id is recognized (its bead lives only there, never on
// a rig store), then the city and rig legs in the order this probe has always
// read them, BeadStores()' name order: the city at its own name's sorted
// position, unless a rig is named like the city: that rig takes its place,
// and once relic-free leaves it empty. The first leg that reads the id
// answers. On a split city a rig's graph convoys live in the binding
// (the S4 placement rule), so a relic-free rig, which holds only work, whose
// convoys are never graph convoys, is not read; a rig that may still hold
// pre-S4 graph rows is read as a relic leg until it is imported.
func isGraphConvoyID(s *Server, id string) bool {
	legs, _ := workflowLegs(s.state, workflowLegQuery{keepRigNamedLikeCity: true})
	cityName := s.state.CityName()
	// A rig named like the city replaces the city in BeadStores(), retired
	// or not: the city is then not read here at all, and the rig only while
	// its leg stands.
	_, rigShadowsCity := s.state.Stores().Rig(cityName)
	byName := make(map[string]workflowStoreInfo, len(legs))
	names := make([]string, 0, len(legs))
	for _, info := range legs {
		name := info.scopeRef
		switch {
		case info.leg.Role == storeref.RoleAuthority:
			// Graph-first leg: a plain (work) convoy id misses here and the
			// scope legs below answer it.
			if b, err := info.leg.Reader.Get(id); err == nil {
				return isGraphConvoyBead(b)
			}
			continue
		case info.scopeKind == beadmeta.ScopeKindCity:
			if rigShadowsCity {
				continue
			}
			name = cityName
		}
		names = append(names, name)
		byName[name] = info
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := byName[name].leg.Reader.Get(id)
		if err != nil {
			continue
		}
		return isGraphConvoyBead(b)
	}
	return false
}
