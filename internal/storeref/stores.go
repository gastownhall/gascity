package storeref

// Stores: the router (typed-stores design §3.2).
//
// A call site does not pick a store for a class; it asks the city's Stores for
// that class's handle, and Stores was minted once, from the city's Topology.
// The mapping is the placement contract planClass already states: the binding
// serving the class, else the work store of the scope that asked. It is
// evaluated here once per epoch instead of once per call site, and this is the
// only production caller of beads.MintClassStores.
//
// Two inputs are the plane's own rather than the topology's, and both are
// StoresOptions:
//
//   - A per-class refusal the plane took itself. cmd/gc refuses a class on a
//     worker site whose descriptor does not attach it to the binding it
//     resolves to (workerSiteClassRefusal). It is per class, unlike
//     Topology.Refused, which is the whole city's, so it cannot be a binding.
//   - The split-strict test gate's projection (S7). Production passes nil.
//
// A refused topology needs no special case: its bindings ARE the refusing
// stores (refusedClassStore in cmd/gc), so the handles minted over them refuse
// every operation, exactly as the routes did. A refused city still serves work
// from its work ledger, and so do these handles.
//
// The rigs are the rigs the plane holds open, which can include a suspended
// one. That is the set the class accessors hand out today. It is wider than
// the serving frame a plan is built over, which is why Stores exposes no plan
// topology yet (T7 adds the federated legs).

import (
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// Projection is the split-strict gate's view of one physical store as two:
// the work view hides infrastructure rows, the class view hides work rows
// (internal/beads/splittest). Installing it at the mint is what lets a
// single-store fixture reveal a class read that reached the work side.
type Projection interface {
	WorkView(beads.Store) beads.Store
	ClassView(beads.Store) beads.Store
}

// RefusingClassStore is a store that answers every operation with a standing
// refusal.
type RefusingClassStore interface {
	beads.Store
	RefusingStore
}

// StoresOptions are the plane's own inputs to the mint.
type StoresOptions struct {
	// ClassRefusals maps a class to the store that refuses it. A refused class
	// gets this store at every scope, relocated or not.
	ClassRefusals map[coordclass.Class]RefusingClassStore

	// Projection is the split-strict gate's hook. Nil in production.
	Projection Projection
}

// Stores is one epoch's class handles for a city. It is a value: a holder
// swaps it whole when the stores it was minted from are replaced, and a
// caller holding an old one keeps the old epoch's stores.
type Stores struct {
	city beads.ClassStores
	// unscoped answers a scope this epoch does not hold: the binding when the
	// class is relocated, else a zero handle.
	unscoped beads.ClassStores
	rigs     map[StoreRef]beads.ClassStores
	rigNames []string
}

// NewStores mints the handles for t.
func NewStores(t Topology, opts StoresOptions) Stores {
	s := Stores{
		city:     mintScope(t, opts, t.Work.Store),
		unscoped: mintScope(t, opts, nil),
	}
	for _, leg := range t.orderedRigs() {
		name, ok := rigName(leg.Ref)
		if !ok {
			continue
		}
		if s.rigs == nil {
			s.rigs = make(map[StoreRef]beads.ClassStores, len(t.Rigs))
		}
		if _, dup := s.rigs[leg.Ref]; !dup {
			s.rigNames = append(s.rigNames, name)
		}
		s.rigs[leg.Ref] = mintScope(t, opts, leg.Store)
	}
	sort.Strings(s.rigNames)
	return s
}

// RigLegs is a rig-name-to-store map as rig legs, by rig name. A nil store is
// kept: a holder hands out a nil rig store as a nil handle, as it did before.
func RigLegs(rigs map[string]beads.Store) []Leg {
	if len(rigs) == 0 {
		return nil
	}
	names := make([]string, 0, len(rigs))
	for name := range rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Leg, 0, len(names))
	for _, name := range names {
		out = append(out, Leg{Ref: RigRef(name), Store: rigs[name]})
	}
	return out
}

// mintScope mints one scope's handles over its work store.
func mintScope(t Topology, opts StoresOptions, work beads.Store) beads.ClassStores {
	class := func(c coordclass.Class) beads.Store { return classStore(t, opts, c, work) }
	return beads.MintClassStores(beads.MintSpec{
		Work:     workView(opts, work),
		Graph:    class(coordclass.ClassGraph),
		Sessions: class(coordclass.ClassSessions),
		Mail:     class(coordclass.ClassMessaging),
		Orders:   class(coordclass.ClassOrders),
		Nudges:   class(coordclass.ClassNudges),
	})
}

