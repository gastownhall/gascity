package api

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// Frozen copies of the API's order-history leg builders, taken verbatim from
// origin/main 8d2fb3120c (huma_handlers_orders.go orderStoreInfosForState and
// appendOrdersClassStoreInfo; handler_convoy_dispatch.go
// workflowWritableStores and workflowStoreByRef's orders: arm; rig_graph_legs.go
// rigGraphLegRetired), for order_history_legs_equivalence_test.go. Three
// substitutions, so these copies outlive T7d.2's deletions, and nothing else:
//
//   - workflowStoreInfo becomes t7b2FrozenOrderInfo, the fields the order reads
//     read (T7d.2 deletes workflowStoreInfo.store);
//   - state.BeadStore(rig) becomes t7b2FrozenBeadStore(state, rig), the same
//     method through a type assertion (T7d.2 removes it from State; the
//     fakes keep it);
//   - rigGraphRelicReporter becomes t7b2FrozenRigGraphRelicReporter.
//
// workflowLegs and workflowCityScopeRef are called live: T7b.2 and T7d.2 leave
// them byte-identical.

// t7b2FrozenOrderInfo is the part of main's workflowStoreInfo an order read reads.
type t7b2FrozenOrderInfo struct {
	ref       string
	scopeKind string
	scopeRef  string
	store     beads.Store
}

// t7b2FrozenBeadStore is main's State.BeadStore.
func t7b2FrozenBeadStore(state State, rig string) beads.Store {
	return state.(interface{ BeadStore(string) beads.Store }).BeadStore(rig)
}

type t7b2FrozenRigGraphRelicReporter interface {
	RigGraphRelicFree(rig string) bool
}

func t7b2FrozenRigGraphLegRetired(state State, rig string) bool {
	reporter, ok := state.(t7b2FrozenRigGraphRelicReporter)
	if !ok || rig == "" {
		return false
	}
	return reporter.RigGraphRelicFree(rig)
}

func t7b2FrozenOrderStoreInfosForState(state State, a orders.Order) ([]t7b2FrozenOrderInfo, error) {
	cityName := workflowCityScopeRef(state.CityName())
	infos := make([]t7b2FrozenOrderInfo, 0, 2)
	if strings.TrimSpace(a.Rig) != "" && !t7b2FrozenRigGraphLegRetired(state, a.Rig) {
		if rigStore := t7b2FrozenBeadStore(state, a.Rig); rigStore != nil {
			infos = append(infos, t7b2FrozenOrderInfo{
				ref:       "rig:" + a.Rig,
				scopeKind: beadmeta.ScopeKindRig,
				scopeRef:  a.Rig,
				store:     rigStore,
			})
		}
	}

	if cityStore := state.CityBeadStore(); cityStore != nil {
		infos = append(infos, t7b2FrozenOrderInfo{
			ref:       "city:" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     cityStore,
		})
	}

	infos = t7b2FrozenAppendOrdersClassStoreInfo(infos, state, cityName)

	if len(infos) == 0 {
		return nil, errNoOrderStores
	}
	return infos, nil
}

func t7b2FrozenAppendOrdersClassStoreInfo(infos []t7b2FrozenOrderInfo, state State, cityName string) []t7b2FrozenOrderInfo {
	ordersStore := state.Stores().Orders().Store
	if ordersStore == nil {
		return infos
	}
	for _, info := range infos {
		if beads.SameStore(info.store, ordersStore) {
			return infos
		}
	}
	return append(infos, t7b2FrozenOrderInfo{
		ref:       ordersClassStoreRefPrefix + ":" + cityName,
		scopeKind: beadmeta.ScopeKindCity,
		scopeRef:  cityName,
		store:     ordersStore,
	})
}

func t7b2FrozenWorkflowWritableStores(state State) []t7b2FrozenOrderInfo {
	infos, resident := workflowLegs(state.Stores(), state.CityName(), workflowLegQuery{})
	out := make([]t7b2FrozenOrderInfo, 0, len(infos))
	for i := range infos {
		out = append(out, t7b2FrozenOrderInfo{ref: infos[i].ref, scopeKind: infos[i].scopeKind, scopeRef: infos[i].scopeRef, store: resident(infos[i])})
	}
	return out
}

// t7b2FrozenEveryOrderInfos is the history detail's and the feed's list:
// appendOrdersClassStoreInfo(workflowWritableStores(state), ...).
func t7b2FrozenEveryOrderInfos(state State) []t7b2FrozenOrderInfo {
	return t7b2FrozenAppendOrdersClassStoreInfo(t7b2FrozenWorkflowWritableStores(state), state, workflowCityScopeRef(state.CityName()))
}

// t7b2FrozenOrdersRefInfo is workflowStoreByRef's orders: arm.
func t7b2FrozenOrdersRefInfo(state State, scopeRef string) (t7b2FrozenOrderInfo, bool) {
	scopeRef = strings.TrimSpace(scopeRef)
	ordersStore := state.Stores().Orders().Store
	cityName := workflowCityScopeRef(state.CityName())
	if ordersStore == nil || ordersStore == state.CityBeadStore() || scopeRef != cityName {
		return t7b2FrozenOrderInfo{}, false
	}
	return t7b2FrozenOrderInfo{
		ref:       ordersClassStoreRefPrefix + ":" + cityName,
		scopeKind: beadmeta.ScopeKindCity,
		scopeRef:  cityName,
		store:     ordersStore,
	}, true
}
