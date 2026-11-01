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
// Stores answers for the city scope only, and reads Topology.Work and
// Topology.Bindings. Per-rig handles and federated legs arrive with the phases
// that have callers for them (S4's Graph(scope), T7's plan legs).
//
// NewStores and Place take raw stores and hand out handles over them, so the
// storeclass lint confines their callers (tools/nogo/analyzers/storeclass).

import (
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
	// gets this store, relocated or not.
	ClassRefusals map[coordclass.Class]RefusingClassStore

	// Projection is the split-strict gate's hook. Nil in production.
	Projection Projection
}

// Stores is one epoch's class handles for a city. It is a value: a holder
// swaps it whole when the stores it was minted from are replaced, and a
// caller holding an old one keeps the old epoch's stores.
type Stores struct{ city beads.ClassStores }

// NewStores mints the handles for t. Every handle wraps the store Place
// answers for its class.
func NewStores(t Topology, opts StoresOptions) Stores {
	return Stores{city: beads.MintClassStores(beads.MintSpec{
		Work:     Place(t, opts, coordclass.ClassWork),
		Graph:    Place(t, opts, coordclass.ClassGraph),
		Sessions: Place(t, opts, coordclass.ClassSessions),
		Mail:     Place(t, opts, coordclass.ClassMessaging),
		Orders:   Place(t, opts, coordclass.ClassOrders),
		Nudges:   Place(t, opts, coordclass.ClassNudges),
	})}
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
