package storeref

// Stores.Topology, Stores.Plan and Stores.Holds (typed-stores T7b, plan
// §4.5): the residency questions answered over one epoch's minted handles.
//
// The topology a holder mints from is the plane's view of its opened routes.
// The topology Stores answers is the MINTED one: every leg is the store the
// handles wrap after the split-strict gate's projection, and a class is a
// binding exactly when its placement is not the work handle's store
// (beads.SameStore). So a production single-store city has no binding, and a
// gate fixture, whose class handles read a class view of the work store,
// keeps the split shape its tests exercise.
//
// A plane's per-class refusal (StoresOptions.ClassRefusals, cmd/gc's worker
// site) is a property of that class's HANDLE, not of the topology: the handle
// refuses every operation, and the topology names the binding the routes
// serve, so the minted refs are the ones the route planes name
// (class:gmnos). The topology carries the plane's own refusal, and fails
// closed when the per-class refusals cover every relocated class
// (everyRelocatedClassRefused); a binding whose every class is refused is
// minted over the refusing store, under its own ref, so its leg refuses too.

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// mintedTopology is t as the handles s minted from it read it: the work leg,
// the rig legs and the bindings over the handles' own stores, with the
// prefixes, census bits, relic-free and suspension bits t carried for them.
// The bindings are grouped from each class's placement without the plane's
// per-class refusals (see the file comment); a binding the plane refuses in
// full is then minted over its refusing store (bindingRefusal).
func mintedTopology(t Topology, opts StoresOptions, s Stores) Topology {
	work := s.city.Work.Raw()
	out := Topology{
		Work:    Leg{Ref: WorkRef, Store: work, Prefix: t.Work.Prefix, RelicFree: t.Work.RelicFree},
		Refused: t.Refused,
	}
	for _, r := range s.rigs {
		leg := Leg{Ref: r.ref, Store: r.work.Raw()}
		for _, have := range t.Rigs {
			if have.Ref == r.ref {
				leg.Prefix, leg.RelicFree, leg.Suspended = have.Prefix, have.RelicFree, have.Suspended
			}
		}
		out.Rigs = append(out.Rigs, leg)
	}

	// Group the relocated classes by the store their placement wraps. A
	// store is keyed by its first class's store; identity is physical.
	placement := StoresOptions{Projection: opts.Projection}
	var order []beads.Store
	byStore := map[beads.Store][]coordclass.Class{}
	for _, class := range coordclass.Classes() {
		if !class.IsInfrastructure() {
			continue
		}
		store := Place(t, placement, class)
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
	// (the gate's class view) is built here with the pessimistic defaults;
	// its refusal, if it had one, is not the plane's and is not carried.
	var fresh []beads.Store
	for _, store := range order {
		if b, ok := bindingOver(t, store); ok && sameClassSet(b.Classes, byStore[store]) {
			b.Leg.Store = store
			out.Bindings = append(out.Bindings, b)
			continue
		}
		fresh = append(fresh, store)
	}
	built, _ := BuildBindings(fresh, byStore, BindingOptions{})
	out.Bindings = append(out.Bindings, built...)
	out.Bindings = (Topology{Bindings: out.Bindings}).orderedBindings()
	if out.Refused == nil {
		out.Refused = everyRelocatedClassRefused(out.Bindings, opts)
	}
	for i, b := range out.Bindings {
		if refusal, ok := bindingRefusal(b, opts); ok {
			out.Bindings[i].Leg.Store = refusal
		}
	}
	return out
}

// bindingRefusal is the store that refuses b when the plane refuses every one
// of b's classes (StoresOptions.ClassRefusals), and false otherwise. Such a
// binding's leg is minted over it, under the binding's own ref: the routes
// name the binding, and every read of it, a by-id probe included, refuses as
// each of its class handles does. That is what keeps a refused topology's
// binding legs refusing (planByID reads the legs, not Refused). A binding
// whose classes are refused only in part keeps the store: the classes the
// plane still serves are served from it.
func bindingRefusal(b ClassBinding, opts StoresOptions) (beads.Store, bool) {
	var first beads.Store
	for _, class := range b.Classes {
		refusal, ok := opts.ClassRefusals[class]
		if !ok || refusal == nil {
			return nil, false
		}
		if first == nil {
			first = refusal
		}
	}
	return first, first != nil
}

// everyRelocatedClassRefused is the plane's refusal when its per-class
// refusals cover every class the topology relocates (a worker site whose
// descriptor attaches none of the binding's classes), and nil otherwise. Such
// a city serves no relocated class from anywhere, so the topology fails
// closed, as the one-shot CLI funnel's routes do there: the refs stay the
// binding's (class:gmnos), and a plan that touches the binding refuses. A
// city that relocates nothing is never refused by this.
func everyRelocatedClassRefused(bindings []ClassBinding, opts StoresOptions) error {
	var first error
	for _, b := range bindings {
		for _, class := range b.Classes {
			refusal, ok := opts.ClassRefusals[class]
			if !ok || refusal == nil {
				return nil
			}
			if first == nil {
				first = refusal.StorageRefusal()
			}
		}
	}
	return first
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

// Topology is the epoch's minted topology. Planned with Plan, it answers
// every existing intent as the plane topology it was minted from does
// (TestStoresTopologyPlansAsTheTopologyItWasMintedFrom; across planes,
// cmd/gc's TestStoresTopologyAgreesWithTheControllerPlanes and
// TestStoresTopologyAgreesWithTheCLIPlane, and internal/api's
// TestFakeStateTopologyMatchesProduction). Its rigs are every rig the holder
// opened, a suspended one marked (Leg.Suspended) and read by no plan. Treat
// it as read-only: it shares its slices with the epoch.
//
// A plan can carry one ref twice: under the split-strict gate's projection a
// scope's class view and work view are two legs of one scope, and a plane's
// per-class refusal is a leg of its own under the binding's ref. A consumer
// keys a leg on its reader or its position, never on its ref alone.
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

// Plan plans a T7 federation intent (Molecule, OrderRuns, Workflow, Convoy,
// CloseOwner) over the
// epoch's minted handles, deciding relocation by handle identity (OD-T7-5).
// Every other intent is the plane's topology question and is refused here: a
// plane plans it with Plan over its own topology.
//
// A standing refusal is not raised here: the binding legs ARE the refusing
// stores, so the refusal reaches the reader through the leg and its policy
// (an attachment probe still finds a v1 molecule past a refusing graph leg).
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
	case Convoy:
		plan, err = s.planConvoy(in)
	case CloseOwner:
		plan, err = s.planCloseOwner(in)
	default:
		return ResolvedPlan{}, fmt.Errorf("storeref: %T is the plane's topology question; plan it with Plan over the plane's topology, not Stores.Plan", i)
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

// leg is a federated plan leg over store, the placement of class c at scope:
// a leg whose miss another leg covers. It is named for the binding it is,
// else for the binding the routes serve c from (a plane's per-class refusal
// is that binding's leg, refusing), else for scope.
func (s Stores) leg(c coordclass.Class, store beads.Store, scope StoreRef, role Role, onError ErrPolicy) PlanLeg {
	ref := scope
	if b, ok := bindingOver(s.topo, store); ok {
		ref = b.Leg.Ref
	} else if b, ok := s.topo.BindingFor(c); ok && c != coordclass.ClassWork {
		ref = b.Leg.Ref
	}
	return PlanLeg{Leg: Leg{Ref: ref, Store: store}, Role: role, OnError: onError, Gate: beads.FederatedLegRole}
}

// rigFrame is the rig scope ref names, or false.
func (s Stores) rigFrame(ref StoreRef) (rigStores, bool) {
	for _, r := range s.rigs {
		if r.ref == ref {
			return r, true
		}
	}
	return rigStores{}, false
}

// noFrame is the error for a scope this epoch holds no store frame for.
func noFrame(scope StoreRef) error {
	return fmt.Errorf("storeref: no store frame for scope %q in this epoch", legName(scope))
}

// graphAt is the graph placement at scope.
func (s Stores) graphAt(scope StoreRef) (beads.Store, error) {
	g, ok := s.GraphFor(scope)
	if !ok {
		return nil, noFrame(scope)
	}
	return g.Raw(), nil
}

// workAt is the work store at scope.
func (s Stores) workAt(scope StoreRef) (beads.Store, error) {
	if scope == WorkRef {
		return s.city.Work.Raw(), nil
	}
	if r, ok := s.rigFrame(scope); ok {
		return r.work.Raw(), nil
	}
	return nil, noFrame(scope)
}

// ordersAt is the orders placement at scope: the city's, or the rig frame's
// (the binding when orders is relocated, else the rig's own store).
func (s Stores) ordersAt(scope StoreRef) (beads.Store, error) {
	if scope == WorkRef {
		return s.city.Orders.Raw(), nil
	}
	if r, ok := s.rigFrame(scope); ok {
		return r.orders.Raw(), nil
	}
	return nil, noFrame(scope)
}

// nearLeg is the intent's Near store as a leg of role, or false for none.
func nearLeg(in Molecule, role Role, onError ErrPolicy) (PlanLeg, bool) {
	if beads.IsNilStore(in.Near) {
		return PlanLeg{}, false
	}
	return PlanLeg{Leg: Leg{Ref: in.NearRef, Store: in.Near}, Role: role, OnError: onError, Gate: beads.FederatedLegRole}, true
}

// planMolecule pins each molecule kind's legs (intents.go).
func (s Stores) planMolecule(in Molecule) (ResolvedPlan, error) {
	graph, err := s.graphAt(in.Scope)
	if err != nil {
		return ResolvedPlan{}, err
	}
	switch in.Kind {
	case MoleculeRootNear, MoleculeParentNear:
		plan := ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{s.leg(coordclass.ClassGraph, graph, in.Scope, RoleAuthority, PolicyFatal)}}
		if near, ok := nearLeg(in, RoleNear, PolicyFatal); ok {
			if in.Kind == MoleculeParentNear {
				plan.Legs = []PlanLeg{near, plan.Legs[0]}
			} else {
				plan.Legs = append(plan.Legs, near)
			}
		}
		return plan, nil
	case MoleculeAttachment:
		plan := ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{s.leg(coordclass.ClassGraph, graph, in.Scope, RoleAuthority, PolicyFatalUnlessResolved)}}
		if near, ok := nearLeg(in, RoleV1, PolicyFatalUnlessResolved); ok {
			plan.Legs = append(plan.Legs, near)
		}
		return plan, nil
	case MoleculeRoots:
		plan := ResolvedPlan{Mode: ModeUnion, Legs: []PlanLeg{s.leg(coordclass.ClassGraph, graph, in.Scope, RoleAuthority, PolicyFatal)}}
		for _, ref := range in.Work {
			store, err := s.storeByRef(ref)
			if err != nil {
				return ResolvedPlan{}, fmt.Errorf("storeref: Molecule(Roots) work leg: %w", err)
			}
			plan.Legs = append(plan.Legs, PlanLeg{Leg: Leg{Ref: ref, Store: store}, Role: RoleV1, OnError: PolicyPartialDegrade, Gate: beads.FederatedLegRole})
		}
		return plan, nil
	case MoleculeActive:
		plan := ResolvedPlan{Mode: ModeUnion, Legs: []PlanLeg{s.leg(coordclass.ClassGraph, graph, in.Scope, RoleAuthority, PolicyFatal)}}
		if near, ok := nearLeg(in, RoleV1, PolicyFatal); ok {
			plan.Legs = append(plan.Legs, near)
		}
		return plan, nil
	default:
		return ResolvedPlan{}, fmt.Errorf("storeref: unknown molecule kind %d", in.Kind)
	}
}

