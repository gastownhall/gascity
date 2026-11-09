package storeref

// Stores.Topology, Stores.Plan and Stores.Holds (typed-stores T7b, plan
// §4.5): the residency questions answered over one epoch's minted handles.
//
// The topology a holder mints from is the plane's view of its opened routes.
// The topology Stores answers is the MINTED one: every leg is the store the
// handles wrap, after the plane's refusals and the split-strict gate's
// projection, and a class is a binding exactly when its handle is not the
// work handle's store (beads.SameStore). So a production single-store city
// has no binding, and a gate fixture, whose class handles read a class view
// of the one store, keeps the split shape its tests exercise. That is the
// API plane's own rule (internal/api/residency_topology.go), stated once.

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// mintedTopology is t as the handles s minted from it read it: the work leg,
// the rig legs and the bindings over the handles' own stores, with the
// prefixes, census bits and relic-free bits t carried for them.
func mintedTopology(t Topology, s Stores) Topology {
	work := s.city.Work.Raw()
	out := Topology{
		Work:    Leg{Ref: WorkRef, Store: work, Prefix: t.Work.Prefix, RelicFree: t.Work.RelicFree},
		Refused: t.Refused,
	}
	for _, r := range s.rigs {
		leg := Leg{Ref: r.ref, Store: r.work.Raw()}
		for _, have := range t.Rigs {
			if have.Ref == r.ref {
				leg.Prefix, leg.RelicFree = have.Prefix, have.RelicFree
			}
		}
		out.Rigs = append(out.Rigs, leg)
	}

	// Group the relocated classes by the store their handle wraps. A store is
	// keyed by its first handle's raw store; identity is physical.
	var order []beads.Store
	byStore := map[beads.Store][]coordclass.Class{}
	for _, class := range coordclass.Classes() {
		if !class.IsInfrastructure() {
			continue
		}
		store := s.cityRaw(class)
		if beads.IsNilStore(store) || beads.SameStore(store, work) {
			continue
		}
		key := store
		for _, have := range order {
			if beads.SameStore(have, store) {
				key = have
				break
			}
		}
		if _, seen := byStore[key]; !seen {
			order = append(order, key)
		}
		byStore[key] = append(byStore[key], class)
	}
	// A binding the plane built over the same store and classes is carried
	// as it is, census and mint bits included. A binding it did not build
	// (the gate's class view, a plane's per-class refusal) is built here with
	// the pessimistic defaults.
	var fresh []beads.Store
	for _, store := range order {
		if b, ok := bindingOver(t, store); ok && sameClassSet(b.Classes, byStore[store]) {
			b.Leg.Store = store
			out.Bindings = append(out.Bindings, b)
			continue
		}
		fresh = append(fresh, store)
	}
	built, refused := BuildBindings(fresh, byStore, BindingOptions{})
	out.Bindings = append(out.Bindings, built...)
	out.Bindings = (Topology{Bindings: out.Bindings}).orderedBindings()
	if out.Refused == nil {
		out.Refused = refused
	}
	return out
}

// sameClassSet reports whether a and b name the same classes.
func sameClassSet(a, b []coordclass.Class) bool {
	if len(a) != len(b) {
		return false
	}
	for _, c := range a {
		if !containsClass(b, c) {
			return false
		}
	}
	return true
}

func containsClass(classes []coordclass.Class, want coordclass.Class) bool {
	for _, c := range classes {
		if c == want {
			return true
		}
	}
	return false
}

// bindingOver is the binding of t over store, by physical identity.
func bindingOver(t Topology, store beads.Store) (ClassBinding, bool) {
	for _, b := range t.Bindings {
		if beads.SameStore(b.Leg.Store, store) {
			return b, true
		}
	}
	return ClassBinding{}, false
}

// Topology is the epoch's minted topology: what Plan reads for the existing
// intents, and the one every plane answers residency from (the API's
// residencyTopology, the CLI's and the controller's, agree with it; see
// TestStoresTopologyAgreesWithEveryPlane). Its rigs are every rig the holder
// opened, suspended ones included, as Rigs lists them; a plane that must not
// read a suspended scope leaves it out of the legs it reads. Treat it as
// read-only: it shares its slices with the epoch.
func (s Stores) Topology() Topology { return s.topo }

