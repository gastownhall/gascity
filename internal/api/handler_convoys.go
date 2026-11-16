package api

import (
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// isGraphConvoyID checks if the bead is a formula-compiled graph convoy
// (workflow) by looking for the gc.kind=workflow marker. It reads the
// workflow legs (workflowLegs): the relocated graph binding first, so a
// relocated graph convoy id is recognized (its bead lives only there, never on
// a rig store), then the city and rig legs in the order this probe has always
// read them, BeadStores()' name order. The names come from the epoch's rig
// aliases: the city's own name, standing for the city work store unless a rig
// is named like the city (that rig's store then takes its place), and every
// other rig's name. They are walked sorted, and a name whose store an earlier
// name already named is skipped, so a store several aliases share is read
// once, at its first name. Each name is resolved onto the scope leg that reads
// its store, by store identity; a name with no leg (a relic-free rig, whose
// leg the plan retires) is not read. The first leg that reads the id answers.
// On a split city a rig's graph convoys live in the binding (the S4 placement
// rule), so a relic-free rig, which holds only work, whose convoys are never
// graph convoys, is not read; a rig that may still hold pre-S4 graph rows is
// read as a relic leg until it is imported.
func isGraphConvoyID(s *Server, id string) bool {
	// One epoch for the request: the legs and the rig aliases below read the
	// same rig set.
	st := s.state.Stores()
	cityName := s.state.CityName()
	legs, _ := workflowLegs(st, cityName, workflowLegQuery{keepRigNamedLikeCity: true})
	scope := make([]workflowStoreInfo, 0, len(legs))
	for _, info := range legs {
		if info.leg.Role == storeref.RoleAuthority {
			// Graph-first leg: a plain (work) convoy id misses here and the
			// scope legs below answer it.
			if b, err := info.leg.Reader.Get(id); err == nil {
				return isGraphConvoyBead(b)
			}
			continue
		}
		scope = append(scope, info)
	}

	// BeadStores()' aliases: the city under its own name, unless a rig named
	// like the city replaces it there, then every other rig by name.
	aliases := map[string]beads.StoreReader{cityName: st.Work()}
	if work, ok := st.Rig(cityName); ok {
		aliases[cityName] = work
	}
	for _, rig := range st.Rigs() {
		if rig.Name != cityName {
			aliases[rig.Name] = rig.Work
		}
	}
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	claimed := make([]beads.StoreReader, 0, len(names))
	for _, name := range names {
		store := aliases[name]
		if beads.IsNilStore(store) {
			continue
		}
		seen := false
		for _, c := range claimed {
			if beads.SameStore(c, store) {
				seen = true
				break
			}
		}
		if seen {
			continue // the first name over a shared store reads it
		}
		claimed = append(claimed, store)
		for _, info := range scope {
			if !beads.SameStore(info.leg.Reader, store) {
				continue
			}
			if b, err := info.leg.Reader.Get(id); err == nil {
				return isGraphConvoyBead(b)
			}
			break
		}
	}
	return false
}
