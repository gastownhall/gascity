package api

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The order-history federation as the API names its legs (typed-stores
// T7d.2): storeref's OrderHistory plan over one epoch, each leg under the
// store_ref the order history hands a client and takes back.

// orderHistoryLegs is the order-history federation in's plan over st, for
// the city named cityName, as the API names its legs:
//
//   - the city work store city:<city>, a rig rig:<name>; in the every-order
//     list a rig named like the city is skipped (workflowLegs' rule), and in
//     an order's own list it is read;
//   - an Authority leg by store identity, never by ref (a plane's per-class
//     refusal shares the binding's ref): in the every-order list the leg over
//     the graph handle's store is graph:<city>, as workflowLegs names it, and
//     any other is the orders binding, orders:<city>; an order's own list has
//     no graph leg, so its Authority leg is orders:<city>.
//
// Each leg reads in its plan role through Reader, or strictly through Strict
// for a read that carried no marker. A plan the epoch cannot make (no store at
// all) is the error, which the order reads report as errNoOrderStores.
func orderHistoryLegs(st storeref.Stores, cityName string, in storeref.OrderHistory) ([]workflowStoreInfo, error) {
	plan, err := st.Plan(in)
	if err != nil {
		return nil, err
	}
	city := workflowCityScopeRef(cityName)
	var out []workflowStoreInfo
	storeref.EachTypedLeg(plan, func(leg storeref.TypedLeg) {
		info := workflowStoreInfo{scopeKind: beadmeta.ScopeKindCity, scopeRef: city, leg: leg}
		switch rig, _ := storeref.ScopeRigContext(string(leg.Ref)); {
		case leg.Role == storeref.RoleAuthority:
			info.ref = ordersClassStoreRefPrefix + ":" + city
			if in.Kind == storeref.OrderHistoryAll && beads.SameStore(leg.Reader, st.Graph()) {
				info.ref = workflowGraphStoreRefPrefix + ":" + city
			}
		case leg.Ref == storeref.WorkRef:
			info.ref = "city:" + city
		case rig != "":
			if rig == city && in.Kind == storeref.OrderHistoryAll {
				return
			}
			info.ref, info.scopeKind, info.scopeRef = "rig:"+rig, beadmeta.ScopeKindRig, rig
		default:
			return
		}
		out = append(out, info)
	})
	return out, nil
}

// orderLegsFor is one order's history legs (OrderHistoryOne at its rig, or
// the city) over st, or errNoOrderStores when the epoch has no store.
func orderLegsFor(st storeref.Stores, cityName, rig string) ([]workflowStoreInfo, error) {
	scope := storeref.WorkRef
	if rig = strings.TrimSpace(rig); rig != "" {
		scope = storeref.RigRef(rig)
	}
	infos, err := orderHistoryLegs(st, cityName, storeref.OrderHistory{Kind: storeref.OrderHistoryOne, Scope: scope, SuspendedRigs: storeref.WorkflowReadsSuspendedRigs})
	if err != nil || len(infos) == 0 {
		return nil, errNoOrderStores
	}
	return infos, nil
}

// everyOrderLegs is every order's history legs (OrderHistoryAll) over st,
// read by the history detail and the order feed; none when the epoch has no
// store.
func everyOrderLegs(st storeref.Stores, cityName string) []workflowStoreInfo {
	infos, err := orderHistoryLegs(st, cityName, storeref.OrderHistory{Kind: storeref.OrderHistoryAll, SuspendedRigs: storeref.WorkflowReadsSuspendedRigs})
	if err != nil {
		return nil
	}
	return infos
}

// orderHistoryOrdersRef resolves the store_ref orders:<scopeRef>: on a city
// whose orders handle is not its work handle, the every-order leg over the
// orders handle's store, named orders:<city> whatever the list labels it (a
// graph binding that also serves orders is graph: there). Elsewhere, and for
// any scope but the city, it names nothing.
func orderHistoryOrdersRef(st storeref.Stores, cityName, scopeRef string) (workflowStoreInfo, bool) {
	city := workflowCityScopeRef(cityName)
	if strings.TrimSpace(scopeRef) != city || beads.IsNilStore(st.Orders()) || beads.SameStore(st.Orders(), st.Work()) {
		return workflowStoreInfo{}, false
	}
	for _, info := range everyOrderLegs(st, cityName) {
		if beads.SameStore(info.leg.Reader, st.Orders()) {
			info.ref, info.scopeKind, info.scopeRef = ordersClassStoreRefPrefix+":"+city, beadmeta.ScopeKindCity, city
			return info, true
		}
	}
	return workflowStoreInfo{}, false
}

// strictOrderLegs is each entry's strict reader, for the reads that carried no
// marker (the fresh event cursor).
func strictOrderLegs(infos []workflowStoreInfo) []beads.FederatedReader {
	out := make([]beads.FederatedReader, 0, len(infos))
	for _, info := range infos {
		out = append(out, info.leg.Strict)
	}
	return out
}