// planOrderRuns is gateStoresFor's leg list (intents.go OrderRuns).
func (s Stores) planOrderRuns(in OrderRuns) (ResolvedPlan, error) {
	work, err := s.workAt(in.Scope)
	if err != nil {
		return ResolvedPlan{}, err
	}
	graph, err := s.graphAt(in.Scope)
	if err != nil {
		return ResolvedPlan{}, err
	}
	orders, err := s.ordersAt(in.Scope)
	if err != nil {
		return ResolvedPlan{}, err
	}
	workLegs := []PlanLeg{s.leg(coordclass.ClassWork, work, in.Scope, RoleAuthority, PolicyFatal)}
	if in.LegacyCity && in.Scope != WorkRef {
		workLegs = append(workLegs, s.leg(coordclass.ClassWork, s.city.Work.Raw(), WorkRef, RoleFederationTail, PolicyFatal))
	}
	var infra []PlanLeg
	if !legsHold(workLegs, graph) {
		infra = append(infra, s.leg(coordclass.ClassGraph, graph, in.Scope, RoleAuthority, PolicyFatal))
	}
	if !legsHold(workLegs, orders) && !legsHold(infra, orders) {
		infra = append(infra, s.leg(coordclass.ClassOrders, orders, in.Scope, RoleAuthority, PolicyFatal))
	}
	if len(infra) == 0 {
		return ResolvedPlan{Mode: ModeUnion, Legs: workLegs}, nil
	}
	if in.PoursWork != nil && !legsHold(infra, work) && in.PoursWork() {
		infra = append(infra, s.leg(coordclass.ClassWork, work, in.Scope, RoleV1, PolicyFatal))
	}
	return ResolvedPlan{Mode: ModeUnion, Legs: infra}, nil
}