// Holds is the classes the store named ref holds on this city, or nil on a
// single-store city, where the one store holds every class: a binding holds
// the classes it serves, and any other store (the city's or a rig's work
// store) holds work plus every class no binding serves. It is the class
// guard's question (coordclass.CheckFlip; the API's PATCH door, and T8's
// guard), asked by ref, so a handle never has to be compared with a leg.
func (s Stores) Holds(ref StoreRef) []coordclass.Class {
	t := s.topo
	if t.IsSingleStore() {
		return nil
	}
	for _, b := range t.Bindings {
		if b.Leg.Ref == ref {
			return append([]coordclass.Class(nil), b.Classes...)
		}
	}
	holds := []coordclass.Class{coordclass.ClassWork}
	for _, class := range coordclass.Classes() {
		if _, served := t.BindingFor(class); class.IsInfrastructure() && !served {
			holds = append(holds, class)
		}
	}
	return holds
}

// Plan resolves an intent over the epoch. The T7 federation intents
// (Molecule, OrderRuns, Workflow) are planned over the minted handles,
// deciding relocation by handle identity; every other intent is the topology
// Plan over Topology(). A standing refusal is not raised here for the T7
// intents: their binding legs ARE the refusing stores, so the refusal reaches
// the reader through the leg and its policy (an attachment probe still finds
// a v1 molecule past a refusing graph leg).
func (s Stores) Plan(i Intent) (ResolvedPlan, error) {
	var (
		plan ResolvedPlan
		err  error
	)
	switch in := i.(type) {
	case Molecule:
		plan, err = s.planMolecule(in)
	case OrderRuns:
		plan, err = s.planOrderRuns(in)
	case Workflow:
		plan, err = s.planWorkflow(in)
	default:
		return Plan(i, s.topo)
	}
	if err != nil {
		return ResolvedPlan{}, err
	}
	plan = dedupeLegs(plan)
	if len(plan.Legs) == 0 {
		return ResolvedPlan{}, fmt.Errorf("storeref: %T resolved to no readable leg — the epoch has no work store", i)
	}
	return plan, nil
}

// relocated reports whether the graph class lives off the work store: by
// handle identity, so the gate's class view counts.
func (s Stores) relocated() bool { return !beads.SameStore(s.city.Graph, s.city.Work) }

// leg is a federated plan leg over store, named for the binding it is, else
// for scope: a leg whose miss another leg covers.
func (s Stores) leg(store beads.Store, scope StoreRef, role Role, onError ErrPolicy) PlanLeg {
	ref := scope
	if b, ok := bindingOver(s.topo, store); ok {
		ref = b.Leg.Ref
	}
	return PlanLeg{Leg: Leg{Ref: ref, Store: store}, Role: role, OnError: onError, Gate: beads.FederatedLegRole}
}

// graphAt is the graph placement at scope.
func (s Stores) graphAt(scope StoreRef) (beads.Store, error) {
	g, ok := s.GraphFor(scope)
	if !ok {
		return nil, fmt.Errorf("storeref: no store frame for scope %q in this epoch", legName(scope))
	}
	return g.Raw(), nil
}

// workAt is the work store at scope.
func (s Stores) workAt(scope StoreRef) (beads.Store, error) {
	if scope == WorkRef {
		return s.city.Work.Raw(), nil
	}
	if name, ok := ScopeRigContext(string(scope)); ok && name != "" {
		if w, ok := s.Rig(name); ok {
			return w.Raw(), nil
		}
	}
	return nil, fmt.Errorf("storeref: no store frame for scope %q in this epoch", legName(scope))
}

