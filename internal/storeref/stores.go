package storeref

// Stores: the router (typed-stores design §3.2).
//
// A call site does not pick a store for a class; it asks the city's Stores for
// that class's handle, and Stores was minted once, from the city's Topology.
// The mapping is the placement contract planClass already states: the binding
// serving the class, else the city's work store. It is evaluated here once per
// epoch instead of once per call site, and this is the only production caller
// of beads.MintClassStores.
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
// Stores reads Topology.Work, Topology.Rigs and Topology.Bindings, and keeps
// the prefixes, census bits and relic-free bits they carry for Topology. It
// answers the city scope for every class, and a rig scope for the two classes
// a rig scope has: its work store (Rig) and the graph store a molecule
// launched at it is placed in (GraphFor). Sessions, mail, orders and nudges are city
// classes, so no rig frame exists for them: a sessions handle over a rig work
// store would be the misroute shape. Stores.Plan (stores_plan.go) plans the
// federations over these handles, and Stores.Topology is the topology they
// were minted as.
//
// NewStores and Place take raw stores and hand out handles over them, so the
// storeclass lint confines their callers (tools/nogo/analyzers/storeclass).

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

// Unviewer is the optional half of a Projection that can see through one of
// its own views: front is the store the view polices, and rebind puts a store
// rebuilt from front behind the same side of the same projection.
type Unviewer interface {
	ViewFront(beads.Store) (front beads.Store, rebind func(beads.Store) beads.Store, ok bool)
}

// Unview returns the store under p's outermost view of s, and a function that
// puts a rebuilt copy of that store behind the same side of p. Without a
// projection, or when s is not one of its views, it returns s and the
// identity. It is for a caller that rebuilds its store rather than reading it
// (gc status's ctx-bound bd clone): unwrapping lets the rebuild find the real
// backing, and the rewrap keeps the rebuilt store's reads policed.
func Unview(p Projection, s beads.Store) (beads.Store, func(beads.Store) beads.Store) {
	identity := func(store beads.Store) beads.Store { return store }
	unviewer, ok := p.(Unviewer)
	if !ok || s == nil {
		return s, identity
	}
	front, rebind, isView := unviewer.ViewFront(s)
	if !isView || rebind == nil {
		return s, identity
	}
	return front, rebind
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
	// gets this store, relocated or not.
	ClassRefusals map[coordclass.Class]RefusingClassStore

	// Projection is the split-strict gate's hook. Nil in production.
	Projection Projection
}

// Stores is one epoch's class handles for a city. It is a value: a holder
// swaps it whole when the stores it was minted from are replaced, and a
// caller holding an old one keeps the old epoch's stores. Its rig handles are
// minted from the same topology as its city handles, so one Stores never
// mixes two epochs.
type Stores struct {
	city beads.ClassStores
	rigs []rigStores
	// topo is the minted topology (mintedTopology): the epoch's legs over the
	// handles' own stores.
	topo Topology
}

// rigStores is one rig scope's handles.
type rigStores struct {
	name  string
	ref   StoreRef
	work  beads.WorkStore
	graph beads.GraphStore
	// orders is the orders placement over the rig's frame, by Place's rule:
	// the order dispatcher reads a rig order's run evidence there
	// (resolveOrderStore). It is not handed out; Stores.Plan reads it.
	orders beads.OrdersStore
}

// RigWork is one rig's work store, as Stores.Rigs lists it.
type RigWork struct {
	Name string
	Work beads.WorkStore
}