// legsHold reports whether a leg of legs is store, physically.
func legsHold(legs []PlanLeg, store beads.Store) bool {
	for _, l := range legs {
		if beads.SameStore(l.Leg.Store, store) {
			return true
		}
	}
	return false
}

// planWorkflow is the workflow-root union: the relocated graph leg, the city,
// then the rigs ascending, each scope leg a relic leg on a relocated city
// unless it is a rig proven relic-free. Every leg is Fatal (intents.go).
func (s Stores) planWorkflow(in Workflow) (ResolvedPlan, error) {
	if in.SuspendedRigs != WorkflowReadsSuspendedRigs && in.SuspendedRigs != WorkflowSkipsSuspendedRigs {
		return ResolvedPlan{}, fmt.Errorf("storeref: a Workflow plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got %d", in.SuspendedRigs)
	}
	relocated := s.relocated()
	plan := ResolvedPlan{Mode: ModeUnion}
	if relocated {
		plan.Legs = append(plan.Legs, s.leg(coordclass.ClassGraph, s.city.Graph.Raw(), WorkRef, RoleAuthority, PolicyFatal))
	}
	scope := func(l Leg, relicFree, keepRetired bool) {
		switch {
		case !relocated:
			plan.Legs = append(plan.Legs, PlanLeg{Leg: l, Role: RoleFederationTail, OnError: PolicyFatal, Gate: beads.FederatedLegRole})
		case !relicFree:
			plan.Legs = append(plan.Legs, PlanLeg{Leg: l, Role: RoleRelic, OnError: PolicyFatal, Gate: beads.RelicLegRole})
		case keepRetired:
			plan.Legs = append(plan.Legs, PlanLeg{Leg: l, Role: RoleFederationTail, OnError: PolicyFatal, Gate: beads.FederatedLegRole})
		}
	}
	// The city leg is never retired (OD-T7-11): Work.RelicFree is not read.
	scope(s.topo.Work, false, false)
	rigs := s.topo.orderedRigs()
	if in.SuspendedRigs == WorkflowReadsSuspendedRigs {
		rigs = s.topo.orderedAllRigs()
	}
	for _, r := range rigs {
		scope(r, r.RelicFree, in.IncludeRetiredRigs)
	}
	return plan, nil
}

