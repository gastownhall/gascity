package main

// The legacy front door and the placement plan must name the same store.
//
// resolveClassStore is what every class-scoped write in cmd/gc still goes
// through, and storeref.Plan(Class{…}) is what the resolver answers the same
// question with. They are separate code reading the same routes, and a class
// they disagree about is a class whose writes land in one store while every
// resolver-built reader looks in the other — a bead that exists and cannot be
// found, with no error anywhere to say so.
//
// This is an agreement pin, not a behavior test: neither side is asserted
// against a literal store here, only against each other, so the day one of them
// gains a class the other does not, this fails rather than the fleet.

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// configClasses is every class name resolveClassStore can be asked about.
func configClasses() []string {
	return []string{
		config.BeadClassWork,
		config.BeadClassGraph,
		config.BeadClassMessaging,
		config.BeadClassSessions,
		config.BeadClassOrders,
		config.BeadClassNudges,
	}
}

func TestResolveClassStoreAgreesWithThePlacementPlan(t *testing.T) {
	binding := beads.NewMemStore()
	graphOnly := beads.NewMemStore()
	sessionsOnly := beads.NewMemStore()

	wholeSplit := map[coordclass.Class]beads.Store{}
	for _, class := range coordclass.Classes() {
		if class.IsInfrastructure() {
			wholeSplit[class] = binding
		}
	}

	tests := []struct {
		name   string
		routes *storageRoutes
	}{
		{"single-store city", nil},
		{"whole split", &storageRoutes{stores: wholeSplit, binding: "infra"}},
		{
			// Not a shape this build serves (storageSupportedTopologyStatement),
			// but both sides accept it as data and a disagreement here is the
			// same disagreement.
			name: "per-class split",
			routes: &storageRoutes{stores: map[coordclass.Class]beads.Store{
				coordclass.ClassGraph:    graphOnly,
				coordclass.ClassSessions: sessionsOnly,
			}, binding: "infra"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			work := beads.NewMemStore()
			bindings, refused := residencyBindingsFromRoutes(tt.routes)
			topo := assembleResidencyTopology(nil, work, nil, bindings, refused)

			for _, class := range configClasses() {
				got := resolveClassStore(tt.routes, work, nil, "", class, nil)
				plan, err := storeref.Plan(storeref.Class{C: coordclassFor(class)}, topo)
				if err != nil {
					t.Fatalf("planning placement for %q: %v", class, err)
				}
				if plan.Mode != storeref.ModeSingleOwner || len(plan.Legs) != 1 {
					t.Fatalf("placement plan for %q is %s, want one single-owner leg", class, plan)
				}
				if want := plan.Legs[0].Leg.Store; got != want {
					t.Errorf("%q: the front door writes to %p, the plan reads %p (%s) — a bead written through one is invisible to the other",
						class, got, want, plan.Legs[0].Leg.Ref)
				}
			}
		})
	}
}

// An unrecognized class name is the residual, on both sides: coordclassFor maps
// it to work rather than guessing, so the front door leaves it on the work
// ledger and the plan names the same leg. The control that matters is that it
// is NOT silently routed at whatever binding happens to be open.
func TestAnUnknownClassNameStaysOnTheWorkLedger(t *testing.T) {
	work := beads.NewMemStore()
	binding := beads.NewMemStore()
	stores := map[coordclass.Class]beads.Store{}
	for _, class := range coordclass.Classes() {
		if class.IsInfrastructure() {
			stores[class] = binding
		}
	}
	routes := &storageRoutes{stores: stores, binding: "infra"}

	got := resolveClassStore(routes, work, nil, "", "a-class-this-build-does-not-know", nil)
	if got == binding {
		t.Fatal("an unknown class name routed at the binding; a name this build cannot interpret must never be relocated")
	}
	if got != work {
		t.Fatalf("unknown class resolved to %p, want the work store %p", got, work)
	}
}

// A refused city refuses its classes on both sides. The front door hands back a
// store that answers the refusal, and the plan errors with it; what neither may
// do is answer the work ledger, which is the degraded answer that looks like
// success while reading the store the class was moved off.
func TestRefusedCityNeverPlacesAClassOnTheWorkLedger(t *testing.T) {
	work := beads.NewMemStore()
	routes := refusingStorageRoutes("infra", errors.New("storage: this build cannot serve the configured split"))
	bindings, refused := residencyBindingsFromRoutes(routes)
	topo := assembleResidencyTopology(nil, work, nil, bindings, refused)

	for _, class := range configClasses() {
		got := resolveClassStore(routes, work, nil, "", class, nil)
		_, err := storeref.Plan(storeref.Class{C: coordclassFor(class)}, topo)

		if class == config.BeadClassWork {
			// Work is untouched by a refusal: a refused city still serves its
			// work beads from its work ledger.
			if got != work {
				t.Errorf("work resolved to %p on a refused city, want the work store", got)
			}
			if err != nil {
				t.Errorf("planning work placement on a refused city: %v", err)
			}
			continue
		}
		if got == work {
			t.Errorf("%q resolved to the work ledger on a refused city; the refusal exists to stop exactly that read", class)
		}
		if _, readErr := got.Get("gcg-probe"); !storeref.IsStandingRefusal(readErr) {
			t.Errorf("%q resolved to a store whose read fails with %v rather than the standing refusal; a not-found is what an empty serving store says too", class, readErr)
		}
		if err == nil {
			t.Errorf("planning %q placement on a refused city returned no error", class)
		} else if !storeref.IsStandingRefusal(err) {
			t.Errorf("planning %q placement failed with %v, want the standing storage refusal", class, err)
		}
	}
}