// planMolecule pins each molecule kind's legs (intents.go).
func (s Stores) planMolecule(in Molecule) (ResolvedPlan, error) {
	graph, err := s.graphAt(in.Scope)
	if err != nil {
		return ResolvedPlan{}, err
	}
	graphLeg := s.leg(graph, in.Scope, RoleAuthority, PolicyFatal)
	near := PlanLeg{Leg: Leg{Ref: in.NearRef, Store: in.Near}, Role: RoleNear, OnError: PolicyFatal, Gate: beads.FederatedLegRole}
	switch in.Kind {
	case MoleculeRootNear:
		return ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{graphLeg, near}}, nil
	case MoleculeParentNear:
		return ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{near, graphLeg}}, nil
	case MoleculeAttachment:
		work, err := s.workAt(in.Scope)
		if err != nil {
			return ResolvedPlan{}, err
		}
		graphLeg.OnError = PolicyFatalUnlessResolved
		return ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{
			graphLeg,
			s.leg(work, in.Scope, RoleV1, PolicyFatalUnlessResolved),
		}}, nil
	case MoleculeRoots:
		legs := []PlanLeg{graphLeg, s.leg(s.city.Work.Raw(), WorkRef, RoleV1, PolicyPartialDegrade)}
		for _, r := range s.topo.orderedRigs() {
			legs = append(legs, s.leg(r.Store, r.Ref, RoleV1, PolicyPartialDegrade))
		}
		return ResolvedPlan{Mode: ModeUnion, Legs: legs}, nil
	default:
		return ResolvedPlan{}, fmt.Errorf("storeref: unknown molecule kind %d", in.Kind)
	}
}

// planOrderRuns is the order-run evidence union: orders, graph, then the
// scope's work store.
func (s Stores) planOrderRuns(in OrderRuns) (ResolvedPlan, error) {
	graph, err := s.graphAt(in.Scope)
	if err != nil {
		return ResolvedPlan{}, err
	}
	work, err := s.workAt(in.Scope)
	if err != nil {
		return ResolvedPlan{}, err
	}
	return ResolvedPlan{Mode: ModeUnion, Legs: []PlanLeg{
		s.leg(s.city.Orders.Raw(), WorkRef, RoleAuthority, PolicyFatal),
		s.leg(graph, in.Scope, RoleAuthority, PolicyFatal),
		s.leg(work, in.Scope, RoleV1, PolicyFatal),
	}}, nil
}

// planWorkflow is the workflow-root union: the relocated graph leg, the city,
// then the rigs ascending, each scope leg a relic leg on a relocated city
// unless its scope is proven relic-free.
func (s Stores) planWorkflow(in Workflow) (ResolvedPlan, error) {
	scopePolicy, graphPolicy := PolicyPartialDegrade, PolicyPartialDegrade
	switch in.Door {
	case WorkflowDoorRead:
	case WorkflowDoorSourceGuard:
		graphPolicy = PolicyFatal
	default:
		return ResolvedPlan{}, fmt.Errorf("storeref: unknown workflow door %d", in.Door)
	}
	relocated := s.relocated()
	plan := ResolvedPlan{Mode: ModeUnion}
	if relocated {
		plan.Legs = append(plan.Legs, s.leg(s.city.Graph.Raw(), WorkRef, RoleAuthority, graphPolicy))
	}
	scope := func(l Leg, keepRetired bool) {
		switch {
		case !relocated:
			plan.Legs = append(plan.Legs, PlanLeg{Leg: l, Role: RoleAuthority, OnError: scopePolicy, Gate: beads.FederatedLegRole})
		case !l.RelicFree:
			plan.Legs = append(plan.Legs, PlanLeg{Leg: l, Role: RoleRelic, OnError: scopePolicy, Gate: beads.RelicLegRole})
		case keepRetired:
			plan.Legs = append(plan.Legs, PlanLeg{Leg: l, Role: RoleFederationTail, OnError: scopePolicy, Gate: beads.FederatedLegRole})
		}
	}
	scope(s.topo.Work, false)
	for _, r := range s.topo.orderedRigs() {
		scope(r, in.IncludeRetiredRigs)
	}
	return plan, nil
}

// cityRaw is the store class c's city handle wraps.
func (s Stores) cityRaw(c coordclass.Class) beads.Store {
	switch c {
	case coordclass.ClassWork:
		return s.city.Work.Raw()
	case coordclass.ClassGraph:
		return s.city.Graph.Raw()
	case coordclass.ClassSessions:
		return s.city.Sessions.Raw()
	case coordclass.ClassMessaging:
		return s.city.Mail.Raw()
	case coordclass.ClassOrders:
		return s.city.Orders.Raw()
	case coordclass.ClassNudges:
		return s.city.Nudges.Raw()
	default:
		return nil
	}
}
