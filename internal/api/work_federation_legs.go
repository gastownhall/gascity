package api

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The work ledger's legs as GET /v0/beads, /status, store health and the
// retired-session sweep read them (typed-stores T7e.2, t7b3-design.md §5):
// storeref's WorkFederation intent picks and orders the legs over one Stores()
// epoch, and the helpers here name them the way State.BeadStores() did. Every
// leg is PolicyFatal; whether a leg's failure is tolerated stays each reader's
// rule, so no dark rig fails a request it did not fail before.

// workScopeByName is the scope a rig-scoped read names: the rig the epoch
// opened under name, else the city under its own name (State.BeadStores'
// alias, which a rig of that name replaces), else none.
func workScopeByName(st storeref.Stores, cityName, name string) (storeref.StoreRef, bool) {
	if _, ok := st.Rig(name); ok {
		return storeref.RigRef(name), true
	}
	if name == cityName && !beads.IsNilStore(st.Work()) {
		return storeref.WorkRef, true
	}
	return "", false
}

// workLegName is the name a WorkFederation work leg was listed under in
// State.BeadStores(): its rig's, or cityName for the city's own leg.
func workLegName(l storeref.TypedLeg, cityName string) string {
	if l.Ref == storeref.WorkRef {
		return cityName
	}
	name, _ := storeref.ScopeRigContext(string(l.Ref))
	return name
}

// cityWorkLeg is the epoch's city work store as one leg, and false when the
// epoch opened none (where CityBeadStore() answered nil).
func cityWorkLeg(st storeref.Stores) (storeref.TypedLeg, bool) {
	plan, err := st.Plan(storeref.WorkFederation{Kind: storeref.WorkScope, Scope: storeref.WorkRef})
	if err != nil {
		return storeref.TypedLeg{}, false
	}
	var leg storeref.TypedLeg
	storeref.EachTypedLeg(plan, func(l storeref.TypedLeg) { leg = l })
	return leg, true
}

// coldRigRefs is the refs of the rigs st opened that cold names (the API's
// own per-request census of suspended rigs, cacheColdRigNames), for a
// WorkFederation's Skip, and how many there are. A cold name the epoch opened
// no rig under names no leg, so it is not counted.
func coldRigRefs(st storeref.Stores, cold map[string]bool) ([]storeref.StoreRef, int) {
	var skip []storeref.StoreRef
	for _, r := range st.Rigs() {
		if cold[r.Name] {
			skip = append(skip, storeref.RigRef(r.Name))
		}
	}
	return skip, len(skip)
}