// storeByRef is the store ref names in this epoch: the city work store for
// WorkRef, a rig's work store for its ref (suspended or not: the caller named
// it), a binding's store for its ref. Any other ref is an error.
func (s Stores) storeByRef(ref StoreRef) (beads.Store, error) {
	if ref == WorkRef {
		return s.city.Work.Raw(), nil
	}
	if r, ok := s.rigFrame(ref); ok {
		return r.work.Raw(), nil
	}
	for _, b := range s.topo.Bindings {
		if b.Leg.Ref == ref {
			return b.Leg.Store, nil
		}
	}
	return nil, fmt.Errorf("storeref: this epoch holds no store for %q", legName(ref))
}

// refOf names store in this epoch: its binding's ref, the city work ref, a
// rig's ref, or fallback when it is none of them.
func (s Stores) refOf(store beads.Store, fallback StoreRef) StoreRef {
	if b, ok := bindingOver(s.topo, store); ok {
		return b.Leg.Ref
	}
	if beads.SameStore(store, s.city.Work.Raw()) {
		return WorkRef
	}
	for _, r := range s.rigs {
		if beads.SameStore(store, r.work.Raw()) {
			return r.ref
		}
	}
	return fallback
}

// isWorkStore reports whether store is the city's or a rig's work store in
// this epoch, by physical identity.
func (s Stores) isWorkStore(store beads.Store) bool {
	if beads.SameStore(store, s.city.Work.Raw()) {
		return true
	}
	for _, r := range s.rigs {
		if beads.SameStore(store, r.work.Raw()) {
			return true
		}
	}
	return false
}

// workHome is the work store id's prefix names: the opened rig (suspended or
// not) whose configured prefix is the longest id carries, else the city work
// store.
func (s Stores) workHome(id string) Leg {
	home := Leg{Ref: WorkRef, Store: s.city.Work.Raw()}
	best := -1
	for _, leg := range s.topo.Rigs {
		r, ok := s.rigFrame(leg.Ref)
		if !ok || leg.Prefix == "" || !strings.HasPrefix(id, leg.Prefix+"-") || len(leg.Prefix) <= best {
			continue
		}
		best = len(leg.Prefix)
		home = Leg{Ref: r.ref, Store: r.work.Raw()}
	}
	return home
}