// Place is the store NewStores mints class c's handle over at scope: the
// plane's refusal, else the binding serving c (planClass's rule), else the
// scope's work store seen through the projection's class view. A scope t does
// not hold has no work store. ClassWork is the scope's work store through the
// work view.
//
// It is the raw form of the router, for the callers the retyping phases (T3-T9)
// have not yet given a handle: they still take a beads.Store, and this keeps
// them on the one mapping rather than a second copy of it. A caller that can
// hold a handle takes it from Stores instead.
func Place(t Topology, opts StoresOptions, c coordclass.Class, scope StoreRef) beads.Store {
	work := t.Work.Store
	if scope != WorkRef {
		work = nil
		for _, leg := range t.Rigs {
			if leg.Ref == scope {
				work = leg.Store
				break
			}
		}
	}
	if c == coordclass.ClassWork {
		return workView(opts, work)
	}
	return classStore(t, opts, c, work)
}

// classStore is the mapping. A refused topology's binding is the refusing
// store, which planClass reports as an error and this hands out, so the
// refusal reaches the caller through every operation rather than as a missing
// handle.
func classStore(t Topology, opts StoresOptions, c coordclass.Class, work beads.Store) beads.Store {
	if refusal := opts.ClassRefusals[c]; refusal != nil {
		return refusal
	}
	if b, ok := t.BindingFor(c); ok {
		return b.Leg.Store
	}
	if opts.Projection != nil && work != nil {
		return opts.Projection.ClassView(work)
	}
	return work
}

// workView is work through the projection's work view.
func workView(opts StoresOptions, work beads.Store) beads.Store {
	if opts.Projection != nil && work != nil {
		return opts.Projection.WorkView(work)
	}
	return work
}

// rigName returns the rig a rig ref names.
func rigName(ref StoreRef) (string, bool) {
	name, ok := ScopeRigContext(string(ref))
	return name, ok && name != ""
}

// scope returns the handles for ref: the city's for WorkRef, a held rig's, or
// the unscoped set.
func (s Stores) scope(ref StoreRef) beads.ClassStores {
	if ref == WorkRef {
		return s.city
	}
	if cs, ok := s.rigs[ref]; ok {
		return cs
	}
	return s.unscoped
}

// Work is the city/HQ work store.
func (s Stores) Work() beads.WorkStore { return s.city.Work }

// Rig is the named rig's work store, when this epoch holds the rig.
func (s Stores) Rig(name string) (beads.WorkStore, bool) {
	cs, ok := s.rigs[RigRef(name)]
	return cs.Work, ok
}

// Rigs is every held rig's work store by rig name, as a fresh map.
func (s Stores) Rigs() map[string]beads.WorkStore {
	if len(s.rigNames) == 0 {
		return nil
	}
	out := make(map[string]beads.WorkStore, len(s.rigNames))
	for _, name := range s.rigNames {
		out[name] = s.rigs[RigRef(name)].Work
	}
	return out
}

// WorkLegs is the city's work store, then each held rig's by rig name.
func (s Stores) WorkLegs() []beads.WorkStore {
	out := make([]beads.WorkStore, 0, 1+len(s.rigNames))
	out = append(out, s.city.Work)
	for _, name := range s.rigNames {
		out = append(out, s.rigs[RigRef(name)].Work)
	}
	return out
}

// Graph is the graph store for a new root or class-pure operation at scope:
// the binding when graph is relocated, at every scope (S4a), else the scope's
// work store. A scope this epoch does not hold gets the binding, or a zero
// handle on a city that relocates nothing.
func (s Stores) Graph(scope StoreRef) beads.GraphStore { return s.scope(scope).Graph }

// Orders is the orders store at scope, by the same rule as Graph.
func (s Stores) Orders(scope StoreRef) beads.OrdersStore { return s.scope(scope).Orders }

// Sessions is the city's sessions store.
func (s Stores) Sessions() beads.SessionStore { return s.city.Sessions }

// Mail is the city's messaging store (mail and extmsg).
func (s Stores) Mail() beads.MailStore { return s.city.Mail }

// Nudges is the city's nudges store.
func (s Stores) Nudges() beads.NudgesStore { return s.city.Nudges }
