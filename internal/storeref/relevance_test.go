package storeref

import (
	"slices"
	"strings"
	"testing"
)

// The relevance descriptor's own properties.
//
// Narrow is a FILTER, and every test here exists to keep it one. The failure it
// is guarding against is not "the wrong legs were dropped" — it is a controller
// that answers the latency question by REORDERING the plan, because
// Plan(RoutedWork) puts the binding last on purpose (#5148 co-residence) and a
// consumer-side reorder recreates D6 in reverse.

func TestNarrowKeepsPlanOrderAndOnlyDropsLegs(t *testing.T) {
	for _, f := range allTopologies() {
		if f.topo.Refused != nil {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			full := mustPlan(t, RoutedWork{}, f.topo)
			narrowed, err := Narrow(full, PlaneRuntime)
			if err != nil {
				t.Fatalf("Narrow(runtime): %v", err)
			}
			if narrowed.Mode != full.Mode {
				t.Fatalf("narrowing changed the mode from %s to %s; the descriptor selects legs, not executors", full.Mode, narrowed.Mode)
			}
			// Subsequence, not merely subset: the surviving legs appear in the
			// SAME relative order, with the same role and policy.
			var want []string
			for _, l := range full.Legs {
				if slices.ContainsFunc(narrowed.Legs, func(k PlanLeg) bool { return k.Leg.Ref == l.Leg.Ref }) {
					want = append(want, l.String())
				}
			}
			var got []string
			for _, l := range narrowed.Legs {
				got = append(got, l.String())
			}
			if !slices.Equal(got, want) {
				t.Fatalf("narrowed plan reads\n %v\nbut the full plan's surviving legs are\n %v\n— the descriptor reordered or re-policied a leg", got, want)
			}
		})
	}
}

// TestNarrowRuntimeRefusesTheLedgerOnASplitCity is the operator invariant as a
// resolver property (bd memory gascity-runtime-infra-store-invariant): a
// runtime-plane caller may not even be HANDED a work-ledger leg.
func TestNarrowRuntimeRefusesTheLedgerOnASplitCity(t *testing.T) {
	f := newT2() // work ledger + two rigs + one binding: every leg family at once
	full := mustPlan(t, RoutedWork{}, f.topo)
	narrowed, err := Narrow(full, PlaneRuntime)
	if err != nil {
		t.Fatalf("Narrow(runtime): %v", err)
	}
	for _, l := range narrowed.Legs {
		if !IsClassRef(string(l.Leg.Ref)) {
			t.Fatalf("the runtime plane kept leg %q; on a split city it reads the infra binding and nothing else", l.Leg.Ref)
		}
	}
	// Control: the narrowing is real. The full plan HAS the legs it dropped, so
	// a Narrow that had simply returned its input would fail here.
	if len(narrowed.Legs) >= len(full.Legs) {
		t.Fatalf("the runtime plane kept %d of %d legs; it narrowed nothing", len(narrowed.Legs), len(full.Legs))
	}
}

// TestNarrowReconcileNeverNarrows is the other half of the plane doctrine. The
// two planes are NOT complements: the runtime plane is a latency contract and
// narrows, while the reconcile plane is a CONVERGENCE contract and a leg it
// skips is a leg nothing converges.
func TestNarrowReconcileNeverNarrows(t *testing.T) {
	for _, f := range allTopologies() {
		if f.topo.Refused != nil {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			for _, in := range []struct {
				name   string
				intent Intent
			}{
				{"RoutedWork", RoutedWork{}},
				{"AssignedWork(sweep)", AssignedWork{}},
				{"Census(all)", Census{}},
				{"Session", Session{}},
			} {
				full := mustPlan(t, in.intent, f.topo)
				narrowed, err := Narrow(full, PlaneReconcile)
				if err != nil {
					t.Fatalf("%s: Narrow(reconcile): %v", in.name, err)
				}
				if got, want := narrowed.String(), full.String(); got != want {
					t.Fatalf("%s: the reconcile plane rendered\n %s\nwant the full plan\n %s\n— convergence never narrows", in.name, got, want)
				}
			}
		})
	}
}

// TestNarrowRuntimeKeepsTheOnlyStoreASingleStoreCityHas pins the degradation
// that keeps the invariant from disabling the runtime plane outright: a city
// that relocates no class has no binding, and there its work store IS its infra
// store.
func TestNarrowRuntimeKeepsTheOnlyStoreASingleStoreCityHas(t *testing.T) {
	f := newT0()
	full := mustPlan(t, RoutedWork{}, f.topo)
	narrowed, err := Narrow(full, PlaneRuntime)
	if err != nil {
		t.Fatalf("Narrow(runtime): %v", err)
	}
	if got, want := narrowed.String(), full.String(); got != want {
		t.Fatalf("single-store city narrowed to\n %s\nwant\n %s\n— the rule must degrade to \"the only store there is\", never to \"no store at all\"", got, want)
	}
}

// TestNarrowRefusesToProduceALeglessPlan mirrors Plan's own rule. A legless plan
// is not an empty answer, it is a reader with nothing to read — and a Union over
// one reports zero rows as a complete answer, which is the silent-shrink shape
// this package exists to close.
func TestNarrowRefusesToProduceALeglessPlan(t *testing.T) {
	if _, err := Narrow(ResolvedPlan{Mode: ModeUnion}, PlaneRuntime); err == nil {
		t.Fatal("narrowing a legless plan succeeded; an empty leg list must fail loud")
	}
	// Control: the same call over a plan WITH legs succeeds, so the refusal
	// above is about the leg list and not about the call shape.
	f := newT1()
	if _, err := Narrow(mustPlan(t, RoutedWork{}, f.topo), PlaneRuntime); err != nil {
		t.Fatalf("narrowing a real plan failed: %v", err)
	}
}

func TestPlaneStringIsStable(t *testing.T) {
	if got, want := PlaneRuntime.String(), "runtime"; got != want {
		t.Fatalf("PlaneRuntime renders %q, want %q", got, want)
	}
	if got, want := PlaneReconcile.String(), "reconcile"; got != want {
		t.Fatalf("PlaneReconcile renders %q, want %q", got, want)
	}
	if got := Plane(7).String(); !strings.Contains(got, "7") {
		t.Fatalf("an unknown plane renders %q, which names nothing an operator can act on", got)
	}
}