// NewStores mints the handles for t. Every city handle wraps the store Place
// answers for its class over t. Each rig leg of t gets two handles, with the
// same options: its work store (Place ClassWork with the rig leg as the work
// leg) and its graph store (Place ClassGraph over that same frame, the
// binding when graph is relocated, else the rig's own work store). A rig leg
// whose ref is not a rig ref names no rig and is skipped.
func NewStores(t Topology, opts StoresOptions) Stores {
	s := Stores{city: beads.MintClassStores(beads.MintSpec{
		Work:     Place(t, opts, coordclass.ClassWork),
		Graph:    Place(t, opts, coordclass.ClassGraph),
		Sessions: Place(t, opts, coordclass.ClassSessions),
		Mail:     Place(t, opts, coordclass.ClassMessaging),
		Orders:   Place(t, opts, coordclass.ClassOrders),
		Nudges:   Place(t, opts, coordclass.ClassNudges),
	})}
	for _, leg := range t.Rigs {
		name, ok := ScopeRigContext(string(leg.Ref))
		if !ok || name == "" {
			continue
		}
		frame := Topology{Work: leg, Bindings: t.Bindings, Refused: t.Refused}
		h := beads.MintClassStores(beads.MintSpec{
			Work:   Place(frame, opts, coordclass.ClassWork),
			Graph:  Place(frame, opts, coordclass.ClassGraph),
			Orders: Place(frame, opts, coordclass.ClassOrders),
		})
		s.rigs = append(s.rigs, rigStores{name: name, ref: RigRef(name), work: h.Work, graph: h.Graph, orders: h.Orders})
	}
	s.topo = mintedTopology(t, opts, s)
	return s
}

// Place is the store class c lives in on t: the plane's refusal, else the
// binding serving c (planClass's rule), else the work store seen through the
// projection's class view. ClassWork is the work store through the work view.
//
// A refused topology's binding is the refusing store, which planClass reports
// as an error and this hands out, so the refusal reaches the caller through
// every operation rather than as a missing store.
//
// It is the raw form of the router, for the callers the retyping phases
// (T3-T9) have not yet given a handle. NewStores mints every handle from it,
// so the two cannot disagree.
func Place(t Topology, opts StoresOptions, c coordclass.Class) beads.Store {
	work := t.Work.Store
	if c == coordclass.ClassWork {
		if opts.Projection != nil && work != nil {
			return opts.Projection.WorkView(work)
		}
		return work
	}
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

// Work is the city/HQ work store.
func (s Stores) Work() beads.WorkStore { return s.city.Work }

// Graph is the graph store: the binding when graph is relocated, else the
// work store.
func (s Stores) Graph() beads.GraphStore { return s.city.Graph }

// Orders is the orders store, by the same rule as Graph.
func (s Stores) Orders() beads.OrdersStore { return s.city.Orders }

// Sessions is the sessions store.
func (s Stores) Sessions() beads.SessionStore { return s.city.Sessions }

// Mail is the messaging store (mail and extmsg).
func (s Stores) Mail() beads.MailStore { return s.city.Mail }

// Nudges is the nudges store.
func (s Stores) Nudges() beads.NudgesStore { return s.city.Nudges }

// RigLegs is the rig legs of a holder's rig stores, keyed by rig name, in
// name order: a rig whose store did not open has no leg. It is how a holder
// that keeps its rigs in a map hands them to NewStores.
func RigLegs(stores map[string]beads.Store) []Leg {
	names := make([]string, 0, len(stores))
	for name, store := range stores {
		if store != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	legs := make([]Leg, len(names))
	for i, name := range names {
		legs[i] = Leg{Ref: RigRef(name), Store: stores[name]}
	}
	return legs
}

// Rig is rig name's work store, and false for a rig this epoch did not open.
func (s Stores) Rig(name string) (beads.WorkStore, bool) {
	for _, r := range s.rigs {
		if r.name == name {
			return r.work, true
		}
	}
	return beads.WorkStore{}, false
}

// Rigs is every rig this epoch opened, in the topology's order (the holder's:
// cmd/gc's router lists them by name), each with its work store. A holder
// opens suspended rigs too, so they are listed: a suspended rig's store runs
// no cache, and a bd read of it can wake its retired proxy and Dolt. A caller
// that must leave a suspended scope untouched skips it by name.
func (s Stores) Rigs() []RigWork {
	out := make([]RigWork, len(s.rigs))
	for i, r := range s.rigs {
		out[i] = RigWork{Name: r.name, Work: r.work}
	}
	return out
}

// GraphFor is the graph store at scope ref (S4a placement): the binding when
// graph is relocated, else that scope's work store, the city's for WorkRef and
// a rig's for its RigRef. It answers false for a scope this epoch has no store
// frame for.
func (s Stores) GraphFor(ref StoreRef) (beads.GraphStore, bool) {
	if ref == WorkRef {
		return s.city.Graph, true
	}
	for _, r := range s.rigs {
		if r.ref == ref {
			return r.graph, true
		}
	}
	return beads.GraphStore{}, false
}
