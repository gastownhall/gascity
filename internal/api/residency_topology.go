package api

// The API plane's residency topology.
//
// The API holds no storageRoutes: it sees the split only as STORE IDENTITY,
// through the per-class State accessors. That is the same gate
// relocatedGraphStore already applies — on a default city GraphBeadStore()
// returns the city store itself, so there is nothing to federate — generalized
// from the graph class to all five.
//
// Building from the opened accessors rather than from [storage] is deliberate
// and is the cityQueryTopology lesson: a city whose section was deleted after
// it had served a split still has its infrastructure beads in a binding, and a
// config read would answer "no split" and send every plan to the work ledger.

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// residencyTopology builds the API's view of which stores answer which
// questions.
//
// A class accessor that returns the city store, or nil, relocates nothing and
// contributes no binding — the identity gate that keeps a single-store city
// byte-identical. A plan's residence probe retires only when both halves say
// so: the mint bit observed from the store, the relic bit carried across the
// State surface from the boot that censused the binding.
func (s *Server) residencyTopology() storeref.Topology {
	cfg := s.state.Config()
	city := s.state.CityBeadStore()

	byStore := map[beads.Store][]coordclass.Class{}
	var order []beads.Store
	for _, entry := range s.classStoreAccessors() {
		store := entry.store
		if store == nil || store == city {
			continue
		}
		if _, seen := byStore[store]; !seen {
			order = append(order, store)
		}
		byStore[store] = append(byStore[store], entry.class)
	}

	topo := storeref.Topology{
		Work: storeref.Leg{Ref: storeref.WorkRef, Store: city, Prefix: strings.TrimSpace(config.EffectiveHQPrefix(cfg))},
	}
	topo.Bindings, topo.Refused = apiResidencyBindings(order, byStore, s.state.ClassBindingHasLegacyResidents)

	if cfg != nil {
		for _, rig := range cfg.Rigs {
			store := s.state.BeadStore(rig.Name)
			if store == nil || store == city {
				continue
			}
			topo.Rigs = append(topo.Rigs, storeref.Leg{
				Ref:    storeref.RigRef(rig.Name),
				Store:  store,
				Prefix: strings.TrimSpace(rig.EffectivePrefix()),
			})
		}
	}
	return topo
}

// classStoreAccessor pairs a coordination class with the store the State
// surface serves it from, in coordclass order.
type classStoreAccessor struct {
	class coordclass.Class
	store beads.Store
}

// classStoreAccessors is every coordination class the State surface can
// OBSERVE a store for.
//
// Messaging is absent, and deliberately so: State exposes no messaging
// accessor (mail reaches its store through the mail provider), so this plane
// cannot see where the class is served from. completeObservedClasses is what
// keeps that blind spot from producing a different StoreRef for the same
// physical binding than the CLI and controller planes produce.
func (s *Server) classStoreAccessors() []classStoreAccessor {
	return []classStoreAccessor{
		{coordclass.ClassGraph, s.state.GraphBeadStore().Store},
		{coordclass.ClassSessions, s.state.SessionsBeadStore().Store},
		{coordclass.ClassOrders, s.state.OrdersBeadStore().Store},
		{coordclass.ClassNudges, s.state.NudgesBeadStore().Store},
	}
}

// observableInfraClasses is the set classStoreAccessors covers.
func observableInfraClasses() []coordclass.Class {
	return []coordclass.Class{coordclass.ClassGraph, coordclass.ClassSessions, coordclass.ClassOrders, coordclass.ClassNudges}
}

// completeObservedClasses adds the classes this plane cannot observe to a
// binding that carries every one it can.
//
// A StoreRef must name the same physical binding on every plane — it is the
// vocabulary a persisted census row and the wake filter that reads it back
// share. Deriving it from the OBSERVED class set alone would give the API
// "class:gnos" where the CLI and controller give "class:gmnos", for one store,
// and the filter would drop rows written by the other half of the program.
//
// The completion is an observation about the opened stores, not a config read:
// when every infrastructure class this plane can see resolves to ONE non-work
// store, that store is the whole-split binding, and this build serves the whole
// split or none of it (cmd/gc's storageSupportedTopologyStatement). A partial
// arrangement never reaches a serving runtime. A binding carrying only SOME
// observable classes is left exactly as observed, so a future per-class split
// is described honestly rather than rounded up.
func completeObservedClasses(classes []coordclass.Class) []coordclass.Class {
	observable := observableInfraClasses()
	if len(classes) != len(observable) {
		return classes
	}
	out := append([]coordclass.Class(nil), classes...)
	for _, class := range coordclass.Classes() {
		if !class.IsInfrastructure() || containsClass(out, class) {
			continue
		}
		out = append(out, class)
	}
	return out
}

func containsClass(classes []coordclass.Class, want coordclass.Class) bool {
	for _, c := range classes {
		if c == want {
			return true
		}
	}
	return false
}

// apiResidencyBindings groups the relocated classes by the store serving them.
//
// The body is storeref.BuildBindings, shared with the CLI plane. What survives
// here is the only thing that is this plane's own: it observes four of the five
// infrastructure classes, so it hands the builder completeObservedClasses to
// round a whole-split binding back up to the class set — and therefore the
// StoreRef — the other planes name for the same store.
//
// relics is the boot census's verdict, read across the State surface. A nil one
// is the pessimistic answer for every store: a caller with no census to offer
// has not cleared anything.
func apiResidencyBindings(order []beads.Store, byStore map[beads.Store][]coordclass.Class, relics func(beads.Store) bool) ([]storeref.ClassBinding, error) {
	return storeref.BuildBindings(order, byStore, storeref.BindingOptions{
		Relics:          relics,
		CompleteClasses: completeObservedClasses,
	})
}