// classReservedID reports whether id is in any reserved class namespace, the
// binding-minted ids, relocated or not (cmd/gc bdIDIsClassReserved's rule).
func classReservedID(id string) bool {
	for _, prefix := range beadmeta.AllReservedClassPrefixes() {
		if prefix != "" && strings.HasPrefix(id, prefix+"-") {
			return true
		}
	}
	return false
}

// planConvoy pins each convoy kind's legs (intents.go).
func (s Stores) planConvoy(in Convoy) (ResolvedPlan, error) {
	if beads.IsNilStore(in.Near) {
		return ResolvedPlan{}, fmt.Errorf("storeref: a Convoy(%s) plan needs its Near store", in.Kind)
	}
	near := PlanLeg{Leg: Leg{Ref: in.NearRef, Store: in.Near}, Role: RoleNear, OnError: PolicyFatal, Gate: beads.FederatedLegRole}
	switch in.Kind {
	case ConvoyParent:
		id := strings.TrimSpace(in.ID)
		if id == "" {
			return ResolvedPlan{}, fmt.Errorf("storeref: a Convoy(%s) plan needs the parent's id", in.Kind)
		}
		plan := ResolvedPlan{Mode: ModeFirstOwner, ID: id, Legs: []PlanLeg{near}}
		if !s.isWorkStore(in.Near) && !classReservedID(id) {
			plan.Legs = append(plan.Legs, PlanLeg{Leg: s.workHome(id), Role: RoleFederationTail, OnError: PolicyFatal, Gate: beads.FederatedLegRole})
		}
		return plan, nil
	case ConvoyTracking:
		graph, err := s.graphAt(in.Scope)
		if err != nil {
			return ResolvedPlan{}, err
		}
		// Plan dedupes the graph leg when it is Near: one strict read.
		near.Gate = beads.NoLegRole
		return ResolvedPlan{Mode: ModeUnion, Legs: []PlanLeg{near, s.leg(coordclass.ClassGraph, graph, in.Scope, RoleFederationTail, PolicyFatal)}}, nil
	case ConvoyChildren:
		if beads.IsNilStore(in.Elsewhere) || beads.SameStore(in.Elsewhere, in.Near) {
			near.Gate = beads.NoLegRole
			return ResolvedPlan{Mode: ModeUnion, Legs: []PlanLeg{near}}, nil
		}
		elsewhere := PlanLeg{Leg: Leg{Ref: s.refOf(in.Elsewhere, "elsewhere"), Store: in.Elsewhere}, Role: RoleFederationTail, OnError: PolicyFatal, Gate: beads.FederatedLegRole}
		return ResolvedPlan{Mode: ModeUnion, Legs: []PlanLeg{near, elsewhere}}, nil
	default:
		return ResolvedPlan{}, fmt.Errorf("storeref: unknown convoy kind %d", in.Kind)
	}
}

// planCloseOwner is the route's legs, then the relic's (intents.go
// CloseOwner).
func (s Stores) planCloseOwner(in CloseOwner) (ResolvedPlan, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return ResolvedPlan{}, fmt.Errorf("storeref: a CloseOwner plan needs the event's bead id")
	}
	if len(in.Route) == 0 && in.Relic == "" {
		// A relic alone is a plan: the event's prefix names a store this
		// epoch did not open (an unbound rig), and the binding the residence
		// probe proved holds the id is the one place left to confirm it.
		return ResolvedPlan{}, fmt.Errorf("storeref: a CloseOwner plan for %q needs the event's route or its relic", id)
	}
	plan := ResolvedPlan{Mode: ModeFirstOwner, ID: id}
	add := func(ref StoreRef, role Role) error {
		store, err := s.storeByRef(ref)
		if err != nil {
			return fmt.Errorf("storeref: CloseOwner(%q): %w", id, err)
		}
		plan.Legs = append(plan.Legs, PlanLeg{Leg: Leg{Ref: ref, Store: store}, Role: role, OnError: PolicyFatal, Gate: beads.FederatedLegRole})
		return nil
	}
	for _, ref := range in.Route {
		if err := add(ref, RoleAuthority); err != nil {
			return ResolvedPlan{}, err
		}
	}
	if in.Relic != "" {
		if err := add(in.Relic, RoleFederationTail); err != nil {
			return ResolvedPlan{}, err
		}
	}
	return plan, nil
}
