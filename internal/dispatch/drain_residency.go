package dispatch

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// drainResidency is the frame a drain resolves ids over: the routed work class
// store, plus the drain's own ambient store as the one class binding.
//
// It is built here rather than threaded down from the city on purpose — a city
// topology federates every serving rig, and handing that to a drain would let a
// drain resolve a member out of another scope. The two stores the caller
// already routed are the whole frame.
//
// Relics is nil, which BuildBindings reads as "may hold legacy residents" and
// so keeps the residence probe. That is the honest answer for a caller that
// took no census.
func drainResidency(store beads.Store, opts ProcessOptions) (storeref.Topology, error) {
	if len(opts.MemberStores) == 0 {
		return storeref.Topology{Work: storeref.Leg{Ref: storeref.WorkRef, Store: store}}, nil
	}
	if len(opts.MemberStores) > 1 {
		return storeref.Topology{}, fmt.Errorf("drain residency: %d member stores, want at most 1", len(opts.MemberStores))
	}
	order := []beads.Store{store} // residency:allow one ambient binding, grouped for the shared BuildBindings derivation below
	bindings, refused := storeref.BuildBindings(
		order,
		map[beads.Store][]coordclass.Class{store: drainInfrastructureClasses()},
		storeref.BindingOptions{},
	)
	return storeref.Topology{
		Work:     storeref.Leg{Ref: storeref.WorkRef, Store: drainWorkClassStore(store, opts)},
		Bindings: bindings,
		Refused:  refused,
	}, nil
}

// drainInfrastructureClasses is every coordination class but work — the set a
// class split relocates. Derived rather than listed so a new class joins the
// binding the day it is declared.
func drainInfrastructureClasses() []coordclass.Class {
	classes := make([]coordclass.Class, 0, len(coordclass.Classes()))
	for _, class := range coordclass.Classes() {
		if class.IsInfrastructure() {
			classes = append(classes, class)
		}
	}
	return classes
}

// resolveDrainMember answers which store holds a member the drain reads but did
// not mint, and what that store's row says.
//
// The intent is ByID, not RoutedWork: a member's class is not statically known,
// so a binding copy is the live relocated bead and the work store's copy is the
// frozen one a migration retained. (A drain unit convoy is the opposite — the
// drain mints it and coordclass pins it to work — which is why that resolution
// plans RoutedWork instead.)
//
// found=false covers both of ResolveOwnerRow's miss shapes: an id inside a
// reserved namespace misses with a nil store, and an ordinary id ends in the
// unprobed work residual whose own read decides the question. Callers must not
// write through either.
func resolveDrainMember(store beads.Store, memberID string, opts ProcessOptions) (storeref.Owner, bool, error) {
	topo, err := drainResidency(store, opts)
	if err != nil {
		return storeref.Owner{}, false, err
	}
	plan, err := storeref.Plan(storeref.ByID{ID: memberID}, topo)
	if err != nil {
		return storeref.Owner{}, false, err
	}
	owner, err := storeref.ResolveOwnerRow(plan, memberID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return storeref.Owner{}, false, nil
		}
		return storeref.Owner{}, false, err
	}
	if owner.Read {
		return owner, true, nil
	}
	bead, err := owner.Store.Get(memberID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return storeref.Owner{}, false, nil
		}
		return storeref.Owner{}, false, err
	}
	owner.Bead, owner.Read = bead, true
	return owner, true, nil
}
